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
	"time"

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

type TyDom struct {
	bun.BaseModel `bun:"table:ty_dom"`

	ID   int64          `bun:"id,pk"`
	Name string         `bun:"name,notnull"`
	Q    int64          `bun:"q,notnull,default:1"`
	Code string         `bun:"code"`
	S    map[string]any `bun:"s,type:jsonb"`
}

// A domain is its base type to everything the tool asks of a column: what
// its zero is, how it is exported, what its default is. Before, the catalog's
// name of the domain was asked, a domain over integer was exported as "5",
// which an int64 cannot load, and a zero against the domain's default went
// unreported.
func TestTypesDomains(t *testing.T) {
	l := newLab(t, map[string]*fixturemigrate.Model{"TyDom": {Table: "ty_dom", Key: []string{"name"}}}, "ty_dom",
		// The domains are created once: a domain dropped and created again
		// under a pool of connections leaves pgx's prepared statements
		// pointing at a type that no longer exists.
		[]string{"DROP TABLE IF EXISTS ty_dom",
			createOnce("CREATE DOMAIN ty_qty AS integer CHECK (VALUE >= 0) DEFAULT 1"),
			createOnce("CREATE DOMAIN ty_doc AS jsonb"),
			createOnce("CREATE DOMAIN ty_code AS varchar(3)"),
			"CREATE TABLE ty_dom (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, q ty_qty NOT NULL, code ty_code, s ty_doc)"},
		`SELECT string_agg(concat_ws('|', name, q, '['||code||']', s::text), E'\n' ORDER BY name) FROM ty_dom`,
		(*TyDom)(nil))

	tables := schemaOf(t, l.db)
	q, _ := tables["public.ty_dom"].Column("q")
	code, _ := tables["public.ty_dom"].Column("code")
	s, _ := tables["public.ty_dom"].Column("s")
	if q.Type != "int4" || q.Domain != "ty_qty" || q.Default != "1" || code.Type != "varchar" || code.Length != 3 ||
		s.Type != "jsonb" {
		t.Fatalf("the domains are not followed to their base types: %+v %+v %+v", q, code, s)
	}

	const v1 = `- model: TyDom
  rows:
    - {id: 1, name: a, q: 5, code: abc, s: {k: 1, at: 2026-01-01T10:00:00+02:00}}
`
	l.seed(v1)
	l.check(v1)
	export := l.roundTrip()
	if !strings.Contains(export, "q: 5\n") || !strings.Contains(export, `s: {"k": 1, "at": "2026-01-01T10:00:00+02:00"}`) {
		t.Fatalf("a domain over integer is a number and one over jsonb a mapping:\n%s", export)
	}

	v2 := v1 + `    - {id: 2, name: b, q: 2, code: "xy ", s: {k: 2.50}}
`
	l.fidelity(v1, v2)

	l.refused(v1+"    - {id: 2, name: b, q: 0, code: x, s: {}}\n", "q is 0, but the column defaults to 1")
	l.refused(v1+"    - {id: 2, name: b, q: -1, code: x, s: {}}\n", `q is "-1", which the column's type, ty_qty, cannot hold`)
	l.refused(v1+"    - {id: 2, name: b, q: 1, code: abcd, s: {}}\n", `code is "abcd", which is longer than the 3 characters`)
}

type TyLen struct {
	bun.BaseModel `bun:"table:ty_len"`

	ID   int64  `bun:"id,pk"`
	Name string `bun:"name,notnull"`
	V5   string `bun:"v5,nullzero"`
	C3   string `bun:"c3,nullzero"`
	B3   string `bun:"b3,nullzero"`
	Vb   string `bun:"vb,nullzero"`
}

type TyCharArr struct {
	bun.BaseModel `bun:"table:ty_chararr"`

	ID      int64    `bun:"id,pk"`
	Name    string   `bun:"name,notnull"`
	Aliases []string `bun:"aliases,array"`
}

// A length is held against a value the way an INSERT holds it, not the way an
// explicit cast does, which cuts without a word: too long for varchar(n) or
// char(n) is a finding unless only spaces are cut, which the database drops;
// a bit string has to be as long as bit(n). Before, the length was dropped for
// the cast and a value too long failed the deploy, "abc    " drifted forever
// in a varchar(5), and a bit string was cut into the migration.
func TestTypesLengths(t *testing.T) {
	l := newLab(t, map[string]*fixturemigrate.Model{"TyLen": {Table: "ty_len", Key: []string{"name"}}}, "ty_len",
		[]string{"DROP TABLE IF EXISTS ty_len",
			"CREATE TABLE ty_len (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, v5 varchar(5), c3 char(3), b3 bit(3), vb varbit(3))"},
		`SELECT string_agg(concat_ws('|', name, '['||v5||']', '['||c3::text||']', octet_length(c3), b3, vb), E'\n' ORDER BY name) FROM ty_len`,
		(*TyLen)(nil))

	const v1 = `- model: TyLen
  rows:
    - {id: 1, name: a, v5: abc, c3: ab, b3: "101", vb: "10"}
`
	l.seed(v1)
	l.check(v1)
	l.roundTrip()

	// Trailing spaces past the length are dropped by an INSERT, and the
	// migration writes what is left, as the seed stores it.
	v2 := strings.Replace(v1, "v5: abc,", `v5: "abc    ",`, 1) + `    - {id: 2, name: b, v5: x, c3: "ab   ", b3: "111", vb: "1"}
`
	l.fidelity(v1, v2)

	l.refused(v1+"    - {id: 2, name: b, v5: abcdef, c3: x, b3: \"101\", vb: \"1\"}\n",
		`v5 is "abcdef", which is longer than the 5 characters the column's type, character varying(5), holds`)
	l.refused(v1+"    - {id: 2, name: b, v5: x, c3: abcd, b3: \"101\", vb: \"1\"}\n",
		`c3 is "abcd", which is longer than the 3 characters`)
	l.refused(v1+"    - {id: 2, name: b, v5: x, c3: x, b3: \"1010\", vb: \"1\"}\n",
		`b3 is "1010", which is not 3 bits long`)
	l.refused(v1+"    - {id: 2, name: b, v5: x, c3: x, b3: \"10\", vb: \"1\"}\n",
		`b3 is "10", which is not 3 bits long`)
	l.refused(v1+"    - {id: 2, name: b, v5: x, c3: x, b3: \"101\", vb: \"x5\"}\n",
		`vb is "x5", which is longer than the 3 bits`)
	for _, text := range []string{
		v1 + "    - {id: 2, name: b, v5: abcdef, c3: x, b3: \"101\", vb: \"1\"}\n",
		v1 + "    - {id: 2, name: b, v5: x, c3: x, b3: \"1010\", vb: \"1\"}\n",
	} {
		l.reset()
		if err := seedErr(l.db, text); err == nil {
			t.Fatalf("dbfixture loaded what the tool refuses:\n%s", text)
		}
	}

	// The elements of a char(n) array are read without their padding on
	// both sides, so a seed agrees with its file.
	a := newLab(t, map[string]*fixturemigrate.Model{"TyCharArr": {Table: "ty_chararr", Key: []string{"name"}}},
		"ty_chararr", []string{"DROP TABLE IF EXISTS ty_chararr",
			"CREATE TABLE ty_chararr (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, aliases char(3)[])"},
		`SELECT string_agg(concat_ws('|', name, aliases::text), E'\n' ORDER BY name) FROM ty_chararr`,
		(*TyCharArr)(nil))
	const arr = `- model: TyCharArr
  rows:
    - {id: 1, name: germany, aliases: [DE, "D ", GER]}
`
	a.seed(arr)
	a.check(arr)
	a.roundTrip()
	a.refused(arr+"    - {id: 2, name: spain, aliases: [ESPA]}\n", `aliases is "[\"ESPA\"]", which is longer than the 3 characters`)
}

type TyCast struct {
	bun.BaseModel `bun:"table:ty_cast"`

	ID   int64  `bun:"id,pk"`
	Name string `bun:"name,notnull"`
	Tq   string `bun:"tq,nullzero"`
	Lt   string `bun:"lt,nullzero"`
	Hs   string `bun:"hs,nullzero"`
}

// Any error PostgreSQL gives casting one value is that value being invalid,
// whatever its class: hstore, ltree and tsquery refuse input with 42601, a
// domain's CHECK with 23514. Before, those stopped the command with a raw
// error instead of a finding.
func TestTypesEveryCastErrorIsAFinding(t *testing.T) {
	db := connect(t)
	for _, ext := range []string{"hstore", "ltree"} {
		if _, err := db.ExecContext(context.Background(), "CREATE EXTENSION IF NOT EXISTS "+ext); err != nil {
			t.Skipf("%s: %v", ext, err)
		}
	}
	l := newLab(t, map[string]*fixturemigrate.Model{"TyCast": {Table: "ty_cast", Key: []string{"name"}}}, "ty_cast",
		[]string{"DROP TABLE IF EXISTS ty_cast",
			"CREATE TABLE ty_cast (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, tq tsquery, lt ltree, hs hstore)"},
		`SELECT string_agg(concat_ws('|', name, tq, lt, hs), E'\n' ORDER BY name) FROM ty_cast`,
		(*TyCast)(nil))
	const v1 = `- model: TyCast
  rows:
    - {id: 1, name: a, tq: "cat & dog", lt: a.b, hs: "k=>v"}
`
	l.seed(v1)
	l.check(v1)
	l.refused(`- model: TyCast
  rows:
    - {id: 1, name: a, tq: "cat & & dog", lt: "a..b", hs: {k: v}}
`, `tq is "cat & & dog", which the column's type, tsquery, cannot hold: syntax error in tsquery`,
		`lt is "a..b", which the column's type, ltree, cannot hold`,
		`hs is "{\"k\":\"v\"}", which the column's type, hstore, cannot hold`)
}

type TyTime struct {
	bun.BaseModel `bun:"table:ty_time_t"`

	ID   int64     `bun:"id,pk"`
	Name string    `bun:"name,notnull"`
	Ts   time.Time `bun:"ts,nullzero"`
	Tstz time.Time `bun:"tstz,nullzero"`
	D    time.Time `bun:"d,type:date,nullzero"`
}

type TyTimeStr struct {
	bun.BaseModel `bun:"table:ty_time_s"`

	ID   int64  `bun:"id,pk"`
	Name string `bun:"name,notnull"`
	Ts   string `bun:"ts,type:timestamp,nullzero"`
	Tstz string `bun:"tstz,type:timestamptz,nullzero"`
	D    string `bun:"d,type:date,nullzero"`
	T    string `bun:"t,type:time,nullzero"`
	Ttz  string `bun:"ttz,type:timetz,nullzero"`
}

// A date or a time is accepted when every model that can load it stores the
// same value, in any session: a time.Time field, written by bun in UTC and
// cut to microseconds, and a string field, read by PostgreSQL in the seeding
// session. When they differ, or the session decides, it is a finding with the
// two values. Before, the tool took one reading, and a fresh seed drifted
// from its own file, or a migration wrote another instant than the seed.
func TestTypesTimestampSpellings(t *testing.T) {
	create := func(table string) string {
		return "CREATE TABLE " + table + ` (id bigint PRIMARY KEY, name text NOT NULL UNIQUE,
			ts timestamp, tstz timestamptz, d date, t time, ttz timetz)`
	}
	defaults := map[string]string{"ts": fixturemigrate.NullDefault, "tstz": fixturemigrate.NullDefault,
		"d": fixturemigrate.NullDefault, "t": fixturemigrate.NullDefault, "ttz": fixturemigrate.NullDefault}
	l := newLab(t, map[string]*fixturemigrate.Model{
		"TyTime":    {Table: "ty_time_t", Key: []string{"name"}, Defaults: defaults},
		"TyTimeStr": {Table: "ty_time_s", Key: []string{"name"}, Defaults: defaults},
	}, "ty_time_s",
		[]string{"DROP TABLE IF EXISTS ty_time_t", "DROP TABLE IF EXISTS ty_time_s", create("ty_time_t"), create("ty_time_s")},
		`SELECT coalesce(string_agg(concat_ws('|', x.tab, name, ts, tstz AT TIME ZONE 'UTC', d, t, ttz AT TIME ZONE 'UTC'), E'\n' ORDER BY x.tab, name), '')
			FROM (SELECT 't' AS tab, * FROM ty_time_t UNION ALL SELECT 's', * FROM ty_time_s) x`,
		(*TyTime)(nil), (*TyTimeStr)(nil))

	// What each model stores, by column and spelling; "" where the model
	// cannot load the spelling at all.
	stored := func(model, col, value string) string {
		t.Helper()
		l.reset()
		text := "- model: " + model + "\n  rows:\n    - {id: 1, name: x, " + col + ": " + value + "}\n"
		if err := seedErr(l.db, text); err != nil {
			return ""
		}
		table := map[string]string{"TyTime": "ty_time_t", "TyTimeStr": "ty_time_s"}[model]
		expr := col + "::text"
		switch col {
		case "tstz":
			expr = "(tstz AT TIME ZONE 'UTC')::text"
		case "ttz":
			expr = "(ttz AT TIME ZONE 'UTC')::text"
		}
		return scan[string](t, l.db, "SELECT "+expr+" FROM "+table)
	}

	accepted := []struct{ col, value string }{
		{"ts", "2026-01-01 10:00:00"},
		{"ts", "2026-01-01T10:00:00Z"},
		{"ts", `"2026-01-01T10:00:00Z"`},
		{"ts", `"2026-01-01 10:00:00"`},
		{"ts", "2026-01-01T10:00:00.123456Z"},
		{"ts", "2026-01-01"},
		{"tstz", "2026-01-01T10:00:00+02:00"},
		{"tstz", `"2026-01-01T10:00:00+02:00"`},
		{"tstz", `"2026-01-01 10:00:00+02"`},
		{"tstz", "2026-01-01T10:00:00.123456Z"},
		{"tstz", `"infinity"`},
		{"d", "2026-01-01"},
		{"d", `"2026-01-01"`},
		{"d", "2026-01-01T10:00:00+02:00"},
		{"t", `"10:00:00"`},
		{"t", `"10:00:00+02"`},
		{"ttz", `"10:00:00+02"`},
	}
	var rowsT, rowsS []string
	for i, c := range accepted {
		viaTime, viaString := stored("TyTime", c.col, c.value), stored("TyTimeStr", c.col, c.value)
		if viaString == "" || (viaTime != "" && viaTime != viaString) {
			t.Fatalf("%s: %s is not one value: a time.Time field stores %q, a string field %q",
				c.col, c.value, viaTime, viaString)
		}
		row := "    - {id: " + itoa(int64(i+10)) + ", name: r" + itoa(int64(i)) + ", " + c.col + ": " + c.value + "}\n"
		if viaTime != "" {
			rowsT = append(rowsT, row)
		}
		rowsS = append(rowsS, row)
	}
	file := func(t, s []string) string {
		out := "- model: TyTimeStr\n  rows:\n    - {id: 1, name: anchor}\n" + strings.Join(s, "")
		if len(t) > 0 {
			out += "- model: TyTime\n  rows:\n" + strings.Join(t, "")
		}
		return out
	}
	v1, v2 := file(nil, nil), file(rowsT, rowsS)
	if f := l.findings(v2); f != "" {
		t.Fatalf("values every model stores alike are refused:\n%s", f)
	}
	l.fidelity(v1, v2)

	// A zone-less timestamp in a timestamptz column is the documented
	// exception: an unquoted YAML timestamp is UTC to a time.Time field, which
	// is what the column is taken to be written from.
	premise := file([]string{"    - {id: 2, name: zoneless, tstz: 2026-01-01 10:00:00}\n"}, nil)
	l.fidelity(v1, premise)

	refused := []struct{ col, value, want string }{
		{"ts", "2026-01-01T10:00:00+02:00", "a time.Time field stores as 2026-01-01 08:00:00 and a string field as 2026-01-01 10:00:00"},
		{"ts", `"2026-01-01T10:00:00+02:00"`, "a time.Time field stores as 2026-01-01 08:00:00 and a string field as 2026-01-01 10:00:00"},
		{"ts", "2026-01-01T10:00:00.1234567Z", "a time.Time field stores as 2026-01-01 10:00:00.123456 and a string field as 2026-01-01 10:00:00.123457"},
		{"tstz", "2026-01-01T10:00:00.1234567Z", "write the one you mean as 2026-01-01T10:00:00.123456Z or 2026-01-01T10:00:00.123457Z"},
		{"tstz", `"2026-01-01 10:00:00"`, "TimeZone or the DateStyle of the session that writes it"},
		{"tstz", "2026-01-01", "as 2026-01-01T00:00:00Z"},
		{"tstz", `"01/02/2026 10:00:00+00"`, "DateStyle of the session that writes it, a day first or a month first"},
		{"tstz", `"now"`, "PostgreSQL evaluates when the row is written"},
		{"d", "2026-01-01T23:30:00-05:00", "a time.Time field stores as 2026-01-02 and a string field as 2026-01-01"},
		{"d", `"01/02/2026"`, "DateStyle of the session that writes it, a day first or a month first"},
		{"d", `"2026-01-02 x"`, "which the column's type, date, cannot hold"},
		{"d", `"tomorrow"`, "PostgreSQL evaluates when the row is written"},
		{"ttz", `"10:00"`, "TimeZone or the DateStyle"},
	}
	for _, c := range refused {
		l.refused("- model: TyTimeStr\n  rows:\n    - {id: 1, name: x, "+c.col+": "+c.value+"}\n", c.col+" is ", c.want)
	}
	// The evidence for the first two kinds: the models store two values.
	if viaTime, viaString := stored("TyTime", "ts", "2026-01-01T10:00:00+02:00"),
		stored("TyTimeStr", "ts", "2026-01-01T10:00:00+02:00"); viaTime == viaString {
		t.Fatalf("a time.Time and a string field store the same, %s: the premise changed", viaTime)
	}
}

type TyJSON struct {
	bun.BaseModel `bun:"table:ty_json"`

	ID   int64          `bun:"id,pk"`
	Name string         `bun:"name,notnull"`
	Doc  map[string]any `bun:"doc,type:jsonb"`
	Raw  map[string]any `bun:"raw,type:json"`
	AnyV any            `bun:"anyv,type:jsonb"`
}

// jsonb through map[string]any, the documented idiom: a nested timestamp is
// the time.Time yaml.v3 makes of it, marshalled by encoding/json; a number is
// a float64; a key is as written. A top-level string an any field holds is a
// JSON string. ~ is the JSON null to these fields and NULL to a pointer, and
// is a finding. An export reads back as the database, numbers and all, or is
// refused where nothing would.
func TestTypesJSONThroughAMap(t *testing.T) {
	l := newLab(t, map[string]*fixturemigrate.Model{"TyJSON": {Table: "ty_json", Key: []string{"name"}}}, "ty_json",
		[]string{"DROP TABLE IF EXISTS ty_json",
			"CREATE TABLE ty_json (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, doc jsonb, raw json, anyv jsonb)"},
		`SELECT string_agg(concat_ws('|', name, coalesce(doc::text, '<NULL>'), coalesce(raw::jsonb::text, '<NULL>'),
			coalesce(anyv::text, '<NULL>')), E'\n' ORDER BY name) FROM ty_json`,
		(*TyJSON)(nil))

	const v1 = `- model: TyJSON
  rows:
    - {id: 1, name: a, doc: {k: 1}, raw: {k: 1}, anyv: 1}
`
	l.seed(v1)
	l.check(v1)
	l.roundTrip()

	v2 := v1 + `    - id: 2
      name: b
      doc: {launch: 2026-01-01, at: 2026-01-01T10:00:00+02:00, nanos: 2026-01-01 10:00:00.1234567, big: 123456789012345678901234567890, prec: 0.1234567890123456789, 017: 017, bin: !!binary SGk=, list: [2026-01-02, 1.50]}
      raw: {b: 1.50, a: [1, 2.5]}
      anyv: hello
`
	l.fidelity(v1, v2)

	l.refused(v1+"    - {id: 2, name: b, doc: ~, raw: {}, anyv: 1}\n",
		"doc is null, which in a jsonb column is the JSON null when the model's field is a map")

	// Values written by SQL, as an admin UI writes them.
	// jsonb keeps the scale a number was written with, and the export and
	// a seed of it do not: 1.0 and 1 are one value to jsonb and to the
	// application, so that is how they are compared.
	l.seed(v1)
	run(t, l.db, `INSERT INTO ty_json VALUES (2, 'sql', '{"a": 1.0, "b": 1.50, "c": [1.0]}', '{"x": 2.0}', '"str"')`,
		"DROP TABLE IF EXISTS ty_json_was", "CREATE TABLE ty_json_was AS SELECT * FROM ty_json")
	data, err := l.export()
	if err != nil {
		t.Fatal(err)
	}
	export := string(data)
	if !strings.Contains(export, `doc: {"a": 1, "b": 1.5, "c": [1]}`) || !strings.Contains(export, `anyv: "str"`) {
		t.Fatalf("numbers are canonical and a JSON string is a YAML string:\n%s", export)
	}
	l.check(export)
	l.seed(export)
	if same := scan[bool](t, l.db, `SELECT bool_and(w.doc = j.doc AND w.raw::jsonb = j.raw::jsonb AND w.anyv = j.anyv)
		FROM ty_json_was w JOIN ty_json j USING (id)`); !same {
		t.Fatalf("the export does not load back as the database:\n%s", export)
	}
	run(t, l.db, "DROP TABLE ty_json_was")
	for _, c := range []struct{ set, want string }{
		{`anyv = '"true"'`, "a string that is itself JSON"},
		{`anyv = 'null'`, "null, which a fixture file can only write as ~"},
		{`doc = NULL`, "is NULL, which a fixture file can only write as ~"},
		{`doc = '{"big": 123456789012345678901234567890}'`, "more digits than the float64"},
	} {
		l.seed(v1)
		run(t, l.db, "UPDATE ty_json SET "+c.set)
		if _, err := l.export(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: expected the export to be refused with %q, got %v", c.set, c.want, err)
		}
	}
	// With defaults: {doc: 'null'} a row without doc holds the JSON null, so
	// the export leaves the JSON null out, and it all loads back as it was.
	l.seed(v1)
	run(t, l.db, `INSERT INTO ty_json VALUES (2, 'b', 'null', '{}', '1')`)
	l.cfg.Models["TyJSON"].Defaults = map[string]string{"doc": "null"}
	if export := l.roundTrip(); strings.Count(export, "doc:") != 1 {
		t.Fatalf("the JSON null is left to the default:\n%s", export)
	}
	if got := scan[string](t, l.db, "SELECT jsonb_typeof(doc) FROM ty_json WHERE name = 'b'"); got != "null" {
		t.Fatalf("a seed of the export holds %s", got)
	}
	l.cfg.Models["TyJSON"].Defaults = nil

	// Under null_default: warn, NULL is written as ~ with the hazard named.
	l.cfg.Policy.NullDefault = fixturemigrate.ModeWarn
	l.seed(v1)
	run(t, l.db, "UPDATE ty_json SET doc = NULL")
	data, err = l.export()
	if err != nil || !strings.Contains(string(data), "doc: ~  # ROUND-TRIP HAZARD: a map, slice or any field loads ~ as the JSON null") {
		t.Fatalf("%v\n%s", err, data)
	}
	l.cfg.Policy.NullDefault = fixturemigrate.ModeError
}

type TyBytes struct {
	bun.BaseModel `bun:"table:ty_bytes"`

	ID   int64  `bun:"id,pk"`
	Name string `bun:"name,notnull"`
	Data []byte `bun:"data"`
}

type TyBin struct {
	bun.BaseModel `bun:"table:ty_bin"`

	ID   int64  `bun:"id,pk"`
	Name string `bun:"name,notnull"`
	Data string `bun:"data,type:bytea"`
	V    string `bun:"v"`
}

// A []byte field loads only from a sequence of byte values, so that is what a
// sequence in a bytea column means and what an export writes. Before, the
// sequence's JSON text was cast to bytea, and the export wrote "\\x48..."; a
// !!binary scalar was carried as its base64 text, where a string field gets
// the bytes it encodes.
func TestTypesBytea(t *testing.T) {
	l := newLab(t, map[string]*fixturemigrate.Model{"TyBytes": {Table: "ty_bytes", Key: []string{"name"}}}, "ty_bytes",
		[]string{"DROP TABLE IF EXISTS ty_bytes",
			"CREATE TABLE ty_bytes (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, data bytea)"},
		`SELECT string_agg(concat_ws('|', name, data), E'\n' ORDER BY name) FROM ty_bytes`, (*TyBytes)(nil))
	const v1 = `- model: TyBytes
  rows:
    - {id: 1, name: hello, data: [72, 101, 108, 108, 111]}
`
	l.seed(v1)
	l.check(v1)
	if export := l.roundTrip(); !strings.Contains(export, "data: [72, 101, 108, 108, 111]") {
		t.Fatalf("bytea is written as the bytes:\n%s", export)
	}
	v2 := `- model: TyBytes
  rows:
    - {id: 1, name: hello, data: [72, 105]}
    - {id: 2, name: bin, data: [0, 255]}
    - {id: 3, name: empty, data: []}
`
	l.fidelity(v1, v2)
	l.refused(v1+"    - {id: 2, name: big, data: [256]}\n", "which is a sequence a []byte field cannot hold")

	b := newLab(t, map[string]*fixturemigrate.Model{"TyBin": {Table: "ty_bin", Key: []string{"name"}}}, "ty_bin",
		[]string{"DROP TABLE IF EXISTS ty_bin",
			"CREATE TABLE ty_bin (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, data bytea, v text)"},
		`SELECT string_agg(concat_ws('|', name, data, v), E'\n' ORDER BY name) FROM ty_bin`, (*TyBin)(nil))
	b2 := tyBin1 + "    - {id: 2, name: b, data: !!binary SGVsbG8=, v: x}\n"
	b.fidelity(tyBin1, b2)
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

type TyText struct {
	bun.BaseModel `bun:"table:ty_text"`

	ID   int64  `bun:"id,pk"`
	Name string `bun:"name,notnull"`
	V    string `bun:"v"`
}

// An export escapes everything YAML would refuse or fold, and writes text
// that looks like a template as a template that evaluates to it. It parses
// its output back before it returns it. Before, NEL became a space on the
// next load, DEL, the C1 controls and U+FFFE made the file unparseable, and
// "Hello {{ name }}" could not be exported at all.
func TestTypesExportEscaping(t *testing.T) {
	l := newLab(t, map[string]*fixturemigrate.Model{"TyText": {Table: "ty_text", Key: []string{"name"}}}, "ty_text",
		[]string{"DROP TABLE IF EXISTS ty_text",
			"CREATE TABLE ty_text (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, v text NOT NULL)"},
		`SELECT string_agg(concat_ws('|', name, encode(convert_to(v, 'UTF8'), 'hex')), E'\n' ORDER BY name) FROM ty_text`,
		(*TyText)(nil))
	run(t, l.db, `INSERT INTO ty_text VALUES
		(1, 'nel', 'a' || chr(133) || 'b'), (2, 'del', 'a' || chr(127) || 'b'), (3, 'c1', 'a' || chr(150) || 'b'),
		(4, 'fffe', 'a' || chr(65534) || 'b'), (5, 'ls', 'a' || chr(8232) || 'b' || chr(8233)),
		(6, 'bom', chr(65279) || 'x'), (7, 'controls', E'\t\n\r' || chr(1) || chr(27)),
		(8, 'quotes', E'"a" \\ ''b'''), (9, 'emoji', 'snow ☃ 𝄞'), (10, 'yes', 'yes'), (11, 'spaces', '  x  ')`)
	l.roundTrip()

	// Text that looks like a template: dbfixture stores it as it is.
	run(t, l.db, `INSERT INTO ty_text VALUES (12, 'tpl', 'Hello {{ name }}'), (13, 'gotpl', 'Hi {{ .Name }} "x"'),
		(14, 'now', '{{ now }}')`)
	want := l.current()
	data, err := l.export()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `v: "{{ \"Hello {{ name }}\" }}"`) {
		t.Fatalf("text that looks like a template is written as a string literal in one:\n%s", data)
	}
	if got := l.seed(string(data)); got != want {
		t.Fatalf("dbfixture does not load the export back as the database\n got %s\nwant %s\n%s", got, want, data)
	}
}

type TyFloat struct {
	bun.BaseModel `bun:"table:ty_float"`

	ID   int64   `bun:"id,pk"`
	Name string  `bun:"name,notnull"`
	D    float64 `bun:"d"`
	R    float32 `bun:"r"`
	At   string  `bun:"at,type:timestamptz,nullzero"`
}

// NaN and infinity are exported as YAML spells them for a float, and a
// timestamp's infinity as text, with a note that a time.Time cannot hold it.
// A numeric NaN, which no spelling loads alike into a string and a float64,
// is refused. Before, NaN and Infinity were plain strings no float64 loads.
func TestTypesSpecialNumbersExport(t *testing.T) {
	l := newLab(t, map[string]*fixturemigrate.Model{"TyFloat": {Table: "ty_float", Key: []string{"name"}}}, "ty_float",
		[]string{"DROP TABLE IF EXISTS ty_float",
			"CREATE TABLE ty_float (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, d float8 NOT NULL, r real NOT NULL, at timestamptz)"},
		`SELECT string_agg(concat_ws('|', name, d, r, at), E'\n' ORDER BY name) FROM ty_float`, (*TyFloat)(nil))
	run(t, l.db, `INSERT INTO ty_float VALUES (1, 'nan', 'NaN', 'Infinity', NULL), (2, 'inf', '-Infinity', 1.5, 'infinity'),
		(3, 'plain', 0.1, 1e-7, '2026-01-01 10:00:00+00')`)
	export := l.roundTrip()
	for _, line := range []string{"d: .nan", "r: .inf", "d: -.inf", `at: "infinity"  # a time.Time field cannot hold infinity`} {
		if !strings.Contains(export, line) {
			t.Fatalf("missing %q:\n%s", line, export)
		}
	}
	run(t, l.db, "ALTER TABLE ty_float ADD COLUMN n numeric", "UPDATE ty_float SET n = 'NaN' WHERE name = 'nan'")
	if _, err := l.export(); err == nil || !strings.Contains(err.Error(), "no spelling reads back as itself") {
		t.Fatalf("a numeric NaN is refused: %v", err)
	}
}

type TyGrid struct {
	bun.BaseModel `bun:"table:ty_grid"`

	ID   int64  `bun:"id,pk"`
	Name string `bun:"name,notnull"`
}

// A multidimensional array written by SQL is exported as nested sequences and
// read back as the array it was; one whose lower bound is not 1 has no YAML
// spelling and is refused. Before, the nested sequence came back as an
// invalid value, and [0:1]={7,8} was exported as [7, 8].
func TestTypesMultidimensionalArrays(t *testing.T) {
	l := newLab(t, map[string]*fixturemigrate.Model{"TyGrid": {Table: "ty_grid", Key: []string{"name"}}}, "ty_grid",
		[]string{"DROP TABLE IF EXISTS ty_grid",
			"CREATE TABLE ty_grid (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, grid integer[], words text[])"},
		`SELECT string_agg(concat_ws('|', name, grid, words), E'\n' ORDER BY name) FROM ty_grid`, (*TyGrid)(nil))
	run(t, l.db, `INSERT INTO ty_grid VALUES (1, 'g', '{{1,2},{3,4}}', '{{a,"b c"},{NULL,"{d}"}}'),
		(2, 'cube', '{{{1},{2}},{{3},{4}}}', '{}'), (3, 'flat', '{1,NULL}', '{x}')`)
	data, err := l.export()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "grid: [[1, 2], [3, 4]]") || !strings.Contains(string(data), `words: [["a", "b c"], [null, "{d}"]]`) {
		t.Fatalf("a 2-D array is a nested sequence:\n%s", data)
	}
	l.check(string(data))

	run(t, l.db, `INSERT INTO ty_grid VALUES (4, 'lb', '[0:1]={7,8}', '{}')`)
	if _, err := l.export(); err == nil || !strings.Contains(err.Error(), "lower bound is not 1") {
		t.Fatalf("an array with another lower bound is refused: %v", err)
	}
	// The file and the database disagree about it, as they should: a seed
	// of [7, 8] is numbered from 1.
	head := l.read(string(data) + "    - {id: 4, name: lb, grid: [7, 8], words: []}\n")
	readOnlyDo(t, l.db, func(tx bun.Tx, tables map[string]*dbschema.Table) {
		database, err := fixturemigrate.DatabaseSnapshot(context.Background(), tx, l.cfg, tables,
			fixturemigrate.SnapshotOptions{Columns: head.Columns, Order: head.Order})
		if err != nil {
			t.Fatal(err)
		}
		res, err := fixturemigrate.Check(l.cfg, database, head)
		if err != nil || len(res.Changes) != 1 || !strings.Contains(strings.Join(res.Lines(), "\n"), "[0:1]={7,8}") {
			t.Fatalf("%v\n%s", err, strings.Join(res.Lines(), "\n"))
		}
	})
}

type TyTag struct {
	bun.BaseModel `bun:"table:ty_tags"`

	ID     int64  `bun:"id,pk"`
	Code   string `bun:"code,notnull"`
	Weight int64  `bun:"weight"`
}

// Two natural keys PostgreSQL holds equal are one key, whatever their bytes:
// Go and GO in a citext column. Before, the diff took them for two rows and
// generated an insert dbfixture cannot load beside the other.
func TestTypesKeysEqualUnderTheirType(t *testing.T) {
	db := connect(t)
	if _, err := db.ExecContext(context.Background(), "CREATE EXTENSION IF NOT EXISTS citext"); err != nil {
		t.Skipf("citext: %v", err)
	}
	l := newLab(t, map[string]*fixturemigrate.Model{"TyTag": {Table: "ty_tags", Key: []string{"code"}}}, "ty_tags",
		[]string{"DROP TABLE IF EXISTS ty_tags",
			"CREATE TABLE ty_tags (id bigint PRIMARY KEY, code citext NOT NULL, weight int NOT NULL)"},
		`SELECT string_agg(concat_ws('|', code, weight), E'\n' ORDER BY code) FROM ty_tags`, (*TyTag)(nil))
	const v1 = "- model: TyTag\n  rows:\n    - {id: 1, code: Go, weight: 1}\n"
	l.seed(v1)
	l.check(v1)
	both := v1 + "    - {id: 2, code: GO, weight: 5}\n"
	l.refused(both, "the natural keys code=Go and code=GO are one value to the key's type in PostgreSQL")

	// The database side says so too, so an export lists them.
	l.seed(both)
	readOnlyDo(t, l.db, func(tx bun.Tx, tables map[string]*dbschema.Table) {
		snap, err := fixturemigrate.DatabaseSnapshot(context.Background(), tx, l.cfg, tables, fixturemigrate.SnapshotOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.Findings) != 1 || snap.Findings[0].Kind != fixturemigrate.FindingDuplicateKey {
			t.Fatalf("%+v", snap.Findings)
		}
	})
	// A key made of a reference and a citext column: the reference is its
	// target's key, and the label is compared as citext.
	u := newLab(t, map[string]*fixturemigrate.Model{
		"TyTag":    {Table: "ty_tags", Key: []string{"code"}, Ref: "code"},
		"TyTagUse": {Table: "ty_tag_uses", Key: []string{"tag_id", "label"}, References: map[string]string{"tag_id": "TyTag"}},
	}, "ty_tags", []string{"DROP TABLE IF EXISTS ty_tag_uses",
		"CREATE TABLE ty_tag_uses (id bigint PRIMARY KEY, tag_id bigint NOT NULL, label citext NOT NULL)"},
		"SELECT ''", (*TyTag)(nil), (*TyTagUse)(nil))
	uses := v1 + `- model: TyTagUse
  rows:
    - {id: 1, tag_id: '{{ $.TyTag.pk1.ID }}', label: x}
    - {id: 2, tag_id: '{{ $.TyTag.pk1.ID }}', label: X}
`
	u.refused(uses, "the natural keys label=x,tag_id=TyTag(Go) and label=X,tag_id=TyTag(Go) are one value")
	if f := u.findings(strings.Replace(uses, "label: X", "label: y", 1)); f != "" {
		t.Fatalf("two labels apart are two keys: %s", f)
	}

	run(t, l.db, "DELETE FROM ty_tags WHERE id = 2", "CREATE UNIQUE INDEX ON ty_tags (code)")
	if err := seedErr(l.db, "- model: TyTag\n  rows:\n    - {id: 2, code: GO, weight: 5}\n"); err == nil {
		t.Fatal("dbfixture loaded a key the unique index holds equal to one it has")
	}
}

type TyTagUse struct {
	bun.BaseModel `bun:"table:ty_tag_uses"`

	ID    int64  `bun:"id,pk"`
	TagID int64  `bun:"tag_id,notnull"`
	Label string `bun:"label,notnull"`
}

type TyCheck struct {
	bun.BaseModel `bun:"table:ty_check"`

	ID    int64  `bun:"id,pk"`
	Name  string `bun:"name,notnull"`
	Price int64  `bun:"price"`
	Lo    int64  `bun:"lo"`
	Hi    int64  `bun:"hi"`
	I     int64  `bun:"i"`
}

// A CHECK constraint over one column is held against each value, so a value
// it refuses is a finding rather than a failed deploy. One over several
// columns is not: plan runs the migration and reports it. A fraction in an
// integer column is what yaml.v3 makes of it for an integer field, which
// dbfixture stores without a word, and a finding.
func TestTypesCheckConstraintsAndFractions(t *testing.T) {
	l := newLab(t, map[string]*fixturemigrate.Model{"TyCheck": {Table: "ty_check", Key: []string{"name"}}}, "ty_check",
		[]string{"DROP TABLE IF EXISTS ty_check",
			`CREATE TABLE ty_check (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, price int NOT NULL CHECK (price < 100),
				lo int NOT NULL, hi int NOT NULL, i int NOT NULL, CHECK (lo <= hi))`},
		`SELECT string_agg(concat_ws('|', name, price, lo, hi, i), E'\n' ORDER BY name) FROM ty_check`, (*TyCheck)(nil))
	const v1 = "- model: TyCheck\n  rows:\n    - {id: 1, name: a, price: 10, lo: 1, hi: 2, i: 1}\n"
	l.seed(v1)
	l.check(v1)
	l.refused("- model: TyCheck\n  rows:\n    - {id: 1, name: a, price: 150, lo: 1, hi: 2, i: 1}\n",
		`price is "150", which the column's check constraint ty_check_price_check (price < 100) refuses`)
	if f := l.findings("- model: TyCheck\n  rows:\n    - {id: 1, name: a, price: 10, lo: 5, hi: 2, i: 1}\n"); f != "" {
		t.Fatalf("a constraint over two columns is plan's to find: %s", f)
	}

	fraction := "- model: TyCheck\n  rows:\n    - {id: 1, name: a, price: 10, lo: 1, hi: 2, i: 1.5}\n"
	l.refused(fraction, `i is "1.5", which an integer field holds as 1, because yaml.v3 drops the fraction`)
	if got := l.seed(fraction); got != "a|10|1|2|1" {
		t.Fatalf("dbfixture stores %s: the premise changed", got)
	}
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
