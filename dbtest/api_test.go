package dbtest_test

// The Project API against a real PostgreSQL, under the driver the suite runs
// with, and the command on the same project: every method's result, encoded,
// is what the command prints with -json, byte for byte. The command connects
// on its own and the library is handed a *bun.DB, a bun.Conn or a caller's
// bun.Tx, so this also shows that each of those reads what the command reads.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
)

// asJSON is a result as the command prints it with -json.
func asJSON(t *testing.T, v any) string {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// same fails the test unless the command printed what the library returned.
func same(t *testing.T, what, command, library string) {
	t.Helper()
	if command != library {
		t.Fatalf("%s: the command printed\n%s\nthe library returned\n%s", what, command, library)
	}
}

// project is the command's project, loaded by the library.
func (c *cli) project() *fixturemigrate.Project {
	c.t.Helper()
	p, err := fixturemigrate.LoadProject(filepath.Join(c.dir, "fixture-migrate.yml"))
	if err != nil {
		c.t.Fatal(err)
	}
	return p
}

func (c *cli) read(rel string) []byte {
	c.t.Helper()
	data, err := os.ReadFile(filepath.Join(c.dir, rel))
	if err != nil {
		c.t.Fatal(err)
	}
	return data
}

func (c *cli) remove(rel string) {
	c.t.Helper()
	if err := os.Remove(filepath.Join(c.dir, rel)); err != nil {
		c.t.Fatal(err)
	}
}

// json runs a command with -json, fails unless it exits with want, and
// returns what it printed on standard output.
func (c *cli) json(want int, args ...string) string {
	c.t.Helper()
	code, stdout, stderr := c.run(append(args, "-json")...)
	if code != want {
		c.t.Fatalf("%v -json: exit %d, want %d\n%s%s", args, code, want, stdout, stderr)
	}
	return stdout
}

func TestTheProjectAgreesWithTheCommand(t *testing.T) {
	db := itemDB(t)
	ctx := context.Background()
	run(t, db, "DROP TABLE IF EXISTS bun_migrations, bun_migration_locks")
	loadFixture(t, db, itemFixture)
	c := buildCLI(t)
	p := c.project()

	// check, through each way of handing the library a database.
	command := c.json(0, "check")
	report, err := p.Check(ctx, db)
	if err != nil || !report.Agree {
		t.Fatalf("Check: %v %+v", err, report)
	}
	same(t, "check", command, asJSON(t, report))
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report, err = p.Check(ctx, conn); err != nil {
		t.Fatal(err)
	}
	same(t, "check on a bun.Conn", command, asJSON(t, report))
	conn.Close()

	// baseline: the library writes what the command writes.
	b, err := p.Baseline(ctx, db, fixturemigrate.BaselineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write(); err != nil {
		t.Fatal(err)
	}
	state := c.read("migrations/fixture_state.yml")
	c.remove("migrations/fixture_state.yml")
	same(t, "baseline", c.json(0, "baseline"), asJSON(t, b))
	if !bytes.Equal(state, c.read("migrations/fixture_state.yml")) {
		t.Fatal("baseline wrote another state file than the library")
	}

	// An edit: a price and a new item, which status reports, against the
	// database's migrations table.
	edited := replaceOnce(t, itemFixture, "      cost: 120\n", "      cost: 130\n")
	edited = replaceOnce(t, edited, "    - _id: rope", `    - _id: hammer
      id: 3
      region_id: '{{ $.Region.us.ID }}'
      name: "hammer"
      cost: 7
      production_max: 1
      ratio: 1
      active: true
      note: ~
    - _id: rope`)
	c.write("fixtures/fixture.yml", edited)
	if err := p.ReadFiles(); err != nil {
		t.Fatal(err)
	}
	r, err := p.Status(ctx, db, fixturemigrate.StatusOptions{})
	if err != nil || r.Database == nil || r.Database.TableExists ||
		strings.Join(r.Uncovered, ";") != "Item: 1 insert, 1 update" {
		t.Fatalf("Status: %v %+v", err, r)
	}
	same(t, "status", c.json(3, "status"), asJSON(t, r))

	// generate: a dry run, and against the database.
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	g, err := p.Generate(ctx, db, fixturemigrate.GenerateOptions{Name: "hammer", At: at, DryRun: true})
	if err != nil || g.ID != "20261001120000_fixture_hammer" {
		t.Fatalf("Generate: %v %+v", err, g)
	}
	same(t, "generate -dry-run", c.json(0, "generate", "-name", "hammer", "-at", "20261001120000", "-dry-run"),
		asJSON(t, g))
	g, err = p.Generate(ctx, db, fixturemigrate.GenerateOptions{Name: "hammer", At: at, FromDB: true, DryRun: true})
	if err != nil || len(g.Diff.Changes) != 2 {
		t.Fatalf("Generate FromDB: %v %+v", err, g)
	}
	same(t, "generate -from-db", c.json(0, "generate", "-name", "hammer", "-at", "20261001120000", "-from-db",
		"-dry-run"), asJSON(t, g))

	// Written, the library and the command write the same two files.
	g, err = p.Generate(ctx, db, fixturemigrate.GenerateOptions{Name: "hammer", At: at})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Write(); err != nil {
		t.Fatal(err)
	}
	migration, written := c.read("migrations/20261001120000_fixture_hammer.go"), c.read("migrations/fixture_state.yml")
	c.remove("migrations/20261001120000_fixture_hammer.go")
	c.write("migrations/fixture_state.yml", string(state))
	same(t, "generate", c.json(0, "generate", "-name", "hammer", "-at", "20261001120000"), asJSON(t, g))
	if !bytes.Equal(migration, c.read("migrations/20261001120000_fixture_hammer.go")) ||
		!bytes.Equal(written, c.read("migrations/fixture_state.yml")) {
		t.Fatal("generate wrote other files than the library")
	}

	// sync, shown and not made.
	s, err := p.Sync(ctx, db, fixturemigrate.SyncOptions{DryRun: true})
	if err != nil || s.Applied || len(s.Diff.Changes) != 2 {
		t.Fatalf("Sync: %v %+v", err, s)
	}
	same(t, "sync", c.json(0, "sync"), asJSON(t, s))

	// check, now that they disagree.
	if report, err = p.Check(ctx, db); err != nil || report.Agree {
		t.Fatalf("Check: %v %+v", err, report)
	}
	same(t, "check, drifted", c.json(3, "check"), asJSON(t, report))

	// export, last, since it writes the fixture file.
	exp, err := p.Export(ctx, db, fixturemigrate.ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exp.Write(); err != nil {
		t.Fatal(err)
	}
	exported := c.read("fixtures/fixture.yml")
	c.write("fixtures/fixture.yml", edited)
	same(t, "export", c.json(0, "export"), asJSON(t, exp))
	if !bytes.Equal(exported, c.read("fixtures/fixture.yml")) {
		t.Fatal("export wrote another fixture file than the library")
	}
	// The Project reads what it wrote.
	if files, err := p.Files(); err != nil || !bytes.Equal(files[0].Data, exported) {
		t.Fatalf("the project still holds the file it replaced: %v", err)
	}
}

// A refusal is the same refusal from the library and from the command, and a
// program tells its kind with errors.Is.
func TestTheProjectRefusesAsTheCommandDoes(t *testing.T) {
	db := itemDB(t)
	ctx := context.Background()
	run(t, db, "DROP TABLE IF EXISTS bun_migrations, bun_migration_locks")
	loadFixture(t, db, itemFixture)
	c := buildCLI(t)
	c.must(0, "baseline")

	// A zero bun would replace with the column's default: the lint against
	// the database refuses generate.
	zero := replaceOnce(t, itemFixture, "      production_max: 3\n", "      production_max: 0\n")
	c.write("fixtures/fixture.yml", zero)
	p := c.project()
	g, err := p.Generate(ctx, db, fixturemigrate.GenerateOptions{Name: "zero"})
	var refused *fixturemigrate.RefusedError
	if !errors.Is(err, fixturemigrate.ErrFindings) || !errors.As(err, &refused) ||
		refused.Findings[0].Kind != fixturemigrate.FindingZeroDefault || len(g.Lint) != 1 {
		t.Fatalf("Generate: %v %+v", err, g)
	}
	same(t, "generate, refused", c.json(2, "generate", "-name", "zero"), asJSON(t, g))
	b, err := p.Baseline(ctx, db, fixturemigrate.BaselineOptions{})
	if !errors.Is(err, fixturemigrate.ErrUnmigrated) {
		t.Fatalf("Baseline: %v %+v", err, b)
	}
	same(t, "baseline, refused", c.json(2, "baseline"), asJSON(t, b))

	// A rename: sync refuses it as generate would, as ErrSyncRefused too.
	c.write("fixtures/fixture.yml", strings.Replace(itemFixture, `name: "anvil"`, `name: "anvil2"`, 1))
	if err := p.ReadFiles(); err != nil {
		t.Fatal(err)
	}
	s, err := p.Sync(ctx, db, fixturemigrate.SyncOptions{})
	if !errors.Is(err, fixturemigrate.ErrRefused) || !errors.Is(err, fixturemigrate.ErrSyncRefused) ||
		!errors.As(err, &refused) || len(refused.Refusals) != 1 || s.Applied {
		t.Fatalf("Sync: %v %+v", err, s)
	}
	same(t, "sync, refused", c.json(2, "sync", "-yes"), asJSON(t, s))
	if got := scan[int64](t, db, "SELECT count(*) FROM items WHERE name = 'anvil'"); got != 1 {
		t.Fatal("a refused sync changed the database")
	}
}

// Handed a transaction, the library reads in it and leaves it as it was: its
// settings, and usable, even after a read that failed.
func TestTheProjectReadsInACallersTransaction(t *testing.T) {
	db := itemDB(t)
	ctx := context.Background()
	loadFixture(t, db, itemFixture)
	c := buildCLI(t)
	c.must(0, "baseline")
	p := c.project()
	want, err := p.Check(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SET LOCAL TimeZone = 'America/New_York'"); err != nil {
		t.Fatal(err)
	}
	// A row the transaction wrote is drift the check sees, and nobody else.
	if _, err := tx.ExecContext(ctx, "UPDATE items SET cost = 999 WHERE name = 'anvil'"); err != nil {
		t.Fatal(err)
	}
	got, err := p.Check(ctx, tx)
	if err != nil || got.Agree || len(got.Diff.Changes) != 1 {
		t.Fatalf("Check in the transaction: %v %+v", err, got)
	}
	r, err := p.Status(ctx, &tx, fixturemigrate.StatusOptions{})
	if err != nil || r.Database == nil {
		t.Fatalf("Status in the transaction: %v %+v", err, r)
	}
	var tz string
	if err := tx.QueryRowContext(ctx, "SHOW TimeZone").Scan(&tz); err != nil || tz != "America/New_York" {
		t.Fatalf("the transaction's settings were changed: %q %v", tz, err)
	}

	// A read that fails, which fails the transaction in PostgreSQL, leaves it
	// usable.
	c.write("fixture-migrate.yml", strings.Replace(cliConfig, "    key: [name]\n",
		"    key: [name]\n    where: \"cost / 0 > 0\"\n", 1))
	if _, err := c.project().Check(ctx, tx); err == nil || !strings.Contains(err.Error(), "division by zero") {
		t.Fatalf("a where that fails: %v", err)
	}
	var cost int64
	if err := tx.QueryRowContext(ctx, "SELECT cost FROM items WHERE name = 'anvil'").Scan(&cost); err != nil || cost != 999 {
		t.Fatalf("the transaction is not usable after a failed read: %d %v", cost, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	// Outside it, nothing changed.
	if again, err := p.Check(ctx, db); err != nil || asJSON(t, again) != asJSON(t, want) {
		t.Fatalf("%v", err)
	}
}

// What the audit table says each run did is in the library's status report as
// in the command's: before any run, after an apply, and once the file was
// edited after it ran.
func TestTheProjectReadsTheAuditTableAsStatusDoes(t *testing.T) {
	db := itemDB(t)
	ctx := context.Background()
	run(t, db, "DROP TABLE IF EXISTS bun_migrations, bun_migration_locks, bfm_api_audit")
	t.Cleanup(func() { run(t, db, "DROP TABLE IF EXISTS bfm_api_audit") })
	loadFixture(t, db, itemFixture)
	c := buildCLI(t)
	c.write("fixture-migrate.yml", strings.Replace(cliConfig, "seed_guard_table: items\n",
		"seed_guard_table: items\naudit_table: bfm_api_audit\n", 1))
	c.must(0, "baseline")
	p := c.project()

	r, err := p.Status(ctx, db, fixturemigrate.StatusOptions{})
	if err != nil || r.Database.Audit == nil || r.Database.Audit.Exists {
		t.Fatalf("before the first run: %v %+v", err, r.Database)
	}
	same(t, "status before the first run", c.json(0, "status"), asJSON(t, r))

	c.write("fixtures/fixture.yml", replaceOnce(t, itemFixture, "      cost: 120\n", "      cost: 130\n"))
	c.must(0, "generate", "-name", "cost", "-at", "20300101000000")
	file := filepath.Join(c.dir, "migrations", "20300101000000_fixture_cost.go")
	c.must(0, "apply", "-file", file, "-yes")
	if err := p.ReadFiles(); err != nil {
		t.Fatal(err)
	}
	r, err = p.Status(ctx, db, fixturemigrate.StatusOptions{})
	if err != nil || !r.Database.Audit.Exists || len(r.Migrations) != 1 || r.Migrations[0].Audit == nil ||
		r.Migrations[0].Audit.Applied != 1 || r.Migrations[0].Audit.Edited {
		t.Fatalf("after the run: %v %+v", err, r.Migrations)
	}
	same(t, "status after the run", c.json(0, "status"), asJSON(t, r))

	c.write("migrations/20300101000000_fixture_cost.go",
		replaceOnce(t, string(c.read("migrations/20300101000000_fixture_cost.go")), `Lit("130")`, `Lit("131")`))
	r, err = p.Status(ctx, db, fixturemigrate.StatusOptions{})
	if err != nil || !r.Migrations[0].Audit.Edited || len(r.Notes) == 0 {
		t.Fatalf("an edited file: %v %+v %q", err, r.Migrations, r.Notes)
	}
	same(t, "status of an edited file", c.json(0, "status"), asJSON(t, r))
}
