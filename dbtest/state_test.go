package dbtest_test

// The state file and status against a real database and bun's real migrator:
// two branches that each generated a migration from one state, merged and
// deployed; a value written another way; a migration merged late, and the
// lock a migrator that died leaves behind; and a partial generate from the
// database.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun/migrate"
)

// buildMigratorFrom builds bun's migrator over every Go migration of a
// migrations directory, the way the application that owns it would.
func buildMigratorFrom(t *testing.T, migrations string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no Go toolchain to build with")
	}
	dir := "migratorcheck"
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"main.go": []byte(migratorMain), "migrations/migrations.go": []byte(migratorPackage)}
	entries, err := os.ReadDir(migrations)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || e.Name() == "migrations.go" {
			continue
		}
		src, err := os.ReadFile(filepath.Join(migrations, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[filepath.Join("migrations", e.Name())] = src
	}
	for path, content := range files {
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
		t.Fatalf("the migrations do not build: %v\n%s", err, out)
	}
	return bin
}

// git runs git in the project, failing the test unless it fails exactly when
// expected to.
func (c *cli) git(fail bool, args ...string) {
	c.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", c.dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if (err != nil) != fail {
		c.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// The runbook's procedure for two branches that each generated a migration
// from one state, both changing one row, proved against the two databases it
// has to hold for: production, which applied the newer migration before the
// older branch was merged, and a database that runs every migration from the
// start. Keeping both migrations and recording the merge with baseline -force
// would leave production at the newer branch's value with the file saying the
// older's, and a new database at the older's: so that is refused, and
// generating the older one again on top of the newer one is what converges.
func TestMergedBranchesConvergeAfterTheRunbook(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	db := itemDB(t)
	run(t, db, "DROP TABLE IF EXISTS bun_migrations, bun_migration_locks")
	loadFixture(t, db, itemFixture)
	c := buildCLI(t)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"}} {
		c.git(false, args...)
	}
	c.must(0, "baseline")
	c.git(false, "add", ".")
	c.git(false, "commit", "-q", "-m", "baseline")

	older := replaceOnce(t, itemFixture, "      cost: 120\n", "      cost: 130\n")
	newer := replaceOnce(t, replaceOnce(t, itemFixture, "      cost: 120\n", "      cost: 140\n"),
		"      production_max: 3\n", "      production_max: 4\n")
	for _, b := range []struct{ name, fixture, at string }{
		{"older", older, "20261001100000"}, {"newer", newer, "20261001110000"}} {
		c.git(false, "checkout", "-q", "-b", b.name, "main")
		c.write("fixtures/fixture.yml", b.fixture)
		c.must(0, "generate", "-name", b.name, "-at", b.at)
		c.git(false, "add", ".")
		c.git(false, "commit", "-q", "-m", b.name)
	}
	c.git(false, "checkout", "-q", "main")
	c.git(false, "merge", "-q", "newer")
	migrations := filepath.Join(c.dir, "migrations")
	if ok, out := runMigrator(t, buildMigratorFrom(t, migrations), false); !ok {
		t.Fatalf("deploy the newer branch:\n%s", out)
	}

	// The older branch arrives. Its fixture change wins the merge, the newer
	// one's other change stays.
	c.git(true, "merge", "-q", "older")
	c.write("fixtures/fixture.yml", replaceOnce(t, newer, "      cost: 140\n", "      cost: 130\n"))
	for _, side := range []string{"--theirs", "--ours"} {
		c.git(false, "checkout", side, "--", "migrations/fixture_state.yml")
		if out := c.must(2, "baseline", "-force"); !strings.Contains(out, "generating again does") {
			t.Fatalf("%s: baseline -force:\n%s", side, out)
		}
		if out := c.must(3, "status", "-offline"); !strings.Contains(out, "state file does not include") {
			t.Fatalf("%s: status -offline:\n%s", side, out)
		}
	}
	// Production applied the newer one, so the older one would run after it.
	_, stdout, _ := c.run("status", "-json")
	var report struct {
		Migrations []struct {
			ID         string
			OutOfOrder bool `json:"out_of_order"`
		}
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || len(report.Migrations) != 2 ||
		report.Migrations[0].ID != "20261001100000_fixture_older" || !report.Migrations[0].OutOfOrder {
		t.Fatalf("status -json: %v\n%s", err, stdout)
	}

	// The runbook: delete the migration no database applied, take the state
	// file the other one left, generate again.
	c.git(false, "rm", "-q", "-f", "migrations/20261001100000_fixture_older.go")
	c.git(false, "checkout", "--ours", "--", "migrations/fixture_state.yml")
	c.must(0, "generate", "-name", "older")
	c.must(0, "status", "-offline")
	if out := c.must(0, "plan", "-strict"); !strings.Contains(out, "applied Item name=anvil update (1 row)") {
		t.Fatalf("plan:\n%s", out)
	}
	c.git(false, "add", ".")
	c.git(false, "commit", "-q", "-m", "merge older")

	bin := buildMigratorFrom(t, migrations)
	if ok, out := runMigrator(t, bin, false); !ok {
		t.Fatalf("deploy the merge:\n%s", out)
	}
	c.must(0, "check")
	c.must(0, "status", "-require-applied", "-strict-order")
	want := "130 4"
	if got := scan[string](t, db, "SELECT i.cost || ' ' || r.production_max FROM items i, items r "+
		"WHERE i.name = 'anvil' AND r.name = 'rope'"); got != want {
		t.Fatalf("production holds %q, the file says %q", got, want)
	}

	// A database that runs every migration from the start ends in the same
	// place.
	fresh := itemDB(t)
	run(t, fresh, "DROP TABLE IF EXISTS bun_migrations, bun_migration_locks")
	loadFixture(t, fresh, itemFixture)
	if ok, out := runMigrator(t, bin, false); !ok {
		t.Fatalf("migrate a new database:\n%s", out)
	}
	c.must(0, "check")
	if got := scan[string](t, fresh, "SELECT i.cost || ' ' || r.production_max FROM items i, items r "+
		"WHERE i.name = 'anvil' AND r.name = 'rope'"); got != want {
		t.Fatalf("a new database holds %q, the file says %q", got, want)
	}
}

// 1.5 written 1.50 is no change in a double precision column, and only the
// database can say so. status asks it, baseline asks it, and generate records
// the new spelling, so status -offline in CI does not fail on it forever.
func TestARespelledValueIsNoChange(t *testing.T) {
	db := itemDB(t)
	loadFixture(t, db, itemFixture)
	c := buildCLI(t)
	c.must(0, "baseline")
	respelled := replaceOnce(t, itemFixture, "      ratio: 1.5\n", "      ratio: 1.50\n")
	c.write("fixtures/fixture.yml", respelled)

	// Without the column's type, 1.50 could be a string field's text.
	c.must(3, "status", "-offline")
	if out := c.must(0, "status"); !strings.Contains(out, "only in how values are written") {
		t.Fatalf("status:\n%s", out)
	}
	c.must(2, "baseline", "-offline")

	out := c.must(0, "generate", "-name", "ratio")
	if !strings.Contains(out, "nothing changed") || !strings.Contains(out, "only in how it is written") {
		t.Fatalf("generate:\n%s", out)
	}
	entries, _ := os.ReadDir(filepath.Join(c.dir, "migrations"))
	for _, e := range entries {
		if strings.Contains(e.Name(), "_fixture_") {
			t.Fatalf("a migration for no change: %s", e.Name())
		}
	}
	c.must(0, "status", "-offline")

	c.write("fixtures/fixture.yml", replaceOnce(t, respelled, "      ratio: 1.50\n", "      ratio: 1.500\n"))
	if out := c.must(0, "baseline"); !strings.Contains(out, "only in how values are written") {
		t.Fatalf("baseline:\n%s", out)
	}
	c.must(0, "status", "-offline")
}

// A migration merged after a later one was deployed runs after it; status
// marks it, and -strict-order fails on it. A migrator that died between Lock
// and Unlock leaves a row that makes every later migrate fail; status names
// it and says how to remove it.
func TestStatusSeesOrderAndALeftoverLock(t *testing.T) {
	db := itemDB(t)
	run(t, db, "DROP TABLE IF EXISTS bun_migrations, bun_migration_locks")
	loadFixture(t, db, itemFixture)
	c := buildCLI(t)
	c.must(0, "baseline")
	c.write("fixtures/fixture.yml", replaceOnce(t, itemFixture, "      cost: 120\n", "      cost: 130\n"))
	c.must(0, "generate", "-name", "one", "-at", "20261001100000")
	c.write("fixtures/fixture.yml", replaceOnce(t, itemFixture, "      cost: 120\n", "      cost: 140\n"))
	c.must(0, "generate", "-name", "two", "-at", "20261001110000")

	ctx := context.Background()
	m := migrate.NewMigrator(db, migrate.NewMigrations())
	if err := m.Init(ctx); err != nil {
		t.Fatal(err)
	}
	run(t, db, "INSERT INTO bun_migrations (name, group_id) VALUES ('20261001110000', 1)")
	out := c.must(0, "status")
	if !strings.Contains(out, "pending 20261001100000_fixture_one fixture, 1 change out of order: runs after 20261001110000") {
		t.Fatalf("status:\n%s", out)
	}
	if out := c.must(3, "status", "-strict-order"); !strings.Contains(out, "1 pending migration out of order") {
		t.Fatalf("status -strict-order:\n%s", out)
	}

	if err := m.Lock(ctx); err != nil {
		t.Fatal(err)
	}
	out = c.must(3, "status")
	if !strings.Contains(out, "locked: bun_migration_locks holds bun's lock on bun_migrations") ||
		!strings.Contains(out, "DELETE FROM bun_migration_locks WHERE table_name = 'bun_migrations'") {
		t.Fatalf("status with a lock:\n%s", out)
	}
	_, stdout, _ := c.run("status", "-json")
	var report struct {
		Database struct {
			Locked        bool
			LocksTable    string `json:"locks_table"`
			NewestApplied string `json:"newest_applied"`
		}
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || !report.Database.Locked ||
		report.Database.LocksTable != "bun_migration_locks" || report.Database.NewestApplied != "20261001110000" {
		t.Fatalf("status -json: %v\n%s", err, stdout)
	}
	if err := m.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
	c.must(0, "status")
}

// generate -from-db -allow-partial records what it left out like generate
// against the state does: status fails on it until baseline -force.
func TestAPartialGenerateFromTheDatabase(t *testing.T) {
	db := itemDB(t)
	loadFixture(t, db, itemFixture)
	c := buildCLI(t)
	c.must(0, "baseline")
	edited := replaceOnce(t, itemFixture, "      cost: 120\n", "      cost: 130\n")
	c.write("fixtures/fixture.yml", replaceOnce(t, edited, `name: "rope"`, `name: "cord"`))
	c.must(2, "generate", "-from-db", "-name", "cost")
	c.must(0, "generate", "-from-db", "-name", "cost", "-allow-partial")
	if out := c.must(3, "status", "-offline"); !strings.Contains(out, "left out") || !strings.Contains(out, "renamed") {
		t.Fatalf("status:\n%s", out)
	}
	if out := c.must(0, "generate", "-name", "again"); !strings.Contains(out, "nothing changed") {
		t.Fatalf("the accepted change is not generated twice:\n%s", out)
	}
	c.must(2, "baseline")
	c.must(0, "baseline", "-force")
	c.must(0, "status", "-offline")
}

// bun's Migrator.Lock does not wait, so the example's deploy step retries it:
// a deploy started while another holds the lock goes through once that one
// lets go, and against a lock a deploy that died left, it gives up after
// LOCK_WAIT with the statement that removes it, which status prints too.
func TestTheExampleWaitsForTheMigratorLock(t *testing.T) {
	e := newExample(t)
	ctx := context.Background()
	e.exec(0, e.app, "migrate")
	m := migrate.NewMigrator(e.db, migrate.NewMigrations())
	if err := m.Lock(ctx); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(e.app, "migrate")
	cmd.Dir = e.dir
	cmd.Env = append(os.Environ(), "DATABASE_URL="+e.dsn)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if err := m.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil || !strings.Contains(out.String(), "waited") {
		t.Fatalf("a deploy behind another one: %v\n%s", err, out.String())
	}

	if err := m.Lock(ctx); err != nil {
		t.Fatal(err)
	}
	if out := e.exec(3, e.tool, "status"); !strings.Contains(out, "locked: bun_migration_locks") {
		t.Fatalf("status:\n%s", out)
	}
	t.Setenv("LOCK_WAIT", "500ms")
	if out := e.exec(1, e.app, "migrate"); !strings.Contains(out, "DELETE FROM bun_migration_locks") {
		t.Fatalf("a lock nobody releases:\n%s", out)
	}
	if err := m.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
	e.exec(0, e.tool, "status", "-require-applied")
}
