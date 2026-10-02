package dbtest_test

// The example project, built and deployed the way its README says, against a
// database of its own: a new database is migrated and seeded, a database that
// holds the previous fixture data is migrated forward, a drifted one is
// stopped, and a rollback takes the fixture change back.

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
)

type example struct {
	t    *testing.T
	dir  string
	app  string
	tool string
	dsn  string
	db   *bun.DB
}

func newExample(t *testing.T) *example {
	t.Helper()
	admin := connect(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no Go toolchain to build with")
	}
	u, err := url.Parse(os.Getenv("BUN_FIXTURE_MIGRATE_POSTGRES"))
	if err != nil {
		t.Fatal(err)
	}
	run(t, admin, "DROP DATABASE IF EXISTS bfm_example", "CREATE DATABASE bfm_example")
	u.Path = "/bfm_example"
	dir, err := filepath.Abs(filepath.Join("..", "examples", "basic"))
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	e := &example{t: t, dir: dir, app: filepath.Join(bin, "basic"), tool: filepath.Join(bin, "bun-fixture-migrate"), dsn: u.String()}
	build := exec.Command("go", "build", "-o", e.app, ".")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the example: %v\n%s", err, out)
	}
	if out, err := exec.Command("go", "build", "-o", e.tool,
		"github.com/RELAXccc/bun-fixture-migrate/cmd/bun-fixture-migrate").CombinedOutput(); err != nil {
		t.Fatalf("build the command: %v\n%s", err, out)
	}
	e.db = openDB(t, e.dsn, nil)
	t.Cleanup(func() {
		e.db.Close()
		// Closed first: a database with connections cannot be dropped, and
		// WITH (FORCE) needs PostgreSQL 13.
		admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS bfm_example")
	})
	return e
}

// exec runs one of the two programs in the example's directory and fails
// unless it exits with want.
func (e *example) exec(want int, bin string, args ...string) string {
	e.t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = e.dir
	cmd.Env = append(os.Environ(), "DATABASE_URL="+e.dsn)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	if code != want {
		e.t.Fatalf("%s %v: exit %d, want %d\n%s", filepath.Base(bin), args, code, want, out.String())
	}
	return out.String()
}

func (e *example) changeSet() fixturechange.Set {
	e.t.Helper()
	src, err := os.ReadFile(filepath.Join(e.dir, "migrations", "20260930165255_fixture_plan_prices.go"))
	if err != nil {
		e.t.Fatal(err)
	}
	set, _, err := fixturemigrate.ReadChangeSet(src)
	if err != nil {
		e.t.Fatal(err)
	}
	return set
}

func TestTheExampleProject(t *testing.T) {
	e := newExample(t)
	ctx := context.Background()

	// A new database: the schema migration creates the tables, the fixture
	// migration finds plans empty and leaves it to the seed, which loads the
	// fixture file as it is now.
	out := e.exec(0, e.app, "migrate")
	for _, want := range []string{"plans is empty, nothing to do", "migrated to group #1", "seeded the database"} {
		if !strings.Contains(out, want) {
			t.Fatalf("migrate does not say %q:\n%s", want, out)
		}
	}
	e.exec(0, e.tool, "check")
	e.exec(0, e.tool, "status", "-require-applied")
	// The seed moved the sequences past the ids the fixture file names.
	run(t, e.db, "INSERT INTO plans (name, currency_id) VALUES ('custom', 1)")
	if got := scan[int64](t, e.db, "SELECT id FROM plans WHERE name = 'custom'"); got != 4 {
		t.Fatalf("the application's first plan got id %d", got)
	}
	run(t, e.db, "DELETE FROM plans WHERE name = 'custom'")
	// A second deploy has nothing to do.
	if out := e.exec(0, e.app, "migrate"); !strings.Contains(out, "no migrations to run") {
		t.Fatal(out)
	}

	// A database deployed before the change: it holds the fixture data the
	// migration starts from, and the migration is pending.
	if err := fixtureapply.Revert(ctx, e.db, e.changeSet(), quiet()); err != nil {
		t.Fatal(err)
	}
	run(t, e.db, "DELETE FROM bun_migrations WHERE name = '20260930165255'")
	if out := e.exec(3, e.tool, "status", "-require-applied"); !strings.Contains(out, "1 migration not applied") {
		t.Fatal(out)
	}
	if out := e.exec(0, e.tool, "plan"); !strings.Contains(out, "fixture_plan_prices: would succeed") {
		t.Fatalf("plan:\n%s", out)
	}

	// Somebody renamed the team plan by hand meanwhile. The migration's update
	// finds no plan called team, which the policy makes an error: the plan
	// says so, the deploy stops, and the migration stays pending, to run
	// once the database is fixed.
	run(t, e.db, "UPDATE plans SET name = 'teams' WHERE name = 'team'")
	if out := e.exec(3, e.tool, "plan"); !strings.Contains(out, "missing row") {
		t.Fatalf("plan against a drifted database:\n%s", out)
	}
	if out := e.exec(1, e.app, "migrate"); !strings.Contains(out, "name=team") {
		t.Fatalf("migrate against a drifted database:\n%s", out)
	}
	e.exec(3, e.tool, "status", "-require-applied")
	run(t, e.db, "UPDATE plans SET name = 'team' WHERE name = 'teams'")

	// A price changed by hand is a changed row, which the policy makes a
	// warning: the plan passes and lists it, and -strict, for a pipeline
	// that wants every change made, fails on it.
	run(t, e.db, "UPDATE plans SET price_cents = 2200 WHERE name = 'team'")
	if out := e.exec(0, e.tool, "plan"); !strings.Contains(out, "skipped   Plan name=team update [changed row]") {
		t.Fatalf("plan against a changed row:\n%s", out)
	}
	e.exec(3, e.tool, "plan", "-strict")
	// The deploy goes through, making every change but that one.
	if out := e.exec(0, e.app, "migrate"); !strings.Contains(out, "migrated to group #2 (20260930165255_fixture_plan_prices)") {
		t.Fatal(out)
	}
	e.exec(0, e.tool, "status", "-require-applied")
	// The skipped row is drift now, which check finds and sync repairs.
	if out := e.exec(3, e.tool, "check"); !strings.Contains(out, "price_cents") {
		t.Fatalf("check after a skipped change:\n%s", out)
	}
	if out := e.exec(0, e.tool, "sync"); !strings.Contains(out, "would apply") {
		t.Fatalf("sync without -yes:\n%s", out)
	}
	e.exec(3, e.tool, "check")
	e.exec(0, e.tool, "sync", "-yes")
	e.exec(0, e.tool, "check")

	// A rollback takes the fixture change back and nothing else.
	if out := e.exec(0, e.app, "rollback"); !strings.Contains(out, "rolled back group #2") {
		t.Fatal(out)
	}
	if got := scan[int64](t, e.db, "SELECT price_cents FROM plans WHERE name = 'team'"); got != 2000 {
		t.Fatalf("team costs %d after the rollback", got)
	}
	if got := scan[int64](t, e.db, "SELECT count(*) FROM plans WHERE name = 'pro'"); got != 0 {
		t.Fatal("the rollback left the plan the migration added")
	}
	e.exec(3, e.tool, "check")
}
