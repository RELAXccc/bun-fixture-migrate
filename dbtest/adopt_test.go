package dbtest_test

// Adopting the tool on a project that has run bun's migrator for a while, the
// way the README's "In five minutes" and the runbook's adoption steps say:
// scaffold, read it, export, baseline, then a fixture change generated and
// planned. Every step has to work as written, in a directory that has
// neither its fixtures nor its migrations directory yet.

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/uptrace/bun"
)

type adoption struct {
	t    *testing.T
	dir  string
	tool string
	dsn  string
	db   *bun.DB
}

// newAdoption is a database of its own, as a project has it: the master
// tables, a table the application writes, and bun's two.
func newAdoption(t *testing.T) *adoption {
	t.Helper()
	admin := connect(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no Go toolchain to build with")
	}
	u, err := url.Parse(os.Getenv("BUN_FIXTURE_MIGRATE_POSTGRES"))
	if err != nil {
		t.Fatal(err)
	}
	run(t, admin, "DROP DATABASE IF EXISTS bfm_adopt", "CREATE DATABASE bfm_adopt")
	u.Path = "/bfm_adopt"
	a := &adoption{t: t, dir: t.TempDir(), tool: filepath.Join(t.TempDir(), "bun-fixture-migrate"), dsn: u.String()}
	if out, err := exec.Command("go", "build", "-o", a.tool,
		"github.com/RELAXccc/bun-fixture-migrate/cmd/bun-fixture-migrate").CombinedOutput(); err != nil {
		t.Fatalf("build the command: %v\n%s", err, out)
	}
	a.db = openDB(t, a.dsn, nil)
	t.Cleanup(func() {
		a.db.Close()
		admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS bfm_adopt")
	})
	run(t, a.db,
		`CREATE TABLE bun_migrations (id bigserial PRIMARY KEY, name varchar, group_id bigint,
			migrated_at timestamptz NOT NULL DEFAULT current_timestamp)`,
		`CREATE TABLE bun_migration_locks (id bigserial PRIMARY KEY, table_name varchar UNIQUE)`,
		`INSERT INTO bun_migrations (name, group_id) VALUES ('20260101000000', 1)`,
		`CREATE TABLE currencies (id bigint PRIMARY KEY, code text NOT NULL UNIQUE, symbol text NOT NULL)`,
		`CREATE TABLE plans (id bigserial PRIMARY KEY, name text NOT NULL UNIQUE,
			currency_id bigint NOT NULL REFERENCES currencies (id), price_cents bigint NOT NULL DEFAULT 0)`,
		`CREATE TABLE users (id bigserial PRIMARY KEY, email text NOT NULL UNIQUE, plan_id bigint REFERENCES plans (id))`,
		`INSERT INTO currencies VALUES (1, 'EUR', '€'), (2, 'USD', '$')`,
		`INSERT INTO plans (id, name, currency_id, price_cents) VALUES (1, 'free', 1, 0), (2, 'team', 1, 2500)`,
		`SELECT setval('plans_id_seq', 2)`,
		`INSERT INTO users (email, plan_id) VALUES ('ann@example.com', 2)`)
	return a
}

// run runs the command in the project's directory, with DATABASE_URL set as
// the README sets it, and fails unless it exits with want.
func (a *adoption) run(want int, args ...string) string {
	a.t.Helper()
	cmd := exec.Command(a.tool, args...)
	cmd.Dir = a.dir
	cmd.Env = append(os.Environ(), "DATABASE_URL="+a.dsn, "BUN_FIXTURE_MIGRATE_CONFIG=")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		a.t.Fatal(err)
	}
	if code != want {
		a.t.Fatalf("%v: exit %d, want %d\n%s", args, code, want, out.String())
	}
	return out.String()
}

func (a *adoption) read(rel string) string {
	a.t.Helper()
	data, err := os.ReadFile(filepath.Join(a.dir, rel))
	if err != nil {
		a.t.Fatal(err)
	}
	return string(data)
}

func (a *adoption) write(rel, content string) {
	a.t.Helper()
	if err := os.WriteFile(filepath.Join(a.dir, rel), []byte(content), 0o644); err != nil {
		a.t.Fatal(err)
	}
}

func TestAdoptingAsTheREADMESays(t *testing.T) {
	a := newAdoption(t)

	// $ bun-fixture-migrate scaffold -o fixture-migrate.yml
	a.run(0, "scaffold", "-o", "fixture-migrate.yml")
	cfg := a.read("fixture-migrate.yml")
	for _, want := range []string{
		`seed_guard_table: "currencies"`, "migrations_table: bun_migrations\n",
		"  User:\n    # GUESS: proposed because the schema has it. Delete this model unless",
	} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("scaffold is missing %q:\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "table: bun_migrations\n    ") || strings.Contains(cfg, "BunMigration") {
		t.Fatalf("bun's tables proposed as master data:\n%s", cfg)
	}
	// "then read it": the users are the application's, not master data.
	user := regexp.MustCompile(`(?s)\n  User:\n.*?(\n\n|$)`)
	a.write("fixture-migrate.yml", user.ReplaceAllString(cfg, "\n"))

	// $ bun-fixture-migrate export
	if out := a.run(0, "export"); !strings.Contains(out, "wrote fixtures/fixture.yml") {
		t.Fatal(out)
	}
	exported := a.read("fixtures/fixture.yml")
	if strings.Contains(exported, "ann@example.com") || strings.Contains(exported, "20260101000000") {
		t.Fatalf("the export holds data that is not master data:\n%s", exported)
	}
	a.run(0, "check")
	// Exported again, the file is written anew: a comment in it is gone,
	// and the export says so.
	a.write("fixtures/fixture.yml", "# prices are in cents\n"+exported)
	if out := a.run(0, "export"); !strings.Contains(out,
		"note: the export does not keep the comments of fixtures/fixture.yml: 1 comment line not in it") {
		t.Fatal(out)
	}
	if a.read("fixtures/fixture.yml") != exported {
		t.Fatal("two exports of one database differ")
	}

	// $ bun-fixture-migrate baseline
	if out := a.run(0, "baseline"); !strings.Contains(out, "wrote internal/migrations/fixture_state.yml") {
		t.Fatal(out)
	}
	a.run(0, "status")

	// Then, for every change to master data, edit the fixture file and:
	// $ bun-fixture-migrate generate -name "plan prices"
	a.write("fixtures/fixture.yml", strings.Replace(exported, "price_cents: 2500", "price_cents: 2600", 1))
	a.run(3, "status")
	out := a.run(0, "generate", "-name", "plan prices")
	if !strings.Contains(out, "Plan: 1 update") || strings.Contains(out, "seed_guard_table") {
		t.Fatal(out)
	}
	files, _ := filepath.Glob(filepath.Join(a.dir, "internal", "migrations", "*_fixture_plan_prices.go"))
	if len(files) != 1 {
		t.Fatalf("no migration: %v", files)
	}
	if src, _ := os.ReadFile(files[0]); !strings.Contains(string(src), `SeedGuardTable:  "currencies"`) {
		t.Fatalf("the migration is not guarded:\n%s", src)
	}
	// $ bun-fixture-migrate plan
	if out := a.run(0, "plan"); !strings.Contains(out, "would succeed") || !strings.Contains(out, "applied  Plan name=team update") {
		t.Fatal(out)
	}
	a.run(0, "status", "-offline")
}

// What the configuration gets wrong about a table is a sentence, not the
// error of a query nobody wrote; a column the fixture file writes and the
// table lacks is the unknown column finding; and status lints the fixture
// file as generate does, so it does not send anybody to a generate that
// refuses.
func TestTheConfigurationAgainstTheCatalog(t *testing.T) {
	a := newAdoption(t)
	a.run(0, "scaffold", "-o", "fixture-migrate.yml", "-tables", "currencies,plans")
	cfg := a.read("fixture-migrate.yml")
	a.run(0, "export")
	exported := a.read("fixtures/fixture.yml")
	a.run(0, "baseline")

	// A key column the table does not have, which the fixture rows write.
	a.write("fixture-migrate.yml", strings.Replace(cfg, "key: [code]", "key: [iso_code]", 1))
	a.write("fixtures/fixture.yml", regexp.MustCompile(`(?m)^(\s+)code: ("\w+")$`).ReplaceAllString(exported,
		"${1}code: $2\n${1}iso_code: $2"))
	for _, cmd := range []string{"export", "check"} {
		out := a.run(1, cmd)
		if !strings.Contains(out, `model "Currency": its key is [iso_code], and public.currencies has no column iso_code`) ||
			strings.Contains(out, "SQLSTATE") {
			t.Fatalf("%s: %s", cmd, out)
		}
	}
	a.write("fixture-migrate.yml", cfg)

	// A column the fixture file writes that the table does not have.
	a.write("fixtures/fixture.yml", strings.Replace(exported, `symbol: "€"`, "symbol: \"€\"\n      colour: blue", 1))
	if !strings.Contains(a.read("fixtures/fixture.yml"), "colour") {
		t.Fatalf("the edit did not apply:\n%s", exported)
	}
	out := a.run(3, "check")
	if !strings.Contains(out, "Currency colour: the fixture file writes this column, public.currencies does not have it") {
		t.Fatal(out)
	}
	out = a.run(3, "status")
	if !strings.Contains(out, "generate refuses to write their migration") || strings.Contains(out, "run: bun-fixture-migrate generate") ||
		!strings.Contains(out, "unknown column") {
		t.Fatal(out)
	}
	a.run(2, "generate", "-name", "colour")

	// A zero against a default: status says what generate refuses.
	a.write("fixture-migrate.yml", strings.Replace(cfg, `price_cents: "0"`, `price_cents: "1"`, 1))
	run(t, a.db, "ALTER TABLE plans ALTER COLUMN price_cents SET DEFAULT 1")
	a.write("fixtures/fixture.yml", strings.Replace(exported, "price_cents: 2500", "price_cents: 0", 1))
	out = a.run(3, "status")
	if !strings.Contains(out, "zero against a default") || strings.Contains(out, "run: bun-fixture-migrate generate") {
		t.Fatal(out)
	}
	a.run(2, "generate", "-name", "zero")
}

// A table asked for that scaffold would not propose is refused rather than
// left out without a word, and so is a schema with nothing in it.
func TestScaffoldRefusesWhatItCannotPropose(t *testing.T) {
	a := newAdoption(t)
	for args, want := range map[string]string{
		"-tables plan,currencies":      "-tables names plan, which is not a table of schema public",
		"-tables plans,bun_migrations": "-tables names bun_migrations, which is the migrator's own table",
		"-schema nosuch":               "schema nosuch has no table to propose as a model",
	} {
		if out := a.run(1, append([]string{"scaffold"}, strings.Fields(args)...)...); !strings.Contains(out, want) {
			t.Errorf("%s: %s", args, out)
		}
	}
	// Built WithTableName, the migrator's table is named and left out.
	run(t, a.db, "ALTER TABLE bun_migrations RENAME TO schema_migrations")
	out := a.run(0, "scaffold", "-migrations-table", "schema_migrations")
	if !strings.Contains(out, "migrations_table: schema_migrations\n") || strings.Contains(out, "SchemaMigration:") {
		t.Fatal(out)
	}
}

// plan writes and rolls back, so it needs a role that can write what the
// migrations write. As a role that cannot, every change fails, and that is no
// verdict on the deploy, which migrates as another role: a role whose
// transactions start read only is refused like a standby, and a missing grant
// makes the plan inconclusive (exit 1), not a migration that would fail.
func TestPlanAsARoleThatCannotWrite(t *testing.T) {
	a := newAdoption(t)
	a.run(0, "scaffold", "-o", "fixture-migrate.yml", "-tables", "currencies,plans")
	exported := func() string { a.run(0, "export"); return a.read("fixtures/fixture.yml") }()
	a.run(0, "baseline")
	a.write("fixtures/fixture.yml", strings.Replace(exported, "price_cents: 2500", "price_cents: 2600", 1))
	a.run(0, "generate", "-name", "plan prices")
	a.run(0, "plan")

	roles := []string{"bfm_plan_reader", "bfm_plan_readonly"}
	drop := func() {
		for _, role := range roles {
			run(t, a.db, "DO $$ BEGIN IF EXISTS (SELECT FROM pg_roles WHERE rolname = '"+role+"') THEN "+
				"EXECUTE 'DROP OWNED BY "+role+"'; EXECUTE 'DROP ROLE "+role+"'; END IF; END $$")
		}
	}
	drop()
	t.Cleanup(drop)
	for _, role := range roles {
		run(t, a.db, "CREATE ROLE "+role+" LOGIN PASSWORD 'bfm-plan'", "GRANT USAGE ON SCHEMA public TO "+role,
			"GRANT SELECT ON ALL TABLES IN SCHEMA public TO "+role)
	}
	run(t, a.db, "GRANT ALL ON ALL TABLES IN SCHEMA public TO bfm_plan_readonly",
		"GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO bfm_plan_readonly",
		"ALTER ROLE bfm_plan_readonly SET default_transaction_read_only = on")
	dsn := func(role string) string {
		u, err := url.Parse(a.dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(role, "bfm-plan")
		return u.String()
	}
	out := a.run(1, "plan", "-dsn", dsn("bfm_plan_readonly"))
	if !strings.Contains(out, "starts every transaction of this connection read only") {
		t.Fatal(out)
	}
	out = a.run(1, "plan", "-dsn", dsn("bfm_plan_reader"))
	if !strings.Contains(out, "could not be planned") || !strings.Contains(out, "plan as the role the deploy uses") ||
		strings.Contains(out, "and so would the deploy") {
		t.Fatal(out)
	}
	// check and status only read, and are fine as either.
	a.run(3, "check", "-dsn", dsn("bfm_plan_readonly"))
	a.run(0, "status", "-dsn", dsn("bfm_plan_reader"))
}

// plan fails on the problems status reports in the migrations directory,
// the state file's history among them: a deploy of migrations that were not
// generated one after another is no deploy to pass, however well each of
// them plans.
func TestPlanFailsOnWhatTheStateFileSaysOfTheDirectory(t *testing.T) {
	a := newAdoption(t)
	a.run(0, "scaffold", "-o", "fixture-migrate.yml", "-tables", "currencies,plans")
	exported := func() string { a.run(0, "export"); return a.read("fixtures/fixture.yml") }()
	a.run(0, "baseline")
	a.write("fixtures/fixture.yml", strings.Replace(exported, "price_cents: 2500", "price_cents: 2600", 1))
	a.run(0, "generate", "-name", "plan prices", "-at", "20261001100000")
	a.run(0, "plan")
	gen := filepath.Join(a.dir, "internal", "migrations", "20261001100000_fixture_plan_prices.go")
	src := a.read("internal/migrations/20261001100000_fixture_plan_prices.go")

	// Generated on another branch, against the same state.
	other := filepath.Join(a.dir, "internal", "migrations", "20261001090000_fixture_other.go")
	if err := os.WriteFile(other, []byte(strings.ReplaceAll(src, "20261001100000", "20261001090000")), 0o644); err != nil {
		t.Fatal(err)
	}
	out := a.run(3, "plan")
	if !strings.Contains(out, "20261001090000_fixture_other is a generated fixture migration whose changes the state file does not include") {
		t.Fatal(out)
	}
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}

	// The migration the state includes last, deleted.
	if err := os.Remove(gen); err != nil {
		t.Fatal(err)
	}
	out = a.run(3, "plan", "-json")
	if !strings.Contains(out, "the state file includes the changes of 20261001100000_fixture_plan_prices, which is not in") {
		t.Fatal(out)
	}
}
