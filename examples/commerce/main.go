// Command commerce is the deploy step of the example shop's back office: it runs
// the bun migrations, the schema's and the fixture's, and seeds a database that
// has never been seeded.
//
//	export DATABASE_URL=postgres://user:password@localhost:5432/commerce?sslmode=disable
//	go run . migrate    # run what is pending, then seed an empty database
//	go run . rollback   # roll the last group back
//	go run . status     # list the migrations and whether each ran
//
// Every replica of the application may run "migrate" at start-up. bun's
// migrator lock does not wait: a second process that finds the lock taken gets
// an error at once. So migrate retries the lock for COMMERCE_LOCK_WAIT (default
// 60s, "0" to fail at once, which is bun's own behaviour), and the replica that
// gets it second finds nothing left to do.
//
// The fixture files and the tables a seed has to move the sequences of are
// read from fixture-migrate.yml, which is embedded: the seed loads exactly the
// files bun-fixture-migrate generates migrations from, in the same order.
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

	"gopkg.in/yaml.v3"

	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dbfixture"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/migrate"

	"github.com/RELAXccc/bun-fixture-migrate/examples/commerce/migrations"
)

//go:embed fixture-migrate.yml fixtures/*.yml
var project embed.FS

// projectConfig is the part of fixture-migrate.yml the seed needs.
type projectConfig struct {
	Fixture        string   `yaml:"fixture"`
	Fixtures       []string `yaml:"fixtures"`
	SeedGuardTable string   `yaml:"seed_guard_table"`
	Models         map[string]struct {
		Table string `yaml:"table"`
	} `yaml:"models"`
}

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
		return errors.New("usage: commerce migrate|rollback|status")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("set DATABASE_URL to the database to migrate")
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	defer db.Close()
	db.RegisterModel(masterModels...)

	migrator := migrate.NewMigrator(db, migrations.Migrations, migrate.WithMarkAppliedOnSuccess(true))
	if err := migrator.Init(ctx); err != nil {
		return err
	}
	if err := lock(ctx, migrator); err != nil {
		return err
	}
	defer func() {
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

// lock takes bun's migrator lock, retrying while another process holds it.
// bun's Lock inserts a row into the locks table and fails at once when the
// row is there; it never waits, and a deploy that dies holding it leaves the
// row behind (bun's, see examples/saas, F7).
func lock(ctx context.Context, migrator *migrate.Migrator) error {
	wait := 60 * time.Second
	if v := os.Getenv("COMMERCE_LOCK_WAIT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("COMMERCE_LOCK_WAIT: %w", err)
		}
		wait = d
	}
	start := time.Now()
	deadline := start.Add(wait)
	for waited := false; ; waited = true {
		err := migrator.Lock(ctx)
		if err == nil && waited {
			fmt.Printf("waited %s for another deploy to release the migrator lock\n",
				time.Since(start).Round(time.Millisecond))
		}
		if err == nil || !strings.Contains(err.Error(), "already locked") || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// seed loads the fixture files into a database whose seed guard table is
// empty, in one transaction, then moves the sequences past the ids the files
// name. The migrator's lock is held, so two deploys at once do not both seed.
func seed(ctx context.Context, db *bun.DB) error {
	data, err := project.ReadFile("fixture-migrate.yml")
	if err != nil {
		return err
	}
	var cfg projectConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("fixture-migrate.yml: %w", err)
	}
	files := cfg.Fixtures
	if len(files) == 0 {
		files = []string{cfg.Fixture}
	}
	var tables []string
	for _, name := range sortedKeys(cfg.Models) {
		tables = append(tables, cfg.Models[name].Table)
	}
	return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var seeded bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM "+cfg.SeedGuardTable+")").Scan(&seeded); err != nil || seeded {
			return err
		}
		if err := dbfixture.New(tx).Load(ctx, project, files...); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
		moved, err := fixtureapply.SyncSequences(ctx, tx, tables...)
		if err != nil {
			return err
		}
		fmt.Printf("seeded the database from %s; moved the sequences of %s\n",
			strings.Join(files, ", "), strings.Join(moved, ", "))
		return nil
	})
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
