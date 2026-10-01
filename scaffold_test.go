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
		"  Currency:\n    # GUESS: proposed because the schema has it. Delete this model unless",
		"    table: currencies\n",
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

// Not every table is master data, and scaffold cannot tell which are. bun's
// own two never are, so they are left out; every other model says it is a
// guess to delete when the application writes the table; one with nothing to
// guess a key from is written commented out; and the seed guard is guessed,
// because without one a new database runs every fixture migration before its
// seed and fails.
func TestScaffoldMarksWhatItCannotKnow(t *testing.T) {
	col := func(pos int, name, typ string) dbschema.Column {
		return dbschema.Column{Name: name, Position: pos, Type: typ}
	}
	tables := testTables()
	tables["public.bun_migrations"] = &dbschema.Table{Schema: "public", Name: "bun_migrations", PrimaryKey: []string{"id"},
		Uniques: [][]string{{"id"}}, Columns: []dbschema.Column{col(1, "id", "int8"), col(2, "name", "varchar")}}
	tables["public.bun_migration_locks"] = &dbschema.Table{Schema: "public", Name: "bun_migration_locks",
		PrimaryKey: []string{"id"}, Uniques: [][]string{{"id"}, {"table_name"}},
		Columns: []dbschema.Column{col(1, "id", "int8"), col(2, "table_name", "varchar")}}
	tables["public.schema_migrations"] = &dbschema.Table{Schema: "public", Name: "schema_migrations",
		PrimaryKey: []string{"id"}, Uniques: [][]string{{"id"}},
		Columns: []dbschema.Column{col(1, "id", "int8"), col(2, "name", "varchar")}}
	// No unique index besides the id, no name column, and a feature points
	// at it.
	tables["public.events"] = &dbschema.Table{Schema: "public", Name: "events", PrimaryKey: []string{"id"},
		Uniques: [][]string{{"id"}}, Columns: []dbschema.Column{col(1, "id", "int8"), col(2, "payload", "jsonb")}}
	tables["public.features"].ForeignKeys = append(tables["public.features"].ForeignKeys, dbschema.ForeignKey{
		Columns: []string{"event_id"}, RefSchema: "public", RefTable: "events", RefColumns: []string{"id"}})
	tables["public.features"].Columns = append(tables["public.features"].Columns, col(6, "event_id", "int8"))

	text := string(Scaffold(tables, nil, "public", ScaffoldOptions{}))
	for _, want := range []string{
		"migrations_table: bun_migrations\n", "migration_locks_table: bun_migration_locks\n",
		"# GUESS: currencies, the first table the other models point at.",
		`seed_guard_table: "currencies"`,
		"  # public.events has no unique index besides its primary key and no name column",
		"  # Event:\n  #   # GUESS: proposed because the schema has it.",
		"  #   table: events\n",
		"      # event_id points at public.events, which is not in this configuration\n",
		"  SchemaMigration:\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the scaffold is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "    table: bun_migrations\n") || strings.Contains(text, "    table: bun_migration_locks\n") {
		t.Errorf("bun's tables are no master data:\n%s", text)
	}
	path := filepath.Join(t.TempDir(), "fixture-migrate.yml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("the scaffold has to load as a configuration: %v\n%s", err, text)
	}
	if len(cfg.Models) != 4 || cfg.Models["Event"] != nil || cfg.SeedGuardTable != "currencies" {
		t.Fatalf("models %v, guard %q", cfg.ModelNames(), cfg.SeedGuardTable)
	}
	if n := strings.Count(text, "\n    # GUESS: proposed because the schema has it."); n != len(cfg.Models) {
		t.Errorf("%d models say they are a guess, of %d", n, len(cfg.Models))
	}

	// A migrator built WithTableName: its table is named, and left out.
	text = string(Scaffold(tables, nil, "public", ScaffoldOptions{MigrationsTable: "schema_migrations"}))
	if strings.Contains(text, "  SchemaMigration:") || !strings.Contains(text, "migrations_table: schema_migrations\n") ||
		!strings.Contains(text, "    table: bun_migrations\n") {
		t.Errorf("the configured migrations table:\n%s", text)
	}
	got := ScaffoldTables(tables, nil, "public", ScaffoldOptions{MigrationsTable: "public.schema_migrations"})
	if strings.Join(got, ",") != "public.bun_migrations,public.currencies,public.events,public.features,public.plans" {
		t.Errorf("tables %v", got)
	}

	// No model at all, no guard to guess.
	text = string(Scaffold(map[string]*dbschema.Table{"public.events": tables["public.events"]}, nil, "public",
		ScaffoldOptions{}))
	if !strings.Contains(text, `seed_guard_table: ""`) || !strings.Contains(text, "# GUESS: no model to take it from") {
		t.Errorf("no models:\n%s", text)
	}
}

// A primary key that points at another model's row is that model's
// reference, and the tool reads an id as the row's own value: Prepare refuses
// the two together. The scaffold leaves the id out and keys the model on the
// column instead.
func TestScaffoldKeysATableOnAPrimaryKeyThatIsAReference(t *testing.T) {
	tables := testTables()
	tables["public.plan_limits"] = &dbschema.Table{Schema: "public", Name: "plan_limits", PrimaryKey: []string{"plan_id"},
		Columns: []dbschema.Column{
			{Name: "plan_id", Position: 1, Type: "int8"},
			{Name: "seats", Position: 2, Type: "int8"},
		},
		ForeignKeys: []dbschema.ForeignKey{{Columns: []string{"plan_id"},
			RefSchema: "public", RefTable: "plans", RefColumns: []string{"id"}}},
	}
	data := Scaffold(tables, nil, "public", ScaffoldOptions{})
	text := string(data)
	start := strings.Index(text, "    table: plan_limits\n")
	if start < 0 {
		t.Fatalf("no model for plan_limits:\n%s", text)
	}
	model := text[start:]
	if end := strings.Index(model, "\n\n"); end > 0 {
		model = model[:end]
	}
	if strings.Contains(model, "    id: plan_id") || !strings.Contains(model, "    key: [plan_id]\n") ||
		!strings.Contains(model, "      plan_id: Plan") || strings.Contains(model, "    ref: ") {
		t.Fatalf("expected the model keyed on its reference, without an id:\n%s", model)
	}
	path := filepath.Join(t.TempDir(), "fixture-migrate.yml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("the scaffold has to load as a configuration: %v", err)
	}
}

// The audit table generated migrations record their runs in is this tool's,
// as bun's two tables are bun's, and no master data. It has no unique index
// besides its id and no name column, so it was proposed as a model written
// commented out, which uncommented would export the runs into the fixture
// file. It is left out, whatever it is named, and the header proposes its
// name for audit_table instead.
func TestScaffoldLeavesOutTheAuditTable(t *testing.T) {
	col := func(pos int, name, typ string) dbschema.Column {
		return dbschema.Column{Name: name, Position: pos, Type: typ}
	}
	audit := func(name, outcomes string) *dbschema.Table {
		return &dbschema.Table{Schema: "public", Name: name, PrimaryKey: []string{"id"}, Uniques: [][]string{{"id"}},
			Columns: []dbschema.Column{col(1, "id", "int8"), col(2, "set_name", "text"), col(3, "direction", "text"),
				col(4, "set_sha256", "text"), col(5, "applied_at", "timestamptz"), col(6, "applied_by", "text"),
				col(7, "outcomes", outcomes)}}
	}
	tables := testTables()
	tables["public.deploy_audit"] = audit("deploy_audit", "jsonb")
	text := string(Scaffold(tables, nil, "public", ScaffoldOptions{}))
	if strings.Contains(text, " table: deploy_audit") || strings.Contains(text, "DeployAudit") {
		t.Errorf("the audit table is proposed as a model:\n%s", text)
	}
	if !strings.Contains(text, "# This database has one, deploy_audit, left out of the models below") ||
		!strings.Contains(text, "# audit_table: deploy_audit\n") || strings.Contains(text, "\naudit_table:") {
		t.Errorf("the header has to propose it, commented out:\n%s", text)
	}
	got := ScaffoldTables(tables, nil, "public", ScaffoldOptions{})
	if strings.Join(got, ",") != "public.currencies,public.features,public.plans" {
		t.Errorf("tables %v", got)
	}
	if got := ScaffoldTables(tables, []string{"deploy_audit", "plans"}, "public", ScaffoldOptions{}); strings.Join(got,
		",") != "public.plans" {
		t.Errorf("asked for: %v", got)
	}
	cfg := filepath.Join(t.TempDir(), "fixture-migrate.yml")
	if err := os.WriteFile(cfg, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(cfg); err != nil {
		t.Fatalf("the scaffold has to load as a configuration: %v", err)
	}

	// Without one, the header proposes the documented name.
	if text := string(Scaffold(testTables(), nil, "public", ScaffoldOptions{})); !strings.Contains(text,
		"# audit_table: bun_fixture_audit\n") || strings.Contains(text, "This database has one") {
		t.Errorf("no audit table:\n%s", text)
	}
	// A table of the same columns that keeps something else in outcomes is
	// somebody's data.
	tables = testTables()
	tables["public.runs"] = audit("runs", "text")
	if text := string(Scaffold(tables, nil, "public", ScaffoldOptions{})); !strings.Contains(text, " table: runs") {
		t.Errorf("a table that only looks like it:\n%s", text)
	}
}
