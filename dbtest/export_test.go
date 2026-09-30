package dbtest_test

// The export half of the tool, against a real PostgreSQL and the real
// dbfixture. Two things are being shown here.
//
// One: the defect the whole zero-default check exists for. bun's INSERT sends
// DEFAULT, not the value, for a zero in a field that carries a default tag
// (query_insert.go, marshalsToDefault), so a fixture row saying
// "production_max: 0" loads as 1 and the file and the database disagree from
// the first seed onwards. TestBunWritesTheDefaultForAZero is that, written
// down.
//
// Two: that an export reproduces the database it was taken from. Seed from a
// fixture file, export, and the export has to be the file again.

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dbfixture"
)

type Region struct {
	bun.BaseModel `bun:"table:regions"`

	ID   int64  `bun:"id,pk"`
	Code string `bun:"code,notnull,unique"`
	Name string `bun:"name,notnull"`
}

type Item struct {
	bun.BaseModel `bun:"table:items"`

	ID       int64  `bun:"id,pk,autoincrement"`
	RegionID int64  `bun:"region_id,notnull"`
	Name     string `bun:"name,notnull,unique"`
	Cost     int64  `bun:"cost,notnull"`
	// The hazard. A default other than the zero value means bun will never
	// write a zero into this column.
	ProductionMax int64   `bun:"production_max,notnull,default:1"`
	Ratio         float64 `bun:"ratio,notnull"`
	Active        bool    `bun:"active,notnull"`
	Note          string  `bun:"note,nullzero"`
}

const itemFixture = `- model: Region
  rows:
    - _id: eu
      id: 1
      code: "EU"
      name: "Europe"
    - _id: us
      id: 2
      code: "US"
      name: "North America"

- model: Item
  rows:
    - _id: anvil
      id: 1
      region_id: '{{ $.Region.eu.ID }}'
      name: "anvil"
      cost: 120
      production_max: 5
      ratio: 1.5
      active: true
      note: "heavy"
    - _id: rope
      id: 2
      region_id: '{{ $.Region.us.ID }}'
      name: "rope"
      cost: 0
      production_max: 3
      ratio: 0.5
      active: false
      note: ~
`

func itemConfig(t *testing.T) *fixturemigrate.Config {
	t.Helper()
	cfg := &fixturemigrate.Config{
		Schema:         "public",
		SeedGuardTable: "items",
		Models: map[string]*fixturemigrate.Model{
			"Region": {Table: "regions", Ref: "code", Key: []string{"code"}},
			"Item": {
				Table: "items", Serial: true, Key: []string{"name"},
				References: map[string]string{"region_id": "Region"},
			},
		},
	}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func itemDB(t *testing.T) *bun.DB {
	t.Helper()
	db := connect(t)
	db.RegisterModel((*Region)(nil), (*Item)(nil))
	ctx := context.Background()
	for _, model := range []any{(*Item)(nil), (*Region)(nil)} {
		if _, err := db.NewDropTable().Model(model).IfExists().Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, model := range []any{(*Region)(nil), (*Item)(nil)} {
		if _, err := db.NewCreateTable().Model(model).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// bun's CreateTable does not emit the foreign key on its own, and the
	// export needs one to exist for the introspection test to mean anything.
	if _, err := db.ExecContext(ctx,
		"ALTER TABLE items ADD CONSTRAINT items_region_id_fkey FOREIGN KEY (region_id) REFERENCES regions (id)"); err != nil {
		t.Fatal(err)
	}
	return db
}

func loadFixture(t *testing.T, db *bun.DB, text string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fixture.yml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := dbfixture.New(db).Load(context.Background(), os.DirFS(dir), "fixture.yml"); err != nil {
		t.Fatalf("dbfixture: %v", err)
	}
	// dbfixture writes explicit ids straight past the sequence, so the next
	// ordinary insert would reuse one. Every seeder has to do this.
	var tables []string
	if err := db.NewRaw("SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace "+
		"WHERE n.nspname = 'public' AND c.relkind = 'r'").Scan(context.Background(), &tables); err != nil {
		t.Fatal(err)
	}
	var plain []string
	for _, table := range tables {
		if identifier.MatchString(table) {
			plain = append(plain, table)
		}
	}
	if _, err := fixtureapply.SyncSequences(context.Background(), db, plain...); err != nil {
		t.Fatal(err)
	}
}

// identifier is a table name SyncSequences takes; other tests leave tables
// with names built to break quoting behind.
var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*$`)

func schemaOf(t *testing.T, db *bun.DB) map[string]*dbschema.Table {
	t.Helper()
	tables, err := dbschema.Load(context.Background(), db, "public")
	if err != nil {
		t.Fatalf("dbschema.Load: %v", err)
	}
	return tables
}

func databaseSnapshot(t *testing.T, db *bun.DB, cfg *fixturemigrate.Config,
	opts fixturemigrate.SnapshotOptions) *fixturemigrate.Snapshot {
	t.Helper()
	snap, err := fixturemigrate.DatabaseSnapshot(context.Background(), db, cfg, schemaOf(t, db), opts)
	if err != nil {
		t.Fatalf("DatabaseSnapshot: %v", err)
	}
	return snap
}

// Everything the export needs is in the catalog: the tables, the columns and
// their types, the defaults, the primary keys, the unique indexes, the foreign
// keys and the sequences. No Go model type is involved.
func TestIntrospectionSeesWhatTheExportNeeds(t *testing.T) {
	db := itemDB(t)
	tables := schemaOf(t, db)
	items := tables["public.items"]
	if items == nil {
		t.Fatalf("items is missing from %v", dbschema.Names(tables))
	}
	if len(items.PrimaryKey) != 1 || items.PrimaryKey[0] != "id" {
		t.Fatalf("primary key: %v", items.PrimaryKey)
	}
	if id, _ := items.Column("id"); !id.Serial() {
		t.Fatalf("items.id should come from a sequence: %+v", id)
	}
	fk := items.ForeignKeyOf("region_id")
	if fk == nil || fk.RefTable != "regions" || len(fk.RefColumns) != 1 || fk.RefColumns[0] != "id" {
		t.Fatalf("foreign key: %+v", fk)
	}
	var unique bool
	for _, cols := range items.Uniques {
		if len(cols) == 1 && cols[0] == "name" {
			unique = true
		}
	}
	if !unique {
		t.Fatalf("the unique index on name is what the natural key is guessed from: %v", items.Uniques)
	}
	// The whole zero-default check rests on this one column default being
	// visible without any Go type.
	col, _ := items.Column("production_max")
	hazard, stored := col.ZeroIsNotDefault()
	if !hazard || stored != "1" {
		t.Fatalf("production_max: %v, %q (default %q)", hazard, stored, col.Default)
	}
	if cost, _ := items.Column("cost"); func() bool { h, _ := cost.ZeroIsNotDefault(); return h }() {
		t.Fatal("a column with no default holds whatever it is given")
	}
}

// The defect. A fixture row writes 0, bun writes DEFAULT, the database holds 1,
// and nothing in the ordinary path says a word about it.
func TestBunWritesTheDefaultForAZero(t *testing.T) {
	db := itemDB(t)
	cfg := itemConfig(t)
	text := strings.Replace(itemFixture, "      production_max: 3\n", "      production_max: 0\n", 1)
	loadFixture(t, db, text)

	var stored int64
	if err := db.QueryRowContext(context.Background(),
		"SELECT production_max FROM items WHERE name = 'rope'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("this test documents bun's behaviour; if it stores %d now, the tool's premise has changed", stored)
	}

	// The static check catches it without looking at the database's rows at
	// all: the column has a non-zero default, the file writes the zero.
	snap := fixtureSnapshot(t, cfg, text, "fixture.yml")
	fixturemigrate.LintZeroDefaults(cfg, snap, schemaOf(t, db))
	if len(snap.Findings) != 1 || snap.Findings[0].Kind != fixturemigrate.FindingZeroDefault {
		t.Fatalf("expected the zero to be reported, got %+v", snap.Findings)
	}
	if !strings.Contains(snap.Findings[0].Detail, "production_max is 0") {
		t.Fatalf("unhelpful finding: %q", snap.Findings[0].Detail)
	}

	// And the drift check sees the consequence: the file says 0, the database
	// holds 1.
	db2 := databaseSnapshot(t, db, cfg, fixturemigrate.SnapshotOptions{Columns: snap.Columns, Order: snap.Order})
	res, err := fixturemigrate.Check(cfg, db2, snap)
	if err != nil {
		t.Fatal(err)
	}
	report := strings.Join(res.Lines(), "\n")
	if !strings.Contains(report, "production_max: database 1, file 0") {
		t.Fatalf("the check has to show the difference:\n%s", report)
	}
}

// Seed from a file, export, and the export has to be that file again. This is
// the test that says whether the export can be trusted at all.
func TestExportReproducesTheDatabaseItCameFrom(t *testing.T) {
	db := itemDB(t)
	cfg := itemConfig(t)
	loadFixture(t, db, itemFixture)

	tables := schemaOf(t, db)
	snap := databaseSnapshot(t, db, cfg, fixturemigrate.SnapshotOptions{})
	fixturemigrate.LintZeroDefaults(cfg, snap, tables)
	if len(snap.Findings) != 0 {
		t.Fatalf("nothing is wrong with this database: %+v", snap.Findings)
	}
	data, err := fixturemigrate.Export(cfg, snap, tables, []string{"a test"})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	// The export against the file it was seeded from: no difference.
	want := fixtureSnapshot(t, cfg, itemFixture, "the original file")
	got := fixtureSnapshot(t, cfg, string(data), "the export")
	res, err := fixturemigrate.Compute(cfg, want, got)
	if err != nil {
		t.Fatalf("Compute: %v\n%s", err, data)
	}
	if len(res.Changes) != 0 || len(res.Refusals) != 0 {
		t.Fatalf("the export is not the file it came from: %+v / %+v\n%s", res.Changes, res.Refusals, data)
	}

	// And the export loads: a second database seeded from it holds the same
	// rows as the first.
	before := itemState(t, db)
	db2 := itemDB(t)
	loadFixture(t, db2, string(data))
	if after := itemState(t, db2); after != before {
		t.Fatalf("a database seeded from the export differs\n--- export ---\n%s\n--- got ---\n%s\n--- want ---\n%s",
			data, after, before)
	}
}

// An export of a database that really does hold a zero in a column whose
// default is not zero cannot be loaded back as written. The export says so, on
// the line it happened, and the command refuses to write it unless the policy
// says otherwise.
func TestExportMarksAZeroItCannotWriteBack(t *testing.T) {
	db := itemDB(t)
	cfg := itemConfig(t)
	loadFixture(t, db, itemFixture)
	if _, err := db.ExecContext(context.Background(),
		"UPDATE items SET production_max = 0 WHERE name = 'rope'"); err != nil {
		t.Fatal(err)
	}
	tables := schemaOf(t, db)
	snap := databaseSnapshot(t, db, cfg, fixturemigrate.SnapshotOptions{})
	fixturemigrate.LintZeroDefaults(cfg, snap, tables)
	mode, findings := cfg.Worst(snap.Findings)
	if len(findings) != 1 || mode != fixturemigrate.ModeError {
		t.Fatalf("expected one finding the default policy refuses, got %v / %+v", mode, findings)
	}
	data, err := fixturemigrate.Export(cfg, snap, tables, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "production_max: 0  # ROUND-TRIP HAZARD") {
		t.Fatalf("the hazard belongs on the line:\n%s", data)
	}

	// Which is not a matter of taste: loading that export really does produce
	// something else.
	db2 := itemDB(t)
	loadFixture(t, db2, string(data))
	var stored int64
	if err := db2.QueryRowContext(context.Background(),
		"SELECT production_max FROM items WHERE name = 'rope'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("expected the default to win, got %d", stored)
	}
}

// The database and the file agree, so the check says so and finds nothing.
func TestCheckIsQuietWhenTheyAgree(t *testing.T) {
	db := itemDB(t)
	cfg := itemConfig(t)
	loadFixture(t, db, itemFixture)
	file := fixtureSnapshot(t, cfg, itemFixture, "fixture.yml")
	fixturemigrate.LintZeroDefaults(cfg, file, schemaOf(t, db))
	database := databaseSnapshot(t, db, cfg,
		fixturemigrate.SnapshotOptions{Columns: file.Columns, Order: file.Order})
	res, err := fixturemigrate.Check(cfg, database, file)
	if err != nil {
		t.Fatal(err)
	}
	if res.Drifted() {
		t.Fatalf("expected no drift:\n%s", strings.Join(res.Lines(), "\n"))
	}
}

// What an admin UI leaves behind: somebody edited a row in production and
// added another. The check names both, and -from-db turns them into the
// migration that puts the file's version back.
func TestCheckAndFromDatabase(t *testing.T) {
	db := itemDB(t)
	cfg := itemConfig(t)
	loadFixture(t, db, itemFixture)
	ctx := context.Background()
	for _, stmt := range []string{
		`UPDATE items SET cost = 999 WHERE name = 'anvil'`,
		`INSERT INTO items (region_id, name, cost, production_max, ratio, active) VALUES (1, 'hammer', 7, 1, 1, true)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	file := fixtureSnapshot(t, cfg, itemFixture, "fixture.yml")
	database := databaseSnapshot(t, db, cfg,
		fixturemigrate.SnapshotOptions{Columns: file.Columns, Order: file.Order})
	res, err := fixturemigrate.Check(cfg, database, file)
	if err != nil {
		t.Fatal(err)
	}
	report := strings.Join(res.Lines(), "\n")
	if !strings.Contains(report, "cost: database 999, file 120") {
		t.Fatalf("the edited row belongs in the report:\n%s", report)
	}
	if !strings.Contains(report, "In the database, not in the fixture file:") ||
		!strings.Contains(report, "name=hammer") {
		t.Fatalf("the added row belongs in the report:\n%s", report)
	}
}

// A table that holds master data and other rows too. The where clause is the
// one piece of the configuration that reaches a query as SQL rather than as an
// identifier, and it decides what the tool considers master data at all: rows
// outside it are neither exported nor reported as drift.
func TestWhereLimitsWhatIsMasterData(t *testing.T) {
	db := itemDB(t)
	loadFixture(t, db, itemFixture)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO items (region_id, name, cost, production_max, ratio, active, note)
		 VALUES (1, 'player-made sword', 3, 1, 1, true, 'not master data')`); err != nil {
		t.Fatal(err)
	}

	cfg := itemConfig(t)
	cfg.Models["Item"].Where = "name NOT LIKE 'player-%'"
	snap := databaseSnapshot(t, db, cfg, fixturemigrate.SnapshotOptions{})
	var names []string
	for _, e := range snap.Entries["Item"] {
		names = append(names, e.Cells["name"].Lit)
	}
	if strings.Join(names, ",") != "anvil,rope" {
		t.Fatalf("the where clause decides what is read: %v", names)
	}

	// And the row outside it is not drift: without the clause it would be
	// reported as a row the database has and the file does not.
	file := fixtureSnapshot(t, cfg, itemFixture, "fixture.yml")
	limited := databaseSnapshot(t, db, cfg,
		fixturemigrate.SnapshotOptions{Columns: file.Columns, Order: file.Order})
	res, err := fixturemigrate.Check(cfg, limited, file)
	if err != nil {
		t.Fatal(err)
	}
	if res.Drifted() {
		t.Fatalf("expected no drift:\n%s", strings.Join(res.Lines(), "\n"))
	}

	unlimited := itemConfig(t)
	res, err = fixturemigrate.Check(unlimited,
		databaseSnapshot(t, db, unlimited, fixturemigrate.SnapshotOptions{Columns: file.Columns, Order: file.Order}),
		fixtureSnapshot(t, unlimited, itemFixture, "fixture.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(res.Lines(), "\n"), "player-made sword") {
		t.Fatal("without the clause the same row is drift, which is the point of having it")
	}
}

// Every reference a generated migration carries is resolved with
// "WHERE <ref> = ?", so two rows sharing one ref value make that lookup
// ambiguous. A table without a unique index on it is where this happens, and
// nothing else in a project says so.
func TestARefValueTwoRowsShareIsReported(t *testing.T) {
	db := itemDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "ALTER TABLE regions DROP CONSTRAINT regions_code_key"); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO regions (id, code, name) VALUES (1, 'EU', 'Europe'), (2, 'US', 'North America'),
		 (3, 'EU', 'Europe, again')`,
		`INSERT INTO items (region_id, name, cost, production_max, ratio, active)
		 VALUES (1, 'anvil', 120, 5, 1.5, true)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	snap := databaseSnapshot(t, db, itemConfig(t), fixturemigrate.SnapshotOptions{})
	var found *fixturemigrate.Finding
	for i, f := range snap.Findings {
		if f.Kind == fixturemigrate.FindingDuplicateKey && strings.Contains(f.Row, "EU") {
			found = &snap.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("expected the shared code to be reported, got %+v", snap.Findings)
	}
	if !strings.Contains(found.Detail, "1, 3") || !strings.Contains(found.Detail, "unique index") {
		t.Fatalf("the finding names the colliding rows and the cure: %s", found.Detail)
	}
}

// itemState renders the two tables by their natural keys, so two databases
// seeded in different ways can be compared.
func itemState(t *testing.T, db *bun.DB) string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `
SELECT r.code, i.name, i.cost, i.production_max, i.ratio, i.active, coalesce(i.note, '<null>')
FROM items i JOIN regions r ON r.id = i.region_id ORDER BY i.name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var region, name, note string
		var cost, max int64
		var ratio float64
		var active bool
		if err := rows.Scan(&region, &name, &cost, &max, &ratio, &active, &note); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, strings.Join([]string{region, name, note}, " ")+
			" "+itoa(cost)+" "+itoa(max)+" "+ftoa(ratio)+" "+btoa(active))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func ftoa(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

func btoa(v bool) string { return strconv.FormatBool(v) }
