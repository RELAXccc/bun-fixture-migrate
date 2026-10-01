package dbtest_test

// A generated migration, compiled and run by bun's own migrator.
//
// Everything else in this module calls fixtureapply directly. That cannot show
// the one thing that decides whether a failed migration is ever retried: what
// migrate.Migrator records. Unless it is built WithMarkAppliedOnSuccess(true),
// bun inserts a migration's record before running it and leaves it there when
// the migration fails, and a recorded migration never runs again. The
// generated file has to leave the migrations table telling the truth in both
// modes, and the only way to know is to build it and run it.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
)

const migratorMain = `package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

	"github.com/RELAXccc/bun-fixture-migrate/dbtest/migratorcheck/migrations"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/migrate"
)

func main() {
	onSuccess := flag.Bool("on-success", false, "WithMarkAppliedOnSuccess(true)")
	table := flag.String("table", "", "WithTableName")
	down := flag.Bool("down", false, "roll the last group back instead of migrating")
	flag.Parse()
	ctx := context.Background()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(os.Getenv("DSN")))), pgdialect.New())
	defer db.Close()
	opts := []migrate.MigratorOption{migrate.WithMarkAppliedOnSuccess(*onSuccess)}
	if *table != "" {
		opts = append(opts, migrate.WithTableName(*table), migrate.WithLocksTableName(*table+"_locks"))
	}
	m := migrate.NewMigrator(db, migrations.Migrations, opts...)
	if err := m.Init(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "init:", err)
		os.Exit(2)
	}
	if *down {
		if _, err := m.Rollback(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "rollback:", err)
			os.Exit(1)
		}
		return
	}
	if _, err := m.Migrate(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}
`

const migratorPackage = `package migrations

import "github.com/uptrace/bun/migrate"

var Migrations = migrate.NewMigrations()
`

// buildMigrator writes the generated file into a migrations package next to a
// main that runs bun's migrator, builds it, and returns the binary.
func buildMigrator(t *testing.T, stamp, name string, src []byte) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no Go toolchain to build with")
	}
	// Inside this module, so it resolves bun and the library through the
	// module's own go.mod with no network.
	dir := "migratorcheck"
	t.Cleanup(func() { os.RemoveAll(dir) })
	for path, content := range map[string][]byte{
		"main.go":                  []byte(migratorMain),
		"migrations/migrations.go": []byte(migratorPackage),
		filepath.Join("migrations", fixturemigrate.FileName(stamp, name)): src,
	} {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(t.TempDir(), "migrator")
	if out, err := exec.Command("go", "build", "-o", bin, "./"+dir).CombinedOutput(); err != nil {
		t.Fatalf("the generated migration does not build: %v\n%s\n%s", err, out, src)
	}
	return bin
}

func runMigrator(t *testing.T, bin string, onSuccess bool, extra ...string) (bool, string) {
	t.Helper()
	args := append([]string{}, extra...)
	if onSuccess {
		args = append(args, "-on-success")
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "DSN="+os.Getenv("BUN_FIXTURE_MIGRATE_POSTGRES"))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
		return false, out.String()
	}
	if err != nil {
		t.Fatalf("the migrator did not run: %v\n%s", err, out.String())
	}
	return true, out.String()
}

// The migration fails against a database somebody edited, is not left
// recorded, and runs once the database is put right. In both of bun's modes.
func TestAFailedMigrationIsNotLeftRecorded(t *testing.T) {
	connect(t) // skips without a database, before anything is built
	cfg := itemConfig(t)
	cfg.Package = "migrations"
	old := fixtureSnapshot(t, cfg, itemFixture, "HEAD:fixture.yml")
	changed := replaceOnce(t, itemFixture, "      cost: 120\n", "      cost: 130\n")
	res, err := fixturemigrate.Compute(cfg, old, fixtureSnapshot(t, cfg, changed, "fixture.yml"))
	if err != nil || len(res.Refusals) != 0 {
		t.Fatalf("Compute: %v %+v", err, res.Refusals)
	}
	const stamp = "20260921120000"
	src, err := fixturemigrate.Render(cfg, "anvil cost", stamp, res)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	bin := buildMigrator(t, stamp, "anvil cost", src)

	for _, onSuccess := range []bool{false, true} {
		mode := "bun's default (records before running)"
		if onSuccess {
			mode = "WithMarkAppliedOnSuccess(true)"
		}
		t.Run(mode, func(t *testing.T) {
			db := itemDB(t)
			ctx := context.Background()
			if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS bun_migrations, bun_migration_locks"); err != nil {
				t.Fatal(err)
			}
			loadFixture(t, db, itemFixture)
			// Somebody renamed the anvil in this database, so the update
			// cannot find its row and missing_row fails the migration.
			if _, err := db.ExecContext(ctx, "UPDATE items SET name = 'anvil (old)' WHERE name = 'anvil'"); err != nil {
				t.Fatal(err)
			}

			ok, out := runMigrator(t, bin, onSuccess)
			if ok {
				t.Fatalf("the migration should have failed:\n%s", out)
			}
			if !strings.Contains(out, "no row of items has name=anvil") {
				t.Fatalf("the failure has to say what is wrong:\n%s", out)
			}
			if got := scan[int64](t, db, "SELECT count(*) FROM bun_migrations"); got != 0 {
				t.Fatalf("a failed migration is recorded as applied (%d rows), so it will never run again:\n%s", got, out)
			}
			if !onSuccess && !strings.Contains(out, "that record was removed") {
				t.Fatalf("the error has to say the record was taken back:\n%s", out)
			}

			// Put the row back and deploy again: the migration runs now.
			if _, err := db.ExecContext(ctx, "UPDATE items SET name = 'anvil' WHERE name = 'anvil (old)'"); err != nil {
				t.Fatal(err)
			}
			if ok, out := runMigrator(t, bin, onSuccess); !ok {
				t.Fatalf("the second run should succeed:\n%s", out)
			}
			if got := scan[int64](t, db, "SELECT cost FROM items WHERE name = 'anvil'"); got != 130 {
				t.Fatalf("the migration did not run the second time: cost = %d", got)
			}
			if got := scan[string](t, db, "SELECT name FROM bun_migrations"); got != stamp {
				t.Fatalf("recorded as %q, want %q", got, stamp)
			}
		})
	}
}

// A file an earlier version generated registers functions that call Apply
// and Revert themselves, and Apply finds the migration's name on the call
// stack. It still takes back bun's record when it fails.
func TestAFailedMigrationOfAnEarlierVersionIsNotLeftRecorded(t *testing.T) {
	db := connect(t)
	dir := filepath.Join("..", "testdata", "generated", "41ffb5c-example")
	src, err := os.ReadFile(filepath.Join(dir, "20260930165255_fixture_plan_prices.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "fixtureapply.Apply(ctx, db, ") {
		t.Fatal("this is about the registration of earlier versions")
	}
	bin := buildMigrator(t, "20260930165255", "plan prices", src)
	setup, err := os.ReadFile(filepath.Join(dir, "setup.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range strings.Split(string(setup), ";\n") {
		if strings.TrimSpace(stmt) != "" {
			run(t, db, stmt)
		}
	}
	run(t, db, "UPDATE plans SET name = 'team (old)' WHERE name = 'team'")
	ok, out := runMigrator(t, bin, false)
	if ok || !strings.Contains(out, "no row of plans has name=team") || !strings.Contains(out, "that record was removed") {
		t.Fatalf("expected the failure and the record taken back:\n%s", out)
	}
	if got := scan[int64](t, db, "SELECT count(*) FROM bun_migrations"); got != 0 {
		t.Fatalf("%d records left", got)
	}
	run(t, db, "UPDATE plans SET name = 'team' WHERE name = 'team (old)'")
	if ok, out := runMigrator(t, bin, false); !ok {
		t.Fatalf("the second run should succeed:\n%s", out)
	}
}

// What a failing Apply takes back is bounded: the records of this migration
// newer than every other migration's, written within the minute, and only
// through the migrator's own *bun.DB. Anything else stays, because anything
// else is not a record bun made a moment ago for this run.
func TestOnlyTheMigratorsFreshRecordIsTakenBack(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	ctx := context.Background()
	const name = "20260921120000"
	failing := fixturechange.Set{
		Name:   name + "_fixture_x",
		Tables: tables(),
		Changes: []fixturechange.Change{{Model: "Plan", Kind: fixturechange.Update,
			Key: fixturechange.Values{"name": fixturechange.Lit("gone")},
			Old: fixturechange.Values{"price_cents": fixturechange.Lit("1")},
			New: fixturechange.Values{"price_cents": fixturechange.Lit("2")}}},
	}

	reset := func(rows ...string) {
		t.Helper()
		for _, stmt := range append([]string{
			"DROP TABLE IF EXISTS bun_migrations",
			"CREATE TABLE bun_migrations (id bigserial PRIMARY KEY, name varchar, group_id bigint, " +
				"migrated_at timestamptz NOT NULL DEFAULT current_timestamp)",
		}, rows...) {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
	}
	count := func() int64 { return scan[int64](t, db, "SELECT count(*) FROM bun_migrations WHERE name = ?", name) }
	apply := func(idb bun.IDB) error {
		return fixtureapply.Apply(ctx, idb, failing, quiet(), fixtureapply.WithMigrationName(name))
	}

	// The case it exists for: bun has just recorded this migration.
	reset("INSERT INTO bun_migrations (name, group_id) VALUES ('20260101000000', 1)",
		"INSERT INTO bun_migrations (name, group_id) VALUES ('"+name+"', 2)")
	err := apply(db)
	if err == nil || !strings.Contains(err.Error(), "that record was removed") {
		t.Fatalf("expected the failure and the note, got %v", err)
	}
	var ce *fixtureapply.ChangeError
	if !errors.Is(err, fixtureapply.ErrRecordRemoved) || !errors.As(err, &ce) || ce.Outcome.Problem != fixtureapply.ProblemMissingRow {
		t.Fatalf("the failure and the removal have to be visible to errors.Is and errors.As: %v", err)
	}
	if count() != 0 {
		t.Fatal("the fresh record should be gone")
	}
	if got := scan[int64](t, db, "SELECT count(*) FROM bun_migrations"); got != 1 {
		t.Fatalf("the other migration's record has to stay, %d rows left", got)
	}

	// Two replicas recorded it at the same start: both records go.
	reset("INSERT INTO bun_migrations (name, group_id) VALUES ('20260101000000', 1)",
		"INSERT INTO bun_migrations (name, group_id) VALUES ('"+name+"', 2)",
		"INSERT INTO bun_migrations (name, group_id) VALUES ('"+name+"', 2)")
	if err := apply(db); !errors.Is(err, fixtureapply.ErrRecordRemoved) {
		t.Fatalf("expected the records taken back, got %v", err)
	}
	if count() != 0 {
		t.Fatal("both fresh records should be gone")
	}

	for _, c := range []struct {
		why  string
		rows []string
		idb  func() bun.IDB
	}{
		{"it is not the newest record",
			[]string{"INSERT INTO bun_migrations (name, group_id) VALUES ('" + name + "', 1)",
				"INSERT INTO bun_migrations (name, group_id) VALUES ('20260922000000', 2)"},
			func() bun.IDB { return db }},
		{"it was not written just before this run",
			[]string{"INSERT INTO bun_migrations (name, group_id, migrated_at) VALUES ('" + name +
				"', 1, now() - interval '2 minutes')"},
			func() bun.IDB { return db }},
	} {
		reset(c.rows...)
		if err := apply(c.idb()); err == nil || strings.Contains(err.Error(), "record was removed") ||
			errors.Is(err, fixtureapply.ErrRecordRemoved) {
			t.Fatalf("%s: %v", c.why, err)
		}
		if count() != 1 {
			t.Fatalf("%s: the record must stay", c.why)
		}
	}

	// Inside a caller's transaction nothing is deleted: a failing statement
	// there would poison the caller's transaction, and the migrator never
	// calls a migration that way.
	reset("INSERT INTO bun_migrations (name, group_id) VALUES ('" + name + "', 1)")
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply(tx); err == nil {
		t.Fatal("expected the failure")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if count() != 1 {
		t.Fatal("a caller's transaction is not the migrator's")
	}

	// No migrations table at all: the failure is returned as it is.
	if _, err := db.ExecContext(ctx, "DROP TABLE bun_migrations"); err != nil {
		t.Fatal(err)
	}
	if err := apply(db); err == nil || strings.Contains(err.Error(), "bun_migrations") {
		t.Fatalf("expected the plain failure, got %v", err)
	}
}

// Two replicas start at once, under a migrator that records on success and
// without bun's Lock. The first applies the change set and records it while
// the second waits for the advisory lock; the second then fails. The record it
// finds afterwards is the first one's, made after the change set was applied,
// and taking it back would leave the migration pending with its changes made:
// every later start would run it again. Only a record that was there before
// this run began is bun's record of this run.
func TestAnotherReplicasRecordIsNotTakenBack(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	ctx := context.Background()
	const name = "20260921120000"
	failing := fixturechange.Set{
		Name:   name + "_fixture_x",
		Tables: tables(),
		Changes: []fixturechange.Change{{Model: "Plan", Kind: fixturechange.Update,
			Key: fixturechange.Values{"name": fixturechange.Lit("gone")},
			Old: fixturechange.Values{"price_cents": fixturechange.Lit("1")},
			New: fixturechange.Values{"price_cents": fixturechange.Lit("2")}}},
	}
	run(t, db, "DROP TABLE IF EXISTS bun_migrations",
		"CREATE TABLE bun_migrations (id bigserial PRIMARY KEY, name varchar, group_id bigint, "+
			"migrated_at timestamptz NOT NULL DEFAULT current_timestamp)")

	first, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback()
	if _, err := first.ExecContext(ctx, "SELECT pg_advisory_xact_lock(?)", int64(0x62666d0001)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- fixtureapply.Apply(ctx, db, failing, quiet(), fixtureapply.WithMigrationName(name))
	}()
	for i := 0; ; i++ {
		if scan[int64](t, db, "SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted") > 0 {
			break
		}
		if i == 100 {
			t.Fatal("the second replica never waited for the lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The first replica's migrator records the migration as it finishes.
	run(t, db, "INSERT INTO bun_migrations (name, group_id) VALUES ('"+name+"', 1)")
	if err := first.Rollback(); err != nil {
		t.Fatal(err)
	}
	err = <-done
	if err == nil || strings.Contains(err.Error(), "record was removed") {
		t.Fatalf("expected the failure alone, got %v", err)
	}
	if got := scan[int64](t, db, "SELECT count(*) FROM bun_migrations WHERE name = ?", name); got != 1 {
		t.Fatalf("the first replica's record has to stay, %d left", got)
	}
}

// A migrator built WithTableName, in mixed case: bun puts the name into its
// SQL unquoted, so PostgreSQL folds it, and the generated migration has to
// find the same table to take its record back.
func TestAFailedMigrationIsNotLeftRecordedInACustomTable(t *testing.T) {
	connect(t)
	cfg := itemConfig(t)
	cfg.Package = "migrations"
	cfg.MigrationsTable = "Fixture_Migrations"
	old := fixtureSnapshot(t, cfg, itemFixture, "old")
	changed := replaceOnce(t, itemFixture, "      cost: 120\n", "      cost: 130\n")
	res, err := fixturemigrate.Compute(cfg, old, fixtureSnapshot(t, cfg, changed, "new"))
	if err != nil {
		t.Fatal(err)
	}
	const stamp = "20260921120000"
	src, err := fixturemigrate.Render(cfg, "anvil cost", stamp, res)
	if err != nil {
		t.Fatal(err)
	}
	bin := buildMigrator(t, stamp, "anvil cost", src)
	db := itemDB(t)
	run(t, db, "DROP TABLE IF EXISTS fixture_migrations, fixture_migrations_locks")
	loadFixture(t, db, itemFixture)
	run(t, db, "UPDATE items SET name = 'anvil (old)' WHERE name = 'anvil'")
	if ok, out := runMigrator(t, bin, false, "-table", "Fixture_Migrations"); ok || !strings.Contains(out, "that record was removed") {
		t.Fatalf("expected the failure and the record taken back:\n%s", out)
	}
	if got := scan[int64](t, db, "SELECT count(*) FROM fixture_migrations"); got != 0 {
		t.Fatalf("%d records left", got)
	}
	run(t, db, "UPDATE items SET name = 'anvil' WHERE name = 'anvil (old)'")
	if ok, out := runMigrator(t, bin, false, "-table", "Fixture_Migrations"); !ok {
		t.Fatalf("the second run should succeed:\n%s", out)
	}
	if got := scan[int64](t, db, "SELECT cost FROM items WHERE name = 'anvil'"); got != 130 {
		t.Fatalf("cost = %d", got)
	}
}
