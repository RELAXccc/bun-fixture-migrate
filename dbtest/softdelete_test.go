package dbtest_test

// Models bun soft-deletes, against the real dbfixture, bun's own migrator and
// the command: currencies whose code a full unique index holds for good, and
// plans whose name is unique among live rows only, with the application's
// subscriptions pointing at them. The fixture files seed rows soft-deleted,
// the database keeps rows an admin soft-deleted, and a migration soft-deletes
// a row that leaves the files and brings back one that returns.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
)

// SdCurrency spells soft delete the nullzero way, SdPlan the pointer way:
// both write NULL for a live row.
type SdCurrency struct {
	bun.BaseModel `bun:"table:sd_currencies"`
	ID            int64     `bun:"id,pk"`
	Code          string    `bun:"code,notnull"`
	Symbol        string    `bun:"symbol,notnull"`
	DeletedAt     time.Time `bun:"deleted_at,soft_delete,nullzero"`
}

type SdPlan struct {
	bun.BaseModel `bun:"table:sd_plans"`
	ID            int64      `bun:"id,pk,autoincrement"`
	Name          string     `bun:"name,notnull"`
	CurrencyID    int64      `bun:"currency_id,notnull"`
	PriceCents    int64      `bun:"price_cents,notnull"`
	DeletedAt     *time.Time `bun:"deleted_at,soft_delete"`
}

// SdSub is the application's: no master data.
type SdSub struct {
	bun.BaseModel `bun:"table:sd_subs"`
	ID            int64   `bun:"id,pk,autoincrement"`
	PlanID        int64   `bun:"plan_id,notnull"`
	Plan          *SdPlan `bun:"rel:belongs-to,join:plan_id=id"`
}

const sdConfig = `fixture: fixtures/fixture.yml
out: migrations
package: migrations
migrations_table: sd_migrations
migration_locks_table: sd_migrations_locks
seed_guard_table: sd_currencies
database: env:BFM_TEST_DSN
models:
  SdCurrency:
    table: sd_currencies
    key: [code]
    ref: code
    soft_delete: deleted_at
  SdPlan:
    table: sd_plans
    serial: true
    key: [name]
    references: {currency_id: SdCurrency}
    soft_delete: deleted_at
`

// sdV1 seeds the master data, with rows soft-deleted already: GBP and JPY,
// a legacy plan, and a pro plan.
const sdV1 = `- model: SdCurrency
  rows:
    - {_id: eur, id: 1, code: EUR, symbol: "€"}
    - {_id: usd, id: 2, code: USD, symbol: "$"}
    - {_id: gbp, id: 3, code: GBP, symbol: "£", deleted_at: 2026-03-01T00:00:00Z}
    - {_id: jpy, id: 5, code: JPY, symbol: "¥", deleted_at: 2026-04-01T00:00:00Z}
- model: SdPlan
  rows:
    - {_id: free, name: free, currency_id: '{{ $.SdCurrency.eur.ID }}', price_cents: 0}
    - {_id: team, name: team, currency_id: '{{ $.SdCurrency.eur.ID }}', price_cents: 2500}
    - {_id: legacy, name: legacy, currency_id: '{{ $.SdCurrency.usd.ID }}', price_cents: 900, deleted_at: 2026-02-01T00:00:00Z}
    - {_id: pro, name: pro, currency_id: '{{ $.SdCurrency.usd.ID }}', price_cents: 5000, deleted_at: 2026-01-01T00:00:00Z}
`

// sdV2: team leaves the files; legacy and GBP come back as they were; pro
// comes back with another price, which the live-rows index lets in beside the
// old row; JPY comes back with another symbol under another id, which the
// full unique index on code refuses.
const sdV2 = `- model: SdCurrency
  rows:
    - {_id: eur, id: 1, code: EUR, symbol: "€"}
    - {_id: usd, id: 2, code: USD, symbol: "$"}
    - {_id: gbp, id: 3, code: GBP, symbol: "£"}
    - {_id: jpy, id: 6, code: JPY, symbol: "円"}
- model: SdPlan
  rows:
    - {_id: free, name: free, currency_id: '{{ $.SdCurrency.eur.ID }}', price_cents: 0}
    - {_id: legacy, name: legacy, currency_id: '{{ $.SdCurrency.usd.ID }}', price_cents: 900}
    - {_id: pro, name: pro, currency_id: '{{ $.SdCurrency.usd.ID }}', price_cents: 6000}
`

func sdDB(t *testing.T) *bun.DB {
	t.Helper()
	db := connect(t)
	db.RegisterModel((*SdCurrency)(nil), (*SdPlan)(nil), (*SdSub)(nil))
	run(t, db, "DROP TABLE IF EXISTS sd_subs, sd_plans, sd_currencies, sd_migrations, sd_migrations_locks, sd_audit",
		"CREATE TABLE sd_currencies (id bigint PRIMARY KEY, code text NOT NULL UNIQUE, symbol text NOT NULL, "+
			"deleted_at timestamptz)",
		"CREATE TABLE sd_plans (id bigserial PRIMARY KEY, name text NOT NULL, "+
			"currency_id bigint NOT NULL REFERENCES sd_currencies, price_cents bigint NOT NULL, deleted_at timestamptz)",
		"CREATE UNIQUE INDEX sd_plans_name_live ON sd_plans (name) WHERE deleted_at IS NULL",
		"CREATE TABLE sd_subs (id bigserial PRIMARY KEY, plan_id bigint NOT NULL REFERENCES sd_plans)")
	loadFixture(t, db, sdV1)
	// The application's subscriptions, on team and on the legacy plan.
	run(t, db, "INSERT INTO sd_subs (plan_id) SELECT id FROM sd_plans WHERE name IN ('team', 'legacy') ORDER BY id")
	return db
}

func sdCLI(t *testing.T) *cli {
	t.Helper()
	c := buildCLI(t)
	c.write("fixture-migrate.yml", sdConfig)
	c.write("fixtures/fixture.yml", sdV1)
	return c
}

// sdState is the rows of both tables, soft-deleted or not, by id.
func sdState(t *testing.T, db *bun.DB) string {
	t.Helper()
	return scan[string](t, db, `SELECT concat_ws(' | ',
		(SELECT string_agg(id || ':' || code || '=' || symbol || CASE WHEN deleted_at IS NULL THEN '' ELSE '(deleted)' END,
			',' ORDER BY id) FROM sd_currencies),
		(SELECT string_agg(id || ':' || name || '=' || price_cents || CASE WHEN deleted_at IS NULL THEN '' ELSE '(deleted)' END,
			',' ORDER BY id) FROM sd_plans))`)
}

// sdLive is what a column expression holds in the live rows of a table, in
// id order.
func sdLive(t *testing.T, db *bun.DB, expr, table string) string {
	t.Helper()
	return scan[string](t, db, "SELECT string_agg("+expr+", ',' ORDER BY id) FROM "+table+" WHERE deleted_at IS NULL")
}

func contains(t *testing.T, what, out string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Fatalf("%s is missing %q:\n%s", what, want, out)
		}
	}
}

// What bun and dbfixture do with a soft-delete model, which the rest of this
// file builds on (spec §A.1, R1-R6): dbfixture writes deleted_at as the
// field holds it, bun reads live rows only, a delete is an UPDATE, a row
// pointing at a soft-deleted one loads without it, and a unique index over
// every row refuses a key a soft-deleted row holds where one over live rows
// takes it.
func TestSoftDeletePremisesOfBunAndDbfixture(t *testing.T) {
	db := sdDB(t)
	ctx := context.Background()
	if got := sdState(t, db); got != "1:EUR=€,2:USD=$,3:GBP=£(deleted),5:JPY=¥(deleted) | "+
		"1:free=0,2:team=2500,3:legacy=900(deleted),4:pro=5000(deleted)" {
		t.Fatalf("dbfixture seeded %s", got)
	}
	var plans []SdPlan
	if err := db.NewSelect().Model(&plans).Order("id").Scan(ctx); err != nil || len(plans) != 2 {
		t.Fatalf("bun reads live rows only: %v %+v", err, plans)
	}
	if n, err := db.NewSelect().Model((*SdCurrency)(nil)).Count(ctx); err != nil || n != 2 {
		t.Fatalf("the nullzero spelling too: %v %d", err, n)
	}
	var sub SdSub
	if err := db.NewSelect().Model(&sub).Relation("Plan").Where("sd_sub.plan_id = 3").Scan(ctx); err != nil ||
		sub.Plan != nil {
		t.Fatalf("a row pointing at a soft-deleted one loads without it: %v %+v", err, sub.Plan)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.NewDelete().Model((*SdPlan)(nil)).Where("name = 'team'").Exec(ctx); err != nil {
		t.Fatal(err)
	}
	var kept int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sd_plans WHERE name = 'team' AND deleted_at IS NOT NULL").
		Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("bun's delete keeps the row: %v %d", err, kept)
	}
	if _, err := tx.ExecContext(ctx, "SAVEPOINT s"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO sd_currencies (id, code, symbol) VALUES (9, 'GBP', 'x')"); err == nil {
		t.Fatal("the full unique index takes a code a soft-deleted row holds")
	}
	if _, err := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT s"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO sd_plans (name, currency_id, price_cents) VALUES ('legacy', 1, 1)"); err != nil {
		t.Fatalf("the live-rows index refuses a name only a soft-deleted row holds: %v", err)
	}
}

// Every scenario of the spec's table, through check, export, status,
// generate, plan, bun's migrator, a second run, a rollback and a migrate
// again.
func TestSoftDeleteThroughGenerateAndBunsMigrator(t *testing.T) {
	db := sdDB(t)
	c := sdCLI(t)
	ctx := context.Background()

	// Soft-deleted rows, the database's and the files', are no drift.
	out := c.must(0, "check")
	contains(t, "check", out, "the database and fixtures/fixture.yml agree",
		"SdCurrency: 2 rows soft-deleted in the database, which soft_delete leaves out of the master data",
		"SdPlan: 2 rows soft-deleted in the database",
		"SdPlan: 2 rows of fixtures/fixture.yml are soft-deleted (the soft_delete column set) and not master data")

	// An export writes neither them nor the column, so a fresh seed of it
	// does not bring them back to life.
	for _, args := range [][]string{{"export", "-o", filepath.Join(c.dir, "export.yml")},
		{"export", "-all-columns", "-o", filepath.Join(c.dir, "all.yml")}} {
		out := c.must(0, args...)
		contains(t, "export", out, "SdCurrency: 2 soft-deleted rows not exported",
			"SdPlan: 2 soft-deleted rows not exported")
		exported := readFileT(t, args[len(args)-1])
		for _, absent := range []string{"deleted_at", "legacy", "pro", "GBP", "JPY"} {
			if strings.Contains(exported, absent) {
				t.Fatalf("%v holds %q:\n%s", args, absent, exported)
			}
		}
	}

	c.must(0, "baseline")
	c.write("fixtures/fixture.yml", sdV2)
	out = c.must(3, "status", "-offline")
	contains(t, "status", out, "SdCurrency: 2 inserts", "SdPlan: 2 inserts, 1 soft delete")
	out = c.must(3, "check")
	contains(t, "check", out,
		"SdCurrency code=GBP (soft-deleted there at 2026-03-01T00:00:00Z; a migration restores it)",
		"SdCurrency code=JPY (soft-deleted there at 2026-04-01T00:00:00Z; a migration restores it)",
		"SdPlan name=legacy (soft-deleted there at 2026-02-01T00:00:00Z; a migration restores it)",
		"SdPlan name=pro (soft-deleted there at 2026-01-01T00:00:00Z; a migration restores it)",
		"In the database, not in the fixture file:\nSdPlan name=team")

	out = c.must(0, "generate", "-name", "soft", "-at", "20261001000000")
	contains(t, "generate", out, "SdPlan: 2 inserts, 1 soft delete")
	src, err := os.ReadFile(filepath.Join(c.dir, "migrations", "20261001000000_fixture_soft.go"))
	if err != nil {
		t.Fatal(err)
	}
	contains(t, "the generated file", string(src),
		`"SdCurrency": {Name: "sd_currencies", ID: "id", Key: "code", SoftDelete: "deleted_at"}`,
		`"SdPlan":     {Name: "sd_plans", ID: "id", Serial: true, SoftDelete: "deleted_at"}`)

	before := sdState(t, db)
	out = c.must(0, "plan")
	contains(t, "plan", out, "20261001000000_fixture_soft: would succeed",
		"applied SdPlan name=team delete (1 row, soft-deleted): 1 row of sd_subs still points at it through "+
			`constraint "sd_subs_plan_id_fkey"; bun loads a soft-deleted row through no relation`,
		"applied SdCurrency code=GBP insert (1 row, restored): restored the row soft-deleted at 2026-03-01 00:00:00+00 (id 3)",
		"skipped SdCurrency code=JPY insert [changed row]: sd_currencies code=JPY is soft-deleted (id 5, deleted at "+
			`2026-04-01 00:00:00+00) with other values than this change writes, and constraint "sd_currencies_code_key" `+
			"refuses a second row",
		"applied SdPlan name=legacy insert (1 row, restored): restored the row soft-deleted at 2026-02-01 00:00:00+00 (id 3)",
		"applied SdPlan name=pro insert (1 row): inserted beside the soft-deleted row (id 4, deleted at "+
			"2026-01-01 00:00:00+00), which holds other values")
	if got := sdState(t, db); got != before {
		t.Fatalf("plan changed the database:\n got %s\nwant %s", got, before)
	}

	bin := buildMigrator(t, "20261001000000", "soft", src)
	if ok, out := runMigrator(t, bin, false, "-table", "sd_migrations"); !ok {
		t.Fatalf("migrate:\n%s", out)
	}
	up := sdState(t, db)
	if want := "1:EUR=€,2:USD=$,3:GBP=£,5:JPY=¥(deleted) | 1:free=0,2:team=2500(deleted),3:legacy=900," +
		"4:pro=5000(deleted),"; !strings.HasPrefix(up, want) || !strings.HasSuffix(up, ":pro=6000") {
		t.Fatalf("after the migration\n got %s\nwant %s<new id>:pro=6000", up, want)
	}
	newPro := scan[int64](t, db, "SELECT id FROM sd_plans WHERE name = 'pro' AND deleted_at IS NULL")
	// The subscription on team still points at it; to bun, at nothing.
	var subs []SdSub
	if err := db.NewSelect().Model(&subs).Relation("Plan").Order("sd_sub.id").Scan(ctx); err != nil ||
		len(subs) != 2 || subs[0].PlanID != 2 || subs[0].Plan != nil || subs[1].Plan == nil ||
		subs[1].Plan.Name != "legacy" {
		t.Fatalf("the subscriptions: %v %+v", err, subs)
	}

	// The history under the live-rows index makes nothing ambiguous: check
	// sees the live pro, and only JPY differs, which the policy skipped.
	out = c.must(3, "check")
	contains(t, "check", out, "SdCurrency code=JPY (soft-deleted there at 2026-04-01T00:00:00Z")
	if strings.Contains(out, "pro") || strings.Contains(out, "Duplicate") {
		t.Fatalf("check after the migration:\n%s", out)
	}

	// A second run, in this process and its driver, finds every change made.
	set, ok, err := fixturemigrate.ReadChangeSet(src)
	if err != nil || !ok {
		t.Fatal(err)
	}
	var outcomes []fixtureapply.Outcome
	if err := fixtureapply.Apply(ctx, db, set, quiet(), fixtureapply.WithReport(func(o fixtureapply.Outcome) {
		outcomes = append(outcomes, o)
	})); err != nil {
		t.Fatal(err)
	}
	for _, o := range outcomes {
		if o.Index >= 0 && o.Status != fixtureapply.StatusUnchanged && !(o.Key == "code=JPY" &&
			o.Status == fixtureapply.StatusSkipped) {
			t.Fatalf("a second run changed something: %+v", o)
		}
	}
	if got := sdState(t, db); got != up {
		t.Fatalf("a second run changed the database:\n got %s\nwant %s", got, up)
	}

	// The rollback restores team and soft-deletes what the migration
	// restored or inserted; JPY, skipped, is soft-deleted already.
	if ok, out := runMigrator(t, bin, false, "-table", "sd_migrations", "-down"); !ok {
		t.Fatalf("rollback:\n%s", out)
	}
	if got := sdLive(t, db, "name", "sd_plans"); got != "free,team" {
		t.Fatalf("after the rollback the live plans are %s", got)
	}
	if got := sdLive(t, db, "code", "sd_currencies"); got != "EUR,USD" {
		t.Fatalf("after the rollback the live currencies are %s", got)
	}
	c.write("fixtures/fixture.yml", sdV1)
	c.must(0, "check")

	// Migrating again restores the rows the rollback soft-deleted, the new
	// pro included: its newest soft-deleted row holds the file's values.
	if ok, out := runMigrator(t, bin, false, "-table", "sd_migrations"); !ok {
		t.Fatalf("migrate again:\n%s", out)
	}
	if got := sdState(t, db); got != up {
		t.Fatalf("migrating again\n got %s\nwant %s", got, up)
	}
	if id := scan[int64](t, db, "SELECT id FROM sd_plans WHERE name = 'pro' AND deleted_at IS NULL"); id != newPro {
		t.Fatalf("pro came back as %d, not as the row the first migration inserted, %d", id, newPro)
	}
	c.write("fixtures/fixture.yml", sdV2)
	if out := c.must(0, "status", "-require-applied"); !strings.Contains(out, "applied") {
		t.Fatalf("status:\n%s", out)
	}
}

// sync makes the same changes as the migration, in this process and its
// driver, and the database agrees with the files after it.
func TestSoftDeleteThroughSync(t *testing.T) {
	db := sdDB(t)
	path := filepath.Join(t.TempDir(), "fixture-migrate.yml")
	if err := os.WriteFile(path, []byte(sdConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := fixturemigrate.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	files := func(text string) []fixturemigrate.FixtureFile {
		return []fixturemigrate.FixtureFile{{Path: "fixture.yml", Data: []byte(text)}}
	}
	res, err := fixturemigrate.Sync(ctx, db, cfg, files(sdV1), fixturemigrate.SyncOptions{DryRun: true})
	if err != nil || len(res.Diff.Changes) != 0 {
		t.Fatalf("the seed against its files: %v %+v", err, res.Diff.Changes)
	}
	res, err = fixturemigrate.Sync(ctx, db, cfg, files(sdV2), fixturemigrate.SyncOptions{})
	if err != nil || !res.Applied {
		t.Fatalf("sync: %v %+v", err, res)
	}
	actions := map[string]string{}
	for _, o := range res.Outcomes {
		actions[o.Key] = string(o.Status) + "/" + string(o.Action)
	}
	for key, want := range map[string]string{"name=team": "applied/soft-deleted", "name=legacy": "applied/restored",
		"name=pro": "applied/inserted", "code=GBP": "applied/restored", "code=JPY": "skipped/"} {
		if actions[key] != want {
			t.Fatalf("%s: %s, want %s (%+v)", key, actions[key], want, res.Outcomes)
		}
	}
	if len(res.Diff.SoftDeleted) != 4 {
		t.Fatalf("sync knows which rows come back: %+v", res.Diff.SoftDeleted)
	}
	res, err = fixturemigrate.Sync(ctx, db, cfg, files(sdV2), fixturemigrate.SyncOptions{DryRun: true})
	if err != nil || len(res.Diff.Changes) != 1 || res.Diff.Changes[0].Key["code"].Lit != "JPY" {
		t.Fatalf("a second sync: %v %+v", err, res.Diff.Changes)
	}
}

// With an audit table, a soft delete that Apply found made already is not
// restored by Revert; without one, Revert assumes Apply made every change.
func TestSoftDeleteWithTheAuditTable(t *testing.T) {
	for _, audit := range []bool{true, false} {
		db := sdDB(t)
		ctx := context.Background()
		set := fixturechange.Set{
			Name:   "20261001000000_fixture_retire_team",
			Tables: fixturechange.Tables{"SdPlan": {Name: "sd_plans", ID: "id", Serial: true, SoftDelete: "deleted_at"}},
			Changes: []fixturechange.Change{{Model: "SdPlan", Kind: fixturechange.Delete,
				Key: fixturechange.Values{"name": fixturechange.Lit("team")},
				Old: fixturechange.Values{"name": fixturechange.Lit("team"), "currency_id": fixturechange.Lit("1"),
					"price_cents": fixturechange.Lit("2500")}}},
		}
		if audit {
			set.AuditTable = "sd_audit"
		}
		// An admin soft-deleted team before the deploy.
		run(t, db, "UPDATE sd_plans SET deleted_at = '2026-05-01Z' WHERE name = 'team'")
		var outcomes []fixtureapply.Outcome
		if err := fixtureapply.Apply(ctx, db, set, quiet(), fixtureapply.WithReport(func(o fixtureapply.Outcome) {
			outcomes = append(outcomes, o)
		})); err != nil {
			t.Fatal(err)
		}
		if len(outcomes) != 1 || outcomes[0].Status != fixtureapply.StatusUnchanged ||
			!strings.Contains(outcomes[0].Message, "already soft-deleted, since 2026-05-01 00:00:00+00") {
			t.Fatalf("apply: %+v", outcomes)
		}
		if err := fixtureapply.Revert(ctx, db, set, quiet()); err != nil {
			t.Fatal(err)
		}
		live := scan[int64](t, db, "SELECT count(*) FROM sd_plans WHERE name = 'team' AND deleted_at IS NULL")
		if audit && live != 0 {
			t.Fatal("with an audit table, revert restored a row the migration did not soft-delete")
		}
		if !audit && live != 1 {
			t.Fatal("without an audit table, revert restores the row it assumes the migration soft-deleted")
		}
		if audit {
			got := scan[string](t, db, "SELECT string_agg(direction || ':' || (outcomes->0->>'status'), ',' "+
				"ORDER BY id) FROM sd_audit")
			if got != "up:unchanged,down:unchanged" {
				t.Fatalf("the audit table holds %s", got)
			}
		}
	}
}

// What the tool refuses, and says why, about a soft-delete model: a column
// that is not a nullable timestamp without a default, rows holding the zero
// time, a live row pointing at a soft-deleted one, and a rename into a key a
// soft-deleted row holds under a full unique index.
func TestSoftDeleteRefusals(t *testing.T) {
	db := sdDB(t)
	c := sdCLI(t)
	ctx := context.Background()

	c.must(0, "baseline")
	for _, tc := range []struct{ alter, want string }{
		{"ALTER TABLE sd_currencies ALTER deleted_at TYPE bigint USING NULL", "int64 soft-delete field does not work"},
		{"ALTER TABLE sd_currencies ALTER deleted_at SET DEFAULT now()", "born soft-deleted"},
	} {
		run(t, db, "ALTER TABLE sd_currencies RENAME deleted_at TO deleted_at_kept",
			"ALTER TABLE sd_currencies ADD deleted_at timestamptz", tc.alter)
		code, stdout, stderr := c.run("check")
		if code == 0 || !strings.Contains(stdout+stderr, tc.want) {
			t.Fatalf("%s: exit %d\n%s%s", tc.alter, code, stdout, stderr)
		}
		out := c.must(2, "generate", "-dry-run", "-name", "x")
		contains(t, "generate", out, "soft delete", tc.want)
		run(t, db, "ALTER TABLE sd_currencies DROP deleted_at",
			"ALTER TABLE sd_currencies RENAME deleted_at_kept TO deleted_at")
	}

	// E14: a time.Time field without nullzero writes the zero time for a live
	// row, which this configuration reads as deleted.
	run(t, db, "UPDATE sd_currencies SET deleted_at = '0001-01-01 00:00:00+00' WHERE code = 'USD'")
	out := c.must(3, "check")
	contains(t, "check", out, "1 row hold the zero time in deleted_at", "Give the field nullzero or make it a pointer")
	// Live plans point at EUR, which then reads as deleted.
	run(t, db, "UPDATE sd_currencies SET deleted_at = '0001-01-01 00:00:00+00' WHERE code = 'EUR'")
	_, stdout, stderr := c.run("check")
	contains(t, "check", stdout+stderr, "points at public.sd_currencies id 1, whose deleted_at holds the zero time")
	run(t, db, "UPDATE sd_currencies SET deleted_at = NULL WHERE code IN ('EUR', 'USD')")

	// A live master row pointing at a soft-deleted one.
	run(t, db, "UPDATE sd_currencies SET deleted_at = '2026-06-01Z' WHERE code = 'USD'",
		"UPDATE sd_plans SET currency_id = 2 WHERE name = 'free'")
	_, stdout, stderr = c.run("check")
	contains(t, "check", stdout+stderr, "SdPlan: currency_id = 2 points at public.sd_currencies id 2, which is "+
		"soft-deleted (deleted_at = 2026-06-01T00:00:00Z)", "Restore it, or point the row elsewhere")
	run(t, db, "UPDATE sd_plans SET currency_id = 1 WHERE name = 'free'",
		"UPDATE sd_currencies SET deleted_at = NULL WHERE code = 'USD'")
	c.must(0, "check")

	// E16: renaming EUR into GBP, which a soft-deleted row holds under the
	// full unique index, is a changed row, not PostgreSQL's raw refusal.
	set := fixturechange.Set{
		Name:   "20261001000000_fixture_rename",
		Tables: fixturechange.Tables{"SdCurrency": {Name: "sd_currencies", ID: "id", Key: "code", SoftDelete: "deleted_at"}},
		Policy: fixturechange.Policy{ChangedRow: fixturechange.ModeError},
		Changes: []fixturechange.Change{{Model: "SdCurrency", Kind: fixturechange.Update, ID: "1",
			Key: fixturechange.Values{"code": fixturechange.Lit("EUR")},
			Old: fixturechange.Values{"code": fixturechange.Lit("EUR")},
			New: fixturechange.Values{"code": fixturechange.Lit("GBP")}}},
	}
	err := fixtureapply.Apply(ctx, db, set, quiet())
	if err == nil || !strings.Contains(err.Error(), `constraint "sd_currencies_code_key" refuses them: a soft-deleted `+
		"row holds code=GBP, deleted at 2026-03-01 00:00:00+00") {
		t.Fatalf("the rename: %v", err)
	}
	set.Policy.ChangedRow = fixturechange.ModeWarn
	if err := fixtureapply.Apply(ctx, db, set, quiet()); err != nil {
		t.Fatalf("under changed_row warn the rename is skipped and the set goes on: %v", err)
	}
	if got := scan[string](t, db, "SELECT code FROM sd_currencies WHERE id = 1"); got != "EUR" {
		t.Fatalf("EUR is %s", got)
	}

	// An update of a row that is soft-deleted is a missing row that says so.
	set = fixturechange.Set{
		Name:   "20261001000000_fixture_gbp",
		Tables: fixturechange.Tables{"SdCurrency": {Name: "sd_currencies", ID: "id", Key: "code", SoftDelete: "deleted_at"}},
		Changes: []fixturechange.Change{{Model: "SdCurrency", Kind: fixturechange.Update,
			Key: fixturechange.Values{"code": fixturechange.Lit("GBP")},
			Old: fixturechange.Values{"symbol": fixturechange.Lit("£")},
			New: fixturechange.Values{"symbol": fixturechange.Lit("GBP")}}},
	}
	if err := fixtureapply.Apply(ctx, db, set, quiet()); err == nil || !strings.Contains(err.Error(),
		"It is soft-deleted, since 2026-03-01 00:00:00+00, and the fixture file still holds it") {
		t.Fatalf("the update of a soft-deleted row: %v", err)
	}
	// A reference to it says so too.
	set = fixturechange.Set{
		Name: "20261001000000_fixture_gbp_plan",
		Tables: fixturechange.Tables{
			"SdCurrency": {Name: "sd_currencies", ID: "id", Key: "code", SoftDelete: "deleted_at"},
			"SdPlan":     {Name: "sd_plans", ID: "id", Serial: true, SoftDelete: "deleted_at"},
		},
		Changes: []fixturechange.Change{{Model: "SdPlan", Kind: fixturechange.Insert,
			Key: fixturechange.Values{"name": fixturechange.Lit("uk")},
			New: fixturechange.Values{"name": fixturechange.Lit("uk"), "price_cents": fixturechange.Lit("1"),
				"currency_id": fixturechange.RefTo("SdCurrency", "GBP")}}},
	}
	if err := fixtureapply.Apply(ctx, db, set, quiet()); err == nil || !strings.Contains(err.Error(),
		`SdCurrency "GBP", and the row of sd_currencies with code = "GBP" is soft-deleted, since 2026-03-01 00:00:00+00`) {
		t.Fatalf("a reference to a soft-deleted row: %v", err)
	}
}

// Under mode upsert nothing is deleted, soft or hard, and a row that comes
// back is restored; under ids: database the row is found by its key alone.
func TestSoftDeleteWithOwnership(t *testing.T) {
	db := sdDB(t)
	c := sdCLI(t)
	// The plans only: the currencies' ids are no sequence's.
	c.write("fixture-migrate.yml", strings.Replace(sdConfig, "    references: {currency_id: SdCurrency}\n",
		"    references: {currency_id: SdCurrency}\n    mode: upsert\n    ids: database\n", 1))
	c.write("fixtures/fixture.yml", sdV2)
	out := c.must(3, "check")
	contains(t, "check", out, "SdPlan name=legacy (soft-deleted there",
		"SdPlan: 1 row only in the database, which mode upsert never deletes")
	c.must(0, "sync", "-yes")
	if got := sdLive(t, db, "id || ':' || name", "sd_plans"); !strings.HasPrefix(got, "1:free,2:team,3:legacy,") ||
		!strings.HasSuffix(got, ":pro") {
		t.Fatalf("the live plans: %s", got)
	}
	// GBP comes back under its own id 3, and JPY is still refused by the
	// full unique index.
	if got := sdLive(t, db, "id || ':' || code", "sd_currencies"); got != "1:EUR,2:USD,3:GBP" {
		t.Fatalf("the live currencies: %s", got)
	}
	c.write("fixture-migrate.yml", strings.Replace(sdConfig, "    soft_delete: deleted_at\n",
		"    soft_delete: deleted_at\n    mode: insert\n", 1))
	if code, stdout, stderr := c.run("check"); code == 0 ||
		!strings.Contains(stdout+stderr, "mode insert, which this version does not support") {
		t.Fatalf("mode insert: exit %d\n%s%s", code, stdout, stderr)
	}
}

// A table without an id column, its soft delete a timestamp without a zone:
// the row is found again by where it is, and the time is UTC whatever the
// session's TimeZone.
func TestSoftDeleteWithoutAnIDColumn(t *testing.T) {
	connect(t) // skips without a database
	db := openDB(t, os.Getenv("BUN_FIXTURE_MIGRATE_POSTGRES"), map[string]string{"TimeZone": "Asia/Tokyo"})
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if tz := scan[string](t, db, "SHOW TimeZone"); tz != "Asia/Tokyo" {
		t.Fatalf("the session's TimeZone is %s", tz)
	}
	run(t, db, "DROP TABLE IF EXISTS sd_tags",
		"CREATE TABLE sd_tags (plan text NOT NULL, tag text NOT NULL, label text NOT NULL, deleted_at timestamp, "+
			"PRIMARY KEY (plan, tag))",
		"INSERT INTO sd_tags VALUES ('team', 'popular', 'Popular', NULL), ('team', 'new', 'New', NULL)")
	set := fixturechange.Set{
		Name:   "20261001000000_fixture_tags",
		Tables: fixturechange.Tables{"SdTag": {Name: "sd_tags", ID: "id", SoftDelete: "deleted_at"}},
		Changes: []fixturechange.Change{{Model: "SdTag", Kind: fixturechange.Delete,
			Key: fixturechange.Values{"plan": fixturechange.Lit("team"), "tag": fixturechange.Lit("new")},
			Old: fixturechange.Values{"plan": fixturechange.Lit("team"), "tag": fixturechange.Lit("new"),
				"label": fixturechange.Lit("New")}}},
	}
	if err := fixtureapply.Apply(ctx, db, set, quiet()); err != nil {
		t.Fatal(err)
	}
	if off := scan[float64](t, db, "SELECT abs(extract(epoch FROM deleted_at - (now() AT TIME ZONE 'UTC'))) "+
		"FROM sd_tags WHERE tag = 'new'"); off > 600 {
		t.Fatalf("the soft delete's time is %v seconds off UTC", off)
	}
	var outcomes []fixtureapply.Outcome
	if err := fixtureapply.Revert(ctx, db, set, quiet(), fixtureapply.WithReport(func(o fixtureapply.Outcome) {
		outcomes = append(outcomes, o)
	})); err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 1 || outcomes[0].Action != fixtureapply.ActionRestored ||
		strings.Contains(outcomes[0].Message, "(id") {
		t.Fatalf("revert: %+v", outcomes)
	}
	if got := scan[int64](t, db, "SELECT count(*) FROM sd_tags WHERE deleted_at IS NULL"); got != 2 {
		t.Fatalf("%d live tags", got)
	}
}

// E11 at run time: a key two rows hold names the index that let the second
// one in, and asks for an index only where the table has none.
func TestADuplicateKeyAtRunTimeNamesTheIndex(t *testing.T) {
	db := connect(t)
	ctx := context.Background()
	run(t, db, "DROP TABLE IF EXISTS dk_categories, dk_regions, dk_tags",
		"CREATE TABLE dk_categories (id bigint PRIMARY KEY, parent_id bigint, code text NOT NULL, label text, "+
			"UNIQUE (parent_id, code))",
		"INSERT INTO dk_categories VALUES (1, NULL, 'root', 'a'), (2, NULL, 'root', 'b')",
		"CREATE TABLE dk_regions (id bigint PRIMARY KEY, code text NOT NULL, archived boolean NOT NULL, label text)",
		"CREATE UNIQUE INDEX dk_regions_code_open ON dk_regions (code) WHERE NOT archived",
		"INSERT INTO dk_regions VALUES (1, 'eu', true, 'a'), (2, 'eu', true, 'b')",
		"CREATE TABLE dk_tags (id bigint PRIMARY KEY, code text NOT NULL, label text)",
		"INSERT INTO dk_tags VALUES (1, 'hot', 'a'), (2, 'hot', 'b')")
	for _, tc := range []struct{ model, table, keyCol, key, want string }{
		{"Category", "dk_categories", "code", "root", "unique index dk_categories_parent_id_code_key holds NULLs " +
			"distinct, and lets in a second row with parent_id NULL: declare it NULLS NOT DISTINCT"},
		{"Region", "dk_regions", "code", "eu", "unique index dk_regions_code_open holds only where (NOT archived), " +
			"and lets in the rows outside it"},
		{"Tag", "dk_tags", "code", "hot", "add a unique index on the key so they cannot come back"},
	} {
		key := fixturechange.Values{tc.keyCol: fixturechange.Lit(tc.key)}
		if tc.model == "Category" {
			key["parent_id"] = fixturechange.Null()
		}
		set := fixturechange.Set{Name: "20261001000000_fixture_dup",
			Tables: fixturechange.Tables{tc.model: {Name: tc.table, ID: "id"}},
			Changes: []fixturechange.Change{{Model: tc.model, Kind: fixturechange.Update, Key: key,
				Old: fixturechange.Values{"label": fixturechange.Lit("a")},
				New: fixturechange.Values{"label": fixturechange.Lit("c")}}}}
		err := fixtureapply.Apply(ctx, db, set, quiet())
		if err == nil || !strings.Contains(err.Error(), "2 rows of "+tc.table+" hold") ||
			!strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", tc.model, err)
		}
	}
}
