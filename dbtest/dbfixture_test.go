package dbtest_test

// The fixture reader claims to resolve references the way dbfixture does. Each
// claim is checked here against the real loader: what it binds a template to,
// what it refuses to load, and what the tool makes of the same file.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/dbschema"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dbfixture"
)

// tryLoad is loadFixture without the test failure, for the files dbfixture is
// expected to refuse.
func tryLoad(db *bun.DB, text string) error {
	dir, err := os.MkdirTemp("", "dbfixture")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "fixture.yml"), []byte(text), 0o644); err != nil {
		return err
	}
	return dbfixture.New(db).Load(context.Background(), os.DirFS(dir), "fixture.yml")
}

func itemRegion(t *testing.T, db *bun.DB, item string) string {
	t.Helper()
	return scan[string](t, db,
		"SELECT r.code FROM items i JOIN regions r ON r.id = i.region_id WHERE i.name = ?", item)
}

func toolRegion(t *testing.T, cfg *fixturemigrate.Config, text, item string) string {
	t.Helper()
	snap := fixtureSnapshot(t, cfg, text, "fixture.yml")
	for _, e := range snap.Entries["Item"] {
		if e.Cells["name"].Lit == item {
			if ref := e.Cells["region_id"].Ref; ref != nil {
				return ref.Key
			}
		}
	}
	t.Fatalf("the tool resolved no region for %s", item)
	return ""
}

func TestDbfixtureNamesARowWithoutAnAnchorByItsPrimaryKey(t *testing.T) {
	db := itemDB(t)
	cfg := itemConfig(t)
	text := `- model: Region
  rows:
    - id: 1
      code: "EU"
      name: "Europe"
    - id: 2
      code: "US"
      name: "North America"
- model: Item
  rows:
    - _id: anvil
      id: 1
      region_id: '{{ $.Region.pk2.ID }}'
      name: "anvil"
      cost: 1
      production_max: 1
      ratio: 1
      active: true
`
	loadFixture(t, db, text)
	if got := itemRegion(t, db, "anvil"); got != "US" {
		t.Fatalf("dbfixture bound pk2 to %s", got)
	}
	if got := toolRegion(t, cfg, text, "anvil"); got != "US" {
		t.Fatalf("the tool bound pk2 to %s, dbfixture to US", got)
	}
}

func TestDbfixtureBindsTheLatestRowOfAnAnchor(t *testing.T) {
	db := itemDB(t)
	cfg := itemConfig(t)
	item := func(name, id string) string {
		return `    - _id: ` + name + `
      id: ` + id + `
      region_id: '{{ $.Region.main.ID }}'
      name: "` + name + `"
      cost: 1
      production_max: 1
      ratio: 1
      active: true
`
	}
	text := `- model: Region
  rows:
    - _id: main
      id: 1
      code: "EU"
      name: "Europe"
- model: Item
  rows:
` + item("early", "1") + `- model: Region
  rows:
    - _id: main
      id: 2
      code: "US"
      name: "North America"
- model: Item
  rows:
` + item("late", "2")
	loadFixture(t, db, text)
	for name, want := range map[string]string{"early": "EU", "late": "US"} {
		if got := itemRegion(t, db, name); got != want {
			t.Fatalf("dbfixture bound %s to %s, not %s", name, got, want)
		}
		if got := toolRegion(t, cfg, text, name); got != want {
			t.Fatalf("the tool bound %s to %s, dbfixture to %s", name, got, want)
		}
	}
}

// Both refuse the file, for the same reason: one row names another that is
// only defined further down, and one uses template delimiters dbfixture does
// not evaluate.
func TestDbfixtureAndTheToolRefuseTheSameFiles(t *testing.T) {
	cfg := itemConfig(t)
	forward := `- model: Item
  rows:
    - _id: anvil
      id: 1
      region_id: '{{ $.Region.eu.ID }}'
      name: "anvil"
      cost: 1
      production_max: 1
      ratio: 1
      active: true
- model: Region
  rows:
    - _id: eu
      id: 1
      code: "EU"
      name: "Europe"
`
	unspaced := strings.Replace(itemFixture, `'{{ $.Region.eu.ID }}'`, `'{{$.Region.eu.ID}}'`, 1)
	for name, text := range map[string]string{"forward reference": forward, "unspaced template": unspaced} {
		db := itemDB(t)
		if err := tryLoad(db, text); err == nil {
			t.Fatalf("%s: dbfixture loaded it; the tool's premise has changed", name)
		}
		doc, err := fixturemigrate.ParseDoc([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixturemigrate.FixtureSnapshot(cfg, doc, "fixture.yml"); err == nil {
			t.Fatalf("%s: dbfixture cannot load this file, and the tool accepted it", name)
		}
	}
}

type Tag struct {
	bun.BaseModel `bun:"table:tags"`

	ID int64 `bun:"id,pk"`
	// A pointer is bun's idiom for a nullable column.
	Label *string `bun:"label"`
	// So is nullzero on a plain field.
	Note string `bun:"note,nullzero"`
}

// The second face of marshalsToDefault: (IsPtr && nil) || (zero && (NullZero ||
// SQLDefault != "")). An explicit ~ in a fixture row, loaded into a pointer or
// a nullzero field of a column that has a default, is written as DEFAULT, so
// the database holds the default and not NULL. The default here is set in SQL,
// so no Go tag is involved at all.
func TestBunWritesTheDefaultForANull(t *testing.T) {
	db := connect(t)
	db.RegisterModel((*Tag)(nil))
	ctx := context.Background()
	for _, stmt := range []string{
		"DROP TABLE IF EXISTS tags",
		"CREATE TABLE tags (id bigint PRIMARY KEY, label text DEFAULT 'none', note text DEFAULT now()::text)",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	text := `- model: Tag
  rows:
    - _id: a
      id: 1
      label: ~
      note: ~
`
	loadFixture(t, db, text)
	if got := scan[bool](t, db, "SELECT label = 'none' AND note IS NOT NULL FROM tags WHERE id = 1"); !got {
		t.Fatal("this test documents bun's behaviour; if NULL is stored now, the tool's premise has changed")
	}

	cfg := &fixturemigrate.Config{Models: map[string]*fixturemigrate.Model{
		"Tag": {Table: "tags", Key: []string{"id"}, Ref: "id"}}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	snap := fixtureSnapshot(t, cfg, text, "fixture.yml")
	fixturemigrate.LintNullDefaults(cfg, snap, schemaOf(t, db))
	var cols []string
	for _, f := range snap.Findings {
		if f.Kind == fixturemigrate.FindingNullDefault {
			cols = append(cols, f.Detail[:strings.Index(f.Detail, " ")])
		}
	}
	if strings.Join(cols, ",") != "label,note" {
		t.Fatalf("expected both nulls to be reported, got %+v", snap.Findings)
	}
}

// The same rule holds for bun's UPDATE since v1.2.17 (uptrace/bun#1315): an
// application that saves a row through db.NewUpdate().Model(row) writes
// DEFAULT for a nil pointer and for a zero in a nullzero field. An admin UI
// that clears such a field therefore leaves the column's default, not NULL,
// and check reports the difference from a file that says ~. This pins the
// premise the documentation states.
func TestBunUpdatesANullToTheDefault(t *testing.T) {
	db := connect(t)
	ctx := context.Background()
	run(t, db, "DROP TABLE IF EXISTS tags",
		"CREATE TABLE tags (id bigint PRIMARY KEY, label text DEFAULT 'none', note text DEFAULT 'n/a')",
		"INSERT INTO tags VALUES (1, 'set', 'set')")
	if _, err := db.NewUpdate().Model(&Tag{ID: 1}).WherePK().Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if got := scan[string](t, db, "SELECT concat_ws('|', label, note) FROM tags WHERE id = 1"); got != "none|n/a" {
		t.Fatalf("this test documents bun's UPDATE; if it stores NULL now, the documentation is out of date: %s", got)
	}
}

type Plain struct {
	bun.BaseModel `bun:"table:plains"`

	ID    int64  `bun:"id,pk"`
	Name  string `bun:"name,notnull,unique"`
	Label string `bun:"label"`
	Count int64  `bun:"count"`
}

// A null written into a NOT NULL column without a default is stored as the
// field's zero by a plain field, so the file and the database disagree from
// the first seed, and a migration writing that NULL fails. It is an invalid
// value.
func TestANullIntoANotNullColumnIsAnInvalidValue(t *testing.T) {
	db := connect(t)
	db.RegisterModel((*Plain)(nil))
	run(t, db, "DROP TABLE IF EXISTS plains",
		"CREATE TABLE plains (id bigint PRIMARY KEY, name text UNIQUE NOT NULL, label text NOT NULL, count bigint NOT NULL)")
	text := "- model: Plain\n  rows:\n    - {id: 1, name: a, label: ~, count: ~}\n"
	loadFixture(t, db, text)
	if got := scan[string](t, db, "SELECT '['||label||']'||count FROM plains"); got != "[]0" {
		t.Fatalf("this documents what dbfixture stores; if it changed, so did the premise: %s", got)
	}
	cfg := &fixturemigrate.Config{Models: map[string]*fixturemigrate.Model{"Plain": {Table: "plains", Key: []string{"name"}}}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	snap := fixtureSnapshot(t, cfg, text, "fixture.yml")
	fixturemigrate.LintNullDefaults(cfg, snap, schemaOf(t, db))
	var cols []string
	for _, f := range snap.Findings {
		if f.Kind == fixturemigrate.FindingInvalidValue && strings.Contains(f.Detail, "NOT NULL and has no default") {
			cols = append(cols, f.Detail[:strings.Index(f.Detail, " ")])
		}
	}
	if strings.Join(cols, ",") != "count,label" {
		t.Fatalf("expected both nulls to be reported, got %+v", snap.Findings)
	}
}

// '{{ "Hello {{ name }}" }}' is how a file has dbfixture store a value that
// holds template delimiters: it evaluates to its text. The tool reads it as
// that text, so the database seeded from the file agrees with it.
func TestATemplateOfAStringConstantIsStoredAsItsText(t *testing.T) {
	db := connect(t)
	db.RegisterModel((*Plain)(nil))
	run(t, db, "DROP TABLE IF EXISTS plains",
		"CREATE TABLE plains (id bigint PRIMARY KEY, name text UNIQUE NOT NULL, label text NOT NULL, count bigint NOT NULL)")
	text := "- model: Plain\n  rows:\n    - {id: 1, name: a, label: '{{ \"Hello {{ name }}\" }}!', count: '{{ \"017\" }}'}\n"
	loadFixture(t, db, text)
	if got := scan[string](t, db, "SELECT label||'|'||count FROM plains"); got != "Hello {{ name }}!|17" {
		t.Fatalf("this documents what dbfixture stores; if it changed, so did the premise: %s", got)
	}
	cfg := &fixturemigrate.Config{Schema: "public",
		Models: map[string]*fixturemigrate.Model{"Plain": {Table: "plains", Key: []string{"name"}}}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	head := fixtureSnapshot(t, cfg, text, "fixture.yml")
	readOnlyDo(t, db, func(tx bun.Tx, tables map[string]*dbschema.Table) {
		ctx := context.Background()
		if err := fixturemigrate.Canonicalize(ctx, tx, cfg, head, tables); err != nil {
			t.Fatal(err)
		}
		database, err := fixturemigrate.DatabaseSnapshot(ctx, tx, cfg, tables,
			fixturemigrate.SnapshotOptions{Columns: head.Columns, Order: head.Order})
		if err != nil {
			t.Fatal(err)
		}
		check, err := fixturemigrate.Check(cfg, database, head)
		if err != nil {
			t.Fatal(err)
		}
		if check.Drifted() {
			t.Fatalf("the database seeded from the file disagrees with it:\n%s", strings.Join(check.Lines(), "\n"))
		}
	})
}
