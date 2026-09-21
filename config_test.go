package fixturemigrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigFillsInTheDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture-migrate.yml")
	if err := os.WriteFile(path, []byte(`fixture: fixtures/fixture.yml
out: internal/migrations
seed_guard_table: plans
models:
  Plan:
    table: plans
  Feature:
    table: features
    serial: true
    key: [plan_id, code]
    references:
      plan_id: Plan
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Package != "migrations" || cfg.Migrator != "Migrations" {
		t.Fatalf("unexpected defaults: %q / %q", cfg.Package, cfg.Migrator)
	}
	plan := cfg.Models["Plan"]
	if plan.ID != "id" || plan.Ref != "name" || len(plan.Key) != 1 || plan.Key[0] != "name" {
		t.Fatalf("unexpected Plan defaults: %+v", plan)
	}
}

func TestLoadConfigRejectsAnUnknownField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yml")
	os.WriteFile(path, []byte("fixture: f.yml\nmodls:\n  Plan:\n    table: plans\n"), 0o644)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected an error about the unknown field")
	}
}

func TestPrepareRejectsBadConfigurations(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg  *Config
		want string
	}{
		"no models":       {&Config{}, "no models"},
		"no table":        {&Config{Models: map[string]*Model{"Plan": {}}}, "no table"},
		"unknown target":  {&Config{Models: map[string]*Model{"Plan": {Table: "plans", References: map[string]string{"currency_id": "Currency"}}}}, "unknown model"},
		"empty key group": {&Config{Models: map[string]*Model{"Plan": {Table: "plans", KeyAnyOf: [][]string{{}}}}}, "is empty"},
	} {
		err := tc.cfg.Prepare()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: expected an error containing %q, got %v", name, tc.want, err)
		}
	}
}

func TestUnderscore(t *testing.T) {
	for in, want := range map[string]string{
		"ID": "id", "Name": "name", "PlanID": "plan_id", "GroupName": "group_name", "HTTPPort": "http_port", "ID2": "id2",
	} {
		if got := underscore(in); got != want {
			t.Errorf("underscore(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseDocKeepsStructuredValuesApart(t *testing.T) {
	d, err := ParseDoc([]byte("- model: Plan\n  rows:\n    - name: free\n      meta: {a: 1}\n      note: ~\n"))
	if err != nil {
		t.Fatal(err)
	}
	row := d[0].Rows[0]
	if !row["meta"].Structured {
		t.Fatalf("a mapping should be marked structured: %+v", row["meta"])
	}
	if !row["note"].IsNull {
		t.Fatalf("~ is null, not an empty string: %+v", row["note"])
	}
	if row["name"].Text != "free" {
		t.Fatalf("unexpected scalar %+v", row["name"])
	}
}
