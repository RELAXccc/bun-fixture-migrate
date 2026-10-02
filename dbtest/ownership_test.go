package dbtest_test

// Who owns which rows and values, against the real dbfixture, bun's own
// migrator and the command: an admin's price under insert_only, an
// operator's flag, a tenant's roles and grants in tables the fixture files
// share under mode upsert, a country table the files only seed under mode
// insert, and roles whose ids the database gives, so a new master role takes
// the next id of a sequence the tenants draw from too.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
)

type OwnPlan struct {
	bun.BaseModel `bun:"table:own_plans"`
	ID            int64  `bun:"id,pk,autoincrement"`
	Name          string `bun:"name,notnull"`
	PriceCents    int64  `bun:"price_cents,notnull"`
}

type OwnFlag struct {
	bun.BaseModel `bun:"table:own_flags"`
	ID            int64  `bun:"id,pk,autoincrement"`
	Code          string `bun:"code,notnull"`
	Description   string `bun:"description,notnull"`
	Enabled       bool   `bun:"enabled,notnull"`
}

type OwnRole struct {
	bun.BaseModel `bun:"table:own_roles"`
	ID            int64  `bun:"id,pk,autoincrement"`
	Code          string `bun:"code,notnull"`
	Label         string `bun:"label,notnull"`
	TenantID      *int64 `bun:"tenant_id"`
}

type OwnGrant struct {
	bun.BaseModel `bun:"table:own_grants"`
	RoleID        int64  `bun:"role_id,pk"`
	Perm          string `bun:"perm,pk"`
}

type OwnCountry struct {
	bun.BaseModel `bun:"table:own_countries"`
	ID            int64  `bun:"id,pk,autoincrement"`
	Code          string `bun:"code,notnull"`
	Name          string `bun:"name,notnull"`
	VatBP         int64  `bun:"vat_bp,notnull"`
}

const ownConfig = `fixture: fixtures/fixture.yml
out: migrations
package: migrations
migrations_table: own_migrations
migration_locks_table: own_migrations_locks
seed_guard_table: own_plans
database: env:BFM_TEST_DSN
policy:
  renames: update
models:
  OwnPlan:
    table: own_plans
    serial: true
    # finance edits prices in the admin UI
    insert_only: [price_cents]
  OwnFlag:
    table: own_flags
    serial: true
    key: [code]
    ref: code
    # ops toggle flags in production
    insert_only: [enabled]
  OwnRole:
    table: own_roles
    key: [code]
    ref: code
    # tenants add roles of their own, numbered by the same sequence
    mode: upsert
    ids: database
  OwnGrant:
    table: own_grants
    key: [role_id, perm]
    references: {role_id: OwnRole}
    mode: upsert
  OwnCountry:
    table: own_countries
    serial: true
    key: [code]
    ref: code
    # seeded once, the admin's from then on
    mode: insert
`

const ownV1 = `- model: OwnPlan
  rows:
    - {_id: basic, id: 1, name: basic, price_cents: 1000}
    - {_id: pro, id: 2, name: pro, price_cents: 5000}
- model: OwnFlag
  rows:
    - {_id: beta, id: 1, code: beta, description: the beta, enabled: false}
- model: OwnRole
  rows:
    - {_id: admin, id: 1, code: admin, label: Administrator}
    - {_id: viewer, id: 2, code: viewer, label: Viewer}
- model: OwnGrant
  rows:
    - {role_id: '{{ $.OwnRole.admin.ID }}', perm: all}
    - {role_id: 2, perm: read}
- model: OwnCountry
  rows:
    - {_id: de, id: 1, code: DE, name: Germany, vat_bp: 1900}
`

// ownV2 changes a price and the beta flag's description, adds a plan, a flag
// that starts enabled, a role numbered 3 in the file, which is a tenant's id
// in production, with a grant naming it by that id, and a country; it
// changes a country's rate, and leaves the viewer role and its grant out.
const ownV2 = `- model: OwnPlan
  rows:
    - {_id: basic, id: 1, name: basic, price_cents: 1500}
    - {_id: pro, id: 2, name: pro, price_cents: 5000}
    - {_id: team, id: 3, name: team, price_cents: 3000}
- model: OwnFlag
  rows:
    - {_id: beta, id: 1, code: beta, description: the beta programme, enabled: false}
    - {_id: dark, id: 2, code: dark, description: dark mode, enabled: true}
- model: OwnRole
  rows:
    - {_id: admin, id: 1, code: admin, label: Administrator}
    - {_id: auditor, id: 3, code: auditor, label: Auditor}
- model: OwnGrant
  rows:
    - {role_id: '{{ $.OwnRole.admin.ID }}', perm: all}
    - {role_id: 3, perm: audit}
- model: OwnCountry
  rows:
    - {_id: de, id: 1, code: DE, name: Germany, vat_bp: 2000}
    - {_id: fr, id: 2, code: FR, name: France, vat_bp: 2000}
`

// ownDB creates the tables, seeds them from v1 with the real dbfixture and
// moves the sequences, then makes production's own edits: a tenant's role
// and grant, an admin's price and rate, an operator's flag.
func ownDB(t *testing.T) *bun.DB {
	t.Helper()
	db := connect(t)
	db.RegisterModel((*OwnPlan)(nil), (*OwnFlag)(nil), (*OwnRole)(nil), (*OwnGrant)(nil), (*OwnCountry)(nil))
	run(t, db, "DROP TABLE IF EXISTS own_grants, own_roles, own_flags, own_plans, own_countries, own_migrations, "+
		"own_migrations_locks",
		"CREATE TABLE own_plans (id bigserial PRIMARY KEY, name text UNIQUE NOT NULL, price_cents bigint NOT NULL)",
		"CREATE TABLE own_flags (id bigserial PRIMARY KEY, code text UNIQUE NOT NULL, description text NOT NULL, "+
			"enabled boolean NOT NULL DEFAULT false)",
		"CREATE TABLE own_roles (id bigserial PRIMARY KEY, code text UNIQUE NOT NULL, label text NOT NULL, tenant_id bigint)",
		"CREATE TABLE own_grants (role_id bigint NOT NULL REFERENCES own_roles, perm text NOT NULL, "+
			"PRIMARY KEY (role_id, perm))",
		"CREATE TABLE own_countries (id bigserial PRIMARY KEY, code text UNIQUE NOT NULL, name text NOT NULL, "+
			"vat_bp bigint NOT NULL)")
	loadFixture(t, db, ownV1)
	run(t, db,
		"INSERT INTO own_roles (code, label, tenant_id) VALUES ('t7_custom', 'Custom', 7)",
		"INSERT INTO own_grants (role_id, perm) SELECT id, 'custom' FROM own_roles WHERE code = 't7_custom'",
		"UPDATE own_plans SET price_cents = 1200 WHERE name = 'basic'",
		"UPDATE own_flags SET enabled = true WHERE code = 'beta'",
		"UPDATE own_countries SET vat_bp = 1600 WHERE code = 'DE'")
	if id := scan[int64](t, db, "SELECT id FROM own_roles WHERE code = 't7_custom'"); id != 3 {
		t.Fatalf("the tenant's role took id %d, and this test needs it to take the file's 3", id)
	}
	return db
}

func ownCLI(t *testing.T) *cli {
	t.Helper()
	c := buildCLI(t)
	c.write("fixture-migrate.yml", ownConfig)
	c.write("fixtures/fixture.yml", ownV1)
	return c
}

// ownState is what the database holds of what this test looks at, named by
// natural keys.
func ownState(t *testing.T, db *bun.DB) string {
	t.Helper()
	return scan[string](t, db, `SELECT concat_ws(' | ',
		(SELECT string_agg(name || '=' || price_cents, ',' ORDER BY name) FROM own_plans),
		(SELECT string_agg(code || '=' || description || '/' || enabled, ',' ORDER BY code) FROM own_flags),
		(SELECT string_agg(code || coalesce('@' || tenant_id, ''), ',' ORDER BY code) FROM own_roles),
		(SELECT string_agg(r.code || ':' || g.perm, ',' ORDER BY r.code, g.perm) FROM own_grants g JOIN own_roles r ON r.id = g.role_id),
		(SELECT string_agg(code || '=' || vat_bp, ',' ORDER BY code) FROM own_countries))`)
}

// What v2 leaves production holding: the admin's price, the operator's flag
// and the admin's rate kept, the tenant's role and grant and the viewer kept,
// the new rows inserted.
const ownAfterV2 = "basic=1200,pro=5000,team=3000 | beta=the beta programme/true,dark=dark mode/true | " +
	"admin,auditor,t7_custom@7,viewer | admin:all,auditor:audit,t7_custom:custom,viewer:read | DE=1600,FR=2000"

func TestOwnershipThroughGenerateAndBunsMigrator(t *testing.T) {
	db := ownDB(t)
	c := ownCLI(t)

	// What production owns is no drift, and check says what it left alone.
	out := c.must(0, "check")
	for _, want := range []string{
		"the database and fixtures/fixture.yml agree",
		"Left to the database by the configuration, which is no drift:",
		"OwnPlan: 1 row with another price_cents in the database, which insert_only leaves to the database",
		"OwnFlag: 1 row with another enabled in the database, which insert_only leaves to the database",
		"OwnRole: 1 row only in the database, which mode upsert never deletes",
		"OwnGrant: 1 row only in the database, which mode upsert never deletes",
		"OwnCountry: 1 row with other values in the database, which mode insert never updates",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("check is missing %q:\n%s", want, out)
		}
	}

	c.must(0, "baseline")
	c.write("fixtures/fixture.yml", ownV2)
	if out := c.must(3, "status", "-offline"); !strings.Contains(out, "OwnPlan: 1 insert") ||
		!strings.Contains(out, "OwnFlag: 1 insert, 1 update") || !strings.Contains(out, "OwnRole: 1 insert") ||
		strings.Contains(out, "delete") || !strings.Contains(out, "OwnCountry: 1 insert") {
		t.Fatalf("status names the inserts and the one update, and no delete:\n%s", out)
	}
	out = c.must(0, "generate", "-name", "ownership")
	for _, want := range []string{
		"note: OwnRole: 1 row only in the state after baseline, which mode upsert never deletes",
		"note: OwnPlan: 1 row with another price_cents in the state after baseline, which insert_only leaves to the database",
		"note: OwnCountry: 1 row with other values in the state after baseline, which mode insert never updates",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("generate is missing %q:\n%s", want, out)
		}
	}
	var generated string
	entries, _ := os.ReadDir(filepath.Join(c.dir, "migrations"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "_fixture_ownership.go") {
			generated = e.Name()
		}
	}
	src, err := os.ReadFile(filepath.Join(c.dir, "migrations", generated))
	if err != nil {
		t.Fatal(err)
	}
	set, ok, err := fixturemigrate.ReadChangeSet(src)
	if err != nil || !ok {
		t.Fatalf("%v\n%s", err, src)
	}
	for _, ch := range set.Changes {
		switch {
		case ch.Kind == fixturechange.Delete:
			t.Fatalf("nothing is deleted:\n%s", src)
		case ch.Model == "OwnRole" && ch.Kind == fixturechange.Insert:
			if _, ok := ch.New["id"]; ok {
				t.Fatalf("an insert leaves the id to the database:\n%s", src)
			}
		case ch.Kind == fixturechange.Update && (ch.Model != "OwnFlag" || len(ch.New) != 1 || ch.New["description"].Lit == ""):
			t.Fatalf("the one update is the flag's description:\n%s", src)
		}
	}

	// The plan says so too, against production.
	if out := c.must(0, "plan"); !strings.Contains(out, "would succeed") || strings.Contains(out, "skipped") {
		t.Fatalf("plan:\n%s", out)
	}

	bin := buildMigrator(t, generated[:14], "ownership", src)
	if ok, out := runMigrator(t, bin, false, "-table", "own_migrations"); !ok {
		t.Fatalf("migrate:\n%s", out)
	}
	if got := ownState(t, db); got != ownAfterV2 {
		t.Fatalf("after the migration\n got %s\nwant %s", got, ownAfterV2)
	}
	// The new role took an id of the sequence, past the tenant's and past
	// the one plan drew, and the grant naming it by the file's id found it
	// all the same.
	if id := scan[int64](t, db, "SELECT id FROM own_roles WHERE code = 'auditor'"); id <= 3 ||
		id != scan[int64](t, db, "SELECT last_value FROM own_roles_id_seq") {
		t.Fatalf("the auditor role is %d, not the sequence's next id", id)
	}
	out = c.must(0, "check")
	if !strings.Contains(out, "agree") || !strings.Contains(out, "OwnRole: 2 rows only in the database") {
		t.Fatalf("check after the deploy:\n%s", out)
	}
	if out := c.must(0, "status", "-require-applied"); !strings.Contains(out, "applied") {
		t.Fatalf("status after the deploy:\n%s", out)
	}

	// The export writes what the file owns: its own rows of the upsert
	// models, its own values of the insert_only columns and of the insert
	// model, and no role ids.
	c.must(0, "export", "-o", filepath.Join(c.dir, "export.yml"))
	exported := readFileT(t, filepath.Join(c.dir, "export.yml"))
	for _, absent := range []string{"t7_custom", "viewer", "custom", "1200", "1600"} {
		if strings.Contains(exported, absent) {
			t.Fatalf("the export holds %q, which the database owns:\n%s", absent, exported)
		}
	}
	roles := exported[strings.Index(exported, "- model: OwnRole"):strings.Index(exported, "- model: OwnGrant")]
	if strings.Contains(roles, "\n      id: ") || !strings.Contains(exported, "{{ $.OwnRole.auditor.ID }}") {
		t.Fatalf("the roles carry no id, and their grants name them by anchor:\n%s", exported)
	}
	for _, want := range []string{"price_cents: 1500", "vat_bp: 2000"} {
		if !strings.Contains(exported, want) {
			t.Fatalf("the export keeps the file's %q:\n%s", want, exported)
		}
	}
	beta := exported[strings.Index(exported, "code: \"beta\""):]
	if !strings.Contains(beta[:strings.Index(beta, "\n    - ")], "enabled: false") {
		t.Fatalf("the export keeps the file's enabled:\n%s", exported)
	}
	// And the database agrees with it as with the file.
	c.write("fixtures/fixture.yml", exported)
	c.must(0, "check")
}

// sync makes the same changes as the migration, leaves alone what it leaves
// alone, and generate -from-db writes the same set.
func TestOwnershipThroughSyncAndFromTheDatabase(t *testing.T) {
	db := ownDB(t)
	c := ownCLI(t)
	c.write("fixtures/fixture.yml", ownV2)

	code, out, stderr := c.run("generate", "-from-db", "-dry-run", "-name", "from_db")
	if code != 0 || !strings.Contains(stderr, "note: OwnRole: 2 rows only in the database, which mode upsert never deletes") {
		t.Fatalf("generate -from-db: exit %d\n%s%s", code, out, stderr)
	}
	set, ok, err := fixturemigrate.ReadChangeSet([]byte(out[strings.Index(out, "package migrations"):]))
	if err != nil || !ok {
		t.Fatalf("%v\n%s", err, out)
	}
	count := map[string]int{}
	for _, ch := range set.Changes {
		count[ch.Model+" "+string(ch.Kind)]++
		if ch.Model == "OwnRole" {
			if _, ok := ch.New["id"]; ok {
				t.Fatalf("an insert leaves the id to the database:\n%s", out)
			}
		}
	}
	want := map[string]int{"OwnPlan insert": 1, "OwnFlag insert": 1, "OwnFlag update": 1, "OwnRole insert": 1,
		"OwnGrant insert": 1, "OwnCountry insert": 1}
	if len(count) != len(want) {
		t.Fatalf("got %v, want %v\n%s", count, want, out)
	}
	for k, n := range want {
		if count[k] != n {
			t.Fatalf("got %v, want %v\n%s", count, want, out)
		}
	}

	c.must(0, "sync", "-yes")
	if got := ownState(t, db); got != ownAfterV2 {
		t.Fatalf("after sync\n got %s\nwant %s", got, ownAfterV2)
	}
	if out := c.must(0, "sync"); !strings.Contains(out, "the database already holds the fixture files") {
		t.Fatalf("a second sync:\n%s", out)
	}
}

// The library's Sync and Check, and the snapshot of a database whose role
// ids are its own: nothing of what the configuration gives to the database
// is a change.
func TestOwnershipThroughTheLibrary(t *testing.T) {
	db := ownDB(t)
	path := filepath.Join(t.TempDir(), "fixture-migrate.yml")
	if err := os.WriteFile(path, []byte(ownConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := fixturemigrate.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	res, err := fixturemigrate.Sync(ctx, db, cfg, []fixturemigrate.FixtureFile{{Path: "f.yml", Data: []byte(ownV1)}},
		fixturemigrate.SyncOptions{DryRun: true})
	if err != nil || len(res.Diff.Changes) != 0 || len(res.Diff.LeftAlone) != 5 {
		t.Fatalf("v1 against production: %v %+v %+v", err, res.Diff.Changes, res.Diff.LeftAlone)
	}
	snap := databaseSnapshot(t, db, cfg, fixturemigrate.SnapshotOptions{})
	for _, e := range snap.Entries["OwnRole"] {
		if e.ID != "" {
			t.Fatalf("a role's id is the database's own and left out of the snapshot: %+v", e)
		}
	}
}

// type OwnSeat is written by bun the way an application writes it: seats
// with a default tag, note through a pointer.
type OwnSeat struct {
	bun.BaseModel `bun:"table:own_seats"`
	ID            int64   `bun:"id,pk,autoincrement"`
	Name          string  `bun:"name,notnull"`
	Seats         int64   `bun:"seats,notnull,default:1"`
	Note          *string `bun:"note"`
}

// A zero and a null bun wrote as DEFAULT, on the seed's INSERT and on an
// admin's UPDATE of the model, are drift whose line says why.
func TestCheckHintsAtTheDefaultBunWrote(t *testing.T) {
	db := connect(t)
	db.RegisterModel((*OwnSeat)(nil))
	run(t, db, "DROP TABLE IF EXISTS own_seats",
		"CREATE TABLE own_seats (id bigserial PRIMARY KEY, name text UNIQUE NOT NULL, "+
			"seats bigint NOT NULL DEFAULT 1, note text DEFAULT 'none')")
	loadFixture(t, db, `- model: OwnSeat
  rows:
    - {_id: a, id: 1, name: a, seats: 0, note: ~}
    - {_id: b, id: 2, name: b, seats: 3, note: x}
`)
	// The admin clears b's note, through bun, which since v1.2.17 writes
	// DEFAULT for the nil pointer on an UPDATE too.
	if _, err := db.NewUpdate().Model(&OwnSeat{ID: 2, Name: "b", Seats: 3}).WherePK().Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := scan[string](t, db, "SELECT string_agg(name || '=' || seats || '/' || coalesce(note, 'NULL'), ',' ORDER BY name) FROM own_seats"); got != "a=1/none,b=3/none" {
		t.Fatalf("bun wrote %s", got)
	}
	c := buildCLI(t)
	c.write("fixture-migrate.yml", `fixture: fixtures/fixture.yml
database: env:BFM_TEST_DSN
policy:
  zero_default: ignore
  null_default: ignore
models:
  OwnSeat: {table: own_seats, serial: true}
`)
	c.write("fixtures/fixture.yml", `- model: OwnSeat
  rows:
    - {_id: a, id: 1, name: a, seats: 0, note: ~}
    - {_id: b, id: 2, name: b, seats: 3, note: ~}
`)
	out := c.must(3, "check")
	for _, want := range []string{
		"note: database none, file NULL\nhint: the column defaults to none, and bun writes DEFAULT for a nil pointer or a nullzero field, on INSERT and, since v1.2.17, on UPDATE too",
		"seats: database 1, file 0\nhint: the column defaults to 1, and bun writes DEFAULT for a zero in a field with a default",
	} {
		if strings.Count(out, "hint:") != 3 || !strings.Contains(out, want) {
			t.Fatalf("check is missing %q, or says it more or less than three times:\n%s", want, out)
		}
	}
}
