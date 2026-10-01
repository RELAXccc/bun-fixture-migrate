package dbtest_test

// What a review of the value fidelity found, each held against the real
// dbfixture: a fresh seed of a file has to read as that file, and a
// migration or a sync has to store what the seed stores.

import (
	"context"
	"strings"
	"testing"
	"time"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/dbschema"

	"github.com/uptrace/bun"
)

type RvJ struct {
	bun.BaseModel `bun:"table:rv_j"`
	ID            int64          `bun:"id,pk"`
	Code          string         `bun:"code,notnull"`
	Doc           map[string]any `bun:"doc,type:jsonb"`
}

// A finding names its row by the key's label, as every other one does, not
// by the encoding two keys are compared in.
func TestReviewFindingsNameTheRowByItsLabel(t *testing.T) {
	db := connect(t)
	if _, err := db.ExecContext(context.Background(), "CREATE EXTENSION IF NOT EXISTS citext"); err != nil {
		t.Skipf("citext: %v", err)
	}
	l := newLab(t, map[string]*fixturemigrate.Model{"RvJ": {Table: "rv_j", Key: []string{"code"}}}, "rv_j",
		[]string{"DROP TABLE IF EXISTS rv_j", "CREATE TABLE rv_j (id bigint PRIMARY KEY, code citext NOT NULL, doc jsonb)"},
		`SELECT ''`, (*RvJ)(nil))
	head := l.read("- model: RvJ\n  rows:\n    - {id: 1, code: Go, doc: ~}\n    - {id: 2, code: GO, doc: {}}\n")
	kinds := map[fixturemigrate.FindingKind]bool{}
	for _, f := range head.Findings {
		kinds[f.Kind] = true
		if f.Row != "RvJ/code=Go" {
			t.Errorf("%s names its row %q", f.Kind, f.Row)
		}
	}
	if !kinds[fixturemigrate.FindingNullDefault] || !kinds[fixturemigrate.FindingDuplicateKey] {
		t.Fatalf("expected a null and a duplicate key, got %+v", head.Findings)
	}
}

type RvAny struct {
	bun.BaseModel `bun:"table:rv_any"`
	ID            int64       `bun:"id,pk"`
	Name          string      `bun:"name,notnull"`
	Doc           any         `bun:"doc,type:jsonb"`
	List          []any       `bun:"list,type:jsonb"`
	At            time.Time   `bun:"at,nullzero"`
	Ats           []time.Time `bun:"ats,array"`
}

// A sequence or a scalar at the top of a jsonb column is what an any or a
// slice field makes of it, as it is inside a mapping: a timestamp keeps its
// offset, a date alone is midnight UTC, a float is a float64. And a date alone
// in a timestamptz column is the midnight UTC a time.Time field makes of it,
// not a value the seeding session decides.
func TestReviewTopLevelJSONAndDatesReadAsDbfixtureStoresThem(t *testing.T) {
	l := newLab(t, map[string]*fixturemigrate.Model{"RvAny": {Table: "rv_any", Key: []string{"name"}}}, "rv_any",
		[]string{"DROP TABLE IF EXISTS rv_any", "CREATE TABLE rv_any (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, " +
			"doc jsonb, list jsonb, at timestamptz, ats timestamptz[])"},
		`SELECT string_agg(concat_ws('|', name, doc::text, list::text, at AT TIME ZONE 'UTC', ats::text), E'\n' ORDER BY name) FROM rv_any`,
		(*RvAny)(nil))
	v1 := "- model: RvAny\n  rows:\n    - {id: 1, name: a, doc: 1, list: [1], at: 2025-06-01T00:00:00Z, ats: [2025-06-01T00:00:00Z]}\n"
	for _, row := range []string{
		`{id: 1, name: a, doc: 1, list: [2026-01-01T10:00:00+02:00], at: 2026-01-01, ats: [2026-01-01]}`,
		`{id: 1, name: a, doc: 1, list: [2026-01-01], at: 2026-01-01T10:00:00+02:00, ats: [2026-01-01, 2026-01-02T10:00:00+02:00]}`,
		`{id: 1, name: a, doc: 1, list: [0.1234567890123456789, {k: 2026-01-01}], at: 2026-01-01, ats: [2026-01-01]}`,
		`{id: 1, name: a, doc: 2026-01-01T10:00:00+02:00, list: [1], at: 2026-01-01, ats: [2026-01-01]}`,
		`{id: 1, name: a, doc: 0.1234567890123456789, list: [1], at: 2026-01-01, ats: [2026-01-01]}`,
		`{id: 1, name: a, doc: 2026-01-01, list: [1], at: 2026-01-01, ats: [2026-01-01]}`,
		`{id: 1, name: a, doc: 2026-01-01 10:00:00, list: [2026-01-01 10:00:00], at: 2026-01-01 10:00:00, ats: [2026-01-01]}`,
	} {
		t.Run(row, func(t *testing.T) {
			l.t = t
			l.fidelity(v1, "- model: RvAny\n  rows:\n    - "+row+"\n")
		})
	}
}

type RvDoc struct {
	bun.BaseModel `bun:"table:rv_doc"`
	ID            int64          `bun:"id,pk"`
	Name          string         `bun:"name,notnull"`
	Meta          map[string]any `bun:"meta,type:jsonb"`
}

// A merge key inside a jsonb mapping is merged the way yaml.v3 merges it for
// dbfixture, rather than refusing the file.
func TestReviewAMergeKeyInsideAJSONMapping(t *testing.T) {
	l := newLab(t, map[string]*fixturemigrate.Model{"RvDoc": {Table: "rv_doc", Key: []string{"name"}}}, "rv_doc",
		[]string{"DROP TABLE IF EXISTS rv_doc", "CREATE TABLE rv_doc (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, meta jsonb)"},
		`SELECT string_agg(concat_ws('|', name, meta::text), E'\n' ORDER BY name) FROM rv_doc`, (*RvDoc)(nil))
	v1 := "- model: RvDoc\n  rows:\n    - {id: 1, name: a, meta: {k: 1}}\n    - {id: 2, name: b, meta: {k: 1}}\n"
	v2 := `- model: RvDoc
  rows:
    - {id: 1, name: a, meta: &m {k: 1, j: 2, deep: &d {x: 1}}}
    - {id: 2, name: b, meta: {<<: [*m, {k: 0, z: 9}], j: 3, deep: {<<: *d, y: 2}}}
`
	l.fidelity(v1, v2)
	if got := l.current(); got != `a|{"j": 2, "k": 1, "deep": {"x": 1}}`+"\n"+`b|{"j": 3, "k": 1, "z": 9, "deep": {"x": 1, "y": 2}}` {
		t.Fatalf("dbfixture stored %s", got)
	}
}

type RvSrc struct {
	bun.BaseModel `bun:"table:rv_src"`
	ID            int64     `bun:"id,pk"`
	Name          string    `bun:"name,notnull"`
	Note          *string   `bun:"note"`
	N             int64     `bun:"n"`
	F             float64   `bun:"f"`
	At            time.Time `bun:"at,nullzero"`
}

type RvDst struct {
	bun.BaseModel `bun:"table:rv_dst"`
	ID            int64  `bun:"id,pk"`
	Name          string `bun:"name,notnull"`
	Copy          string `bun:"copy"`
}

// dbfixture copies a field by printing it with fmt: a string or an integer
// as it is, which the tool reads and a migration writes; a nil pointer as
// <nil>, a float64 of 100000000 as 1e+08 and a time.Time with its zone's
// name, which no file can write, and which are refused.
func TestReviewATemplateCopyIsWhatFmtPrints(t *testing.T) {
	l := newLab(t, map[string]*fixturemigrate.Model{
		"RvSrc": {Table: "rv_src", Key: []string{"name"}},
		"RvDst": {Table: "rv_dst", Key: []string{"name"}},
	}, "rv_dst",
		[]string{"DROP TABLE IF EXISTS rv_src", "DROP TABLE IF EXISTS rv_dst",
			"CREATE TABLE rv_src (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, note text, n bigint, f float8, at timestamptz)",
			"CREATE TABLE rv_dst (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, copy text)"},
		`SELECT string_agg(concat_ws('|', name, coalesce(copy, '<NULL>')), E'\n' ORDER BY name) FROM rv_dst`,
		(*RvSrc)(nil), (*RvDst)(nil))
	src := "- model: RvSrc\n  rows:\n    - {_id: s, id: 1, name: 1.10, note: ~, n: 0x1F, f: 100000000, at: 2026-01-01T10:00:00Z}\n"
	dst := func(field string) string {
		return src + "- model: RvDst\n  rows:\n    - {id: 1, name: d, copy: '{{ $.RvSrc.s." + field + " }}'}\n"
	}
	v1 := src + "- model: RvDst\n  rows:\n    - {id: 1, name: d, copy: x}\n"
	l.fidelity(v1, dst("Name"))
	if got := l.current(); got != "d|1.10" {
		t.Fatalf("dbfixture stored %s", got)
	}
	l.fidelity(v1, dst("N"))
	if got := l.current(); got != "d|31" {
		t.Fatalf("dbfixture stored %s", got)
	}
	if _, err := fixturemigrate.FixtureSnapshot(l.cfg, mustParse(t, dst("Note")), "fixture.yml"); err == nil ||
		!strings.Contains(err.Error(), "which is null in that row") {
		t.Fatalf("expected a copy of a null to be refused, got %v", err)
	}
	for field, column := range map[string]string{"F": "f of a RvSrc row, a double precision", "At": "at of a RvSrc row, a timestamp with time zone"} {
		l.seed(dst(field))
		l.refused(dst(field), "copy copies "+column+" column")
	}
}

func mustParse(t *testing.T, text string) fixturemigrate.Doc {
	t.Helper()
	doc, err := fixturemigrate.ParseDoc([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

type RvPlan struct {
	bun.BaseModel `bun:"table:rv_plans"`
	ID            int64  `bun:"id,pk"`
	Name          string `bun:"name,notnull"`
}

type RvLimit struct {
	bun.BaseModel `bun:"table:rv_limits"`
	PlanID        int64 `bun:"plan_id,pk"`
	MaxUsers      int64 `bun:"max_users,notnull"`
}

// A primary key that is a reference, as scaffold configured it, read the
// template naming the plan as the id. The configuration is refused now, and
// without the id the table reads, migrates and syncs as dbfixture seeds it.
func TestReviewAPrimaryKeyThatIsAReference(t *testing.T) {
	models := map[string]*fixturemigrate.Model{
		"RvPlan":  {Table: "rv_plans"},
		"RvLimit": {Table: "rv_limits", ID: "plan_id", Key: []string{"plan_id"}, References: map[string]string{"plan_id": "RvPlan"}},
	}
	cfg := &fixturemigrate.Config{Schema: "public", Models: models}
	if err := cfg.Prepare(); err == nil || !strings.Contains(err.Error(), "its id, plan_id, is also a reference to RvPlan") {
		t.Fatalf("expected the configuration to be refused, got %v", err)
	}
	models["RvLimit"].ID = ""
	l := newLab(t, models, "rv_plans",
		[]string{"DROP TABLE IF EXISTS rv_limits", "DROP TABLE IF EXISTS rv_plans",
			"CREATE TABLE rv_plans (id bigint PRIMARY KEY, name text NOT NULL UNIQUE)",
			"CREATE TABLE rv_limits (plan_id bigint PRIMARY KEY REFERENCES rv_plans, max_users bigint NOT NULL)"},
		`SELECT string_agg(concat_ws('|', p.name, l.max_users), E'\n' ORDER BY p.name) FROM rv_limits l JOIN rv_plans p ON p.id = l.plan_id`,
		(*RvPlan)(nil), (*RvLimit)(nil))
	plans := "- model: RvPlan\n  rows:\n    - {_id: basic, id: 1, name: basic}\n    - {_id: pro, id: 2, name: pro}\n- model: RvLimit\n  rows:\n"
	l.fidelity(plans+"    - {plan_id: '{{ $.RvPlan.basic.ID }}', max_users: 5}\n",
		plans+"    - {plan_id: '{{ $.RvPlan.basic.ID }}', max_users: 6}\n    - {plan_id: '{{ $.RvPlan.pro.ID }}', max_users: 50}\n")
	l.roundTrip()
}

type RvArr struct {
	bun.BaseModel `bun:"table:rv_arr"`
	ID            int64   `bun:"id,pk"`
	Name          string  `bun:"name,notnull"`
	Nums          []int64 `bun:"nums,array"`
}

// An array holding a NULL was exported with a null in the sequence, which the
// tool then refused to read back and an []int64 field loads without it. The
// export is refused now, unless the model says its array fields keep a null.
func TestReviewAnExportOfAnArrayHoldingANull(t *testing.T) {
	l := newLab(t, map[string]*fixturemigrate.Model{"RvArr": {Table: "rv_arr", Key: []string{"name"}}}, "rv_arr",
		[]string{"DROP TABLE IF EXISTS rv_arr", "CREATE TABLE rv_arr (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, nums bigint[])"},
		`SELECT string_agg(concat_ws('|', name, nums::text), E'\n' ORDER BY name) FROM rv_arr`, (*RvArr)(nil))
	run(t, l.db, "INSERT INTO rv_arr VALUES (1, 'a', '{1,NULL,3}')")
	if _, err := l.export(); err == nil || !strings.Contains(err.Error(), "RvArr.nums holds [1, null, 3], an array with a NULL element") {
		t.Fatalf("expected the export to be refused, got %v", err)
	}
	run(t, l.db, "UPDATE rv_arr SET nums = '{1,3}'")
	l.roundTrip()
}

type RvGrant struct {
	bun.BaseModel `bun:"table:rv_grants"`
	ID            int64  `bun:"id,pk"`
	Perm          string `bun:"perm,notnull"`
	UserID        *int64 `bun:"user_id"`
	TeamID        *int64 `bun:"team_id"`
}

type RvUser struct {
	bun.BaseModel `bun:"table:rv_users"`
	ID            int64  `bun:"id,pk"`
	Name          string `bun:"name,notnull"`
}

// A key_any_of column a row leaves out is NULL, as the database holds it:
// before, a row leaving out the whole group was keyed by "" and read as a
// rename, and a group column no row writes made every row a difference.
func TestReviewAKeyAnyOfColumnLeftOutIsNull(t *testing.T) {
	l := newLab(t, map[string]*fixturemigrate.Model{
		"RvUser": {Table: "rv_users"},
		"RvGrant": {Table: "rv_grants", Key: []string{"perm"}, KeyAnyOf: [][]string{{"user_id", "team_id"}},
			References: map[string]string{"user_id": "RvUser", "team_id": "RvUser"}},
	}, "rv_users",
		[]string{"DROP TABLE IF EXISTS rv_grants", "DROP TABLE IF EXISTS rv_users",
			"CREATE TABLE rv_users (id bigint PRIMARY KEY, name text NOT NULL UNIQUE)",
			"CREATE TABLE rv_grants (id bigint PRIMARY KEY, perm text NOT NULL, user_id bigint REFERENCES rv_users, team_id bigint REFERENCES rv_users)"},
		`SELECT string_agg(concat_ws('|', perm, coalesce(user_id::text, '-'), coalesce(team_id::text, '-')), E'\n' ORDER BY id) FROM rv_grants`,
		(*RvUser)(nil), (*RvGrant)(nil))
	head := "- model: RvUser\n  rows:\n    - {_id: u, id: 1, name: u}\n    - {_id: v, id: 2, name: v}\n- model: RvGrant\n  rows:\n"
	files := []string{
		head + "    - {id: 1, perm: admin}\n",
		head + "    - {id: 1, perm: admin, user_id: ~, team_id: ~}\n",
		head + "    - {id: 1, perm: admin, user_id: '{{ $.RvUser.u.ID }}'}\n    - {id: 2, perm: admin}\n",
		head + "    - {id: 1, perm: admin, user_id: '{{ $.RvUser.u.ID }}'}\n    - {id: 2, perm: read, user_id: '{{ $.RvUser.v.ID }}'}\n",
		head + "    - {id: 1, perm: admin, user_id: '{{ $.RvUser.u.ID }}'}\n    - {id: 2, perm: read, user_id: '{{ $.RvUser.v.ID }}'}\n" +
			"    - {id: 3, perm: read, team_id: '{{ $.RvUser.u.ID }}'}\n",
	}
	l.cfg.Policy.Renames = fixturemigrate.RenameUpdate
	for i := 1; i < len(files); i++ {
		l.fidelity(files[i-1], files[i])
	}
}

type RvColor struct {
	bun.BaseModel `bun:"table:rv_colors"`
	ID            int64  `bun:"id,pk"`
	Name          string `bun:"name,notnull"`
}

type RvSize struct {
	bun.BaseModel `bun:"table:rv_sizes"`
	ID            int64  `bun:"id,pk"`
	Name          string `bun:"name,notnull"`
	Rank          int64  `bun:"rank,notnull"`
}

// A configured model the files hold no block of has no rows in a fresh seed:
// check and sync hold the database against that, as generate does when a
// block leaves the files. Before, they read only the models the files hold
// and agreed with a database the files no longer describe.
func TestReviewAModelWithoutABlockHasNoRows(t *testing.T) {
	l := newLab(t, map[string]*fixturemigrate.Model{
		"RvColor": {Table: "rv_colors"},
		"RvSize":  {Table: "rv_sizes"},
	}, "rv_colors",
		[]string{"DROP TABLE IF EXISTS rv_colors", "DROP TABLE IF EXISTS rv_sizes",
			"CREATE TABLE rv_colors (id bigint PRIMARY KEY, name text NOT NULL UNIQUE)",
			"CREATE TABLE rv_sizes (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, rank bigint NOT NULL)"},
		`SELECT (SELECT coalesce(string_agg(name, ',' ORDER BY name), '') FROM rv_colors) || ' / ' ||
			(SELECT coalesce(string_agg(name, ',' ORDER BY name), '') FROM rv_sizes)`,
		(*RvColor)(nil), (*RvSize)(nil))
	both := "- model: RvColor\n  rows:\n    - {id: 1, name: red}\n- model: RvSize\n  rows:\n    - {id: 1, name: s, rank: 1}\n"
	colors := "- model: RvColor\n  rows:\n    - {id: 1, name: red}\n"
	l.seed(both)

	head := l.read(colors)
	var lines []string
	readOnlyDo(t, l.db, func(tx bun.Tx, tables map[string]*dbschema.Table) {
		database, err := fixturemigrate.DatabaseSnapshot(context.Background(), tx, l.cfg, tables,
			fixturemigrate.SnapshotOptions{Columns: head.Columns, Order: head.Order})
		if err != nil {
			t.Fatal(err)
		}
		res, err := fixturemigrate.Check(l.cfg, database, head)
		if err != nil {
			t.Fatal(err)
		}
		lines = res.Lines()
		if len(res.Changes) != 1 || res.Changes[0].Model != "RvSize" || res.Changes[0].Kind != "delete" {
			t.Fatalf("expected the size to be in the database only:\n%s", strings.Join(lines, "\n"))
		}
		// Read by its key alone, as a column no file writes is no master data.
		if _, ok := res.Changes[0].Old["rank"]; ok {
			t.Fatalf("the delete is guarded by a column no file writes: %+v", res.Changes[0].Old)
		}
	})
	// generate says the same of the block that left the files.
	res, err := fixturemigrate.Compute(l.cfg, fixtureSnapshot(t, l.cfg, both, "old"), fixtureSnapshot(t, l.cfg, colors, "new"))
	if err != nil || len(res.Changes) != 1 || res.Changes[0].Model != "RvSize" {
		t.Fatalf("%v %+v", err, res)
	}
	files := []fixturemigrate.FixtureFile{{Path: "fixture.yml", Data: []byte(colors)}}
	if _, err := fixturemigrate.Sync(context.Background(), l.db, l.cfg, files, fixturemigrate.SyncOptions{}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got, want := l.current(), l.seed(colors); got != want {
		t.Fatalf("the sync left %q, a seed holds %q", got, want)
	}
}
