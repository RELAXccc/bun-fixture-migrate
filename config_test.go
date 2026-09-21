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

func TestPolicyDefaults(t *testing.T) {
	cfg := &Config{Models: map[string]*Model{"Plan": {Table: "plans"}}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	p := cfg.Policy
	if p.IDDrift != ModeError || p.MissingRow != ModeError || p.ZeroDefault != ModeError ||
		p.DuplicateKey != ModeError {
		t.Fatalf("the strict settings are the defaults: %+v", p)
	}
	if p.ChangedRow != ModeWarn {
		t.Fatalf("a hand edit is kept, not overwritten: %+v", p)
	}
	if p.Renames != RenameRefuse || p.Deletes != DeleteAllow {
		t.Fatalf("unexpected defaults: %+v", p)
	}
	if cfg.Models["Plan"].Deletes != DeleteAllow {
		t.Fatal("a model with no deletes of its own takes the policy's")
	}
}

func TestPolicyRejectsAValueItDoesNotKnow(t *testing.T) {
	cfg := &Config{Policy: Policy{IDDrift: "maybe"}, Models: map[string]*Model{"Plan": {Table: "plans"}}}
	err := cfg.Prepare()
	if err == nil || !strings.Contains(err.Error(), "id_drift") {
		t.Fatalf("expected the field to be named, got %v", err)
	}
	// missing_row has no ignore, because ignoring it loses the change for good.
	cfg = &Config{Policy: Policy{MissingRow: ModeIgnore}, Models: map[string]*Model{"Plan": {Table: "plans"}}}
	if err := cfg.Prepare(); err == nil {
		t.Fatal("missing_row must not accept ignore")
	}
}

func TestUnknownKeyInTheConfigurationIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture-migrate.yml")
	if err := os.WriteFile(path, []byte("fixture: f.yml\nmodls:\n  Plan:\n    table: plans\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "modls") {
		t.Fatalf("a typo has to be reported, got %v", err)
	}
}

// The example configuration is documentation, so it has to stay loadable and
// stay in step with the code.
func TestTheExampleConfigurationLoads(t *testing.T) {
	cfg, err := LoadConfig("fixture-migrate.example.yml")
	if err != nil {
		t.Fatalf("the shipped example has to load: %v", err)
	}
	if len(cfg.Models) != 5 {
		t.Fatalf("unexpected models: %v", cfg.ModelNames())
	}
	if cfg.Models["Plan"].Deletes != DeleteRefuse || cfg.Models["Feature"].Deletes != DeleteAllow {
		t.Fatal("a model's deletes overrides the policy's")
	}
	if cfg.Policy.ZeroDefault != ModeError {
		t.Fatal("the example has to show the defaults it documents")
	}
	if _, err := cfg.DependencyOrder(); err != nil {
		t.Fatalf("the example models have to sort: %v", err)
	}
}
