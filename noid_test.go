package fixturemigrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// A table whose primary key, id, is a plan's id: scaffold writes id: none
// and keys the model on the reference, and a table pointing at it points at
// the plan whose id it holds. Before, the model was written without an id,
// which made "id" the id, and the configuration was refused.
func TestScaffoldWritesIDNoneForAnIDThatIsAReference(t *testing.T) {
	tables := testTables()
	tables["public.plan_details"] = &dbschema.Table{Schema: "public", Name: "plan_details", PrimaryKey: []string{"id"},
		Uniques: [][]string{{"id"}},
		Columns: []dbschema.Column{{Name: "id", Position: 1, Type: "int8"}, {Name: "blurb", Position: 2, Type: "text"}},
		ForeignKeys: []dbschema.ForeignKey{{Columns: []string{"id"},
			RefSchema: "public", RefTable: "plans", RefColumns: []string{"id"}}},
	}
	tables["public.detail_notes"] = &dbschema.Table{Schema: "public", Name: "detail_notes", PrimaryKey: []string{"id"},
		Uniques: [][]string{{"id"}, {"name"}},
		Columns: []dbschema.Column{{Name: "id", Position: 1, Type: "int8"}, {Name: "name", Position: 2, Type: "text"},
			{Name: "detail_id", Position: 3, Type: "int8"}},
		ForeignKeys: []dbschema.ForeignKey{{Columns: []string{"detail_id"},
			RefSchema: "public", RefTable: "plan_details", RefColumns: []string{"id"}}},
	}
	data := Scaffold(tables, nil, "public", ScaffoldOptions{})
	text := string(data)
	model := func(table string) string {
		start := strings.Index(text, "    table: "+table+"\n")
		if start < 0 {
			t.Fatalf("no model for %s:\n%s", table, text)
		}
		m := text[start:]
		if end := strings.Index(m, "\n\n"); end > 0 {
			m = m[:end+1]
		}
		return m
	}
	if m := model("plan_details"); !strings.Contains(m, "    id: none\n") || !strings.Contains(m, "    key: [id]\n") ||
		!strings.Contains(m, "      id: Plan\n") || strings.Contains(m, "serial") {
		t.Fatalf("expected the details keyed on their plan, without an id of their own:\n%s", m)
	}
	if m := model("detail_notes"); !strings.Contains(m, "      detail_id: Plan\n") ||
		!strings.Contains(m, "# detail_id points at plan_details, whose rows are keyed by the id of a Plan") {
		t.Fatalf("expected the note pointing at the plan whose id it holds:\n%s", m)
	}
	path := filepath.Join(t.TempDir(), "fixture-migrate.yml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("the scaffold has to load as a configuration: %v", err)
	}
	if m := cfg.Models["PlanDetail"]; m == nil || m.ID != "" || !m.hasNoID() {
		t.Fatalf("got %+v", m)
	}
}

// A model without an id is diffed by its key, the reference: an update, an
// insert and a delete, none of which carries an id, in a change set the run
// time takes, after the plan it points at.
func TestAModelWithoutAnIDMigratesByItsKey(t *testing.T) {
	cfg := &Config{Models: map[string]*Model{
		"Plan":       {Table: "plans", Serial: true},
		"PlanDetail": {Table: "plan_details", ID: NoID, Key: []string{"id"}, References: map[string]string{"id": "Plan"}},
	}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	plans := "- model: Plan\n  rows:\n    - {_id: free, id: 1, name: free}\n    - {_id: pro, id: 2, name: pro}\n" +
		"    - {_id: team, id: 3, name: team}\n- model: PlanDetail\n  rows:\n"
	old := plans + "    - {id: '{{ $.Plan.free.ID }}', blurb: Free!}\n    - {id: '{{ $.Plan.team.ID }}', blurb: Team!}\n"
	next := plans + "    - {id: '{{ $.Plan.free.ID }}', blurb: 'Free, forever!'}\n    - {id: '{{ $.Plan.pro.ID }}', blurb: Pro!}\n"
	s := snap(t, cfg, next, "new")
	for _, e := range s.Entries["PlanDetail"] {
		if e.ID != "" || e.Key["id"].Ref == nil {
			t.Fatalf("the key is the reference, and there is no id: %+v", e)
		}
	}
	res := computeWith(t, cfg, old, next)
	if len(res.Refusals) != 0 || len(res.Warnings) != 0 {
		t.Fatalf("%+v %+v", res.Refusals, res.Warnings)
	}
	if got := kindsOf(res); got != "delete PlanDetail/id=Plan(team); update PlanDetail/id=Plan(free); insert PlanDetail/id=Plan(pro)" {
		t.Fatalf("got %s", got)
	}
	for _, c := range res.Changes {
		if c.ID != "" || c.New["id"].Ref == nil && c.Kind == fixturechange.Insert {
			t.Fatalf("got %+v", c)
		}
	}
	if tbl := res.Tables["PlanDetail"]; tbl.ID != "" || tbl.Serial {
		t.Fatalf("got %+v", tbl)
	}
	set := fixturechange.Set{Name: "details", Tables: res.Tables, Changes: res.Changes}
	if err := fixtureapply.Validate(set); err != nil {
		t.Fatalf("the run time refuses the set: %v", err)
	}
	if _, err := Render(cfg, "details", "20261001120000", res); err != nil {
		t.Fatal(err)
	}
}
