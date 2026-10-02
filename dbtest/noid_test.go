package dbtest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"

	"github.com/uptrace/bun"
)

type NiPlan struct {
	bun.BaseModel `bun:"table:ni_plans"`
	ID            int64  `bun:"id,pk,autoincrement"`
	Name          string `bun:"name,notnull"`
}

type NiDetail struct {
	bun.BaseModel `bun:"table:ni_details"`
	ID            int64  `bun:"id,pk"`
	Blurb         string `bun:"blurb,notnull"`
}

// A 1:1 extension table whose primary key is its plan's id, under id: none:
// seeded by dbfixture, migrated, synced and exported by the reference its
// key is, as dbfixture stores it.
func TestAnExtensionTableKeyedByItsParentsID(t *testing.T) {
	l := newLab(t, map[string]*fixturemigrate.Model{
		"NiPlan":   {Table: "ni_plans", Serial: true},
		"NiDetail": {Table: "ni_details", ID: fixturemigrate.NoID, Key: []string{"id"}, References: map[string]string{"id": "NiPlan"}},
	}, "ni_plans",
		[]string{"DROP TABLE IF EXISTS ni_details", "DROP TABLE IF EXISTS ni_plans",
			"CREATE TABLE ni_plans (id bigserial PRIMARY KEY, name text NOT NULL UNIQUE)",
			"CREATE TABLE ni_details (id bigint PRIMARY KEY REFERENCES ni_plans (id), blurb text NOT NULL)"},
		`SELECT string_agg(concat_ws('|', p.name, d.blurb), E'\n' ORDER BY p.name) FROM ni_details d JOIN ni_plans p ON p.id = d.id`,
		(*NiPlan)(nil), (*NiDetail)(nil))
	plans := "- model: NiPlan\n  rows:\n    - {_id: free, id: 1, name: free}\n    - {_id: pro, id: 2, name: pro}\n" +
		"    - {_id: team, id: 3, name: team}\n- model: NiDetail\n  rows:\n"
	v1 := plans + "    - {id: '{{ $.NiPlan.free.ID }}', blurb: Free!}\n    - {id: '{{ $.NiPlan.team.ID }}', blurb: Team!}\n"
	v2 := plans + "    - {id: '{{ $.NiPlan.free.ID }}', blurb: 'Free, forever!'}\n    - {id: '{{ $.NiPlan.pro.ID }}', blurb: Pro!}\n"
	l.fidelity(v1, v2)
	exported := l.roundTrip()
	if !strings.Contains(exported, "id: '{{ $.NiPlan.pro.ID }}'") {
		t.Fatalf("the details name their plan:\n%s", exported)
	}
}

// The command, as the README adopts a database: scaffold writes id: none for
// a table whose primary key id is a plan's id, and export, check, generate
// and plan take it. Before, every command after scaffold refused the
// configuration scaffold wrote.
func TestScaffoldToPlanOfAnExtensionTable(t *testing.T) {
	a := newAdoption(t)
	run(t, a.db, "CREATE TABLE plan_details (id bigint PRIMARY KEY REFERENCES plans (id), blurb text NOT NULL)",
		"INSERT INTO plan_details VALUES (1, 'Free!')")
	a.run(0, "scaffold", "-o", "fixture-migrate.yml")
	cfg := a.read("fixture-migrate.yml")
	if !strings.Contains(cfg, "    table: plan_details\n") || !strings.Contains(cfg, "    id: none\n") {
		t.Fatalf("scaffold:\n%s", cfg)
	}
	// The users are the application's.
	start := strings.Index(cfg, "\n  User:\n")
	end := strings.Index(cfg[start+1:], "\n\n")
	if end < 0 {
		cfg = cfg[:start+1]
	} else {
		cfg = cfg[:start+1] + cfg[start+1+end+2:]
	}
	a.write("fixture-migrate.yml", cfg)
	a.run(0, "export")
	exported := a.read("fixtures/fixture.yml")
	if !strings.Contains(exported, "- model: PlanDetail\n  rows:\n    - _id: free\n      id: '{{ $.Plan.free.ID }}'\n") {
		t.Fatalf("export:\n%s", exported)
	}
	a.run(0, "check")
	a.run(0, "baseline")
	a.write("fixtures/fixture.yml", strings.Replace(exported, `blurb: "Free!"`, `blurb: "Free, forever!"`, 1)+
		"    - _id: team\n      id: '{{ $.Plan.team.ID }}'\n      blurb: \"Team!\"\n")
	if out := a.run(0, "generate", "-name", "details"); !strings.Contains(out, "PlanDetail: 1 insert, 1 update") {
		t.Fatal(out)
	}
	files, _ := filepath.Glob(filepath.Join(a.dir, "internal", "migrations", "*_fixture_details.go"))
	if len(files) != 1 {
		t.Fatalf("no migration: %v", files)
	}
	if src, _ := os.ReadFile(files[0]); !strings.Contains(string(src), `"id": fixturechange.RefTo("Plan", "team")`) {
		t.Fatalf("the insert names the plan:\n%s", src)
	}
	if out := a.run(0, "plan"); !strings.Contains(out, "would succeed") ||
		!strings.Contains(out, "applied  PlanDetail id=Plan(team) insert") {
		t.Fatal(out)
	}
}
