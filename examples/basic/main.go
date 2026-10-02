// Command basic is the example project's deploy step: it runs the bun
// migrations, the schema's and the fixture's, and seeds a database that has
// never been seeded.
//
//	export DATABASE_URL=postgres://user:password@localhost:5432/example?sslmode=disable
//	go run . migrate    # run what is pending, then seed an empty database
//	go run . rollback   # roll the last group back
//	go run . status     # list the migrations and whether each ran
//
// LOCK_WAIT, default 1m, is how long it waits for another deploy to release
// the migrator's lock.
//
// Fixture migrations change a database that holds the fixture data already,
// and leave one that does not alone: plans is their seed guard table. A new
// database gets the fixture file as it is now instead, which already holds
// every change the fixture migrations would have made. So a deploy is always
// both steps, in this order.
package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dbfixture"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/migrate"

	"github.com/RELAXccc/bun-fixture-migrate/examples/basic/migrations"
)

//go:embed fixtures/fixture.yml
var fixtures embed.FS

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: basic migrate|rollback|status")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("set DATABASE_URL to the database to migrate")
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	defer db.Close()
	db.RegisterModel((*Currency)(nil), (*Plan)(nil), (*Feature)(nil))

	// WithMarkAppliedOnSuccess records a migration once it succeeded. Without
	// it bun records a migration before running it, and a fixture migration
	// that fails takes that record back itself; with it there is nothing to
	// take back. Either works; this is the one without the window.
	migrator := migrate.NewMigrator(db, migrations.Migrations, migrate.WithMarkAppliedOnSuccess(true))
	if err := migrator.Init(ctx); err != nil {
		return err
	}
	if err := lock(ctx, migrator); err != nil {
		return err
	}
	defer func() {
		// The lock is released even when the deploy was interrupted.
		if err := migrator.Unlock(context.WithoutCancel(ctx)); err != nil {
			fmt.Fprintln(os.Stderr, "unlock:", err)
		}
	}()

	switch args[0] {
	case "migrate":
		group, err := migrator.Migrate(ctx)
		if err != nil {
			return err
		}
		if group.IsZero() {
			fmt.Println("no migrations to run")
		} else {
			fmt.Printf("migrated to %s\n", group)
		}
		return seed(ctx, db)
	case "rollback":
		group, err := migrator.Rollback(ctx)
		if err != nil {
			return err
		}
		if group.IsZero() {
			fmt.Println("no migrations to roll back")
		} else {
			fmt.Printf("rolled back %s\n", group)
		}
		return nil
	case "status":
		ms, err := migrator.MigrationsWithStatus(ctx)
		if err != nil {
			return err
		}
		for _, m := range ms {
			state := "pending"
			if m.IsApplied() {
				state = "applied"
			}
			fmt.Printf("%-8s %s\n", state, m.String())
		}
		return nil
	}
	return fmt.Errorf("unknown command %q: migrate, rollback or status", args[0])
}

// lock takes the migrator's lock, waiting for a while if another deploy holds
// it. bun's Migrator.Lock does not wait: it inserts a row into
// bun_migration_locks and fails at once while another process's row is
// there, so of replicas starting together all but one would fail. A deploy
// that died holding the lock left its row behind, which no wait ends; the
// error says how to remove it.
func lock(ctx context.Context, migrator *migrate.Migrator) error {
	wait := time.Minute
	if v := os.Getenv("LOCK_WAIT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("LOCK_WAIT: %w", err)
		}
		wait = d
	}
	start := time.Now()
	for {
		err := migrator.Lock(ctx)
		if err == nil {
			if waited := time.Since(start); waited > time.Second {
				fmt.Printf("waited %s for another deploy to release the migrator lock\n", waited.Round(time.Second))
			}
			return nil
		}
		if !strings.Contains(err.Error(), "already locked") {
			return err
		}
		if time.Since(start) > wait {
			return fmt.Errorf("%w, still after %s: another deploy is running, or one died holding the lock. "+
				"If none is running, remove it: DELETE FROM bun_migration_locks WHERE table_name = 'bun_migrations'",
				err, wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// seed loads the fixture file into a database whose plans table is empty, in
// one transaction: a load that fails halfway would otherwise leave a database
// the next deploy takes for seeded. The migrator's lock is held, so two
// deploys at once do not both seed.
func seed(ctx context.Context, db *bun.DB) error {
	return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		seeded, err := tx.NewSelect().Model((*Plan)(nil)).Exists(ctx)
		if err != nil || seeded {
			return err
		}
		if err := dbfixture.New(tx).Load(ctx, fixtures, "fixtures/fixture.yml"); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
		// The fixture file names its ids, which the tables' sequences do not
		// see go by; without this the application's first insert fails.
		if _, err := fixtureapply.SyncSequences(ctx, tx, "currencies", "plans", "features"); err != nil {
			return err
		}
		fmt.Println("seeded the database from the fixture file")
		return nil
	})
}
