package fixturemigrate

import (
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// softConfig is testConfig with soft_delete on the plans, whose deletes it
// allows.
func softConfig(t *testing.T) *Config {
	t.Helper()
	return ownedConfig(t, func(cfg *Config) {
		cfg.Models["Plan"].SoftDelete = "deleted_at"
		cfg.Models["Plan"].Deletes = DeleteAllow
	})
}

// softTables is testTables with a nullable deleted_at in plans.
func softTables() map[string]*dbschema.Table {
	tables := testTables()
	plans := *tables["public.plans"]
	plans.Columns = append(append([]dbschema.Column{}, plans.Columns...),
		dbschema.Column{Name: "deleted_at", Position: 8, Type: "timestamptz", FullType: "timestamp with time zone",
			Nullable: true})
	tables["public.plans"] = &plans
	return tables
}

func TestSoftDeleteIsAColumnOfItsOwn(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(m *Model)
		want   string
	}{
		"not a name":   {func(m *Model) { m.SoftDelete = "deleted at" }, "not a plain column name"},
		"the id":       {func(m *Model) { m.SoftDelete = "id" }, "is the id"},
		"in the key":   {func(m *Model) { m.SoftDelete = "name" }, "natural key"},
		"the ref":      {func(m *Model) { m.Ref = "slug"; m.SoftDelete = "slug" }, "ref column"},
		"ignored":      {func(m *Model) { m.Ignore = []string{"deleted_at"} }, "is in ignore"},
		"derived":      {func(m *Model) { m.Derived = []string{"deleted_at"} }, "is in derived"},
		"insert_only":  {func(m *Model) { m.InsertOnly = []string{"deleted_at"} }, "is in insert_only"},
		"defaults":     {func(m *Model) { m.Defaults = Defaults{"deleted_at": NullDefault} }, "has a default"},
		"a reference":  {func(m *Model) { m.References = map[string]string{"deleted_at": "Currency"} }, "is a reference"},
		"cascade":      {func(m *Model) { m.Deletes = DeleteCascade }, "reaches no row through a foreign key"},
		"mode insert":  {func(m *Model) { m.Deletes = ""; m.Mode = OwnInsert }, "mode insert"},
		"in the where": {func(m *Model) { m.Where = "Deleted_At IS NULL" }, "drop it from where"},
		"quoted in the where": {func(m *Model) { m.Where = `"deleted_at" IS NULL AND tenant_id IS NULL` },
			"drop it from where"},
	} {
		cfg := testConfig(t)
		m := cfg.Models["Plan"]
		m.Deletes = DeleteAllow
		m.SoftDelete = "deleted_at"
		tc.change(m)
		err := cfg.Prepare()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", name, err, tc.want)
		}
	}
	// A cascade the policy block gives every model is no cascade of a
	// model that soft-deletes.
	cfg := ownedConfig(t, func(cfg *Config) {
		cfg.Policy.Deletes = DeleteCascade
		cfg.Models["Plan"].Deletes = ""
		cfg.Models["Plan"].SoftDelete = "deleted_at"
	})
	if tables := tablesFor(cfg, []fixturechange.Change{{Model: "Plan"}}); tables["Plan"].Cascade ||
		tables["Plan"].SoftDelete != "deleted_at" {
		t.Fatalf("tables: %+v", tables["Plan"])
	}
}

func TestMentions(t *testing.T) {
	for where, want := range map[string]bool{
		"deleted_at IS NULL":                        true,
		"p.DELETED_AT is null":                      true,
		`"deleted_at" IS NULL`:                      true,
		`"Deleted_At" IS NULL`:                      false,
		"tenant_id IS NULL":                         false,
		"note <> 'deleted_at'":                      false,
		"note <> E'it\\'s deleted_at'":              false,
		"x = $$deleted_at$$":                        false,
		"tenant_id IS NULL -- not deleted_at":       false,
		"tenant_id IS NULL /* deleted_at */":        false,
		"was_deleted_at IS NULL":                    false,
		"deleted_at_old IS NULL":                    false,
		"tenant_id IS NULL AND\ndeleted_at < now()": true,
	} {
		if got := mentions(where, "deleted_at"); got != want {
			t.Errorf("mentions(%q) = %v, want %v", where, got, want)
		}
	}
}

// A fixture row that sets the column is seeded soft-deleted by dbfixture: it
// is no master data, and a row naming it is an error. One that sets the zero
// time means one thing to one Go field and another to another.
func TestAFixtureRowWithItsSoftDeleteSetIsNoMasterData(t *testing.T) {
	cfg := softConfig(t)
	text := replace(t, base, "      price_cents: 2000\n", "      price_cents: 2000\n      deleted_at: 2026-02-01T00:00:00Z\n")
	// The features of team go with it; the file names team no more.
	text = text[:strings.Index(text, "    - plan_id: '{{ $.Plan.team.ID }}'")]
	s := snap(t, cfg, text, "head")
	if len(s.Entries["Plan"]) != 1 || s.Entries["Plan"][0].Key["name"].Lit != "free" || s.softDeleted["Plan"] != 1 {
		t.Fatalf("plans: %+v, %v", s.Entries["Plan"], s.softDeleted)
	}
	for _, col := range s.Columns["Plan"] {
		if col == "deleted_at" {
			t.Fatalf("the column is never compared: %v", s.Columns["Plan"])
		}
	}
	res, err := Compute(cfg, snap(t, cfg, base[:strings.Index(base, "    - plan_id: '{{ $.Plan.team.ID }}'")], "base"), s)
	if err != nil {
		t.Fatal(err)
	}
	if k := kinds(res, "Plan"); k[fixturechange.Delete] != 1 || len(res.Changes) != 1 {
		t.Fatalf("a row going from live to soft-deleted is a delete: %+v", res.Changes)
	}
	if got := strings.Join(res.Summary(), "|"); got != "Plan: 1 soft delete" {
		t.Fatalf("summary: %q", got)
	}
	if lines := strings.Join(res.LeftAloneLines(), "|"); lines !=
		"Plan: 1 row of head is soft-deleted (the soft_delete column set) and not master data" {
		t.Fatalf("left alone: %q", lines)
	}
	if res.Tables["Plan"].SoftDelete != "deleted_at" {
		t.Fatalf("tables: %+v", res.Tables)
	}

	// A row naming a soft-deleted row of the file, by template or by id.
	named := replace(t, base, "      price_cents: 2000\n", "      price_cents: 2000\n      deleted_at: 2026-02-01T00:00:00Z\n")
	if _, err := FixtureSnapshot(cfg, doc(t, named), "head"); err == nil ||
		!strings.Contains(err.Error(), "points at a row of Plan that is soft-deleted (deleted_at = 2026-02-01T00:00:00Z)") {
		t.Fatalf("a row naming a soft-deleted row: %v", err)
	}
	byID := replace(t, named, "plan_id: '{{ $.Plan.team.ID }}'", "plan_id: 2")
	if _, err := FixtureSnapshot(cfg, doc(t, byID), "head"); err == nil || !strings.Contains(err.Error(), "soft-deleted") {
		t.Fatalf("a row naming a soft-deleted row by its id: %v", err)
	}

	// ~ and an absent column are live; the zero time is ambiguous.
	live := replace(t, base, "      price_cents: 2000\n", "      price_cents: 2000\n      deleted_at: ~\n")
	if s := snap(t, cfg, live, "head"); len(s.Entries["Plan"]) != 2 || len(s.Findings) != 0 {
		t.Fatalf("~ is live: %+v %+v", s.Entries["Plan"], s.Findings)
	}
	zero := replace(t, base, "      price_cents: 2000\n", "      price_cents: 2000\n      deleted_at: 0001-01-01T00:00:00Z\n")
	s = snap(t, cfg, zero, "head")
	if len(s.Entries["Plan"]) != 2 || len(s.Findings) != 1 || s.Findings[0].Kind != FindingAmbiguousValue ||
		!strings.Contains(s.Findings[0].Detail, "the zero time") {
		t.Fatalf("the zero time: %+v %+v", s.Entries["Plan"], s.Findings)
	}
}

func TestZeroTime(t *testing.T) {
	for text, want := range map[string]bool{
		"0001-01-01":                     true,
		"0001-01-01T00:00:00Z":           true,
		"0001-01-01 00:00:00+00":         true,
		"0001-01-01 00:00:00+00:00":      true,
		"0001-01-01T00:00:00.000000000Z": true,
		"0001-01-01T01:00:00+01:00":      true,
		"0001-01-01T00:00:01Z":           false,
		"2026-01-01":                     false,
		"0001-01-02":                     false,
		"soon":                           false,
	} {
		if got := zeroTime(text); got != want {
			t.Errorf("zeroTime(%q) = %v, want %v", text, got, want)
		}
	}
}

// The database's side reads live rows only, and the soft-deleted ones newest
// first, apart.
func TestSelectQueryOfASoftDeleteModel(t *testing.T) {
	cfg := softConfig(t)
	cfg.Models["Plan"].Where = "archived_at IS NULL"
	m, table := cfg.Models["Plan"], softTables()["public.plans"]
	query, _, err := selectQuery(cfg, m, table, []string{"name"})
	if err != nil {
		t.Fatal(err)
	}
	const want = `SELECT ("id")::text, ("name")::text FROM "public"."plans" WHERE (archived_at IS NULL` + "\n" +
		`) AND "public"."plans"."deleted_at" IS NULL ORDER BY "public"."plans"."id", "public"."plans"."name"`
	if query != want {
		t.Fatalf("live:\n%s\nwant\n%s", query, want)
	}
	query, _, err = rowsQuery(cfg, m, table, []string{"name", "deleted_at"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, `"deleted_at" IS NOT NULL ORDER BY "public"."plans"."deleted_at" DESC, "public"."plans"."id"`) {
		t.Fatalf("soft-deleted: %s", query)
	}
	cols, err := readColumns(m, table, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range cols {
		if col == "deleted_at" {
			t.Fatalf("the column is never read as a value, nor exported: %v", cols)
		}
	}
}

// Only a nullable timestamp column without a default works.
func TestSoftDeleteProblem(t *testing.T) {
	cfg := softConfig(t)
	m := cfg.Models["Plan"]
	column := func(c dbschema.Column) *dbschema.Table {
		table := *softTables()["public.plans"]
		table.Columns = append([]dbschema.Column{}, table.Columns[:7]...)
		c.Name = "deleted_at"
		table.Columns = append(table.Columns, c)
		return &table
	}
	for name, tc := range map[string]struct {
		table *dbschema.Table
		want  string
	}{
		"good":        {column(dbschema.Column{Type: "timestamptz", Nullable: true}), ""},
		"no zone":     {column(dbschema.Column{Type: "timestamp", Nullable: true}), ""},
		"null":        {column(dbschema.Column{Type: "timestamptz", Nullable: true, Default: "NULL::timestamp with time zone"}), ""},
		"missing":     {testTables()["public.plans"], "does not have"},
		"int64":       {column(dbschema.Column{Type: "int8", FullType: "bigint", Nullable: true}), "int64 soft-delete field"},
		"not null":    {column(dbschema.Column{Type: "timestamptz", Nullable: false}), "NOT NULL"},
		"a default":   {column(dbschema.Column{Type: "timestamptz", Nullable: true, Default: "now()"}), "born soft-deleted"},
		"a zero time": {column(dbschema.Column{Type: "timestamptz", Nullable: true, Default: "'0001-01-01 00:00:00+00'::timestamp with time zone"}), "Drop the default"},
	} {
		got := softDeleteProblem(m, tc.table)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
	s := snap(t, cfg, base, "head")
	LintSoftDelete(cfg, s, testTables())
	if len(s.Findings) != 1 || s.Findings[0].Kind != FindingSoftDelete || cfg.ModeOf(s.Findings[0]) != ModeError {
		t.Fatalf("findings: %+v", s.Findings)
	}
}

// The generated file says which tables soft-delete, and reads back.
func TestRenderWritesTheSoftDeleteColumn(t *testing.T) {
	cfg := softConfig(t)
	res := computeWith(t, cfg, base, base[:strings.Index(base, "    - _id: team")]+
		"- model: Feature\n  rows:\n    - plan_id: '{{ $.Plan.free.ID }}'\n      code: api\n      quota: 100\n")
	src, err := Render(cfg, "retire team", "20261001000000", res)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `"Plan":     {Name: "plans", ID: "id", Key: "name", SoftDelete: "deleted_at"},`) {
		t.Fatalf("the Tables entry:\n%s", src)
	}
	set, ok, err := ReadChangeSet(src)
	if err != nil || !ok {
		t.Fatalf("%v\n%s", err, src)
	}
	if set.Tables["Plan"].SoftDelete != "deleted_at" || set.Tables["Plan"].Cascade {
		t.Fatalf("read back: %+v", set.Tables["Plan"])
	}
	if err := fixtureapply.Validate(set); err != nil {
		t.Fatal(err)
	}
}

// check says of a row the files hold that the database holds it soft-deleted.
func TestCheckSaysARowIsSoftDeletedThere(t *testing.T) {
	cfg := softConfig(t)
	database := snap(t, cfg, base, "the database")
	database.database = true
	head := snap(t, cfg, base, "fixtures/fixture.yml")
	// The database holds team soft-deleted, and two rows of history.
	var live []*Entry
	for _, e := range database.Entries["Plan"] {
		if e.Key["name"].Lit != "team" {
			live = append(live, e)
		}
	}
	database.Entries["Plan"] = live
	database.Entries["Feature"] = database.Entries["Feature"][:1]
	head.Entries["Feature"] = head.Entries["Feature"][:1]
	database.softDeleted = map[string]int{"Plan": 3}
	database.deleted = map[string]map[string]string{"Plan": {
		keyString("Plan", fixturechange.Values{"name": fixturechange.Lit("team")}): "2026-02-01T00:00:00Z"}}
	res, err := Check(cfg, database, head)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Join(res.Lines(), "\n")
	for _, want := range []string{
		"In the fixture file, not in the database:\n  Plan name=team (soft-deleted there at 2026-02-01T00:00:00Z; a migration restores it)",
		"Plan: 3 rows soft-deleted in the database, which soft_delete leaves out of the master data",
	} {
		if !strings.Contains(lines, want) {
			t.Fatalf("check is missing %q:\n%s", want, lines)
		}
	}
	report := newCheckReport(cfg, res)
	data, err := report.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"soft_deleted":"2026-02-01T00:00:00Z"`, `"soft_deleted":3`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("check -json is missing %s:\n%s", want, data)
		}
	}
}
