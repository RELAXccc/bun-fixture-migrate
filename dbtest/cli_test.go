package dbtest_test

// The command, built and driven the way a project drives it, through the whole
// life of a fixture change against a real database: record where the
// databases stand, edit the file, see CI catch the edit, generate, dry-run it
// against a database that is fine and against one that drifted, deploy it with
// bun's migrator, and check that everything agrees afterwards.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const cliConfig = `fixture: fixtures/fixture.yml
out: migrations
package: migrations
seed_guard_table: items
database: env:BFM_TEST_DSN
models:
  Region:
    table: regions
    ref: code
    key: [code]
  Item:
    table: items
    serial: true
    key: [name]
    references:
      region_id: Region
`

type cli struct {
	t   *testing.T
	bin string
	dir string
}

func buildCLI(t *testing.T) *cli {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no Go toolchain to build with")
	}
	bin := filepath.Join(t.TempDir(), "bun-fixture-migrate")
	out, err := exec.Command("go", "build", "-o", bin,
		"github.com/RELAXccc/bun-fixture-migrate/cmd/bun-fixture-migrate").CombinedOutput()
	if err != nil {
		t.Fatalf("build the command: %v\n%s", err, out)
	}
	dir := t.TempDir()
	c := &cli{t: t, bin: bin, dir: dir}
	c.write("fixture-migrate.yml", cliConfig)
	c.write("fixtures/fixture.yml", itemFixture)
	c.write("migrations/migrations.go", migratorPackage)
	return c
}

func (c *cli) write(rel, content string) {
	c.t.Helper()
	path := filepath.Join(c.dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		c.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		c.t.Fatal(err)
	}
}

// run runs one command and returns its exit code and what it printed.
func (c *cli) run(args ...string) (int, string, string) {
	c.t.Helper()
	cmd := exec.Command(c.bin, append(args[:1:1], append([]string{"-config", filepath.Join(c.dir, "fixture-migrate.yml")}, args[1:]...)...)...)
	cmd.Env = append(os.Environ(), "BFM_TEST_DSN="+os.Getenv("BUN_FIXTURE_MIGRATE_POSTGRES"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		c.t.Fatal(err)
	}
	return code, stdout.String(), stderr.String()
}

// must runs one command, fails unless it exits with want, and returns what it
// printed with every run of spaces collapsed, so a check does not depend on
// how a column was padded.
func (c *cli) must(want int, args ...string) string {
	c.t.Helper()
	code, stdout, stderr := c.run(args...)
	if code != want {
		c.t.Fatalf("%v: exit %d, want %d\n%s%s", args, code, want, stdout, stderr)
	}
	var lines []string
	for _, line := range strings.Split(stdout+stderr, "\n") {
		lines = append(lines, strings.Join(strings.Fields(line), " "))
	}
	return strings.Join(lines, "\n")
}

func TestTheLifeOfAFixtureChange(t *testing.T) {
	db := itemDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS bun_migrations, bun_migration_locks"); err != nil {
		t.Fatal(err)
	}
	loadFixture(t, db, itemFixture)
	c := buildCLI(t)

	// The databases hold the file as it is: that is the baseline.
	c.must(0, "baseline")
	out := c.must(0, "status")
	if !strings.Contains(out, "bun_migrations does not exist") || !strings.Contains(out, "not migrated nothing") {
		t.Fatalf("status of a fresh project:\n%s", out)
	}
	c.must(0, "check")

	// An edit: a price and a new item. CI catches it before a migration exists.
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
	if out := c.must(3, "status", "-offline"); !strings.Contains(out, "Item: 1 insert, 1 update") {
		t.Fatalf("status has to name what is not migrated:\n%s", out)
	}
	c.must(3, "check")

	out = c.must(0, "generate", "-name", "hammer")
	var generated string
	entries, _ := os.ReadDir(filepath.Join(c.dir, "migrations"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "_fixture_hammer.go") {
			generated = e.Name()
		}
	}
	if generated == "" {
		t.Fatalf("no migration written:\n%s", out)
	}
	c.must(0, "status", "-offline")

	// The dry run says what the deploy will do, and does none of it.
	out = c.must(0, "plan")
	if !strings.Contains(out, "_fixture_hammer: would succeed") ||
		!strings.Contains(out, "applied Item name=hammer insert (1 row)") ||
		!strings.Contains(out, "applied Item name=anvil update (1 row)") {
		t.Fatalf("plan:\n%s", out)
	}
	if got := scan[int64](t, db, "SELECT count(*) FROM items WHERE name = 'hammer' OR cost = 130"); got != 0 {
		t.Fatal("plan changed the database")
	}

	// Against a database somebody edited, the plan fails the way the deploy
	// would, and -strict turns a skipped row into a failure too.
	if _, err := db.ExecContext(ctx, "UPDATE items SET name = 'anvil (old)' WHERE name = 'anvil'"); err != nil {
		t.Fatal(err)
	}
	if out := c.must(3, "plan"); !strings.Contains(out, "would FAIL") || !strings.Contains(out, "[missing row]") {
		t.Fatalf("plan against a drifted database:\n%s", out)
	}
	if _, err := db.ExecContext(ctx, "UPDATE items SET name = 'anvil', cost = 125 WHERE name = 'anvil (old)'"); err != nil {
		t.Fatal(err)
	}
	if out := c.must(0, "plan"); !strings.Contains(out, "skipped Item name=anvil update [changed row]") {
		t.Fatalf("a hand-edited row is skipped under the default policy:\n%s", out)
	}
	c.must(3, "plan", "-strict")
	if _, err := db.ExecContext(ctx, "UPDATE items SET cost = 120 WHERE name = 'anvil'"); err != nil {
		t.Fatal(err)
	}

	var plan struct {
		Migrations []struct {
			Result  string
			Changes []struct{ Status, Model, Key string }
		}
	}
	_, stdout, _ := c.run("plan", "-json")
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil || len(plan.Migrations) != 1 ||
		plan.Migrations[0].Result != "succeeds" || len(plan.Migrations[0].Changes) != 2 {
		t.Fatalf("plan -json: %v\n%s", err, stdout)
	}

	// Deploy with bun's migrator, then everything agrees.
	src, err := os.ReadFile(filepath.Join(c.dir, "migrations", generated))
	if err != nil {
		t.Fatal(err)
	}
	bin := buildMigrator(t, generated[:14], "hammer", src)
	if ok, out := runMigrator(t, bin, false); !ok {
		t.Fatalf("migrate:\n%s", out)
	}
	out = c.must(0, "status", "-require-applied")
	if !strings.Contains(out, "applied "+strings.TrimSuffix(generated, ".go")) {
		t.Fatalf("status after the deploy:\n%s", out)
	}
	if out := c.must(0, "plan"); !strings.Contains(out, "no pending fixture migrations") {
		t.Fatalf("plan after the deploy:\n%s", out)
	}
	_, stdout, _ = c.run("check", "-json")
	var report struct{ Agree bool }
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || !report.Agree {
		t.Fatalf("check -json after the deploy: %v\n%s", err, stdout)
	}
}

// export, check, status and scaffold write nothing, and PostgreSQL holds them
// to it: even a where clause that calls a function with a side effect fails
// rather than writes.
func TestReadingCommandsCannotWrite(t *testing.T) {
	db := itemDB(t)
	loadFixture(t, db, itemFixture)
	c := buildCLI(t)
	c.write("fixture-migrate.yml", strings.Replace(cliConfig, "    key: [name]\n",
		"    key: [name]\n    where: \"nextval('items_id_seq') > 0\"\n", 1))
	before := scan[int64](t, db, "SELECT last_value FROM items_id_seq")
	for _, cmd := range []string{"export", "check"} {
		code, _, stderr := c.run(cmd, "-o", filepath.Join(c.dir, "out.yml"))
		if cmd == "check" {
			code, _, stderr = c.run(cmd)
		}
		if code == 0 || !strings.Contains(stderr, "read-only transaction") {
			t.Fatalf("%s: exit %d, expected PostgreSQL to refuse the write:\n%s", cmd, code, stderr)
		}
	}
	if after := scan[int64](t, db, "SELECT last_value FROM items_id_seq"); after != before {
		t.Fatalf("the sequence moved from %d to %d", before, after)
	}
}

// A database the fixture loader has not seeded yet is left alone, and a
// pending migration this tool did not write is named rather than guessed at.
func TestPlanSaysWhatItCannotSimulate(t *testing.T) {
	db := itemDB(t)
	if _, err := db.ExecContext(context.Background(), "DROP TABLE IF EXISTS bun_migrations"); err != nil {
		t.Fatal(err)
	}
	c := buildCLI(t)
	c.must(0, "baseline")
	c.write("fixtures/fixture.yml", replaceOnce(t, itemFixture, "      cost: 120\n", "      cost: 130\n"))
	c.must(0, "generate", "-name", "cost")
	c.write("migrations/20000101000000_add_column.up.sql", "ALTER TABLE items ADD COLUMN x int")
	out := c.must(0, "plan")
	for _, want := range []string{
		"_fixture_cost: would do nothing, the database is not seeded yet",
		"not simulated, not fixture migrations: 20000101000000_add_column",
		"rolled back: nothing was changed",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "sequence") {
		t.Fatalf("nothing was inserted, so no sequence moved:\n%s", out)
	}
}
