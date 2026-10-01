package dbtest_test

// Two replicas starting at once, under bun's default migrator and without its
// Lock. Each records the migration before running it, so there are two
// records, and each replica's Apply has to take back both when the change set
// fails on both: taking back only the newest, both deleted the same row and
// the other one kept the failed migration recorded as applied.
//
// The file is named like a migration because bun's MustRegister names the
// migration after the file it is called in.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun/migrate"
)

const raceMigration = "20260921120000"

// raceFailing fails on every database: no plan is named "gone".
func raceFailing() fixturechange.Set {
	return fixturechange.Set{
		Name:   raceMigration + "_fixture_race",
		Tables: tables(),
		Changes: []fixturechange.Change{{Model: "Plan", Kind: fixturechange.Update,
			Key: fixturechange.Values{"name": fixturechange.Lit("gone")},
			Old: fixturechange.Values{"price_cents": fixturechange.Lit("1")},
			New: fixturechange.Values{"price_cents": fixturechange.Lit("2")}}},
	}
}

// What bun's Migrate does on each replica, interleaved the way a simultaneous
// start interleaves it: both insert their record, then both run the
// migration, which fails on both.
func TestTwoReplicasThatBothFailLeaveNoRecord(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	ctx := context.Background()
	run(t, db, "DROP TABLE IF EXISTS bun_migrations, bun_migration_locks")
	if err := migrate.NewMigrator(db, migrate.NewMigrations()).Init(ctx); err != nil {
		t.Fatal(err)
	}
	run(t, db, "INSERT INTO bun_migrations (name, group_id) VALUES ('20260101000000', 1)",
		"INSERT INTO bun_migrations (name, group_id) VALUES ('"+raceMigration+"', 2)", // replica A's MarkApplied
		"INSERT INTO bun_migrations (name, group_id) VALUES ('"+raceMigration+"', 2)") // replica B's
	// Neither change set runs until both replicas have looked for their
	// records: they wait for the advisory lock every change set takes.
	hold, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback()
	if _, err := hold.ExecContext(ctx, "SELECT pg_advisory_xact_lock(?)", int64(0x62666d0001)); err != nil {
		t.Fatal(err)
	}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = fixtureapply.Apply(ctx, db, raceFailing(), quiet(), fixtureapply.WithMigrationName(raceMigration))
		}()
	}
	for i := 0; scan[int64](t, db, "SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted") < 2; i++ {
		if i == 250 {
			t.Fatal("the replicas never waited for the advisory lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := hold.Rollback(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if errs[0] == nil || errs[1] == nil {
		t.Fatalf("both replicas have to fail: %v, %v", errs[0], errs[1])
	}
	if n := scan[int64](t, db, "SELECT count(*) FROM bun_migrations WHERE name = ?", raceMigration); n != 0 {
		t.Fatalf("both replicas failed and %d record(s) of the migration remain: it is recorded as applied and "+
			"never runs again", n)
	}
	if n := scan[int64](t, db, "SELECT count(*) FROM bun_migrations"); n != 1 {
		t.Fatalf("the other migration's record has to stay, %d rows left", n)
	}
}

// The same with bun's own migrators racing, a few rounds. Before, most rounds
// left the failed migration recorded.
func TestTwoRacingMigratorsLeaveNoRecordOfAFailedMigration(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	ctx := context.Background()
	ms := migrate.NewMigrations()
	set := raceFailing()
	ms.MustRegister(fixtureapply.Up(set, quiet()), fixtureapply.Down(set, quiet()))
	for round := 0; round < 10; round++ {
		run(t, db, "DROP TABLE IF EXISTS bun_migrations, bun_migration_locks")
		if err := migrate.NewMigrator(db, ms).Init(ctx); err != nil {
			t.Fatal(err)
		}
		// Hold the table, so both migrators read what is applied at the same
		// moment, as at a simultaneous start.
		hold, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := hold.ExecContext(ctx, "LOCK TABLE bun_migrations IN ACCESS EXCLUSIVE MODE"); err != nil {
			t.Fatal(err)
		}
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = migrate.NewMigrator(db, ms).Migrate(ctx)
			}()
		}
		time.Sleep(100 * time.Millisecond)
		if err := hold.Rollback(); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		// A migrator that read the other one's record found nothing to do;
		// the other failed. Either way the migration never succeeded, so no
		// record of it may remain.
		if errs[0] == nil && errs[1] == nil {
			t.Fatalf("round %d: the migration cannot succeed", round)
		}
		if n := scan[int64](t, db, "SELECT count(*) FROM bun_migrations"); n != 0 {
			t.Fatalf("round %d: the migration failed (%v; %v) and %d record(s) of it remain", round, errs[0], errs[1], n)
		}
	}
}
