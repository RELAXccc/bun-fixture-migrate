package dbtest_test

// plan against bun's migrator: what plan says a deploy does, next to what
// bun's migrator does with the same migrations on the same database. plan runs
// everything in one transaction and rolls it back; the deploy commits each
// migration, and reads each SQL file its own way. Wherever the two differ,
// plan has to find out the deploy's answer or say it cannot.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

const deferredConfig = `fixture: fixtures/fixture.yml
out: migrations
package: migrations
seed_guard_table: d_items
database: env:BFM_TEST_DSN
models:
  DRegion: {table: d_regions, ref: code, key: [code]}
  DItem: {table: d_items, key: [name]}
`

const deferredFixture = `- model: DRegion
  rows:
    - {id: 1, code: us}
- model: DItem
  rows:
    - {id: 1, name: anvil, region_id: 1}
`

// deferredDB holds two tables joined by a foreign key PostgreSQL checks only
// at COMMIT. region_id is a plain column to the tool, as a column holding an
// id of a table that is not master data is.
func deferredDB(t *testing.T) *bun.DB {
	t.Helper()
	db := connect(t)
	run(t, db, "DROP TABLE IF EXISTS d_items, d_regions, bun_migrations, bun_migration_locks",
		"CREATE TABLE d_regions (id bigint PRIMARY KEY, code text UNIQUE NOT NULL)",
		"CREATE TABLE d_items (id bigint PRIMARY KEY, name text UNIQUE NOT NULL, region_id bigint NOT NULL "+
			"CONSTRAINT d_items_region_fkey REFERENCES d_regions DEFERRABLE INITIALLY DEFERRED)",
		"INSERT INTO d_regions VALUES (1, 'us')", "INSERT INTO d_items VALUES (1, 'anvil', 1)")
	return db
}

func deferredCLI(t *testing.T) *cli {
	t.Helper()
	c := buildCLI(t)
	c.write("fixture-migrate.yml", deferredConfig)
	c.write("fixtures/fixture.yml", deferredFixture)
	c.must(0, "baseline")
	return c
}

// projectMigrator builds bun's migrator over a project's migrations directory
// the way a project builds it: the Go migrations registering themselves, the
// SQL migrations discovered from an embedded file system.
func projectMigrator(t *testing.T, dir string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no Go toolchain to build with")
	}
	// Inside this module, like buildMigrator's, under a name of its own.
	const pkg = "projectmigratorcheck"
	t.Cleanup(func() { os.RemoveAll(pkg) })
	files := map[string]string{"main.go": strings.Replace(migratorMain,
		"/dbtest/migratorcheck/migrations", "/dbtest/"+pkg+"/migrations", 1)}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	discover := ""
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".sql") {
			discover = "\n//go:embed *.sql\nvar sqlFiles embed.FS\n\nfunc init() {\n\tif err := Migrations.Discover(sqlFiles); err != nil {\n\t\tpanic(err)\n\t}\n}\n"
		} else if !strings.HasSuffix(name, ".go") || name == "migrations.go" {
			continue
		}
		files["migrations/"+name] = readFileT(t, filepath.Join(dir, name))
	}
	files["migrations/migrations.go"] = migratorPackage
	if discover != "" {
		files["migrations/migrations.go"] = strings.Replace(migratorPackage, `import "github.com/uptrace/bun/migrate"`,
			"import (\n\t\"embed\"\n\n\t\"github.com/uptrace/bun/migrate\"\n)", 1) + discover
	}
	for path, content := range files {
		full := filepath.Join(pkg, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(t.TempDir(), "migrator")
	if out, err := exec.Command("go", "build", "-o", bin, "./"+pkg).CombinedOutput(); err != nil {
		t.Fatalf("the migrations do not build: %v\n%s", err, out)
	}
	return bin
}

// bunMigrate runs bun's migrator, in this process, over the SQL migrations of
// a directory.
func bunMigrate(t *testing.T, db *bun.DB, dir string, opts ...migrate.MigratorOption) error {
	t.Helper()
	ctx := context.Background()
	ms := migrate.NewMigrations()
	if err := ms.Discover(os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	m := migrate.NewMigrator(db, ms, opts...)
	if err := m.Init(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := m.Migrate(ctx)
	return err
}

// A fixture migration whose insert breaks a deferred foreign key is fine
// until it commits. plan and a dry-run sync never commit, so they have to ask
// for the check where the deploy commits, and pin the failure on the
// migration that caused it.
func TestPlanAndSyncCheckDeferredConstraints(t *testing.T) {
	db := deferredDB(t)
	c := deferredCLI(t)
	withHammer := deferredFixture + "    - {id: 2, name: hammer, region_id: 1}\n"
	c.write("fixtures/fixture.yml", withHammer)
	c.must(0, "generate", "-name", "hammer", "-at", "20300101000000")
	withSaw := withHammer + "    - {id: 3, name: saw, region_id: 9}\n"
	c.write("fixtures/fixture.yml", withSaw)
	c.must(0, "generate", "-name", "saw", "-at", "20300101000001")

	out := c.must(3, "plan")
	for _, want := range []string{
		"20300101000000_fixture_hammer: would succeed",
		"20300101000001_fixture_saw: would FAIL",
		"when it commits, where PostgreSQL checks the constraints it defers",
		"d_items_region_fkey",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan is missing %q:\n%s", want, out)
		}
	}
	if out := c.must(1, "sync"); !strings.Contains(out, "would apply DItem name=saw insert") ||
		!strings.Contains(out, "would fail when committed") || strings.Contains(out, "run it again with -yes") {
		t.Fatalf("sync without -yes:\n%s", out)
	}
	if out := c.must(1, "sync", "-yes"); !strings.Contains(out, "d_items_region_fkey") {
		t.Fatalf("sync -yes:\n%s", out)
	}
	if got := scan[int64](t, db, "SELECT count(*) FROM d_items"); got != 1 {
		t.Fatalf("%d items after a sync that failed", got)
	}

	// The deploy: the first migration commits, the second fails at its commit.
	ok, deploy := runMigrator(t, projectMigrator(t, filepath.Join(c.dir, "migrations")), false)
	if ok || !strings.Contains(deploy, "d_items_region_fkey") || !strings.Contains(deploy, "20300101000001") {
		t.Fatalf("the deploy has to fail at the second migration, as plan said:\n%s", deploy)
	}
	if got := scan[string](t, db, "SELECT string_agg(name, ',' ORDER BY id) FROM d_items"); got != "anvil,hammer" {
		t.Fatalf("after the deploy: %s", got)
	}
}

// A SQL migration bun runs without a transaction commits each statement by
// itself; a .tx.up.sql commits once at the end. A deferred constraint is
// checked at each of those points, in plan as in the deploy.
func TestPlanChecksDeferredConstraintsWhereASQLMigrationCommits(t *testing.T) {
	const childFirst = "INSERT INTO d_items VALUES (3, 'saw', 3);\n--bun:split\nINSERT INTO d_regions VALUES (3, 'eu');\n"
	for _, tc := range []struct {
		file    string
		deploys bool
	}{
		{"20000101000000_eu.up.sql", false},
		{"20000101000000_eu.tx.up.sql", true},
	} {
		db := deferredDB(t)
		c := deferredCLI(t)
		c.write("migrations/"+tc.file, childFirst)
		var out string
		if tc.deploys {
			out = c.must(0, "plan", "-with-sql")
		} else {
			out = c.must(3, "plan", "-with-sql")
		}
		if !strings.Contains(out, "20000101000000_eu (SQL): would") {
			t.Fatalf("%s: plan -with-sql:\n%s", tc.file, out)
		}
		deferredDB(t)
		err := bunMigrate(t, db, filepath.Join(c.dir, "migrations"))
		if (err == nil) != tc.deploys {
			t.Fatalf("%s: bun's migrator returned %v, plan said:\n%s", tc.file, err, out)
		}
	}
}

// bunVersion is the version of bun this module is built with.
func bunVersion() string {
	info, _ := debug.ReadBuildInfo()
	for _, d := range info.Deps {
		if d.Path == "github.com/uptrace/bun" {
			return d.Version
		}
	}
	return ""
}

// plan -with-sql reads a SQL migration as bun v1.2.18 does, and says so where
// what the deploy does depends on more than the file.
func TestPlanReadsSQLMigrationsAsBunDoes(t *testing.T) {
	// A line longer than 64 KiB: bun's scanner stops, nothing in the file
	// runs, and under the default migrator the migration stays recorded.
	db := deferredDB(t)
	c := deferredCLI(t)
	c.write("migrations/20000101000000_long.up.sql",
		"INSERT INTO d_regions VALUES (5, '"+strings.Repeat("x", 70*1024)+"');\n")
	out := c.must(3, "plan", "-with-sql")
	if !strings.Contains(out, "20000101000000_long (SQL): would FAIL") || !strings.Contains(out, "token too long") ||
		!strings.Contains(out, "keeps the migration recorded as applied") {
		t.Fatalf("a line over 64 KiB:\n%s", out)
	}
	err := bunMigrate(t, db, filepath.Join(c.dir, "migrations"))
	if err == nil || !strings.Contains(err.Error(), "token too long") {
		t.Fatalf("bun's migrator: %v", err)
	}
	if got := scan[int64](t, db, "SELECT count(*) FROM bun_migrations"); got != 1 {
		t.Fatalf("bun recorded %d migrations; the premise is that it keeps the failed one", got)
	}

	// A blank line inside a literal: bun v1.2.18 keeps it, and so does plan,
	// with a note that later bun does not.
	db = deferredDB(t)
	c = deferredCLI(t)
	c.write("migrations/20000101000000_blank.up.sql", "INSERT INTO d_regions VALUES (5, 'para one\n\npara two');\n"+
		"--bun:split\nDO $$ BEGIN IF NOT EXISTS (SELECT FROM d_regions WHERE code = E'para one\\n\\npara two') "+
		"THEN RAISE EXCEPTION 'the blank line was dropped'; END IF; END $$;\n")
	out = c.must(0, "plan", "-with-sql")
	if !strings.Contains(out, "20000101000000_blank (SQL): would succeed") ||
		!strings.Contains(out, "bun after v1.2.18 drops blank lines") {
		t.Fatalf("a blank line in a literal:\n%s", out)
	}
	err = bunMigrate(t, db, filepath.Join(c.dir, "migrations"))
	switch {
	case bunVersion() == "v1.2.18" && err != nil:
		t.Fatalf("bun v1.2.18 keeps the blank line, as plan does: %v", err)
	case err != nil && !strings.Contains(err.Error(), "the blank line was dropped"):
		t.Fatalf("bun %s: %v", bunVersion(), err)
	}

	// A template: what bun runs depends on the data the migrator is built
	// with, so plan runs none of it. Run as it is written, it would fail.
	db = deferredDB(t)
	c = deferredCLI(t)
	c.write("migrations/20000101000000_tpl.up.sql", "INSERT INTO d_regions VALUES ({{ .ID }}, 'tpl');\n")
	out = c.must(0, "plan", "-with-sql")
	if !strings.Contains(out, "not simulated, not fixture migrations: 20000101000000_tpl") ||
		!strings.Contains(out, "WithTemplateData") || strings.Contains(out, "FAIL") {
		t.Fatalf("a template:\n%s", out)
	}
	if err := bunMigrate(t, db, filepath.Join(c.dir, "migrations"),
		migrate.WithTemplateData(map[string]int{"ID": 7})); err != nil {
		t.Fatalf("bun's migrator with template data: %v", err)
	}
	if got := scan[int64](t, db, "SELECT count(*) FROM d_regions WHERE id = 7"); got != 1 {
		t.Fatal("bun did not render the template")
	}
}

// An enum value a SQL migration adds, used by the fixture migration after it.
// The deploy commits the first before it runs the second, and succeeds; one
// transaction cannot use a value it added, so plan cannot tell, and says so.
func TestPlanCannotUseAnEnumValueAMigrationAdds(t *testing.T) {
	db := connect(t)
	run(t, db, "DROP TABLE IF EXISTS d_moods, bun_migrations, bun_migration_locks", "DROP TYPE IF EXISTS d_mood",
		"CREATE TYPE d_mood AS ENUM ('sad', 'ok')",
		"CREATE TABLE d_moods (id bigint PRIMARY KEY, name text UNIQUE NOT NULL, m d_mood NOT NULL)",
		"INSERT INTO d_moods VALUES (1, 'a', 'sad')")
	c := buildCLI(t)
	c.write("fixture-migrate.yml", `fixture: fixtures/fixture.yml
out: migrations
package: migrations
seed_guard_table: d_moods
database: env:BFM_TEST_DSN
models:
  Mood: {table: d_moods, key: [name]}
`)
	c.write("fixtures/fixture.yml", "- model: Mood\n  rows:\n    - {id: 1, name: a, m: sad}\n")
	c.must(0, "baseline")
	c.write("migrations/20000101000000_add_happy.tx.up.sql", "ALTER TYPE d_mood ADD VALUE 'happy';\n")
	c.write("fixtures/fixture.yml", "- model: Mood\n  rows:\n    - {id: 1, name: a, m: happy}\n")
	c.must(0, "generate", "-name", "happy", "-no-lint", "-at", "20300101000000")

	out := c.must(1, "plan", "-with-sql")
	if !strings.Contains(out, "20000101000000_add_happy (SQL): would succeed") ||
		!strings.Contains(out, "20300101000000_fixture_happy: could not be planned") ||
		!strings.Contains(out, "enum value") {
		t.Fatalf("plan -with-sql:\n%s", out)
	}
	if ok, deploy := runMigrator(t, projectMigrator(t, filepath.Join(c.dir, "migrations")), false); !ok {
		t.Fatalf("the deploy:\n%s", deploy)
	}
	if got := scan[string](t, db, "SELECT m::text FROM d_moods WHERE name = 'a'"); got != "happy" {
		t.Fatalf("after the deploy: %s", got)
	}
}

// A column the database lacks fails a fixture migration, unless a migration
// plan did not run comes before it: a Go schema migration can add it, and
// the deploy then succeeds.
func TestAMissingColumnIsNoVerdictAfterAMigrationPlanDidNotRun(t *testing.T) {
	deferredDB(t)
	c := deferredCLI(t)
	// The rows written before the column existed hold NULL in it.
	c.write("fixture-migrate.yml", strings.Replace(deferredConfig, "DItem: {table: d_items, key: [name]}",
		"DItem: {table: d_items, key: [name], defaults: {color: ~}}", 1))
	c.write("fixtures/fixture.yml", strings.Replace(deferredFixture, "region_id: 1}", "region_id: 1, color: red}", 1))
	c.must(0, "generate", "-name", "color", "-no-lint", "-at", "20300101000000")
	if out := c.must(3, "plan"); !strings.Contains(out, "_fixture_color: would FAIL") ||
		!strings.Contains(out, `column "color"`) {
		t.Fatalf("nothing pending before it:\n%s", out)
	}
	c.write("migrations/20200101000000_add_color.go", `package migrations

import (
	"context"

	"github.com/uptrace/bun"
)

func init() {
	Migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		_, err := db.ExecContext(ctx, "ALTER TABLE d_items ADD COLUMN color text")
		return err
	}, nil)
}
`)
	out := c.must(1, "plan")
	for _, want := range []string{
		"_fixture_color: could not be planned",
		"pending before it and not simulated: 20200101000000_add_color",
		"a migration that runs before it in the deploy but was not simulated can create it",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan is missing %q:\n%s", want, out)
		}
	}
	if ok, deploy := runMigrator(t, projectMigrator(t, filepath.Join(c.dir, "migrations")), false); !ok {
		t.Fatalf("the deploy:\n%s", deploy)
	}
}

// A directory bun cannot run as it stands fails the plan, however each
// migration in it went: here two migrations bun records under one name, so
// that one of them never runs.
func TestPlanFailsOnAProblemInTheMigrationsDirectory(t *testing.T) {
	deferredDB(t)
	c := deferredCLI(t)
	c.write("migrations/20000101000000_a.up.sql", "SELECT 1;\n")
	c.write("migrations/20000101000000_b.up.sql", "SELECT 2;\n")
	out := c.must(3, "plan", "-with-sql")
	if !strings.Contains(out, "share the name 20000101000000") ||
		!strings.Contains(out, "1 problem in the migrations directory") {
		t.Fatalf("plan:\n%s", out)
	}
	_, stdout, _ := c.run("plan", "-json")
	if !strings.Contains(stdout, `"problems": [`) || !strings.Contains(stdout, "share the name") {
		t.Fatalf("plan -json:\n%s", stdout)
	}
}

// plan holds what it writes locked until it rolls back, and says how much and
// how long. A sequence is outside every transaction, so what a SQL migration
// does to one stays done after the rollback, and plan says that too.
func TestPlanSaysWhatItHeldAndWhatItLeaves(t *testing.T) {
	db := deferredDB(t)
	run(t, db, "DROP SEQUENCE IF EXISTS d_seq", "CREATE SEQUENCE d_seq")
	c := deferredCLI(t)
	c.write("fixtures/fixture.yml", deferredFixture+"    - {id: 2, name: hammer, region_id: 1}\n")
	c.must(0, "generate", "-name", "hammer", "-at", "20300101000000")
	var report struct {
		RowsLocked    int64   `json:"rows_locked"`
		LockedSeconds float64 `json:"locked_seconds"`
	}
	_, stdout, _ := c.run("plan", "-json")
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || report.RowsLocked != 1 || report.LockedSeconds <= 0 {
		t.Fatalf("plan -json: %v\n%s", err, stdout)
	}
	if out := c.must(0, "plan"); !strings.Contains(out, "it held locked the 1 row it wrote") {
		t.Fatalf("plan:\n%s", out)
	}

	c.write("migrations/20000101000000_seq.up.sql", "SELECT setval('d_seq', 42);\n")
	out := c.must(0, "plan", "-with-sql")
	if !strings.Contains(out, "it calls setval or nextval, which no rollback undoes") ||
		!strings.Contains(out, "except sequences") {
		t.Fatalf("plan -with-sql:\n%s", out)
	}
	if got := scan[int64](t, db, "SELECT last_value FROM d_seq"); got != 42 {
		t.Fatalf("the premise: setval in a rolled back transaction stays, and d_seq is at %d", got)
	}
}
