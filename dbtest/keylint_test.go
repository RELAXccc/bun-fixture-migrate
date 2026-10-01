package dbtest_test

// The natural-key lint against a real PostgreSQL: every verdict of the
// spec's table (P1-P8), and the edge cases E7-E13, under the driver the run
// uses. NULLS NOT DISTINCT is PostgreSQL 15 and later, and is skipped
// before.

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/dbschema"

	"github.com/uptrace/bun"
)

// keyTables are the tables of the verdicts, each named for its case.
var keyTables = []string{"kl_exact", "kl_subset", "kl_lower", "kl_super", "kl_none", "kl_null", "kl_nnd",
	"kl_coal", "kl_lower_null", "kl_partial", "kl_outside", "kl_defer", "kl_excl", "kl_excl_range", "kl_invalid",
	"kl_citext", "kl_include", "kl_any"}

func keyDB(t *testing.T) (*bun.DB, int) {
	t.Helper()
	db := connect(t)
	var version int
	if err := db.QueryRowContext(context.Background(), "SHOW server_version_num").Scan(&version); err != nil {
		t.Fatal(err)
	}
	run(t, db, "DROP TABLE IF EXISTS "+strings.Join(keyTables, ", ")+" CASCADE",
		"CREATE EXTENSION IF NOT EXISTS citext",
		`CREATE TABLE kl_exact (id bigserial PRIMARY KEY, plan_id bigint NOT NULL, code text NOT NULL,
			UNIQUE (plan_id, code))`,
		`CREATE TABLE kl_subset (id bigserial PRIMARY KEY, plan_id bigint NOT NULL, code text NOT NULL UNIQUE)`,
		`CREATE TABLE kl_lower (id bigserial PRIMARY KEY, email text NOT NULL)`,
		`CREATE UNIQUE INDEX kl_lower_email ON kl_lower (lower(email))`,
		`CREATE TABLE kl_super (id bigserial PRIMARY KEY, code text NOT NULL, plan_id bigint NOT NULL,
			UNIQUE (code, plan_id))`,
		`CREATE TABLE kl_none (id bigserial PRIMARY KEY, plan_id bigint NOT NULL, code text NOT NULL)`,
		`CREATE TABLE kl_null (id bigserial PRIMARY KEY, parent_id bigint, code text NOT NULL,
			UNIQUE (parent_id, code))`,
		`CREATE TABLE kl_coal (id bigserial PRIMARY KEY, parent_id bigint, code text NOT NULL)`,
		`CREATE UNIQUE INDEX kl_coal_key ON kl_coal ((COALESCE(parent_id, 0)), code)`,
		`CREATE TABLE kl_lower_null (id bigserial PRIMARY KEY, email text)`,
		`CREATE UNIQUE INDEX kl_lower_null_email ON kl_lower_null (lower(email))`,
		`CREATE TABLE kl_partial (id bigserial PRIMARY KEY, code text NOT NULL, tenant_id bigint,
			status text NOT NULL DEFAULT 'active', deleted_at timestamptz)`,
		`CREATE UNIQUE INDEX kl_partial_live ON kl_partial (code) WHERE deleted_at IS NULL`,
		`CREATE UNIQUE INDEX kl_partial_global_live ON kl_partial (code) WHERE deleted_at IS NULL AND tenant_id IS NULL`,
		`CREATE UNIQUE INDEX kl_partial_not_retired ON kl_partial (code, status) WHERE status <> 'retired'`,
		`CREATE TABLE kl_outside (id bigserial PRIMARY KEY, code text NOT NULL, plan_id bigint NOT NULL)`,
		`CREATE UNIQUE INDEX kl_outside_key ON kl_outside ((code || plan_id::text))`,
		`CREATE TABLE kl_defer (id bigserial PRIMARY KEY, code text NOT NULL,
			CONSTRAINT kl_defer_code UNIQUE (code) DEFERRABLE INITIALLY IMMEDIATE)`,
		`CREATE TABLE kl_excl (id bigserial PRIMARY KEY, code text NOT NULL, EXCLUDE USING btree (code WITH =))`,
		`CREATE TABLE kl_excl_range (id bigserial PRIMARY KEY, during tstzrange NOT NULL,
			EXCLUDE USING gist (during WITH &&))`,
		`CREATE TABLE kl_invalid (id bigserial PRIMARY KEY, code text NOT NULL)`,
		`INSERT INTO kl_invalid (code) VALUES ('a'), ('a')`,
		`CREATE TABLE kl_citext (id bigserial PRIMARY KEY, code citext NOT NULL UNIQUE)`,
		`CREATE TABLE kl_include (id bigserial PRIMARY KEY, code text NOT NULL, label text NOT NULL)`,
		`CREATE UNIQUE INDEX kl_include_code ON kl_include (code) INCLUDE (label)`,
		`CREATE TABLE kl_any (id bigserial PRIMARY KEY, plan_id bigint NOT NULL, feature_id bigint, addon_id bigint)`,
		`CREATE UNIQUE INDEX kl_any_feature ON kl_any (plan_id, feature_id) WHERE feature_id IS NOT NULL`,
		`CREATE UNIQUE INDEX kl_any_addon ON kl_any (plan_id, addon_id) WHERE addon_id IS NOT NULL`)
	// A unique index CREATE INDEX CONCURRENTLY could not build over the
	// duplicates is left behind invalid (P7).
	if _, err := db.ExecContext(context.Background(),
		"CREATE UNIQUE INDEX CONCURRENTLY kl_invalid_code ON kl_invalid (code)"); err == nil {
		t.Fatal("the premise: a unique index over duplicates cannot be built")
	}
	if version >= 150000 {
		run(t, db, `CREATE TABLE kl_nnd (id bigserial PRIMARY KEY, parent_id bigint, code text NOT NULL,
			UNIQUE NULLS NOT DISTINCT (parent_id, code))`)
	}
	return db, version
}

// Every row of the spec's verdict table, asked of PostgreSQL itself:
// expressions evaluated over a row named like the key, a partial index's
// predicate put to the planner.
func TestTheVerdictsOnAKey(t *testing.T) {
	db, version := keyDB(t)
	ctx := context.Background()
	tables, err := dbschema.Load(ctx, db, "public")
	if err != nil {
		t.Fatal(err)
	}
	soft := fixturemigrate.RowFilter("", "deleted_at")
	for _, c := range []struct {
		what, table string
		key         []string
		filter      string
		backed      bool
		index       string
		detail      string
	}{
		{"exactly the key", "kl_exact", []string{"plan_id", "code"}, "", true, "kl_exact_plan_id_code_key", ""},
		{"a part of the key", "kl_subset", []string{"plan_id", "code"}, "", true, "kl_subset_code_key", ""},
		{"lower() of the key", "kl_lower", []string{"email"}, "", true, "kl_lower_email", ""},
		{"more columns than the key", "kl_super", []string{"code"}, "", false, "kl_super_code_plan_id_key",
			"UNIQUE (code, plan_id) is over more columns than key [code], so two rows may share code: key on " +
				"[code, plan_id], or CREATE UNIQUE INDEX ON kl_super (code)"},
		{"no index", "kl_none", []string{"plan_id", "code"}, "", false, "",
			"no unique index or constraint backs key [plan_id, code]: the database lets a second row with this key " +
				"in, and every change to it then fails as a duplicate key. CREATE UNIQUE INDEX ON kl_none (plan_id, code)"},
		{"a nullable column held distinct", "kl_null", []string{"parent_id", "code"}, "", false,
			"kl_null_parent_id_code_key", "parent_id is nullable and UNIQUE (parent_id, code) holds NULLs distinct, " +
				"so any number of rows may hold the same code with parent_id NULL: "},
		{"COALESCE over the nullable column", "kl_coal", []string{"parent_id", "code"}, "", true, "kl_coal_key", ""},
		{"lower() of a nullable column", "kl_lower_null", []string{"email"}, "", false, "kl_lower_null_email",
			"lower(email) is NULL where email is, and UNIQUE (lower(email)) holds NULLs distinct"},
		{"live rows under a soft-delete filter", "kl_partial", []string{"code"}, soft, true, "kl_partial_live", ""},
		{"live global rows under a where and a soft-delete filter", "kl_partial", []string{"code"},
			fixturemigrate.RowFilter("tenant_id is null", "deleted_at"), true, "", ""},
		{"a partial index and every row", "kl_partial", []string{"code"}, "", false, "kl_partial_live",
			"UNIQUE (code) WHERE deleted_at IS NULL holds only where deleted_at IS NULL, and this model reads every row"},
		{"a where the planner proves implies the predicate", "kl_partial", []string{"code", "status"},
			fixturemigrate.RowFilter("status IN ('active', 'draft')", ""), true, "kl_partial_not_retired", ""},
		{"a where that does not imply the predicate", "kl_partial", []string{"code", "status"},
			fixturemigrate.RowFilter("status <> 'archived'", ""), false, "kl_partial_not_retired",
			"UNIQUE (code, status) WHERE status <> 'retired'::text holds only where status <> 'retired'::text, and " +
				"the rows this model reads, where status <> 'archived', are not all within it"},
		{"an expression reading a column outside the key", "kl_outside", []string{"code"}, "", false,
			"kl_outside_key", "reads columns outside key [code], so two rows may share code: CREATE UNIQUE INDEX ON " +
				"kl_outside (code)"},
		{"a deferrable constraint", "kl_defer", []string{"code"}, "", true, "kl_defer_code", ""},
		{"an exclusion constraint WITH =", "kl_excl", []string{"code"}, "", true, "kl_excl_code_excl", ""},
		{"an exclusion constraint WITH &&", "kl_excl_range", []string{"during"}, "", false, "",
			"no unique index or constraint backs key [during]"},
		{"an invalid index", "kl_invalid", []string{"code"}, "", false, "kl_invalid_code",
			"unique index kl_invalid_code is invalid, left by a CREATE INDEX CONCURRENTLY that failed, and backs " +
				"nothing: delete the duplicate rows it failed on, then REINDEX INDEX CONCURRENTLY kl_invalid_code"},
		{"a citext key under a plain index", "kl_citext", []string{"code"}, "", true, "kl_citext_code_key", ""},
		{"an index with INCLUDE columns", "kl_include", []string{"code"}, "", true, "kl_include_code", ""},
		// Asked as a plain key, feature_id may be NULL; key_any_of's own
		// test is below.
		{"a nullable column a partial index leaves out", "kl_any", []string{"plan_id", "feature_id"}, "", false,
			"kl_any_feature", "feature_id is nullable"},
	} {
		t.Run(c.what, func(t *testing.T) {
			v, err := fixturemigrate.KeyBacked(ctx, db, tables["public."+c.table], c.key, c.filter)
			if err != nil {
				t.Fatal(err)
			}
			if v.Backed != c.backed || c.index != "" && v.Index != c.index || v.Unsure {
				t.Fatalf("backed %v by %q unsure %v, want backed %v by %q:\n%s", v.Backed, v.Index, v.Unsure,
					c.backed, c.index, v.Detail)
			}
			if !strings.Contains(v.Detail, c.detail) {
				t.Fatalf("detail:\n%s\nwant it to say:\n%s", v.Detail, c.detail)
			}
		})
	}
	// The remedy for NULLs follows the server: NULLS NOT DISTINCT from 15 on,
	// COALESCE before.
	v, err := fixturemigrate.KeyBacked(ctx, db, tables["public.kl_null"], []string{"parent_id", "code"}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := "declare it NULLS NOT DISTINCT (PostgreSQL 15 and later), or make parent_id NOT NULL"
	if version < 150000 {
		want = "index (COALESCE(parent_id, 0), code) in its place, or make parent_id NOT NULL"
	}
	if !strings.HasSuffix(v.Detail, want) {
		t.Fatalf("PostgreSQL %d:\n%s", version, v.Detail)
	}
	if version >= 150000 {
		v, err := fixturemigrate.KeyBacked(ctx, db, tables["public.kl_nnd"], []string{"parent_id", "code"}, "")
		if err != nil || !v.Backed || v.Index != "kl_nnd_parent_id_code_key" {
			t.Fatalf("NULLS NOT DISTINCT: %+v, %v", v, err)
		}
	}
}

// A key_any_of model is looked up by its key and the one column of each
// group a row sets, never NULL: col = $n in the lookup proves a partial
// index's col IS NOT NULL, so one partial index per column backs it.
func TestAKeyAnyOfKeyUnderPartialIndexes(t *testing.T) {
	db, _ := keyDB(t)
	ctx := context.Background()
	lint := func(m *fixturemigrate.Model) []fixturemigrate.Finding {
		t.Helper()
		cfg := &fixturemigrate.Config{Models: map[string]*fixturemigrate.Model{"Any": m}}
		if err := cfg.Prepare(); err != nil {
			t.Fatal(err)
		}
		tables, err := dbschema.Load(ctx, db, "public")
		if err != nil {
			t.Fatal(err)
		}
		snap := &fixturemigrate.Snapshot{Order: []string{"Any"}}
		if err := fixturemigrate.LintKeys(ctx, db, cfg, snap, tables); err != nil {
			t.Fatal(err)
		}
		return snap.Findings
	}
	if f := lint(&fixturemigrate.Model{Table: "kl_any", Key: []string{"plan_id"},
		KeyAnyOf: [][]string{{"feature_id", "addon_id"}}}); len(f) != 0 {
		t.Fatalf("key_any_of under one partial index per column: %+v", f)
	}
	run(t, db, "DROP INDEX kl_any_addon")
	f := lint(&fixturemigrate.Model{Table: "kl_any", Key: []string{"plan_id"},
		KeyAnyOf: [][]string{{"feature_id", "addon_id"}}})
	if len(f) != 1 || f[0].Row != "key [plan_id, addon_id]" || f[0].Detail != "UNIQUE (plan_id, feature_id) "+
		"WHERE feature_id IS NOT NULL is over more columns than key [plan_id, addon_id], so two rows may share "+
		"plan_id and addon_id: CREATE UNIQUE INDEX ON kl_any (plan_id, addon_id) WHERE addon_id IS NOT NULL" {
		t.Fatalf("without the addon index: %+v", f)
	}
}

// The lint reads in the caller's transaction and leaves it as it was: no
// statement prepared, no setting changed, still usable after an
// expression PostgreSQL refused. A pooled connection would otherwise carry
// what it prepared into somebody else's session.
func TestTheKeyLintLeavesTheTransactionAsItWas(t *testing.T) {
	db, _ := keyDB(t)
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	tables, err := dbschema.Load(ctx, tx, "public")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		table  string
		key    []string
		filter string
	}{
		{"kl_partial", []string{"code", "status"}, fixturemigrate.RowFilter("status = 'active'", "")},
		{"kl_outside", []string{"code"}, ""},
		{"kl_lower_null", []string{"email"}, ""},
		// A where PostgreSQL refuses, naming a column the table lacks.
		{"kl_partial", []string{"code", "status"}, fixturemigrate.RowFilter("nosuch = 1", "")},
	} {
		if _, err := fixturemigrate.KeyBacked(ctx, tx, tables["public."+c.table], c.key, c.filter); err != nil {
			t.Fatalf("%s: %v", c.table, err)
		}
	}
	var prepared int
	var seqscan, cacheMode string
	if err := tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM pg_prepared_statements WHERE name LIKE 'bfm%'), "+
		"current_setting('enable_seqscan'), current_setting('plan_cache_mode')").Scan(&prepared, &seqscan,
		&cacheMode); err != nil {
		t.Fatalf("the transaction is not usable: %v", err)
	}
	if prepared != 0 || seqscan != "on" || cacheMode != "auto" {
		t.Fatalf("left behind: %d prepared statements, enable_seqscan %s, plan_cache_mode %s", prepared, seqscan,
			cacheMode)
	}
	// A where the planner cannot be asked about leaves the verdict
	// undecided, which says so.
	v, err := fixturemigrate.KeyBacked(ctx, tx, tables["public.kl_partial"], []string{"code", "status"},
		fixturemigrate.RowFilter("status = 'x' AND nosuch = 1", ""))
	if err != nil || v.Backed || !v.Unsure || !strings.Contains(v.Detail, "PostgreSQL's planner could not be "+
		"asked: column \"nosuch\" does not exist") {
		t.Fatalf("%+v, %v", v, err)
	}
}

// E12 and E13: an invalid index is no unique key, and an exclusion
// constraint WITH = is one, for everything that reads Uniques: scaffold's
// guess and the order of changes.
func TestUniquesTakesValidIndexesAndExclusionConstraints(t *testing.T) {
	db, _ := keyDB(t)
	tables, err := dbschema.Load(context.Background(), db, "public")
	if err != nil {
		t.Fatal(err)
	}
	has := func(table string, cols ...string) bool {
		for _, u := range tables["public."+table].Uniques {
			if strings.Join(u, ",") == strings.Join(cols, ",") {
				return true
			}
		}
		return false
	}
	switch {
	case has("kl_invalid", "code"):
		t.Fatal("an invalid index is in Uniques")
	case !has("kl_excl", "code"):
		t.Fatal("EXCLUDE (code WITH =) is not in Uniques")
	case has("kl_excl_range", "during"):
		t.Fatal("EXCLUDE (during WITH &&) is in Uniques")
	case !has("kl_include", "code") || has("kl_include", "code", "label"):
		t.Fatalf("an INCLUDE column is in Uniques: %v", tables["public.kl_include"].Uniques)
	}
	var invalid *dbschema.KeyIndex
	for i, k := range tables["public.kl_invalid"].KeyIndexes {
		if k.Name == "kl_invalid_code" {
			invalid = &tables["public.kl_invalid"].KeyIndexes[i]
		}
	}
	if invalid == nil || invalid.Valid {
		t.Fatalf("KeyIndexes: %+v", tables["public.kl_invalid"].KeyIndexes)
	}
}

// E7 and E8: scaffold keys a table on a partial, an expression or an
// exclusion index, marks a nullable key column, and passes over an invalid
// index, saying so.
func TestScaffoldGuessesKeysFromWhatTheCatalogHas(t *testing.T) {
	db, version := keyDB(t)
	tables, err := dbschema.Load(context.Background(), db, "public")
	if err != nil {
		t.Fatal(err)
	}
	only := []string{"kl_partial", "kl_lower", "kl_excl", "kl_null", "kl_coal", "kl_invalid"}
	if version >= 150000 {
		only = append(only, "kl_nnd")
	}
	text := string(fixturemigrate.Scaffold(tables, only, "public", fixturemigrate.ScaffoldOptions{}))
	flat := strings.Join(strings.Fields(strings.NewReplacer("#", " ").Replace(text)), " ")
	for _, want := range []string{
		"KlPartial: GUESS: proposed",
		// A live-rows index is a table bun soft-deletes, which soft_delete
		// says rather than a where.
		"GUESS: taken from UNIQUE (code) WHERE deleted_at IS NULL, which holds only where deleted_at IS NULL: the " +
			"live rows of a table bun soft-deletes, which the soft_delete below says.",
		"key: [code]",
		"GUESS: UNIQUE (code) WHERE deleted_at IS NULL holds only where deleted_at IS NULL, which is how a table " +
			"keeps the rows bun's soft delete deleted",
		"soft_delete: deleted_at",
		"GUESS: UNIQUE (lower(email)) backs key [email] and is stricter",
		"GUESS: taken from EXCLUDE (code WITH =), an exclusion constraint",
		"GUESS: parent_id is nullable, and UNIQUE (parent_id, code) holds NULLs distinct",
		"GUESS: UNIQUE (COALESCE(parent_id, 0",
		"backs key [parent_id, code] and is stricter",
		"public.kl_invalid has no valid unique index besides its primary key (kl_invalid_code is invalid)",
		"GUESS: unique index kl_invalid_code is invalid",
		"key_index: error",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("scaffold is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(flat, "public.kl_partial has no unique index") || strings.Contains(flat,
		"public.kl_lower has no unique index") || strings.Contains(flat, "public.kl_excl has no unique index") {
		t.Errorf("a table with a key to guess is commented out:\n%s", text)
	}
	if version >= 150000 && !strings.Contains(flat, "KlNnd: GUESS: proposed") {
		t.Errorf("no KlNnd:\n%s", text)
	}
}

// keyLintConfig is the spec's c4/c5 project: a key with no index, one under
// an index over more columns, one with a nullable column held distinct, one
// under lower(), and one under a live-rows index.
const keyLintConfig = `fixture: fixtures/fixture.yml
out: migrations
package: migrations
seed_guard_table: kl_plans
database: env:BFM_TEST_DSN
models:
  Plan:
    table: kl_plans
    key: [name]
  Feature:
    table: kl_features
    key: [plan_id, code]
    references:
      plan_id: Plan
  Tag:
    table: kl_tags
    key: [code]
  Category:
    table: kl_categories
    key: [parent_id, code]
    ref: code
    references:
      parent_id: Category
  Admin:
    table: kl_admins
    key: [email]
    ref: email
  Region:
    table: kl_regions
    key: [code]
    ref: code
`

const keyLintFixture = `- model: Plan
  rows:
    - {_id: free, id: 1, name: free}
    - {_id: team, id: 2, name: team}
- model: Feature
  rows:
    - {id: 1, plan_id: '{{ $.Plan.free.ID }}', code: api}
    - {id: 2, plan_id: '{{ $.Plan.team.ID }}', code: api}
- model: Tag
  rows:
    - {id: 1, code: hot, plan_id: 1}
- model: Category
  rows:
    - {_id: root, id: 1, parent_id: ~, code: root}
- model: Admin
  rows:
    - {id: 1, email: ann@example.com}
- model: Region
  rows:
    - {id: 1, code: eu}
`

func keyLintCLI(t *testing.T) (*cli, *bun.DB) {
	t.Helper()
	db := connect(t)
	run(t, db, "DROP TABLE IF EXISTS kl_features, kl_tags, kl_categories, kl_admins, kl_regions, kl_plans, "+
		"bun_migrations, bun_migration_locks CASCADE",
		`CREATE TABLE kl_plans (id bigint PRIMARY KEY, name text NOT NULL UNIQUE)`,
		`CREATE TABLE kl_features (id bigint PRIMARY KEY, plan_id bigint NOT NULL REFERENCES kl_plans (id),
			code text NOT NULL)`,
		`CREATE TABLE kl_tags (id bigint PRIMARY KEY, code text NOT NULL, plan_id bigint NOT NULL, UNIQUE (code, plan_id))`,
		`CREATE TABLE kl_categories (id bigint PRIMARY KEY, parent_id bigint REFERENCES kl_categories (id),
			code text NOT NULL, UNIQUE (parent_id, code))`,
		`CREATE TABLE kl_admins (id bigint PRIMARY KEY, email text NOT NULL)`,
		`CREATE UNIQUE INDEX kl_admins_email_lower ON kl_admins (lower(email))`,
		`CREATE TABLE kl_regions (id bigint PRIMARY KEY, code text NOT NULL, deleted_at timestamptz)`,
		`CREATE UNIQUE INDEX kl_regions_code_live ON kl_regions (code) WHERE deleted_at IS NULL`,
		`INSERT INTO kl_plans VALUES (1, 'free'), (2, 'team')`,
		`INSERT INTO kl_features VALUES (1, 1, 'api'), (2, 2, 'api')`,
		`INSERT INTO kl_tags VALUES (1, 'hot', 1)`,
		`INSERT INTO kl_categories VALUES (1, NULL, 'root')`,
		`INSERT INTO kl_admins VALUES (1, 'ann@example.com')`,
		`INSERT INTO kl_regions VALUES (1, 'eu', NULL)`)
	c := buildCLI(t)
	c.write("fixture-migrate.yml", keyLintConfig)
	c.write("fixtures/fixture.yml", keyLintFixture)
	return c, db
}

// E9: check and generate say which keys no index backs, as warnings by
// default and as errors under key_index: error; a model can set its own.
func TestCheckSaysWhichKeysNoIndexBacks(t *testing.T) {
	c, _ := keyLintCLI(t)
	c.must(0, "baseline")
	out := c.must(0, "check")
	for _, want := range []string{
		"Unbacked key:",
		"Category key [parent_id, code]: parent_id is nullable and UNIQUE (parent_id, code) holds NULLs distinct",
		"Feature key [plan_id, code]: no unique index or constraint backs key [plan_id, code]: the database lets a " +
			"second row with this key in, and every change to it then fails as a duplicate key. CREATE UNIQUE INDEX " +
			"ON kl_features (plan_id, code)",
		"Region key [code]: UNIQUE (code) WHERE deleted_at IS NULL holds only where deleted_at IS NULL, and this " +
			"model reads every row",
		// Category references itself, by code, which its index over
		// (parent_id, code) leaves two rows free to share.
		"Category ref code: UNIQUE (parent_id, code) is over more columns than ref code, so two rows may share " +
			"code: CREATE UNIQUE INDEX ON kl_categories (code)\n",
		"Tag key [code]: UNIQUE (code, plan_id) is over more columns than key [code]",
		"the database and fixtures/fixture.yml agree",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("check is missing %q:\n%s", want, out)
		}
	}
	// Plan's ref column is its key, and lower(email) backs Admin's.
	if strings.Contains(out, "Admin key") || strings.Contains(out, "Plan ref") || strings.Contains(out, "Plan key") {
		t.Errorf("check reports a backed key:\n%s", out)
	}
	jsonOut := c.json(0, "check")
	if !strings.Contains(jsonOut, `"kind": "unbacked key"`) || !strings.Contains(jsonOut, `"level": "warn"`) {
		t.Errorf("check -json:\n%s", jsonOut)
	}

	c.write("fixture-migrate.yml", strings.Replace(keyLintConfig, "models:\n",
		"policy:\n  key_index: error\nmodels:\n", 1))
	c.must(3, "check")
	c.write("fixtures/fixture.yml", keyLintFixture+"    - {id: 2, code: us}\n")
	out = c.must(2, "generate", "-name", "us", "-dry-run")
	if !strings.Contains(out, "unbacked key: Feature key [plan_id, code]: no unique index") {
		t.Errorf("generate under key_index: error:\n%s", out)
	}
	out = c.must(2, "sync")
	if !strings.Contains(out, "unbacked key: Feature key [plan_id, code]") {
		t.Errorf("sync under key_index: error:\n%s", out)
	}
	// A model can ignore its own.
	cfg := strings.Replace(keyLintConfig, "models:\n", "policy:\n  key_index: error\nmodels:\n", 1)
	for _, model := range []string{"Feature", "Tag", "Category", "Region"} {
		cfg = strings.Replace(cfg, "  "+model+":\n", "  "+model+":\n    key_index: ignore\n", 1)
	}
	c.write("fixture-migrate.yml", cfg)
	c.write("fixtures/fixture.yml", keyLintFixture)
	if out := c.must(0, "check"); strings.Contains(out, "Unbacked key") {
		t.Errorf("key_index: ignore per model:\n%s", out)
	}
}

// E10: two keys the tool tells apart that an index stricter than the key
// holds equal are a duplicate key before anything is written, not a raw
// 23505 in the deploy.
func TestAStricterIndexMakesTwoKeysOne(t *testing.T) {
	c, db := keyLintCLI(t)
	c.must(0, "baseline")
	text := strings.Replace(keyLintFixture, "    - {id: 1, email: ann@example.com}\n",
		"    - {id: 1, email: ann@example.com}\n    - {id: 2, email: Ann@example.com}\n"+
			"    - {id: 3, email: bob@example.com}\n", 1)
	c.write("fixtures/fixture.yml", text)
	want := "Admin/email=ann@example.com: UNIQUE (lower(email)) holds the natural keys email=ann@example.com and " +
		"email=Ann@example.com equal, so dbfixture cannot load these 2 rows (1, 2), and a migration inserting " +
		"one after the other fails on the index"
	if out := c.must(3, "check"); !strings.Contains(out, want) {
		t.Fatalf("check:\n%s", out)
	}
	if out := c.must(2, "generate", "-name", "admins", "-dry-run"); !strings.Contains(out, want) {
		t.Fatalf("generate:\n%s", out)
	}
	if out := c.must(2, "sync", "-yes"); !strings.Contains(out, want) {
		t.Fatalf("sync:\n%s", out)
	}
	if got := scan[int64](t, db, "SELECT count(*) FROM kl_admins"); got != 1 {
		t.Fatalf("sync wrote %d admins", got)
	}
}

// E11: the duplicate-key message on a table that has an index which let
// the second row in names it and says why, and asks for a unique index only
// where there is none.
func TestADuplicateKeyMessageNamesTheIndex(t *testing.T) {
	c, db := keyLintCLI(t)
	run(t, db, `INSERT INTO kl_categories VALUES (2, NULL, 'root')`,
		`INSERT INTO kl_features VALUES (3, 1, 'api')`,
		`UPDATE kl_regions SET deleted_at = now()`,
		`INSERT INTO kl_regions VALUES (2, 'eu', NULL)`)
	out := c.must(3, "check")
	for _, want := range []string{
		"Category/code=root/parent_id=NULL: this natural key is held by 2 rows (1, 2), so no lookup by it can tell " +
			"them apart: UNIQUE (parent_id, code) holds NULLs distinct, and lets in a second row with parent_id " +
			"NULL: declare it NULLS NOT DISTINCT (PostgreSQL 15 and later), or make parent_id NOT NULL, or add a " +
			"column to key",
		"Region/code=eu: this natural key is held by 2 rows (1, 2), so no lookup by it can tell them apart: UNIQUE " +
			"(code) WHERE deleted_at IS NULL holds only where deleted_at IS NULL, and lets in the rows outside it",
		"Feature/code=api/plan_id=Plan(free): this natural key is held by 2 rows (1, 3), so no lookup by it can " +
			"tell them apart: give the table a unique index, or add a column to key",
		// The index over (parent_id, code) never meant code to be unique.
		"Category code=root: 2 rows share this code (1, 2), and every generated reference resolves with WHERE " +
			"code = ?, so it cannot name one of them: add a unique index on code",
		"Category/code=root/parent_id=NULL: the natural key is not unique (2 rows before, 1 row after) and the " +
			"rows differ: hand-write the migration; UNIQUE (parent_id, code) holds NULLs distinct",
		"Feature/code=api/plan_id=Plan(free): the natural key is not unique (2 rows before, 1 row after) and the " +
			"rows differ: hand-write the migration, and give the table a unique index",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("check is missing %q:\n%s", want, out)
		}
	}
}

// plan notes a key no index backs, for each pending fixture migration, and
// fails on none.
func TestPlanNotesAKeyNoIndexBacks(t *testing.T) {
	c, _ := keyLintCLI(t)
	c.must(0, "baseline")
	text := strings.Replace(keyLintFixture, "    - {id: 2, plan_id: '{{ $.Plan.team.ID }}', code: api}\n",
		"    - {id: 2, plan_id: '{{ $.Plan.team.ID }}', code: api}\n"+
			"    - {id: 3, plan_id: '{{ $.Plan.team.ID }}', code: sso}\n", 1)
	c.write("fixtures/fixture.yml", text)
	c.must(0, "generate", "-name", "sso", "-at", "20300101000000")
	out := c.must(0, "plan")
	if !strings.Contains(out, "note: Feature: no unique index or constraint backs key [code, plan_id]: the "+
		"database lets a second row with this key in") {
		t.Fatalf("plan:\n%s", out)
	}
}
