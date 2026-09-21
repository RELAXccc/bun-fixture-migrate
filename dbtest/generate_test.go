package dbtest_test

// The whole pipeline against a real database and the real dbfixture: load the
// old fixture file, generate the change set from the diff, apply it, and check
// that the database now holds what loading the new fixture file into an empty
// database would have produced.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dbfixture"
)

type Currency struct {
	bun.BaseModel `bun:"table:currencies"`

	ID     int64  `bun:"id,pk"`
	Code   string `bun:"code,notnull,unique"`
	Symbol string `bun:"symbol,notnull"`
}

type Plan struct {
	bun.BaseModel `bun:"table:plans"`

	ID         int64   `bun:"id,pk,autoincrement"`
	Name       string  `bun:"name,notnull,unique"`
	CurrencyID int64   `bun:"currency_id,notnull"`
	PriceCents int64   `bun:"price_cents,notnull"`
	Seats      int64   `bun:"seats,notnull"`
	Rating     float64 `bun:"rating,notnull"`
	Public     bool    `bun:"public,notnull"`
}

type Feature struct {
	bun.BaseModel `bun:"table:features"`

	ID     int64  `bun:"id,pk,autoincrement"`
	PlanID int64  `bun:"plan_id,notnull"`
	Code   string `bun:"code,notnull"`
	Quota  int64  `bun:"quota,notnull"`
}

const oldFixture = `- model: Currency
  rows:
    - _id: eur
      id: 1
      code: EUR
      symbol: "E"
- model: Plan
  rows:
    - _id: free
      id: 1
      name: free
      currency_id: '{{ $.Currency.eur.ID }}'
      price_cents: 0
      seats: 1
      rating: 4
      public: true
    - _id: team
      id: 2
      name: team
      currency_id: '{{ $.Currency.eur.ID }}'
      price_cents: 2000
      seats: 10
      rating: 4.5
      public: true
- model: Feature
  rows:
    - plan_id: '{{ $.Plan.free.ID }}'
      code: api
      quota: 100
    - plan_id: '{{ $.Plan.team.ID }}'
      code: api
      quota: 5000
    - plan_id: '{{ $.Plan.team.ID }}'
      code: sso
      quota: 1
`

// Against the old file: a new currency, a new plan that uses it, a price, a
// rating, a flag and a quota change, and a feature that is gone.
const newFixture = `- model: Currency
  rows:
    - _id: eur
      id: 1
      code: EUR
      symbol: "E"
    - _id: usd
      id: 2
      code: USD
      symbol: "$"
- model: Plan
  rows:
    - _id: free
      id: 1
      name: free
      currency_id: '{{ $.Currency.eur.ID }}'
      price_cents: 0
      seats: 1
      rating: 4
      public: false
    - _id: team
      id: 2
      name: team
      currency_id: '{{ $.Currency.eur.ID }}'
      price_cents: 2500
      seats: 10
      rating: 4.75
      public: true
    - _id: pro
      id: 3
      name: pro
      currency_id: '{{ $.Currency.usd.ID }}'
      price_cents: 9000
      seats: 100
      rating: 5
      public: true
- model: Feature
  rows:
    - plan_id: '{{ $.Plan.free.ID }}'
      code: api
      quota: 250
    - plan_id: '{{ $.Plan.team.ID }}'
      code: api
      quota: 5000
    - plan_id: '{{ $.Plan.pro.ID }}'
      code: sso
      quota: 1
`

func pipelineConfig(t *testing.T) *fixturemigrate.Config {
	t.Helper()
	cfg := &fixturemigrate.Config{
		SeedGuardTable: "plans",
		Models: map[string]*fixturemigrate.Model{
			"Currency": {Table: "currencies", Ref: "code", Key: []string{"code"}, StableID: "id"},
			"Plan": {
				Table: "plans", Serial: true, Key: []string{"name"}, StableID: "id",
				References: map[string]string{"currency_id": "Currency"},
			},
			"Feature": {
				Table: "features", Serial: true, Key: []string{"plan_id", "code"},
				References: map[string]string{"plan_id": "Plan"},
				Defaults:   map[string]string{"quota": "0"},
			},
		},
	}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func resetSchema(t *testing.T, db *bun.DB) {
	t.Helper()
	ctx := context.Background()
	for _, model := range []any{(*Feature)(nil), (*Plan)(nil), (*Currency)(nil)} {
		if _, err := db.NewDropTable().Model(model).IfExists().Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, model := range []any{(*Currency)(nil), (*Plan)(nil), (*Feature)(nil)} {
		if _, err := db.NewCreateTable().Model(model).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// load seeds the database with dbfixture, the way the application would.
func load(t *testing.T, db *bun.DB, text string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fixture.yml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := dbfixture.New(db).Load(context.Background(), os.DirFS(dir), "fixture.yml"); err != nil {
		t.Fatalf("dbfixture: %v", err)
	}
	// dbfixture writes explicit ids straight past the sequences.
	for _, table := range []string{"plans", "features"} {
		if _, err := db.ExecContext(context.Background(),
			"SELECT setval(pg_get_serial_sequence('"+table+"', 'id'), GREATEST((SELECT COALESCE(MAX(id), 0) FROM "+table+"), 1))"); err != nil {
			t.Fatal(err)
		}
	}
}

// snapshot renders the whole database, keyed by the natural keys rather than
// by id, so two databases seeded in different ways can be compared.
func snapshot(t *testing.T, db *bun.DB) string {
	t.Helper()
	var lines []string
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, `SELECT c.code, c.symbol FROM currencies c ORDER BY c.code`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var code, symbol string
		if err := rows.Scan(&code, &symbol); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, "currency "+code+" "+symbol)
	}
	rows.Close()
	rows, err = db.QueryContext(ctx, `SELECT p.name, c.code, p.price_cents, p.seats, p.rating, p.public
		FROM plans p JOIN currencies c ON c.id = p.currency_id ORDER BY p.name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name, code string
		var price, seats int64
		var rating float64
		var public bool
		if err := rows.Scan(&name, &code, &price, &seats, &rating, &public); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("plan %s %s %d %d %g %t", name, code, price, seats, rating, public))
	}
	rows.Close()
	rows, err = db.QueryContext(ctx, `SELECT p.name, f.code, f.quota
		FROM features f JOIN plans p ON p.id = f.plan_id ORDER BY p.name, f.code`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var plan, code string
		var quota int64
		if err := rows.Scan(&plan, &code, &quota); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("feature %s %s %d", plan, code, quota))
	}
	rows.Close()
	return strings.Join(lines, "\n")
}

func changeSetFor(t *testing.T, oldText, newText string) fixturechange.Set {
	t.Helper()
	cfg := pipelineConfig(t)
	oldDoc, err := fixturemigrate.ParseDoc([]byte(oldText))
	if err != nil {
		t.Fatal(err)
	}
	newDoc, err := fixturemigrate.ParseDoc([]byte(newText))
	if err != nil {
		t.Fatal(err)
	}
	res, err := fixturemigrate.Compute(cfg, oldDoc, newDoc)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if len(res.Refusals) != 0 {
		t.Fatalf("nothing here should be refused: %+v", res.Refusals)
	}
	// The same rendering the generated file gets, so a mistake in Render shows
	// up here and not at deploy time.
	if _, err := fixturemigrate.Render(cfg, "pipeline", "20260921120000", "HEAD", res); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return fixturechange.Set{
		Name:           "pipeline",
		SeedGuardTable: cfg.SeedGuardTable,
		Tables:         res.Tables,
		Changes:        res.Changes,
	}
}

func TestGeneratedChangesReproduceTheNewFixture(t *testing.T) {
	db := connect(t)
	db.RegisterModel((*Currency)(nil), (*Plan)(nil), (*Feature)(nil))
	ctx := context.Background()

	// What the new file looks like in a database that was seeded with it.
	resetSchema(t, db)
	load(t, db, newFixture)
	want := snapshot(t, db)

	// What the old file plus the generated migration looks like.
	resetSchema(t, db)
	load(t, db, oldFixture)
	set := changeSetFor(t, oldFixture, newFixture)
	if err := fixtureapply.Apply(ctx, db, set, quiet()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := snapshot(t, db)
	if got != want {
		t.Fatalf("the migrated database differs from a freshly seeded one\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	// Applying it again changes nothing.
	if err := fixtureapply.Apply(ctx, db, set, quiet()); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if again := snapshot(t, db); again != want {
		t.Fatalf("a second run changed the database\n%s", again)
	}

	// And reverting it puts the old state back.
	resetSchema(t, db)
	load(t, db, oldFixture)
	before := snapshot(t, db)
	if err := fixtureapply.Apply(ctx, db, set, quiet()); err != nil {
		t.Fatal(err)
	}
	if err := fixtureapply.Revert(ctx, db, set, quiet()); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if after := snapshot(t, db); after != before {
		t.Fatalf("revert did not restore the old state\n--- after ---\n%s\n--- before ---\n%s", after, before)
	}
}

// The migration chain runs before the seed on a fresh database, where it must
// keep its hands off.
func TestNothingHappensBeforeTheFixtureIsLoaded(t *testing.T) {
	db := connect(t)
	db.RegisterModel((*Currency)(nil), (*Plan)(nil), (*Feature)(nil))
	resetSchema(t, db)
	if err := fixtureapply.Apply(context.Background(), db, changeSetFor(t, oldFixture, newFixture), quiet()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := snapshot(t, db); got != "" {
		t.Fatalf("expected an untouched database, got\n%s", got)
	}
}
