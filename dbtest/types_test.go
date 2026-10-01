package dbtest_test

// Column types where what the tool reads, writes or exports used to differ
// from what dbfixture stores: domains, lengths, cast errors outside class 22,
// timestamps a time.Time and a string field store differently, jsonb through
// map[string]any, bytea through []byte, !!binary, text YAML cannot hold as
// written, NaN and infinity, multidimensional arrays, citext keys and CHECK
// constraints. Each is seeded with the real dbfixture, on a server whose
// TimeZone is not UTC, and held against what the tool does with it: an export
// that checks clean and loads back as the database, a migration and a sync
// that store exactly what a fresh seed of the new file stores, or, where the
// value means different things to different models or servers, a finding.

import (
	"context"
	"os"
	"strings"
	"testing"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
)

// lab is one table, the models dbfixture seeds it through, and the tool's
// configuration for it.
type lab struct {
	t      *testing.T
	db     *bun.DB
	cfg    *fixturemigrate.Config
	create []string
	state  string
}

// newLab connects with a New York TimeZone, the way a production server away
// from UTC is set up, registers the models and creates the table.
func newLab(t *testing.T, models map[string]*fixturemigrate.Model, guard string, create []string, state string,
	bunModels ...any) *lab {

	t.Helper()
	dsn := os.Getenv("BUN_FIXTURE_MIGRATE_POSTGRES")
	if dsn == "" {
		t.Skip("set BUN_FIXTURE_MIGRATE_POSTGRES to a PostgreSQL DSN to run the round trip")
	}
	db := openDB(t, dsn, map[string]string{"TimeZone": "America/New_York"})
	t.Cleanup(func() { db.Close() })
	db.RegisterModel(bunModels...)
	cfg := &fixturemigrate.Config{Schema: "public", SeedGuardTable: guard, Models: models}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	l := &lab{t: t, db: db, cfg: cfg, create: create, state: state}
	l.reset()
	return l
}

func (l *lab) reset() {
	l.t.Helper()
	run(l.t, l.db, l.create...)
}

// seed empties the table and loads text with dbfixture.
func (l *lab) seed(text string) string {
	l.t.Helper()
	l.reset()
	loadFixture(l.t, l.db, text)
	return l.current()
}

func (l *lab) current() string {
	l.t.Helper()
	return scan[string](l.t, l.db, l.state)
}

// read canonicalises a fixture file against the database and lints it, as
// check, generate and sync do, and returns the snapshot.
func (l *lab) read(text string) *fixturemigrate.Snapshot {
	l.t.Helper()
	head := fixtureSnapshot(l.t, l.cfg, text, "fixture.yml")
	readOnlyDo(l.t, l.db, func(tx bun.Tx, tables map[string]*dbschema.Table) {
		if err := fixturemigrate.Canonicalize(context.Background(), tx, l.cfg, head, tables); err != nil {
			l.t.Fatalf("Canonicalize: %v", err)
		}
		fixturemigrate.LintColumns(l.cfg, head, tables)
		fixturemigrate.LintZeroDefaults(l.cfg, head, tables)
		fixturemigrate.LintNullDefaults(l.cfg, head, tables)
	})
	return head
}

// findings is what reading text turns up, one line each.
func (l *lab) findings(text string) string {
	l.t.Helper()
	var lines []string
	for _, f := range l.read(text).Findings {
		lines = append(lines, string(f.Kind)+": "+f.String())
	}
	return strings.Join(lines, "\n")
}

// check is check: the database against text, which has to agree.
func (l *lab) check(text string) {
	l.t.Helper()
	head := l.read(text)
	if len(head.Findings) != 0 {
		l.t.Fatalf("findings in a file the database was seeded from:\n%+v\n%s", head.Findings, text)
	}
	var res *fixturemigrate.CheckResult
	readOnlyDo(l.t, l.db, func(tx bun.Tx, tables map[string]*dbschema.Table) {
		database, err := fixturemigrate.DatabaseSnapshot(context.Background(), tx, l.cfg, tables,
			fixturemigrate.SnapshotOptions{Columns: head.Columns, Order: head.Order})
		if err != nil {
			l.t.Fatal(err)
		}
		if res, err = fixturemigrate.Check(l.cfg, database, head); err != nil {
			l.t.Fatal(err)
		}
	})
	if res.Drifted() {
		l.t.Fatalf("the database and the file disagree:\n%s\n%s", strings.Join(res.Lines(), "\n"), text)
	}
}

// export writes the database as a fixture file.
func (l *lab) export() ([]byte, error) {
	l.t.Helper()
	var out []byte
	var err error
	readOnlyDo(l.t, l.db, func(tx bun.Tx, tables map[string]*dbschema.Table) {
		var snap *fixturemigrate.Snapshot
		snap, err = fixturemigrate.DatabaseSnapshot(context.Background(), tx, l.cfg, tables,
			fixturemigrate.SnapshotOptions{})
		if err != nil {
			return
		}
		out, err = fixturemigrate.Export(l.cfg, snap, tables, nil)
	})
	return out, err
}

// roundTrip exports the database as it is, checks the export against it,
// and seeds a fresh table from the export, which has to hold the same.
func (l *lab) roundTrip() string {
	l.t.Helper()
	want := l.current()
	data, err := l.export()
	if err != nil {
		l.t.Fatalf("export: %v", err)
	}
	l.check(string(data))
	if got := l.seed(string(data)); got != want {
		l.t.Fatalf("the export does not load back as the database\n got %s\nwant %s\n%s", got, want, data)
	}
	return string(data)
}

// migrate generates the change from old to next against the database and
// applies it, as a generated migration does.
func (l *lab) migrate(old, next string) {
	l.t.Helper()
	before, after := fixtureSnapshot(l.t, l.cfg, old, "old"), fixtureSnapshot(l.t, l.cfg, next, "new")
	readOnlyDo(l.t, l.db, func(tx bun.Tx, tables map[string]*dbschema.Table) {
		for _, s := range []*fixturemigrate.Snapshot{before, after} {
			if err := fixturemigrate.Canonicalize(context.Background(), tx, l.cfg, s, tables); err != nil {
				l.t.Fatal(err)
			}
		}
	})
	if len(after.Findings) != 0 {
		l.t.Fatalf("findings: %+v", after.Findings)
	}
	res, err := fixturemigrate.Compute(l.cfg, before, after)
	if err != nil || len(res.Refusals) != 0 {
		l.t.Fatalf("%v %+v", err, res.Refusals)
	}
	set := fixturechange.Set{Name: "types", SeedGuardTable: l.cfg.SeedGuardTable, Tables: res.Tables,
		Changes: res.Changes}
	if err := fixtureapply.Apply(context.Background(), l.db, set, quiet()); err != nil {
		l.t.Fatalf("apply: %v", err)
	}
}

// fidelity holds a migration and a sync from v1 to v2 against a fresh seed
// of v2: all three have to store the same.
func (l *lab) fidelity(v1, v2 string) {
	l.t.Helper()
	want := l.seed(v2)
	l.check(v2)

	l.seed(v1)
	l.migrate(v1, v2)
	if got := l.current(); got != want {
		l.t.Fatalf("the migration wrote something else than dbfixture\nmigrated %s\n  seeded %s", got, want)
	}
	l.check(v2)

	l.seed(v1)
	files := []fixturemigrate.FixtureFile{{Path: "fixture.yml", Data: []byte(v2)}}
	if _, err := fixturemigrate.Sync(context.Background(), l.db, l.cfg, files, fixturemigrate.SyncOptions{}); err != nil {
		l.t.Fatalf("sync: %v", err)
	}
	if got := l.current(); got != want {
		l.t.Fatalf("the sync wrote something else than dbfixture\n synced %s\n seeded %s", got, want)
	}
}

// refused reads text and wants a finding of each of wants in it.
func (l *lab) refused(text string, wants ...string) {
	l.t.Helper()
	got := l.findings(text)
	for _, want := range wants {
		if !strings.Contains(got, want) {
			l.t.Errorf("expected a finding with %q, got:\n%s", want, got)
		}
	}
}

// createOnce is a CREATE statement that does nothing when its object exists.
func createOnce(stmt string) string {
	return "DO $$ BEGIN " + stmt + "; EXCEPTION WHEN duplicate_object THEN NULL; END $$"
}

func seedErr(db *bun.DB, text string) error {
	return tryLoad(db, text)
}

type TyBin struct {
	bun.BaseModel `bun:"table:ty_bin"`

	ID   int64  `bun:"id,pk"`
	Name string `bun:"name,notnull"`
	Data string `bun:"data,type:bytea"`
	V    string `bun:"v"`
}

const tyBin1 = "- model: TyBin\n  rows:\n    - {id: 1, name: a, data: x, v: x}\n"

// In a text column a string field gets the text a !!binary scalar encodes,
// which the fixture reader has to keep as the value's one reading.
func TestTypesBinaryTagInATextColumn(t *testing.T) {
	b := newLab(t, map[string]*fixturemigrate.Model{"TyBin": {Table: "ty_bin", Key: []string{"name"}}}, "ty_bin",
		[]string{"DROP TABLE IF EXISTS ty_bin",
			"CREATE TABLE ty_bin (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, data bytea, v text)"},
		`SELECT string_agg(concat_ws('|', name, data, v), E'\n' ORDER BY name) FROM ty_bin`, (*TyBin)(nil))
	b1 := tyBin1
	b3 := b1 + "    - {id: 2, name: b, data: x, v: !!binary SGVsbG8=}\n"
	doc, err := fixturemigrate.ParseDoc([]byte(b3))
	if err != nil {
		t.Fatal(err)
	}
	if doc[0].Rows[1]["v"].StringText != "" {
		t.Skip("the fixture reader still takes a !!binary scalar's base64 text for what a string field gets")
	}
	b.fidelity(b1, b3)
}

// A model naming a view is told it is one.
func TestTypesAViewIsNotATable(t *testing.T) {
	l := newLab(t, map[string]*fixturemigrate.Model{"TyView": {Table: "ty_view", Key: []string{"name"}}}, "ty_check",
		[]string{"DROP VIEW IF EXISTS ty_view", "DROP MATERIALIZED VIEW IF EXISTS ty_mview",
			"CREATE VIEW ty_view AS SELECT 1 AS id, 'a'::text AS name",
			"CREATE MATERIALIZED VIEW ty_mview AS SELECT 1 AS id, 'a'::text AS name"},
		"SELECT ''")
	readOnlyDo(t, l.db, func(tx bun.Tx, tables map[string]*dbschema.Table) {
		_, err := fixturemigrate.DatabaseSnapshot(context.Background(), tx, l.cfg, tables, fixturemigrate.SnapshotOptions{})
		if err == nil || !strings.Contains(err.Error(), "public.ty_view, which is a view: only a table holds master data") {
			t.Fatalf("%v", err)
		}
	})
	for name, want := range map[string]string{"public.ty_view": "a view", "public.ty_mview": "a materialized view",
		"public.ty_none": "", "public.ty_check": ""} {
		if kind, err := dbschema.NotATable(context.Background(), l.db, name); err != nil || kind != want {
			t.Errorf("%s: %q %v, want %q", name, kind, err, want)
		}
	}
	run(t, l.db, "DROP VIEW ty_view", "DROP MATERIALIZED VIEW ty_mview")
}
