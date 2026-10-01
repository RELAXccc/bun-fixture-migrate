package dbtest_test

// The same full cycle over the shapes master data comes in: a join table with
// a composite key and no id, uuid keys, a tree that points into itself, names
// that are reserved words or mixed case, a schema other than public, and
// strings built to break quoting, with an enum.
//
// For each: the old and the new fixture file are seeded with the real
// dbfixture; the change set goes through the generated Go file and is read
// back from it, as status and plan read it; it is applied, applied again,
// reverted, and every database state is compared with what dbfixture seeds;
// and the new state is checked against its file, by Sync as well, exported,
// and loaded back. The catalog is read from the schemas the configuration
// says, as the commands read it.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
)

type variant struct {
	ddl       []string
	models    []any
	config    string
	old, next string
	// dump lists the tables whose rows make up a database state.
	dump []string
	// noRerun, when set, says why a second Apply is not run: the run time
	// cannot run that set twice yet.
	noRerun string
}

func (v variant) reset(t *testing.T, db *bun.DB) {
	t.Helper()
	run(t, db, v.ddl...)
}

func (v variant) state(t *testing.T, db *bun.DB) string {
	t.Helper()
	var parts []string
	for _, table := range v.dump {
		parts = append(parts, table+":\n"+scan[string](t, db,
			"SELECT coalesce(string_agg(to_jsonb(t)::text, E'\\n' ORDER BY to_jsonb(t)::text), '') FROM "+table+" t"))
	}
	return strings.Join(parts, "\n")
}

func (v variant) run(t *testing.T) {
	db := connect(t)
	db.RegisterModel(v.models...)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "c.yml")
	if err := os.WriteFile(path, []byte(v.config), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := fixturemigrate.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	v.reset(t, db)
	loadFixture(t, db, v.next)
	wantNext := v.state(t, db)
	v.reset(t, db)
	loadFixture(t, db, v.old)
	wantOld := v.state(t, db)

	res, err := fixturemigrate.Compute(cfg, fixtureSnapshot(t, cfg, v.old, "old"), fixtureSnapshot(t, cfg, v.next, "new"))
	if err != nil || len(res.Refusals) != 0 {
		t.Fatalf("Compute: %v %+v", err, res.Refusals)
	}
	if len(res.Changes) == 0 {
		t.Fatal("the variant changes nothing")
	}
	src, err := fixturemigrate.Render(cfg, "variant", "20260921120000", res)
	if err != nil {
		t.Fatal(err)
	}
	set, ok, err := fixturemigrate.ReadChangeSet(src)
	if err != nil || !ok {
		t.Fatalf("ReadChangeSet: %v\n%s", err, src)
	}

	apply := func(f func(context.Context, bun.IDB, fixturechange.Set, ...fixtureapply.Option) error) []fixtureapply.Outcome {
		t.Helper()
		var out []fixtureapply.Outcome
		if err := f(ctx, db, set, quiet(), fixtureapply.WithReport(func(o fixtureapply.Outcome) {
			out = append(out, o)
		})); err != nil {
			t.Fatalf("%v\n%s", err, src)
		}
		return out
	}
	apply(fixtureapply.Apply)
	if got := v.state(t, db); got != wantNext {
		t.Fatalf("the migration does not reproduce the new file\n got %s\nwant %s\n%s", got, wantNext, src)
	}
	if v.noRerun != "" {
		t.Logf("not run twice: %s", v.noRerun)
	} else {
		for _, o := range apply(fixtureapply.Apply) {
			if o.Index >= 0 && o.Status != fixtureapply.StatusUnchanged {
				t.Fatalf("a second run changed something: %+v", o)
			}
		}
	}
	apply(fixtureapply.Revert)
	if got := v.state(t, db); got != wantOld {
		t.Fatalf("revert does not restore the old file\n got %s\nwant %s", got, wantOld)
	}

	v.reset(t, db)
	loadFixture(t, db, v.next)
	synced, err := fixturemigrate.Sync(ctx, db, cfg, []fixturemigrate.FixtureFile{{Path: "new", Data: []byte(v.next)}},
		fixturemigrate.SyncOptions{DryRun: true})
	if err != nil || len(synced.Outcomes) != 0 {
		t.Fatalf("Sync of the database seeded from the file: %v %+v", err, synced)
	}
	head := fixtureSnapshot(t, cfg, v.next, "new")
	var exported []byte
	readOnlyDo(t, db, func(tx bun.Tx, _ map[string]*dbschema.Table) {
		tables, err := dbschema.Load(ctx, tx, cfg.Schemas()...)
		if err != nil {
			t.Fatal(err)
		}
		if err := fixturemigrate.Canonicalize(ctx, tx, cfg, head, tables); err != nil {
			t.Fatal(err)
		}
		database, err := fixturemigrate.DatabaseSnapshot(ctx, tx, cfg, tables,
			fixturemigrate.SnapshotOptions{Columns: head.Columns, Order: head.Order})
		if err != nil {
			t.Fatal(err)
		}
		check, err := fixturemigrate.Check(cfg, database, head)
		if err != nil {
			t.Fatal(err)
		}
		if check.Drifted() {
			t.Fatalf("the database seeded from the file disagrees with it:\n%s", strings.Join(check.Lines(), "\n"))
		}
		all, err := fixturemigrate.DatabaseSnapshot(ctx, tx, cfg, tables, fixturemigrate.SnapshotOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if exported, err = fixturemigrate.Export(cfg, all, tables, nil); err != nil {
			t.Fatal(err)
		}
	})
	v.reset(t, db)
	loadFixture(t, db, string(exported))
	if got := v.state(t, db); got != wantNext {
		t.Fatalf("the export does not load back\n got %s\nwant %s\n%s", got, wantNext, exported)
	}
}

// A join table: two references for a key, a composite primary key, no id.
type VPlan struct {
	bun.BaseModel `bun:"table:v_plans"`
	ID            int64  `bun:"id,pk"`
	Name          string `bun:"name,notnull,unique"`
}

type VFeature struct {
	bun.BaseModel `bun:"table:v_features"`
	ID            int64  `bun:"id,pk"`
	Code          string `bun:"code,notnull,unique"`
}

type VPlanFeature struct {
	bun.BaseModel `bun:"table:v_plan_features"`
	PlanID        int64 `bun:"plan_id,pk"`
	FeatureID     int64 `bun:"feature_id,pk"`
	Quota         int64 `bun:"quota,notnull"`
}

func TestVariantJoinTableWithACompositeKey(t *testing.T) {
	head := `- model: VPlan
  rows:
    - {_id: free, id: 1, name: free}
    - {_id: team, id: 2, name: team}
- model: VFeature
  rows:
    - {_id: api, id: 1, code: api}
    - {_id: sso, id: 2, code: sso}
- model: VPlanFeature
  rows:
`
	variant{
		ddl: []string{"DROP TABLE IF EXISTS v_plan_features, v_features, v_plans",
			"CREATE TABLE v_plans (id bigint PRIMARY KEY, name text UNIQUE NOT NULL)",
			"CREATE TABLE v_features (id bigint PRIMARY KEY, code text UNIQUE NOT NULL)",
			"CREATE TABLE v_plan_features (plan_id bigint REFERENCES v_plans, feature_id bigint REFERENCES v_features, " +
				"quota bigint NOT NULL, PRIMARY KEY (plan_id, feature_id))"},
		models: []any{(*VPlan)(nil), (*VFeature)(nil), (*VPlanFeature)(nil)},
		config: `models:
  VPlan: {table: v_plans, key: [name]}
  VFeature: {table: v_features, ref: code, key: [code]}
  VPlanFeature:
    table: v_plan_features
    key: [plan_id, feature_id]
    references: {plan_id: VPlan, feature_id: VFeature}
`,
		old: head + `    - {plan_id: '{{ $.VPlan.free.ID }}', feature_id: '{{ $.VFeature.api.ID }}', quota: 10}
    - {plan_id: '{{ $.VPlan.team.ID }}', feature_id: '{{ $.VFeature.api.ID }}', quota: 100}
`,
		next: head + `    - {plan_id: '{{ $.VPlan.team.ID }}', feature_id: '{{ $.VFeature.api.ID }}', quota: 500}
    - {plan_id: '{{ $.VPlan.team.ID }}', feature_id: '{{ $.VFeature.sso.ID }}', quota: 1}
`,
		dump: []string{"v_plan_features"},
	}.run(t)
}

// uuid keys: no sequence, and the database writes them in lower case.
type VTenant struct {
	bun.BaseModel `bun:"table:v_tenants"`
	ID            string `bun:"id,pk,type:uuid"`
	Slug          string `bun:"slug,notnull,unique"`
}

type VSetting struct {
	bun.BaseModel `bun:"table:v_settings"`
	ID            string `bun:"id,pk,type:uuid"`
	TenantID      string `bun:"tenant_id,type:uuid,notnull"`
	Key           string `bun:"key,notnull"`
	Value         string `bun:"value,notnull"`
}

func TestVariantUUIDKeys(t *testing.T) {
	head := `- model: VTenant
  rows:
    - {_id: acme, id: "6F9619FF-8B86-D011-B42D-00C04FC964FF", slug: acme}
- model: VSetting
  rows:
`
	variant{
		// gen_random_uuid is core from PostgreSQL 13; before, pgcrypto has it.
		ddl: []string{"CREATE EXTENSION IF NOT EXISTS pgcrypto", "DROP TABLE IF EXISTS v_settings, v_tenants",
			"CREATE TABLE v_tenants (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), slug text UNIQUE NOT NULL)",
			"CREATE TABLE v_settings (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), " +
				"tenant_id uuid NOT NULL REFERENCES v_tenants, key text NOT NULL, value text NOT NULL, UNIQUE (tenant_id, key))"},
		models: []any{(*VTenant)(nil), (*VSetting)(nil)},
		config: `models:
  VTenant: {table: v_tenants, ref: slug, key: [slug]}
  VSetting:
    table: v_settings
    key: [tenant_id, key]
    references: {tenant_id: VTenant}
`,
		old: head + `    - {id: "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11", tenant_id: '{{ $.VTenant.acme.ID }}', key: theme, value: dark}
`,
		next: head + `    - {id: "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11", tenant_id: '{{ $.VTenant.acme.ID }}', key: theme, value: light}
    - {id: "b0eebc99-9c0b-4ef8-bb6d-6bb9bd380a12", tenant_id: '{{ $.VTenant.acme.ID }}', key: locale, value: de}
`,
		dump: []string{"v_tenants", "v_settings"},
	}.run(t)
}

// A tree: a category points at its parent in the same table.
type VCategory struct {
	bun.BaseModel `bun:"table:v_categories"`
	ID            int64  `bun:"id,pk"`
	Name          string `bun:"name,notnull,unique"`
	ParentID      *int64 `bun:"parent_id"`
}

func TestVariantASelfReferencingTree(t *testing.T) {
	variant{
		ddl: []string{"DROP TABLE IF EXISTS v_categories",
			"CREATE TABLE v_categories (id bigint PRIMARY KEY, name text UNIQUE NOT NULL, " +
				"parent_id bigint REFERENCES v_categories)"},
		models: []any{(*VCategory)(nil)},
		config: `models:
  VCategory:
    table: v_categories
    key: [name]
    references: {parent_id: VCategory}
`,
		old: `- model: VCategory
  rows:
    - {_id: root, id: 1, name: root, parent_id: ~}
    - {_id: tools, id: 2, name: tools, parent_id: '{{ $.VCategory.root.ID }}'}
`,
		next: `- model: VCategory
  rows:
    - {_id: root, id: 1, name: root, parent_id: ~}
    - {_id: garden, id: 3, name: garden, parent_id: '{{ $.VCategory.root.ID }}'}
    - {_id: hoses, id: 4, name: hoses, parent_id: '{{ $.VCategory.garden.ID }}'}
`,
		dump: []string{"v_categories"},
	}.run(t)
}

// A tree whose root came after its leaves: the database returns a child
// before its parent, in id order. It has to be read all the same, checked
// against its file, and exported in an order dbfixture can load.
func TestVariantATreeWhoseParentsCameLater(t *testing.T) {
	variant{
		ddl: []string{"DROP TABLE IF EXISTS v_categories",
			"CREATE TABLE v_categories (id bigint PRIMARY KEY, name text UNIQUE NOT NULL, " +
				"parent_id bigint REFERENCES v_categories)"},
		models: []any{(*VCategory)(nil)},
		config: `models:
  VCategory:
    table: v_categories
    key: [name]
    references: {parent_id: VCategory}
`,
		old: `- model: VCategory
  rows:
    - {_id: root, id: 2, name: root, parent_id: ~}
    - {_id: leaf, id: 1, name: leaf, parent_id: '{{ $.VCategory.root.ID }}'}
    - {_id: tools, id: 9, name: tools, parent_id: ~}
    - {_id: api, id: 10, name: api, parent_id: '{{ $.VCategory.tools.ID }}'}
`,
		next: `- model: VCategory
  rows:
    - {_id: root, id: 2, name: root, parent_id: ~}
    - {_id: tools, id: 9, name: tools, parent_id: ~}
    - {_id: branch, id: 5, name: branch, parent_id: '{{ $.VCategory.tools.ID }}'}
    - {_id: leaf, id: 1, name: leaf, parent_id: '{{ $.VCategory.branch.ID }}'}
    - {_id: twig, id: 3, name: twig, parent_id: '{{ $.VCategory.leaf.ID }}'}
    - {_id: api, id: 10, name: api, parent_id: '{{ $.VCategory.tools.ID }}'}
`,
		dump: []string{"v_categories"},
	}.run(t)
}

// A currency keyed by its ISO code, which is its primary key as well, and a
// status table whose first row is keyed by "0". Both are ids like any other:
// the natural key can be the id, and a zero only means "no id" in a serial
// model.
type VCurrency struct {
	bun.BaseModel `bun:"table:v_currencies"`
	Code          string `bun:"code,pk"`
	Label         string `bun:"label,notnull"`
}

type VStatus struct {
	bun.BaseModel `bun:"table:v_statuses"`
	Code          string `bun:"code,pk"`
	Label         string `bun:"label,notnull,unique"`
}

type VPrice struct {
	bun.BaseModel `bun:"table:v_prices"`
	ID            int64  `bun:"id,pk"`
	Name          string `bun:"name,notnull,unique"`
	CurrencyCode  string `bun:"currency_code,notnull"`
	StatusCode    string `bun:"status_code,notnull"`
}

func TestVariantAKeyThatIsTheIDAndAnIDThatIsZero(t *testing.T) {
	head := `- model: VCurrency
  rows:
    - {_id: eur, code: EUR, label: Euro}
    - {_id: usd, code: USD, label: Dollar}
- model: VStatus
  rows:
    - {_id: s0, code: "0", label: pending}
    - {_id: s1, code: "1", label: done}
`
	variant{
		ddl: []string{"DROP TABLE IF EXISTS v_prices, v_currencies, v_statuses",
			"CREATE TABLE v_currencies (code text PRIMARY KEY, label text NOT NULL)",
			"CREATE TABLE v_statuses (code text PRIMARY KEY, label text UNIQUE NOT NULL)",
			"CREATE TABLE v_prices (id bigint PRIMARY KEY, name text UNIQUE NOT NULL, " +
				"currency_code text NOT NULL REFERENCES v_currencies, status_code text NOT NULL REFERENCES v_statuses)"},
		models: []any{(*VCurrency)(nil), (*VStatus)(nil), (*VPrice)(nil)},
		config: `models:
  VCurrency: {table: v_currencies, id: code, key: [code], ref: code}
  VStatus: {table: v_statuses, id: code, key: [label], ref: label}
  VPrice:
    table: v_prices
    key: [name]
    references: {currency_code: VCurrency, status_code: VStatus}
`,
		old: head + `- model: VPrice
  rows:
    - {id: 1, name: a, currency_code: '{{ $.VCurrency.eur.Code }}', status_code: '{{ $.VStatus.s0.Code }}'}
`,
		next: strings.Replace(head, "label: Dollar", "label: US Dollar", 1) + `- model: VPrice
  rows:
    - {id: 1, name: a, currency_code: '{{ $.VCurrency.usd.Code }}', status_code: '{{ $.VStatus.s0.Code }}'}
    - {id: 2, name: b, currency_code: '{{ $.VCurrency.eur.Code }}', status_code: '{{ $.VStatus.s1.Code }}'}
`,
		dump: []string{"v_currencies", "v_statuses", "v_prices"},
	}.run(t)
}

// Effective-dated prices: closing the old price and opening the next one in
// one release. A partial unique index allows one open price per plan, so the
// old one has to be closed first.
type VPricePlan struct {
	bun.BaseModel `bun:"table:v_price_plans"`
	ID            int64  `bun:"id,pk"`
	Code          string `bun:"code,notnull,unique"`
}

type VDatedPrice struct {
	bun.BaseModel `bun:"table:v_dated_prices"`
	ID            int64      `bun:"id,pk"`
	PlanID        int64      `bun:"plan_id,notnull"`
	ValidFrom     time.Time  `bun:"valid_from,notnull"`
	ValidTo       *time.Time `bun:"valid_to"`
	Cents         int64      `bun:"cents,notnull"`
}

func TestVariantClosingAPriceAndOpeningTheNext(t *testing.T) {
	head := `- model: VPricePlan
  rows:
    - {_id: basic, id: 1, code: basic}
- model: VDatedPrice
  rows:
`
	variant{
		ddl: []string{"DROP TABLE IF EXISTS v_dated_prices, v_price_plans",
			"CREATE TABLE v_price_plans (id bigint PRIMARY KEY, code text UNIQUE NOT NULL)",
			"CREATE TABLE v_dated_prices (id bigint PRIMARY KEY, plan_id bigint NOT NULL REFERENCES v_price_plans, " +
				"valid_from timestamptz NOT NULL, valid_to timestamptz, cents bigint NOT NULL, UNIQUE (plan_id, valid_from))",
			"CREATE UNIQUE INDEX v_dated_prices_one_open ON v_dated_prices (plan_id) WHERE valid_to IS NULL"},
		models: []any{(*VPricePlan)(nil), (*VDatedPrice)(nil)},
		config: `models:
  VPricePlan: {table: v_price_plans, ref: code, key: [code]}
  VDatedPrice: {table: v_dated_prices, key: [plan_id, valid_from], references: {plan_id: VPricePlan}}
`,
		old: head + `    - {id: 1, plan_id: '{{ $.VPricePlan.basic.ID }}', valid_from: 2026-01-01T00:00:00Z, valid_to: ~, cents: 900}
`,
		next: head + `    - {id: 1, plan_id: '{{ $.VPricePlan.basic.ID }}', valid_from: 2026-01-01T00:00:00Z, valid_to: 2026-11-01T00:00:00Z, cents: 900}
    - {id: 2, plan_id: '{{ $.VPricePlan.basic.ID }}', valid_from: 2026-11-01T00:00:00Z, valid_to: ~, cents: 1200}
`,
		dump: []string{"v_dated_prices"},
	}.run(t)
}

// A model that leaves the file entirely: its rows go after the rows that
// point at them, although the new file does not mention the model at all.
type VGoneCurrency struct {
	bun.BaseModel `bun:"table:v_gone_currencies"`
	ID            int64  `bun:"id,pk"`
	Code          string `bun:"code,notnull,unique"`
}

type VGonePlan struct {
	bun.BaseModel `bun:"table:v_gone_plans"`
	ID            int64  `bun:"id,pk"`
	Name          string `bun:"name,notnull,unique"`
	CurrencyID    *int64 `bun:"currency_id"`
}

func TestVariantAModelThatLeavesTheFile(t *testing.T) {
	variant{
		ddl: []string{"DROP TABLE IF EXISTS v_gone_plans, v_gone_currencies",
			"CREATE TABLE v_gone_currencies (id bigint PRIMARY KEY, code text UNIQUE NOT NULL)",
			"CREATE TABLE v_gone_plans (id bigint PRIMARY KEY, name text UNIQUE NOT NULL, " +
				"currency_id bigint REFERENCES v_gone_currencies)"},
		models: []any{(*VGoneCurrency)(nil), (*VGonePlan)(nil)},
		config: `models:
  VGoneCurrency: {table: v_gone_currencies, ref: code, key: [code]}
  VGonePlan: {table: v_gone_plans, key: [name], references: {currency_id: VGoneCurrency}}
`,
		old: `- model: VGoneCurrency
  rows:
    - {_id: usd, id: 1, code: USD}
- model: VGonePlan
  rows:
    - {id: 1, name: a, currency_id: '{{ $.VGoneCurrency.usd.ID }}'}
    - {id: 2, name: keep, currency_id: ~}
`,
		next: `- model: VGonePlan
  rows:
    - {id: 2, name: keep, currency_id: ~}
`,
		dump: []string{"v_gone_currencies", "v_gone_plans"},
		noRerun: "a second run resolves the currency the deleted plan's guard names, which the first run " +
			"deleted, and fails instead of finding the plan already gone",
	}.run(t)
}

// A unique value moving from one row to another in one set.
type VSeat struct {
	bun.BaseModel `bun:"table:v_seats"`
	ID            int64  `bun:"id,pk"`
	Code          string `bun:"code,notnull,unique"`
	Slot          int64  `bun:"slot,notnull,unique"`
}

func TestVariantAUniqueValueMovesToAnotherRow(t *testing.T) {
	variant{
		ddl: []string{"DROP TABLE IF EXISTS v_seats",
			"CREATE TABLE v_seats (id bigint PRIMARY KEY, code text UNIQUE NOT NULL, slot bigint UNIQUE NOT NULL)"},
		models: []any{(*VSeat)(nil)},
		config: `models:
  VSeat: {table: v_seats, ref: code, key: [code]}
`,
		old:  "- model: VSeat\n  rows:\n    - {id: 1, code: b, slot: 3}\n    - {id: 2, code: a, slot: 1}\n    - {id: 3, code: c, slot: 4}\n",
		next: "- model: VSeat\n  rows:\n    - {id: 1, code: b, slot: 1}\n    - {id: 2, code: a, slot: 2}\n    - {id: 4, code: d, slot: 4}\n",
		dump: []string{"v_seats"},
	}.run(t)
}

// Names that are reserved words, and a mixed-case table and column.
type VOrder struct {
	bun.BaseModel `bun:"table:VOrder"`
	ID            int64  `bun:"id,pk"`
	Group         string `bun:"group,notnull,unique"`
	User          string `bun:"user,notnull"`
	Select        string `bun:"Select,notnull"`
}

func TestVariantReservedAndMixedCaseNames(t *testing.T) {
	variant{
		ddl: []string{`DROP TABLE IF EXISTS "VOrder"`,
			`CREATE TABLE "VOrder" (id bigint PRIMARY KEY, "group" text UNIQUE NOT NULL, "user" text NOT NULL, "Select" text NOT NULL)`},
		models: []any{(*VOrder)(nil)},
		config: `models:
  VOrder: {table: VOrder, ref: group, key: [group]}
`,
		old:  "- model: VOrder\n  rows:\n    - {id: 1, group: a, user: x, Select: s}\n    - {id: 2, group: b, user: y, Select: t}\n",
		next: "- model: VOrder\n  rows:\n    - {id: 1, group: a, user: z, Select: s}\n    - {id: 3, group: c, user: w, Select: u}\n",
		dump: []string{`"VOrder"`},
	}.run(t)
}

// A schema other than public.
type VProduct struct {
	bun.BaseModel `bun:"table:v_catalog.products"`
	ID            int64  `bun:"id,pk"`
	Sku           string `bun:"sku,notnull,unique"`
	Price         int64  `bun:"price,notnull"`
}

func TestVariantAnotherSchema(t *testing.T) {
	variant{
		ddl: []string{"DROP SCHEMA IF EXISTS v_catalog CASCADE", "CREATE SCHEMA v_catalog",
			"CREATE TABLE v_catalog.products (id bigint PRIMARY KEY, sku text UNIQUE NOT NULL, price bigint NOT NULL)"},
		models: []any{(*VProduct)(nil)},
		config: `seed_guard_table: v_catalog.products
models:
  VProduct: {table: v_catalog.products, ref: sku, key: [sku]}
`,
		old:  "- model: VProduct\n  rows:\n    - {id: 1, sku: A-1, price: 100}\n",
		next: "- model: VProduct\n  rows:\n    - {id: 1, sku: A-1, price: 150}\n    - {id: 2, sku: B-2, price: 5}\n",
		dump: []string{"v_catalog.products"},
	}.run(t)
}

// A table named without a schema in a configuration whose schema is not
// public. The migration runs on a connection whose search_path does not
// look there, so the change set has to name the schema.
type VRole struct {
	bun.BaseModel `bun:"table:v_app.roles"`
	ID            int64  `bun:"id,pk"`
	Code          string `bun:"code,notnull,unique"`
	Name          string `bun:"name,notnull"`
}

func TestVariantATableInTheConfiguredSchema(t *testing.T) {
	variant{
		ddl: []string{"DROP SCHEMA IF EXISTS v_app CASCADE", "CREATE SCHEMA v_app",
			"CREATE TABLE v_app.roles (id bigint PRIMARY KEY, code text UNIQUE NOT NULL, name text NOT NULL)"},
		models: []any{(*VRole)(nil)},
		config: `schema: v_app
models:
  VRole: {table: roles, ref: code, key: [code]}
`,
		old:  "- model: VRole\n  rows:\n    - {id: 1, code: admin, name: Admin}\n    - {id: 2, code: viewer, name: Viewer}\n",
		next: "- model: VRole\n  rows:\n    - {id: 1, code: admin, name: Admin}\n    - {id: 2, code: viewer, name: Read-only}\n",
		dump: []string{"v_app.roles"},
	}.run(t)
}

// Strings built to break quoting, bun's placeholders among them, and an enum.
type VLabel struct {
	bun.BaseModel `bun:"table:v_labels"`
	ID            int64  `bun:"id,pk"`
	Code          string `bun:"code,notnull,unique"`
	Text          string `bun:"text,notnull"`
	Mood          string `bun:"mood,type:v_mood,notnull"`
}

func TestVariantHostileStringsAndAnEnum(t *testing.T) {
	variant{
		ddl: []string{"DROP TABLE IF EXISTS v_labels", "DROP TYPE IF EXISTS v_mood",
			"CREATE TYPE v_mood AS ENUM ('happy', 'sad')",
			"CREATE TABLE v_labels (id bigint PRIMARY KEY, code text UNIQUE NOT NULL, text text NOT NULL, mood v_mood NOT NULL)"},
		models: []any{(*VLabel)(nil)},
		config: `models:
  VLabel: {table: v_labels, ref: code, key: [code]}
`,
		old: `- model: VLabel
  rows:
    - {id: 1, code: "a'b", text: "it's \"quoted\" \\ back", mood: happy}
    - {id: 2, code: "?0", text: "?name ? $1 -- not a comment /* nor this */", mood: sad}
`,
		next: `- model: VLabel
  rows:
    - {id: 1, code: "a'b", text: "line one\nline two\ttab", mood: sad}
    - {id: 2, code: "?0", text: "Ünïcødé 🚀 עברית", mood: sad}
    - {id: 3, code: "NULL", text: "~", mood: happy}
`,
		dump: []string{"v_labels"},
	}.run(t)
}
