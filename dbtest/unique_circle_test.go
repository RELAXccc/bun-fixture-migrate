package dbtest_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/dbschema"

	"github.com/uptrace/bun"
)

type UcItem struct {
	bun.BaseModel `bun:"table:uc_items"`
	ID            int64  `bun:"id,pk"`
	Name          string `bun:"name,notnull"`
	Grp           string `bun:"grp,notnull"`
	Position      int64  `bun:"position,notnull"`
}

const (
	ucInOrder = "- model: UcItem\n  rows:\n    - {id: 1, name: a, grp: g, position: 1}\n" +
		"    - {id: 2, name: b, grp: g, position: 2}\n    - {id: 3, name: c, grp: g, position: 3}\n"
	ucRotated = "- model: UcItem\n  rows:\n    - {id: 1, name: a, grp: g, position: 2}\n" +
		"    - {id: 2, name: b, grp: g, position: 3}\n    - {id: 3, name: c, grp: g, position: 1}\n"
)

// ucDiff is the change from v1 to v2 as generate makes it against the
// database: both files respelled and the unique indexes read from the
// catalog.
func ucDiff(t *testing.T, l *lab, v1, v2 string) *fixturemigrate.Result {
	t.Helper()
	before, after := fixtureSnapshot(t, l.cfg, v1, "old"), fixtureSnapshot(t, l.cfg, v2, "new")
	readOnlyDo(t, l.db, func(tx bun.Tx, tables map[string]*dbschema.Table) {
		for _, s := range []*fixturemigrate.Snapshot{before, after} {
			if err := fixturemigrate.Canonicalize(context.Background(), tx, l.cfg, s, tables); err != nil {
				t.Fatal(err)
			}
		}
	})
	res, err := fixturemigrate.Compute(l.cfg, before, after)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A list rotated under UNIQUE (grp, position): checked after every
// statement, the index refuses whichever update comes first, so generate and
// sync refuse the rotation with what to do, where they used to write a
// migration that could never run. Under a DEFERRABLE constraint, which the
// change set defers to its end, the rotation is written, and a migration,
// a sync and bun's own migrator store what dbfixture seeds.
func TestARotationUnderAUniqueConstraint(t *testing.T) {
	create := func(deferrable string) []string {
		return []string{"DROP TABLE IF EXISTS uc_items", "CREATE TABLE uc_items (id bigint PRIMARY KEY, " +
			"name text NOT NULL UNIQUE, grp text NOT NULL, position bigint NOT NULL, " +
			"CONSTRAINT uc_items_grp_position UNIQUE (grp, position) " + deferrable + ")"}
	}
	l := newLab(t, map[string]*fixturemigrate.Model{"UcItem": {Table: "uc_items", Key: []string{"name"}}}, "uc_items",
		create(""), `SELECT string_agg(concat_ws('|', name, grp, position), E'\n' ORDER BY name) FROM uc_items`,
		(*UcItem)(nil))
	l.cfg.Package = "migrations"

	l.seed(ucInOrder)
	res := ucDiff(t, l, ucInOrder, ucRotated)
	if len(res.Changes) != 0 || len(res.Refusals) != 3 ||
		!strings.Contains(res.Refusals[0].Reason, "DEFERRABLE INITIALLY IMMEDIATE") {
		t.Fatalf("expected the rotation refused: %+v / %+v", res.Changes, res.Refusals)
	}
	files := []fixturemigrate.FixtureFile{{Path: "fixture.yml", Data: []byte(ucRotated)}}
	if _, err := fixturemigrate.Sync(context.Background(), l.db, l.cfg, files,
		fixturemigrate.SyncOptions{}); !errors.Is(err, fixturemigrate.ErrSyncRefused) {
		t.Fatalf("sync has to refuse the rotation, got %v", err)
	}
	if got := l.current(); got != "a|g|1\nb|g|2\nc|g|3" {
		t.Fatalf("the refused sync changed the table: %s", got)
	}

	l.create = create("DEFERRABLE INITIALLY IMMEDIATE")
	l.reset()
	readOnlyDo(t, l.db, func(tx bun.Tx, tables map[string]*dbschema.Table) {
		if d := tables["public.uc_items"].Deferrable; len(d) != 1 || strings.Join(d[0], ",") != "grp,position" {
			t.Fatalf("the catalog has to say which index is deferrable: %v", d)
		}
	})
	l.fidelity(ucInOrder, ucRotated)

	// The same under bun's migrator, as a deploy runs it.
	l.seed(ucInOrder)
	res = ucDiff(t, l, ucInOrder, ucRotated)
	if len(res.Refusals) != 0 || len(res.Warnings) != 0 || len(res.Changes) != 3 {
		t.Fatalf("got %+v / %+v / %+v", res.Changes, res.Refusals, res.Warnings)
	}
	const stamp = "20261001120000"
	src, err := fixturemigrate.Render(l.cfg, "reorder", stamp, res)
	if err != nil {
		t.Fatal(err)
	}
	bin := buildMigrator(t, stamp, "reorder", src)
	run(t, l.db, "DROP TABLE IF EXISTS bun_migrations, bun_migration_locks")
	t.Cleanup(func() { run(t, l.db, "DROP TABLE IF EXISTS bun_migrations, bun_migration_locks") })
	if ok, out := runMigrator(t, bin, false); !ok {
		t.Fatalf("the migrator failed:\n%s", out)
	}
	if got := l.current(); got != "a|g|2\nb|g|3\nc|g|1" {
		t.Fatalf("migrated %s", got)
	}
}

// The command, from generate to plan: a refusal that says what to do while
// the constraint is checked after each statement, and once it is
// DEFERRABLE, a migration plan runs, and bun's migrator after it.
func TestTheCommandRefusesARotationAndPlansItDeferred(t *testing.T) {
	a := newAdoption(t)
	run(t, a.db, "CREATE TABLE items (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, grp text NOT NULL, "+
		"position int NOT NULL, CONSTRAINT items_grp_position UNIQUE (grp, position))",
		"INSERT INTO items VALUES (1, 'a', 'g', 1), (2, 'b', 'g', 2), (3, 'c', 'g', 3)")
	if err := os.MkdirAll(filepath.Join(a.dir, "fixtures"), 0o755); err != nil {
		t.Fatal(err)
	}
	a.write("fixture-migrate.yml", "fixture: fixtures/fixture.yml\nout: migrations\npackage: migrations\n"+
		"seed_guard_table: items\ndatabase: env:DATABASE_URL\nmodels:\n  Item:\n    table: items\n    key: [name]\n")
	inOrder := strings.ReplaceAll(ucInOrder, "UcItem", "Item")
	a.write("fixtures/fixture.yml", inOrder)
	a.run(0, "check")
	a.run(0, "baseline")
	a.write("fixtures/fixture.yml", strings.ReplaceAll(ucRotated, "UcItem", "Item"))

	out := a.run(2, "generate", "-name", "reorder")
	if !strings.Contains(out, "refused: Item/name=c: moves (grp, position) from (g, 3) to (g, 1) while Item/name=a "+
		"and Item/name=b trade values with it in a circle") || !strings.Contains(out, "3 changes refused") {
		t.Fatal(out)
	}
	if out := a.run(3, "check"); !strings.Contains(out, "DEFERRABLE INITIALLY IMMEDIATE") {
		t.Fatal(out)
	}
	// Without the database nothing says the column is unique: a warning.
	if out := a.run(0, "generate", "-name", "reorder", "-no-lint", "-dry-run"); !strings.Contains(out,
		"trade their values of position in a circle") {
		t.Fatal(out)
	}

	run(t, a.db, "ALTER TABLE items DROP CONSTRAINT items_grp_position, "+
		"ADD CONSTRAINT items_grp_position UNIQUE (grp, position) DEFERRABLE INITIALLY IMMEDIATE")
	out = a.run(0, "generate", "-name", "reorder")
	if !strings.Contains(out, "Item: 3 updates") || strings.Contains(out, "circle") {
		t.Fatal(out)
	}
	if out := a.run(0, "plan"); !strings.Contains(out, "would succeed") {
		t.Fatal(out)
	}
	files, _ := filepath.Glob(filepath.Join(a.dir, "migrations", "*_fixture_reorder.go"))
	if len(files) != 1 {
		t.Fatalf("no migration: %v", files)
	}
	src, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	stamp, _, _ := strings.Cut(filepath.Base(files[0]), "_")
	bin := buildMigrator(t, stamp, "reorder", src)
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "DSN="+a.dsn)
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Run(); err != nil {
		t.Fatalf("the migrator failed: %v\n%s", err, log.String())
	}
	if got := scan[string](t, a.db, "SELECT string_agg(name || position, ',' ORDER BY name) FROM items"); got != "a2,b3,c1" {
		t.Fatalf("migrated %s", got)
	}
	a.run(0, "check")
}
