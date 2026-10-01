package dbtest_test

// jsonb, json and array columns, written in the fixture file as YAML mappings
// and sequences, the way a bun model with `type:jsonb` or `array` is seeded.
// Before, the tool refused any mapping or sequence, and a json or point column
// in a guard failed the migration at deploy time: PostgreSQL has no equality
// for either.

import (
	"context"
	"strings"
	"testing"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
)

type Flag struct {
	bun.BaseModel `bun:"table:flags"`

	ID       int64          `bun:"id,pk"`
	Name     string         `bun:"name,notnull,unique"`
	Settings map[string]any `bun:"settings,type:jsonb"`
	Raw      map[string]any `bun:"raw,type:json"`
	Tags     []string       `bun:"tags,array"`
	Limits   []int64        `bun:"limits,array"`
}

const flagFixture = `- model: Flag
  rows:
    - _id: beta
      id: 1
      name: beta
      settings: {rollout: 50, regions: [eu, us], note: "a <b> & c", nested: {on: true, ratio: 0.25}}
      raw: {b: 1, a: [1, 2.5]}
      tags: [x, "y z", "01", "a,b"]
      limits: [1, 2, 3]
    - _id: empty
      id: 2
      name: empty
      settings: {}
      raw: {}
      tags: []
      limits: []
`

func flagConfig(t *testing.T) *fixturemigrate.Config {
	t.Helper()
	cfg := &fixturemigrate.Config{Schema: "public", SeedGuardTable: "flags",
		Models: map[string]*fixturemigrate.Model{"Flag": {Table: "flags", Key: []string{"name"}}}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func flagDB(t *testing.T) *bun.DB {
	t.Helper()
	db := connect(t)
	db.RegisterModel((*Flag)(nil))
	ctx := context.Background()
	if _, err := db.NewDropTable().Model((*Flag)(nil)).IfExists().Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.NewCreateTable().Model((*Flag)(nil)).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	return db
}

func flagState(t *testing.T, db *bun.DB) string {
	t.Helper()
	return scan[string](t, db, `SELECT string_agg(concat_ws('|', name, settings::text, raw::jsonb::text,
		tags::text, limits::text), E'\n' ORDER BY name) FROM flags`)
}

func TestJSONAndArrayColumnsRoundTrip(t *testing.T) {
	db := flagDB(t)
	cfg := flagConfig(t)
	ctx := context.Background()
	loadFixture(t, db, flagFixture)
	want := flagState(t, db)

	head := fixtureSnapshot(t, cfg, flagFixture, "fixture.yml")
	var exported []byte
	readOnlyDo(t, db, func(tx bun.Tx, tables map[string]*dbschema.Table) {
		if err := fixturemigrate.Canonicalize(ctx, tx, cfg, head, tables); err != nil {
			t.Fatal(err)
		}
		if len(head.Findings) != 0 {
			t.Fatalf("findings: %+v", head.Findings)
		}
		database, err := fixturemigrate.DatabaseSnapshot(ctx, tx, cfg, tables,
			fixturemigrate.SnapshotOptions{Columns: head.Columns, Order: head.Order})
		if err != nil {
			t.Fatal(err)
		}
		res, err := fixturemigrate.Check(cfg, database, head)
		if err != nil {
			t.Fatal(err)
		}
		if res.Drifted() {
			t.Fatalf("the file and the database hold the same values:\n%s", strings.Join(res.Lines(), "\n"))
		}
		all, err := fixturemigrate.DatabaseSnapshot(ctx, tx, cfg, tables, fixturemigrate.SnapshotOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if exported, err = fixturemigrate.Export(cfg, all, tables, nil); err != nil {
			t.Fatal(err)
		}
	})
	fresh := flagDB(t)
	loadFixture(t, fresh, string(exported))
	if got := flagState(t, fresh); got != want {
		t.Fatalf("the export does not load back as the database\n got %s\nwant %s\n%s", got, want, exported)
	}
}

// Changes inside a jsonb document, a json one and two arrays: the guards
// compare them as PostgreSQL does, and the result is what dbfixture would
// have loaded from the new file.
func TestAMigrationChangesJSONAndArrays(t *testing.T) {
	cfg := flagConfig(t)
	ctx := context.Background()
	next := strings.Replace(flagFixture, "rollout: 50", "rollout: 75", 1)
	next = strings.Replace(next, "raw: {b: 1, a: [1, 2.5]}", "raw: {a: [1, 2.5], b: 2}", 1)
	next = strings.Replace(next, `tags: [x, "y z", "01", "a,b"]`, `tags: [x, "y z", "01", "a,b", w]`, 1)
	next = strings.Replace(next, "limits: [1, 2, 3]", "limits: [3, 2, 1]", 1)

	db := flagDB(t)
	loadFixture(t, db, next)
	want := flagState(t, db)

	db = flagDB(t)
	loadFixture(t, db, flagFixture)
	res, err := fixturemigrate.Compute(cfg, fixtureSnapshot(t, cfg, flagFixture, "old"), fixtureSnapshot(t, cfg, next, "new"))
	if err != nil || len(res.Refusals) != 0 {
		t.Fatalf("%v %+v", err, res.Refusals)
	}
	if len(res.Changes) != 1 || len(res.Changes[0].New) != 4 {
		t.Fatalf("one update of four columns expected: %+v", res.Changes)
	}
	set := fixturechange.Set{Name: "flags", SeedGuardTable: "flags", Tables: res.Tables, Changes: res.Changes}
	var outcomes []fixtureapply.Outcome
	if err := fixtureapply.Apply(ctx, db, set, quiet(),
		fixtureapply.WithReport(func(o fixtureapply.Outcome) { outcomes = append(outcomes, o) })); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if outcomes[0].Status != fixtureapply.StatusApplied {
		t.Fatalf("the guard did not match the row: %+v", outcomes[0])
	}
	if got := flagState(t, db); got != want {
		t.Fatalf("the migration wrote something else than dbfixture\n got %s\nwant %s", got, want)
	}
	// Once more: every value already there, nothing to do.
	outcomes = nil
	if err := fixtureapply.Apply(ctx, db, set, quiet(),
		fixtureapply.WithReport(func(o fixtureapply.Outcome) { outcomes = append(outcomes, o) })); err != nil {
		t.Fatal(err)
	}
	if outcomes[0].Status != fixtureapply.StatusUnchanged {
		t.Fatalf("a second run should find the values made: %+v", outcomes[0])
	}
	// And back.
	if err := fixtureapply.Revert(ctx, db, set, quiet()); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	fresh := flagDB(t)
	loadFixture(t, fresh, flagFixture)
	if got, before := flagState(t, db), flagState(t, fresh); got != before {
		t.Fatalf("revert did not restore\n got %s\nwant %s", got, before)
	}
}

// point has no equality operator either. A guard on it compares text.
func TestAGuardOnATypeWithoutEqualityWorks(t *testing.T) {
	db := connect(t)
	run(t, db,
		"DROP TABLE IF EXISTS spots",
		"CREATE TABLE spots (id bigint PRIMARY KEY, name text UNIQUE NOT NULL, loc point, doc xml)",
		"INSERT INTO spots VALUES (1, 'a', '(1,2)', '<a/>')")
	set := fixturechange.Set{
		Name:   "spots",
		Tables: fixturechange.Tables{"Spot": {Name: "spots", ID: "id"}},
		Changes: []fixturechange.Change{{Model: "Spot", Kind: fixturechange.Update,
			Key: fixturechange.Values{"name": fixturechange.Lit("a")},
			Old: fixturechange.Values{"loc": fixturechange.Lit("(1,2)"), "doc": fixturechange.Lit("<a/>")},
			New: fixturechange.Values{"loc": fixturechange.Lit("(3,4)"), "doc": fixturechange.Lit("<b/>")}}},
	}
	if err := fixtureapply.Apply(context.Background(), db, set, quiet()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := scan[string](t, db, "SELECT loc::text || doc::text FROM spots"); got != "(3,4)<b/>" {
		t.Fatalf("got %s", got)
	}
}

type Alias struct {
	bun.BaseModel `bun:"table:aliases"`

	ID    int64    `bun:"id,pk"`
	Name  string   `bun:"name,notnull,unique"`
	Label string   `bun:"label,notnull"`
	Note  *string  `bun:"note"`
	Qty   int64    `bun:"qty,notnull"`
	Tags  []string `bun:"tags,array"`
}

// A YAML alias is the value it names: dbfixture stores "shared label" for
// *lbl and NULL for an alias of ~, and so does a migration. Before, the tool
// took an alias for a structured value and wrote its JSON, quotes and all, or
// the text null.
func TestAnAliasIsTheValueItNames(t *testing.T) {
	db := connect(t)
	db.RegisterModel((*Alias)(nil))
	ctx := context.Background()
	reset := func() {
		t.Helper()
		run(t, db, "DROP TABLE IF EXISTS aliases",
			"CREATE TABLE aliases (id bigint PRIMARY KEY, name text UNIQUE NOT NULL, label text NOT NULL, "+
				"note text, qty bigint NOT NULL, tags text[])")
	}
	state := func() string {
		t.Helper()
		return scan[string](t, db, `SELECT string_agg(concat_ws('|', name, label, coalesce(note, '<null>'), qty, tags::text),
			E'\n' ORDER BY name) FROM aliases`)
	}
	cfg := &fixturemigrate.Config{Schema: "public",
		Models: map[string]*fixturemigrate.Model{"Alias": {Table: "aliases", Key: []string{"name"}}}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	first := "    - {id: 1, name: first, label: &lbl \"shared label\", note: &nul ~, qty: &q 0x5, tags: &t [x, *lbl]}\n"
	v1 := "- model: Alias\n  rows:\n" + first
	v2 := v1 + "    - {id: 2, name: second, label: *lbl, note: *nul, qty: *q, tags: *t}\n"

	reset()
	loadFixture(t, db, v2)
	seeded := state()
	if !strings.Contains(seeded, "second|shared label|<null>|5|{x,\"shared label\"}") {
		t.Fatalf("this documents what dbfixture stores; if it changed, so did the premise:\n%s", seeded)
	}
	head := fixtureSnapshot(t, cfg, v2, "fixture.yml")
	readOnlyDo(t, db, func(tx bun.Tx, tables map[string]*dbschema.Table) {
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

	old, next := fixtureSnapshot(t, cfg, v1, "v1"), fixtureSnapshot(t, cfg, v2, "v2")
	readOnlyDo(t, db, func(tx bun.Tx, tables map[string]*dbschema.Table) {
		for _, s := range []*fixturemigrate.Snapshot{old, next} {
			if err := fixturemigrate.Canonicalize(ctx, tx, cfg, s, tables); err != nil {
				t.Fatal(err)
			}
		}
	})
	res, err := fixturemigrate.Compute(cfg, old, next)
	if err != nil || len(res.Refusals) != 0 || len(res.Changes) != 1 {
		t.Fatalf("%v %+v %+v", err, res.Changes, res.Refusals)
	}
	reset()
	loadFixture(t, db, v1)
	if err := fixtureapply.Apply(ctx, db, fixturechange.Set{Name: "alias", Tables: res.Tables, Changes: res.Changes},
		quiet()); err != nil {
		t.Fatal(err)
	}
	if migrated := state(); migrated != seeded {
		t.Fatalf("the migration wrote something else than dbfixture\nmigrated %s\n  seeded %s", migrated, seeded)
	}
}

// A null in a sequence is left out of a []string field and kept in a
// []*string one, and nothing in the database says which the model has. It is
// a finding, and a change carrying it is refused.
func TestANullInASequenceIsAmbiguous(t *testing.T) {
	db := flagDB(t)
	cfg := flagConfig(t)
	text := strings.Replace(flagFixture, `tags: [x, "y z", "01", "a,b"]`, `tags: [x, ~, "01"]`, 1)
	loadFixture(t, db, text)
	if got := scan[string](t, db, "SELECT tags::text FROM flags WHERE name = 'beta'"); got != "{x,01}" {
		t.Fatalf("this documents what dbfixture stores for a []string; if it changed, so did the premise: %s", got)
	}
	head := fixtureSnapshot(t, cfg, text, "fixture.yml")
	if len(head.Findings) != 1 || head.Findings[0].Kind != fixturemigrate.FindingAmbiguousValue ||
		!strings.Contains(head.Findings[0].Detail, "tags: it is a sequence holding a null") {
		t.Fatalf("expected the null to be reported, got %+v", head.Findings)
	}
	res, err := fixturemigrate.Compute(cfg, fixtureSnapshot(t, cfg, flagFixture, "old"), head)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 0 || len(res.Refusals) != 1 || !strings.Contains(res.Refusals[0].Reason, "holding a null") {
		t.Fatalf("expected the change to be refused, got %+v / %+v", res.Changes, res.Refusals)
	}
}
