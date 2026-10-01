package fixturemigrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
)

// The scaffold's only real job is to be right enough to start from, so the
// test is that its output loads as a configuration and says what the catalog
// said.
func TestScaffoldWritesAConfigurationThatLoads(t *testing.T) {
	data := Scaffold(testTables(), nil, "public", ScaffoldOptions{})
	text := string(data)
	for _, want := range []string{
		"  Currency:\n    table: currencies\n",
		"    id: id\n",
		"    serial: true\n", // plans.id is a sequence
		"    key: [code]\n",  // from the unique index on currencies.code
		"    key: [name]\n",  // from the unique index on plans.name
		"      currency_id: Currency\n",
		"      plan_id: Plan\n",
		`      price_cents: "0"`,
		"seats defaults to 1, its zero is 0",
		"policy:",
		"  id_drift: error",
		"  zero_default: error",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the scaffold is missing %q:\n%s", want, text)
		}
	}
	// The features table has a composite natural key, which the narrowest
	// unique index gives away.
	if !strings.Contains(text, "    key: [plan_id, code]\n") {
		t.Errorf("expected the composite key:\n%s", text)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "fixture-migrate.yml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("the scaffold has to load as a configuration: %v", err)
	}
	if cfg.Models["Plan"].References["currency_id"] != "Currency" {
		t.Fatalf("unexpected Plan: %+v", cfg.Models["Plan"])
	}
	if !cfg.Models["Plan"].Serial {
		t.Fatal("plans.id comes from a sequence")
	}
	if cfg.Models["Feature"].Defaults["quota"] != "0" {
		t.Fatalf("the column defaults belong in defaults: %+v", cfg.Models["Feature"].Defaults)
	}
	if _, err := cfg.DependencyOrder(); err != nil {
		t.Fatalf("the scaffolded models have to sort: %v", err)
	}
}

func TestScaffoldLimitsItselfToTheTablesAskedFor(t *testing.T) {
	text := string(Scaffold(testTables(), []string{"plans"}, "public", ScaffoldOptions{}))
	if !strings.Contains(text, "  Plan:") || strings.Contains(text, "  Feature:") {
		t.Fatalf("expected only Plan:\n%s", text)
	}
	// The reference to a table nobody asked for is reported, not invented.
	if !strings.Contains(text, "currency_id points at public.currencies, which is not in this configuration") {
		t.Fatalf("expected the dangling reference to be named:\n%s", text)
	}
}

func TestModelName(t *testing.T) {
	for _, tc := range []struct{ table, model string }{
		{"currencies", "Currency"},
		{"plans", "Plan"},
		{"boxes", "Box"},
		{"user_settings", "UserSetting"},
		{"status", "Status"},
		{"statuses", "Status"},
		{"warehouses", "Warehouse"},
		{"addresses", "Address"},
		{"cases", "Case"},
		{"sizes", "Size"},
		{"batches", "Batch"},
		{"tax_rates", "TaxRate"},
		{"people", "Person"},
		{"price_tiers", "PriceTier"},
		{"news", "News"},
		{"analysis", "Analysis"},
		{"caches", "Cach"}, // wrong, and why the scaffold says to read it
	} {
		if got := modelName(tc.table); got != tc.model {
			t.Errorf("modelName(%q) = %q, want %q", tc.table, got, tc.model)
		}
	}
}

// A foreign key to a column other than the target's id is no reference, a
// partition is no model of its own, and the columns the database writes when
// a row is written are proposed for ignore.
func TestScaffoldLeavesOutWhatIsNotMasterData(t *testing.T) {
	tables := testTables()
	tables["public.prices"] = &dbschema.Table{Schema: "public", Name: "prices", PrimaryKey: []string{"id"},
		Uniques: [][]string{{"id"}, {"sku"}},
		ForeignKeys: []dbschema.ForeignKey{{Columns: []string{"currency_code"},
			RefSchema: "public", RefTable: "currencies", RefColumns: []string{"code"}}},
		Columns: []dbschema.Column{
			{Name: "id", Position: 1, Type: "int8"},
			{Name: "sku", Position: 2, Type: "text"},
			{Name: "currency_code", Position: 3, Type: "text"},
			{Name: "created_at", Position: 4, Type: "timestamptz", Default: "now()"},
			{Name: "updated_at", Position: 5, Type: "timestamptz"},
			{Name: "valid_from", Position: 6, Type: "date", Default: "CURRENT_DATE"},
		}}
	tables["public.prices_2026"] = &dbschema.Table{Schema: "public", Name: "prices_2026",
		Columns: tables["public.prices"].Columns}
	text := string(Scaffold(tables, nil, "public", ScaffoldOptions{
		Partitions: map[string]bool{"public.prices_2026": true},
		Triggers:   map[string][]string{"public.prices": {"prices_touch"}},
	}))
	for _, want := range []string{
		"      # currency_code points at currencies.code, which is not its id: an ordinary column\n",
		"    ignore: [created_at, updated_at]\n",
		"BEFORE row triggers (prices_touch)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the scaffold is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "currency_code: Currency") || strings.Contains(text, "Prices2026") {
		t.Errorf("a reference to a code, or a partition as a model:\n%s", text)
	}
	path := filepath.Join(t.TempDir(), "fixture-migrate.yml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("the scaffold has to load as a configuration: %v", err)
	}
}
