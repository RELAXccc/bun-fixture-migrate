package fixturemigrate

import (
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// testTables is the same schema the fixture tests use, as the catalog would
// report it.
func testTables() map[string]*dbschema.Table {
	col := func(pos int, name, typ, def string, nullable bool) dbschema.Column {
		return dbschema.Column{Name: name, Position: pos, Type: typ, Default: def, Nullable: nullable}
	}
	return map[string]*dbschema.Table{
		"public.currencies": {Schema: "public", Name: "currencies", PrimaryKey: []string{"id"},
			Uniques: [][]string{{"id"}, {"code"}},
			Columns: []dbschema.Column{
				col(1, "id", "int8", "", false),
				col(2, "code", "text", "", false),
				col(3, "symbol", "text", "", false),
			}},
		"public.plans": {Schema: "public", Name: "plans", PrimaryKey: []string{"id"},
			Uniques: [][]string{{"id"}, {"name"}},
			ForeignKeys: []dbschema.ForeignKey{{Columns: []string{"currency_id"},
				RefSchema: "public", RefTable: "currencies", RefColumns: []string{"id"}}},
			Columns: []dbschema.Column{
				col(1, "id", "int8", "nextval('plans_id_seq'::regclass)", false),
				col(2, "name", "text", "", false),
				col(3, "currency_id", "int8", "", false),
				col(4, "price_cents", "int8", "0", false),
				// The hazard: a non-zero default on a column a fixture row can
				// legitimately want to be zero.
				col(5, "seats", "int8", "1", false),
				col(6, "price_per_seat_cents", "int8", "0", false),
				col(7, "note", "text", "", true),
			}},
		"public.features": {Schema: "public", Name: "features", PrimaryKey: []string{"id"},
			Uniques: [][]string{{"id"}, {"plan_id", "code"}},
			ForeignKeys: []dbschema.ForeignKey{{Columns: []string{"plan_id"},
				RefSchema: "public", RefTable: "plans", RefColumns: []string{"id"}}},
			Columns: []dbschema.Column{
				col(1, "id", "int8", "nextval('features_id_seq'::regclass)", false),
				col(2, "plan_id", "int8", "", false),
				col(3, "code", "text", "", false),
				col(4, "quota", "int8", "0", false),
				col(5, "enabled", "bool", "false", false),
			}},
	}
}

// An export has to reproduce the state it was taken from. This is that, without
// a database: take a fixture file as the state, write it out again, read it
// back, and compare the two. Anything the export gets wrong — a lost
// reference, a number turned into a string, a row order that makes a reference
// unresolvable — shows up as a difference.
func TestExportRoundTripsThroughTheFixtureLoader(t *testing.T) {
	cfg := testConfig(t)
	state := snap(t, cfg, base, "the database")
	for _, model := range state.Order {
		for _, e := range state.Entries[model] {
			if e.Anchor == "" {
				e.Anchor = anchorOf(e.Key)
			}
		}
	}
	data, err := Export(cfg, state, testTables(), []string{"a test"})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	back := snap(t, cfg, string(data), "the export")
	res, err := Compute(cfg, state, back)
	if err != nil {
		t.Fatalf("Compute: %v\n%s", err, data)
	}
	if len(res.Changes) != 0 || len(res.Refusals) != 0 {
		t.Fatalf("the export does not reproduce its source: %+v / %+v\n%s", res.Changes, res.Refusals, data)
	}
	text := string(data)
	for _, want := range []string{
		"# a test",
		"- model: Currency",
		"    - _id: eur",
		"      id: 1",
		"      code: \"EUR\"",
		"      currency_id: '{{ $.Currency.eur.ID }}'",
		"      price_cents: 2000",
		"      enabled: true",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the export is missing %q:\n%s", want, text)
		}
	}
	// Dependency order: nothing may be written before the row it points at.
	if strings.Index(text, "- model: Plan") < strings.Index(text, "- model: Currency") {
		t.Errorf("Currency has to come before Plan:\n%s", text)
	}
}

// An export that writes a zero into a column whose default is not zero does not
// describe the database it came from: loading it back stores the default. The
// export says so in the file, on the line it happened.
func TestExportMarksTheZeroDefaultHazard(t *testing.T) {
	cfg := testConfig(t)
	state := snap(t, cfg, base, "the database")
	for _, model := range state.Order {
		for _, e := range state.Entries[model] {
			e.Anchor = anchorOf(e.Key)
		}
	}
	data, err := Export(cfg, state, testTables(), nil)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	// price_cents is 0 all over the file and its default is 0, so there is
	// nothing wrong with it. Nothing is marked yet.
	if strings.Contains(text, "ROUND-TRIP HAZARD") {
		t.Fatalf("a zero against a zero default is not a hazard:\n%s", text)
	}
	// seats defaults to 1. A row that really holds 0 cannot be written back.
	state.Entries["Plan"][1].Cells["seats"] = fixturechange.Lit("0")
	data, err = Export(cfg, state, testTables(), nil)
	if err != nil {
		t.Fatal(err)
	}
	text = string(data)
	if got := strings.Count(text, "ROUND-TRIP HAZARD"); got != 1 {
		t.Fatalf("expected the zero to be marked, got %d:\n%s", got, text)
	}
	if !strings.Contains(text, "seats: 0  # ROUND-TRIP HAZARD: the column defaults to 1") {
		t.Fatalf("the mark belongs on the line it is about:\n%s", text)
	}
}

// The same defect seen from the fixture file's side: the file says 0, the
// database will hold 1, and nothing but this lint says so.
func TestLintZeroDefaults(t *testing.T) {
	cfg := testConfig(t)
	state := snap(t, cfg, replace(t, base, "      seats: 10\n", "      seats: 0\n"), "fixture.yml")
	LintZeroDefaults(cfg, state, testTables())
	var found []Finding
	for _, f := range state.Findings {
		if f.Kind == FindingZeroDefault {
			found = append(found, f)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected one finding, got %+v", state.Findings)
	}
	if !strings.Contains(found[0].Detail, "seats is 0") || !strings.Contains(found[0].Detail, "defaults to 1") {
		t.Fatalf("the finding has to say what will happen: %q", found[0].Detail)
	}
	// price_cents is 0 in the file and defaults to 0, which is fine.
	if strings.Contains(found[0].Detail, "price_cents") {
		t.Fatalf("a zero against a zero default is not a hazard: %q", found[0].Detail)
	}
}

func TestLintColumnsReportsAColumnTheTableDoesNotHave(t *testing.T) {
	cfg := testConfig(t)
	state := snap(t, cfg, replace(t, base, "      seats: 1\n", "      seats: 1\n      trial_days: 14\n"), "fixture.yml")
	LintColumns(cfg, state, testTables())
	if len(state.Findings) != 1 || state.Findings[0].Row != "trial_days" {
		t.Fatalf("expected trial_days to be reported, got %+v", state.Findings)
	}
}

func TestYamlScalarKeepsTheTypeItReadsBackAs(t *testing.T) {
	for _, tc := range []struct{ text, typ, want string }{
		{"5", "int8", "5"},
		{"5", "text", `"5"`},
		{"true", "bool", "true"},
		{"t", "bool", "true"},
		{"yes", "text", `"yes"`},
		{"1.0", "float8", "1.0"},
		{"", "text", `""`},
		{"a\"b", "text", `"a\"b"`},
		{"not a number", "int8", `"not a number"`},
	} {
		if got := yamlScalar(tc.text, tc.typ); got != tc.want {
			t.Errorf("yamlScalar(%q, %q) = %s, want %s", tc.text, tc.typ, got, tc.want)
		}
	}
}

func TestCamelIsTheInverseOfBunsColumnNaming(t *testing.T) {
	for _, tc := range []struct{ col, field string }{
		{"id", "ID"}, {"plan_id", "PlanID"}, {"group_name", "GroupName"}, {"code", "Code"},
	} {
		if got := camel(tc.col); got != tc.field {
			t.Errorf("camel(%q) = %q, want %q", tc.col, got, tc.field)
		}
		if got := underscore(tc.field); got != tc.col {
			t.Errorf("underscore(%q) = %q, want %q", tc.field, got, tc.col)
		}
	}
}

func TestAnchorsAreReadableAndUnique(t *testing.T) {
	taken := map[string]bool{}
	key := func(v string) fixturechange.Values { return fixturechange.Values{"name": fixturechange.Lit(v)} }
	if got := uniqueAnchor(anchorOf(key("Team Plan")), "1", taken); got != "team_plan" {
		t.Fatalf("got %q", got)
	}
	if got := uniqueAnchor(anchorOf(key("Team Plan")), "7", taken); got != "team_plan_7" {
		t.Fatalf("a second row with the same name takes its id, got %q", got)
	}
	if got := uniqueAnchor(anchorOf(key("!!!")), "", taken); got != "row" {
		t.Fatalf("a key with nothing usable in it still needs an anchor, got %q", got)
	}
}

// Everything this tool puts into a query as an identifier comes from the
// configuration or the catalog, and both of them go through here first.
func TestQuoteIdent(t *testing.T) {
	for in, want := range map[string]string{
		"items":   `"items"`,
		"_x1":     `"_x1"`,
		"a$b":     `"a$b"`,
		`a" OR 1`: "",
		"":        "",
		"1st":     "",
		"a b":     "",
	} {
		got, err := quoteIdent(in)
		if want == "" {
			if err == nil {
				t.Errorf("quoteIdent(%q) should have failed, got %q", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("quoteIdent(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for in, want := range map[string]string{
		"items":             `"items"`,
		"master.items":      `"master"."items"`,
		"a.b.c":             `"a"."b"."c"`,
		"master.items; DRO": "",
		".items":            "",
	} {
		got, err := quoteQualified(in)
		if want == "" {
			if err == nil {
				t.Errorf("quoteQualified(%q) should have failed, got %q", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("quoteQualified(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// A value that YAML would read back as something other than the string it is,
// or not read back at all, has to be escaped on the way out.
func TestYamlStringEscapesWhatWouldNotComeBack(t *testing.T) {
	for in, want := range map[string]string{
		"plain":      `"plain"`,
		`say "hi"`:   `"say \"hi\""`,
		`back\slash`: `"back\\slash"`,
		"two\nlines": `"two\nlines"`,
		"tab\there":  `"tab\there"`,
		"bell\a":     `"bell\x07"`,
		"€":          `"€"`,
	} {
		if got := yamlString(in); got != want {
			t.Errorf("yamlString(%q) = %s, want %s", in, got, want)
		}
	}
}

// An anchor is written plain where YAML reads it as the string it is, and
// quoted where it would read as a boolean or a number.
func TestYamlAnchorQuotesWhatYamlWouldReadAsSomethingElse(t *testing.T) {
	for in, want := range map[string]string{
		"eur":   "eur",
		"api_2": "api_2",
		"no":    `"no"`,
		"yes":   `"yes"`,
		"true":  `"true"`,
		"null":  `"null"`,
		"2026":  `"2026"`,
		"":      `""`,
	} {
		if got := yamlAnchor(in); got != want {
			t.Errorf("yamlAnchor(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestFindingsByKindGroupsAndSorts(t *testing.T) {
	grouped := FindingsByKind([]Finding{
		{Kind: FindingZeroDefault, Model: "Plan", Row: "name=team"},
		{Kind: FindingDuplicateKey, Model: "Feature", Row: "code=sso"},
		{Kind: FindingDuplicateKey, Model: "Feature", Row: "code=api"},
	})
	if len(grouped) != 2 {
		t.Fatalf("expected two kinds, got %d", len(grouped))
	}
	dupes := grouped[FindingDuplicateKey]
	if len(dupes) != 2 || dupes[0].Row != "code=api" {
		t.Fatalf("findings of one kind are sorted by model and row: %+v", dupes)
	}
}

// A value dbfixture would read as a template is written as a template whose
// only action is that value as a string literal, which dbfixture evaluates to
// the value itself.
func TestExportWritesTemplateLikeTextAsALiteralTemplate(t *testing.T) {
	cfg := testConfig(t)
	state := snap(t, cfg, base, "the database")
	for _, e := range state.Entries["Currency"] {
		e.Cells["symbol"] = fixturechange.Lit(`Hello {{ .Name }} "x"`)
	}
	out, err := Export(cfg, state, testTables(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `symbol: "{{ \"Hello {{ .Name }} \\\"x\\\"\" }}"`) {
		t.Fatalf("the value is not written as a literal template:\n%s", out)
	}
}

// nullTables is testTables with a default on the nullable note column.
func nullTables() map[string]*dbschema.Table {
	tables := testTables()
	for i, c := range tables["public.plans"].Columns {
		if c.Name == "note" {
			tables["public.plans"].Columns[i].Default = "'none'::text"
		}
	}
	return tables
}

func TestLintNullDefaults(t *testing.T) {
	cfg := testConfig(t)
	text := replace(t, base, "      seats: 10\n", "      seats: 10\n      note: ~\n")
	state := snap(t, cfg, text, "fixture.yml")
	LintNullDefaults(cfg, state, testTables())
	if len(state.Findings) != 0 {
		t.Fatalf("a null into a column without a default is stored as NULL: %+v", state.Findings)
	}
	LintNullDefaults(cfg, state, nullTables())
	if len(state.Findings) != 1 || state.Findings[0].Kind != FindingNullDefault {
		t.Fatalf("expected the null to be reported, got %+v", state.Findings)
	}
	if !strings.Contains(state.Findings[0].Detail, "note is null, but the column defaults to none") {
		t.Fatalf("the finding has to say what will happen: %q", state.Findings[0].Detail)
	}
	if cfg.FindingMode(FindingNullDefault) != ModeError {
		t.Fatal("the null default is strict by default")
	}
}

func TestExportMarksTheNullDefaultHazard(t *testing.T) {
	cfg := testConfig(t)
	state := snap(t, cfg, replace(t, base, "      seats: 10\n", "      seats: 10\n      note: ~\n"), "the database")
	for _, model := range state.Order {
		for _, e := range state.Entries[model] {
			e.Anchor = anchorOf(e.Key)
		}
	}
	data, err := Export(cfg, state, nullTables(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "note: ~  # ROUND-TRIP HAZARD: the column defaults to none") {
		t.Fatalf("the null has to be marked on its line:\n%s", data)
	}
}

// A generated column cannot be written by anybody, so a fixture file that
// writes one does not load.
func TestLintColumnsReportsAGeneratedColumn(t *testing.T) {
	cfg := testConfig(t)
	tables := testTables()
	for i, c := range tables["public.plans"].Columns {
		if c.Name == "seats" {
			tables["public.plans"].Columns[i].Generated = true
		}
	}
	state := snap(t, cfg, base, "fixture.yml")
	LintColumns(cfg, state, tables)
	if len(state.Findings) != 1 || !strings.Contains(state.Findings[0].Detail, "generates it") {
		t.Fatalf("expected seats to be reported, got %+v", state.Findings)
	}
}

// Each model goes back into the file that holds it; one that no file holds
// yet goes into the last; a file left without a model is an empty list, which
// dbfixture loads.
func TestExportFilesKeepsEachModelInItsFile(t *testing.T) {
	cfg := testConfig(t)
	state := snap(t, cfg, base, "the database")
	for _, model := range state.Order {
		for _, e := range state.Entries[model] {
			e.Anchor = anchorOf(e.Key)
		}
	}
	current := []FixtureFile{
		{Path: "a.yml", Data: []byte("- model: Currency\n  rows: []\n")},
		{Path: "b.yml", Data: []byte("- model: Plan\n  rows: []\n")},
		{Path: "c.yml"},
	}
	out, err := ExportFiles(cfg, state, testTables(), nil, current)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"model: Currency", "model: Plan", "model: Feature"} {
		if !strings.Contains(string(out[i]), want) || strings.Count(string(out[i]), "- model:") != 1 {
			t.Fatalf("%s:\n%s", current[i].Path, out[i])
		}
	}
	// A model pointing at one of a later file cannot be loaded.
	current[0].Data = []byte("- model: Plan\n  rows: []\n")
	current[1].Data = []byte("- model: Currency\n  rows: []\n")
	if _, err := ExportFiles(cfg, state, testTables(), nil, current); err == nil ||
		!strings.Contains(err.Error(), "Plan in a.yml points at Currency, which is in b.yml") {
		t.Fatalf("expected the order to be refused: %v", err)
	}
	// An empty file is written as an empty list.
	current = []FixtureFile{{Path: "a.yml", Data: []byte(base)}, {Path: "empty.yml", Data: []byte("- model: Nothing\n  rows: []\n")}}
	out, err = ExportFiles(cfg, state, testTables(), nil, current)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out[1])) != "[]" {
		t.Fatalf("got %q", out[1])
	}
}

// treeConfig and treeTables are a category tree: every row may point at a
// parent in the same table.
func treeConfig(t *testing.T) *Config {
	t.Helper()
	cfg := &Config{Models: map[string]*Model{
		"Node": {Table: "nodes", References: map[string]string{"parent_id": "Node"}},
	}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func treeTables() map[string]*dbschema.Table {
	return map[string]*dbschema.Table{"public.nodes": {Schema: "public", Name: "nodes", PrimaryKey: []string{"id"},
		Columns: []dbschema.Column{
			{Name: "id", Position: 1, Type: "int8"},
			{Name: "name", Position: 2, Type: "text"},
			{Name: "parent_id", Position: 3, Type: "int8", Nullable: true},
		}}}
}

// treeState is a tree as a database returns it, in id order, where a root
// was added after the leaves that hang from it.
func treeState(rows ...[3]string) *Snapshot {
	s := &Snapshot{Source: "the database", Order: []string{"Node"}, Entries: map[string][]*Entry{},
		Columns: map[string][]string{"Node": {"name", "parent_id"}}}
	for _, r := range rows {
		parent := fixturechange.Null()
		if r[2] != "" {
			parent = fixturechange.RefTo("Node", r[2])
		}
		key := fixturechange.Values{"name": fixturechange.Lit(r[1])}
		s.Entries["Node"] = append(s.Entries["Node"], &Entry{Anchor: r[1], ID: r[0], Key: key,
			KeyStr: keyString("Node", key), Cells: fixturechange.Values{"name": key["name"], "parent_id": parent}})
	}
	return s
}

// dbfixture resolves a template against the rows above it, so a child written
// before its parent cannot be loaded. The export puts parents first and keeps
// the id order otherwise.
func TestExportWritesAParentBeforeItsChildren(t *testing.T) {
	cfg := treeConfig(t)
	state := treeState(
		[3]string{"1", "leaf", "branch"},
		[3]string{"2", "other", ""},
		[3]string{"3", "twig", "leaf"},
		[3]string{"5", "branch", "root"},
		[3]string{"7", "root", ""},
	)
	data, err := Export(cfg, state, treeTables(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "      name: ") {
			order = append(order, strings.Trim(strings.TrimPrefix(line, "      name: "), `"`))
		}
	}
	if got := strings.Join(order, ","); got != "other,root,branch,leaf,twig" {
		t.Fatalf("expected parents first and id order otherwise, got %s\n%s", got, data)
	}
	back := snap(t, cfg, string(data), "the export")
	res, err := Compute(cfg, state, back)
	if err != nil {
		t.Fatalf("the export does not load: %v\n%s", err, data)
	}
	if len(res.Changes) != 0 || len(res.Refusals) != 0 {
		t.Fatalf("the export does not reproduce its source: %+v / %+v\n%s", res.Changes, res.Refusals, data)
	}

	// A file that already loads keeps its order.
	again, err := Export(cfg, back, treeTables(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(data) {
		t.Fatalf("exporting the export changed it:\n%s\n---\n%s", data, again)
	}
}

// Rows pointing at each other in a circle load in no order at all.
func TestExportRefusesRowsThatPointAtEachOtherInACircle(t *testing.T) {
	_, err := Export(treeConfig(t), treeState(
		[3]string{"1", "a", "b"},
		[3]string{"2", "b", "a"},
		[3]string{"3", "c", ""},
	), treeTables(), nil)
	if err == nil || !strings.Contains(err.Error(), "Node/name=a; Node/name=b") ||
		strings.Contains(err.Error(), "name=c") {
		t.Fatalf("expected the two rows of the circle to be named, got %v", err)
	}
}

// A null into a NOT NULL column without a default is never stored as written:
// bun writes a plain field's zero, and a pointer field fails the insert.
func TestLintNullDefaultsReportsANullTheColumnCannotHold(t *testing.T) {
	cfg := testConfig(t)
	state := snap(t, cfg, base, "fixture.yml")
	state.Entries["Plan"][1].Cells["currency_id"] = fixturechange.Null()
	LintNullDefaults(cfg, state, testTables())
	var got []string
	for _, f := range state.Findings {
		got = append(got, string(f.Kind)+": "+f.Row+": "+f.Detail)
	}
	if len(got) != 1 || !strings.HasPrefix(got[0], "invalid value: Plan/name=team: currency_id is null, but the column is NOT NULL and has no default") {
		t.Fatalf("expected one invalid value, got %q", got)
	}
}
