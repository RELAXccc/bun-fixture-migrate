package fixturemigrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The scaffold's only real job is to be right enough to start from, so the
// test is that its output loads as a configuration and says what the catalog
// said.
func TestScaffoldWritesAConfigurationThatLoads(t *testing.T) {
	data := Scaffold(testTables(), nil, "public")
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
	text := string(Scaffold(testTables(), []string{"plans"}, "public"))
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
		{"status", "Statu"}, // wrong, and why the scaffold says to read it
	} {
		if got := modelName(tc.table); got != tc.model {
			t.Errorf("modelName(%q) = %q, want %q", tc.table, got, tc.model)
		}
	}
}
