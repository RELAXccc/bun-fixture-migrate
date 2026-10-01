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
		"lock timeout":    {&Config{LockTimeout: "5", Models: map[string]*Model{"Plan": {Table: "plans"}}}, "lock timeout"},
		"array nulls":     {&Config{Policy: Policy{ArrayNulls: "drop"}, Models: map[string]*Model{"Plan": {Table: "plans"}}}, "array_nulls"},
		"model array nulls": {&Config{Models: map[string]*Model{"Plan": {Table: "plans", ArrayNulls: "maybe"}}},
			"array_nulls"},
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
	if len(cfg.Models) != 6 {
		t.Fatalf("unexpected models: %v", cfg.ModelNames())
	}
	if role := cfg.Models["Role"]; role.Mode != OwnUpsert || role.IDs != IDsDatabase || cfg.Models["Plan"].Mode != OwnSync ||
		!cfg.Models["Feature"].insertOnly["enabled"] {
		t.Fatal("the example shows a mode, ids and insert_only")
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

// A column added to a table later holds NULL in the rows written before it;
// ~ in defaults says so.
func TestADefaultCanBeNull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yml")
	if err := os.WriteFile(path, []byte("models:\n  Plan:\n    table: plans\n    defaults:\n      color: ~\n      quota: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models["Plan"].Defaults["color"] != NullDefault || cfg.Models["Plan"].Defaults["quota"] != "0" {
		t.Fatalf("%q", cfg.Models["Plan"].Defaults)
	}
	s := snap(t, cfg, "- model: Plan\n  rows:\n    - name: a\n", "f")
	if v := s.Entries["Plan"][0].Cells["color"]; !v.IsNull {
		t.Fatalf("an omitted color is NULL, got %+v", v)
	}
}

// bun's locks table has a default, and a name that is not a plain identifier
// is refused like the migrations table's.
func TestTheLocksTable(t *testing.T) {
	cfg := &Config{Fixture: "f.yml", Models: map[string]*Model{"Plan": {Table: "plans"}}}
	if err := cfg.Prepare(); err != nil || cfg.MigrationLocksTable != "bun_migration_locks" {
		t.Fatalf("%v %q", err, cfg.MigrationLocksTable)
	}
	cfg = &Config{Fixture: "f.yml", MigrationLocksTable: "locks; drop", Models: map[string]*Model{"Plan": {Table: "plans"}}}
	if err := cfg.Prepare(); err == nil || !strings.Contains(err.Error(), "migration_locks_table") {
		t.Fatalf("got %v", err)
	}
}

// The catalog is read from every schema a model's table is in, the default
// one first and each other once.
func TestSchemasAreEverySchemaATableIsIn(t *testing.T) {
	cfg := &Config{Fixture: "f.yml", Schema: "app", Models: map[string]*Model{
		"A": {Table: "plans"}, "B": {Table: "billing.prices"}, "C": {Table: "billing.taxes"},
		"D": {Table: "app.features"}, "E": {Table: "audit.events"},
	}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Schemas(), ","); got != "app,billing,audit" {
		t.Fatal(got)
	}
}

// A primary key that is also a reference, a plan's limits keyed by the plan,
// would be read as the template text naming the plan: it is refused with what
// to do instead.
func TestAnIDThatIsAReferenceIsRefused(t *testing.T) {
	cfg := &Config{Models: map[string]*Model{
		"Plan":  {Table: "plans"},
		"Limit": {Table: "limits", ID: "plan_id", Key: []string{"plan_id"}, References: map[string]string{"plan_id": "Plan"}},
	}}
	err := cfg.Prepare()
	if err == nil || !strings.Contains(err.Error(), `model "Limit": its id, plan_id, is also a reference to Plan`) ||
		!strings.Contains(err.Error(), "set id: none") {
		t.Fatalf("got %v", err)
	}
	cfg.Models["Limit"].ID = ""
	if err := cfg.Prepare(); err != nil {
		t.Fatalf("without the id: %v", err)
	}
}

// A table whose primary key, id, is the id of another model's row, a plan's
// details keyed by the plan, has no id of its own: id: none says so, and the
// model is keyed and referenced by that column. Before, nothing could say it:
// left out, the id was "id", the reference, and the configuration scaffold
// wrote for such a table was refused.
func TestAModelWithoutAnIDOfItsOwn(t *testing.T) {
	details := func() *Model {
		return &Model{Table: "plan_details", ID: NoID, Key: []string{"id"}, References: map[string]string{"id": "Plan"}}
	}
	cfg := &Config{Models: map[string]*Model{"Plan": {Table: "plans"}, "PlanDetail": details()}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	m := cfg.Models["PlanDetail"]
	if m.ID != "" || !m.hasNoID() || m.skip("id") {
		t.Fatalf("the model has no id, and id is a column like any other: %+v", m)
	}
	// Prepare stays repeatable.
	if err := cfg.Prepare(); err != nil || m.ID != "" {
		t.Fatalf("a second Prepare: %v, id %q", err, m.ID)
	}
	if order, err := cfg.DependencyOrder(); err != nil || strings.Join(order, ",") != "Plan,PlanDetail" {
		t.Fatalf("the details come after the plans they point at: %v %v", order, err)
	}
	// Left out, id is the reference, which is refused with the way out.
	cfg = &Config{Models: map[string]*Model{"Plan": {Table: "plans"},
		"PlanDetail": {Table: "plan_details", Key: []string{"id"}, References: map[string]string{"id": "Plan"}}}}
	if err := cfg.Prepare(); err == nil || !strings.Contains(err.Error(), "its id, id, is also a reference to Plan") ||
		!strings.Contains(err.Error(), "set id: none") {
		t.Fatalf("got %v", err)
	}

	for _, tc := range []struct {
		name   string
		change func(cfg *Config)
		want   string
	}{
		{"serial", func(cfg *Config) { cfg.Models["PlanDetail"].Serial = true }, "serial is true, and id is none"},
		{"ids", func(cfg *Config) { cfg.Models["PlanDetail"].IDs = IDsDatabase }, "ids is database, and id is none"},
		{"referenced", func(cfg *Config) {
			cfg.Models["Order"] = &Model{Table: "orders", References: map[string]string{"detail_id": "PlanDetail"}}
		}, `model "Order": column "detail_id" references PlanDetail, which has no id of its own (id: none), and a ` +
			"reference holds the id of the row it names: point it at the model PlanDetail's key points at, Plan, " +
			"whose id PlanDetail's id holds"},
	} {
		cfg := &Config{Models: map[string]*Model{"Plan": {Table: "plans"}, "PlanDetail": details()}}
		tc.change(cfg)
		if err := cfg.Prepare(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v", tc.name, err)
		}
	}
}
