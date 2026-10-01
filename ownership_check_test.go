package fixturemigrate

import (
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
)

// databaseOf is a fixture file read as a database snapshot, with the catalog
// noted as DatabaseSnapshot notes it.
func databaseOf(t *testing.T, cfg *Config, text string, tables map[string]*dbschema.Table) *Snapshot {
	t.Helper()
	s := snap(t, cfg, text, "the database")
	for _, model := range s.Order {
		if table := tables[cfg.QualifiedTable(cfg.Models[model])]; table != nil {
			s.noteUniques(model, table)
		}
	}
	return s
}

// The drift that looks the most mysterious says why: the database holds the
// column's default where the file has a zero or a null, which bun wrote as
// DEFAULT.
func TestCheckHintsAtADefaultBunWrote(t *testing.T) {
	cfg := testConfig(t)
	tables := testTables()
	plans := tables["public.plans"]
	for i := range plans.Columns {
		if plans.Columns[i].Name == "note" {
			plans.Columns[i].Default = "'none'::text"
		}
	}
	database := replace(t, base, "      seats: 10\n", "      seats: 1\n      note: none\n")
	database = replace(t, database, "      price_cents: 0\n      seats: 1\n", "      price_cents: 0\n      seats: 1\n      note: other\n")
	file := replace(t, base, "      seats: 10\n", "      seats: 0\n      note: ~\n")
	file = replace(t, file, "      price_cents: 0\n      seats: 1\n", "      price_cents: 0\n      seats: 1\n      note: ~\n")
	res, err := Check(cfg, databaseOf(t, cfg, database, tables), snap(t, cfg, file, "fixtures/fixture.yml"))
	if err != nil {
		t.Fatal(err)
	}
	hints := map[string]string{}
	for i, c := range res.Changes {
		for col, hint := range res.Hints[i] {
			hints[c.Key["name"].Lit+"."+col] = hint
		}
	}
	if len(hints) != 2 || !strings.Contains(hints["team.seats"], "defaults to 1") ||
		!strings.Contains(hints["team.seats"], "since v1.2.17, on UPDATE too") ||
		!strings.Contains(hints["team.note"], "nil pointer") {
		t.Fatalf("a zero and a null against the default the database holds, and not a value it does not: %q", hints)
	}
	text := strings.Join(res.Lines(), "\n")
	if !strings.Contains(text, "    seats: database 1, file 0\n      hint: the column defaults to 1") {
		t.Fatalf("%s", text)
	}
}

func TestCheckCountsWhatItLeavesToTheDatabase(t *testing.T) {
	cfg := ownedConfig(t, func(cfg *Config) {
		cfg.Models["Feature"].Mode = OwnUpsert
		cfg.Models["Feature"].InsertOnly = []string{"enabled"}
	})
	database := base + "    - {plan_id: '{{ $.Plan.free.ID }}', code: tenant_made}\n"
	database = replace(t, database, "      enabled: true\n", "      enabled: false\n")
	res := checkOf(t, cfg, database, base)
	if res.Drifted() || !res.Agree(cfg) {
		t.Fatalf("what the database owns is no drift: %+v", res.Changes)
	}
	lines := res.Lines()
	want := []string{
		"the database and fixtures/fixture.yml agree",
		"",
		"Left to the database by the configuration, which is no drift:",
		"  Feature: 1 row only in the database, which mode upsert never deletes",
		"  Feature: 1 row with another enabled in the database, which insert_only leaves to the database",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%q", lines)
	}
}
