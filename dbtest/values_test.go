package dbtest_test

// Values have to arrive exactly: the fixture file, dbfixture, the database, an
// export, a comparison and a migration all have to agree on what each one is.
// Before, a postcode lost its leading zero, a padded label its spaces, an
// integer above 2^53 its last digits, and 017 meant 17 to the tool and 15 to
// dbfixture. Each of these is checked here against what the real loader
// stores, on a server whose TimeZone is not UTC.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
)

type Shop struct {
	bun.BaseModel `bun:"table:shops"`

	ID     int64     `bun:"id,pk"`
	Code   string    `bun:"code,notnull,unique"`
	Label  string    `bun:"label,notnull"`
	Big    int64     `bun:"big,notnull"`
	Octal  int64     `bun:"octal,notnull"`
	Price  string    `bun:"price,type:numeric(30,10),notnull"`
	Ratio  float64   `bun:"ratio,notnull"`
	Opens  time.Time `bun:"opens,notnull"`
	Day    time.Time `bun:"day,type:date,notnull"`
	Active bool      `bun:"active,notnull"`
	Token  string    `bun:"token,type:uuid,notnull"`
}

const shopRow = `    - _id: corner
      id: 1
      code: "01234"
      label: "  padded  "
      big: 9007199254740993
      octal: 017
      price: "12345678901234567890.1234567890"
      ratio: 0.1
      opens: 2026-01-01 10:00:00
      day: 2026-03-04
      active: True
      token: "A0EEBC99-9C0B-4EF8-BB6D-6BB9BD380A11"
`

const shopFixture = "- model: Shop\n  rows:\n" + shopRow

func shopConfig(t *testing.T) *fixturemigrate.Config {
	t.Helper()
	cfg := &fixturemigrate.Config{Schema: "public", SeedGuardTable: "shops",
		Models: map[string]*fixturemigrate.Model{"Shop": {Table: "shops", Key: []string{"code"}, Ref: "code"}}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// awayFromUTC gives a connection whose server-side TimeZone is New York, the
// way a production server outside UTC is configured.
func awayFromUTC(t *testing.T) *bun.DB {
	t.Helper()
	dsn := os.Getenv("BUN_FIXTURE_MIGRATE_POSTGRES")
	if dsn == "" {
		t.Skip("set BUN_FIXTURE_MIGRATE_POSTGRES to a PostgreSQL DSN to run the round trip")
	}
	db := openDB(t, dsn, map[string]string{"TimeZone": "America/New_York"})
	t.Cleanup(func() { db.Close() })
	db.RegisterModel((*Shop)(nil))
	if got := scan[string](t, db, "SHOW TimeZone"); got != "America/New_York" {
		t.Fatalf("TimeZone is %s", got)
	}
	return db
}

func shopTable(t *testing.T, db *bun.DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.NewDropTable().Model((*Shop)(nil)).IfExists().Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.NewCreateTable().Model((*Shop)(nil)).Exec(ctx); err != nil {
		t.Fatal(err)
	}
}

// shopState is the row as PostgreSQL holds it, every column as text in UTC.
func shopState(t *testing.T, db *bun.DB) string {
	t.Helper()
	return scan[string](t, db, `SELECT concat_ws('|', code, '['||label||']', big, octal, price, ratio,
		to_char(opens AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS'), day, active, token) FROM shops`)
}

// readOnlyDo runs fn the way the commands read: one read-only transaction
// with the session's settings fixed.
func readOnlyDo(t *testing.T, db *bun.DB, fn func(tx bun.Tx, tables map[string]*dbschema.Table)) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := fixturemigrate.PrepareSession(ctx, tx); err != nil {
		t.Fatal(err)
	}
	tables, err := dbschema.Load(ctx, tx, "public")
	if err != nil {
		t.Fatal(err)
	}
	fn(tx, tables)
}

func TestValuesArriveExactly(t *testing.T) {
	db := awayFromUTC(t)
	cfg := shopConfig(t)
	ctx := context.Background()
	shopTable(t, db)
	loadFixture(t, db, shopFixture)
	want := "01234|[  padded  ]|9007199254740993|15|12345678901234567890.1234567890|0.1|" +
		"2026-01-01 10:00:00|2026-03-04|t|a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	if got := shopState(t, db); got != want {
		t.Fatalf("this documents what dbfixture stores; if it changed, so did the premise\n got %s\nwant %s", got, want)
	}

	// The database and the file agree, once the database has spelled the
	// file's values: nothing is drift.
	head := fixtureSnapshot(t, cfg, shopFixture, "fixture.yml")
	var exported []byte
	readOnlyDo(t, db, func(tx bun.Tx, tables map[string]*dbschema.Table) {
		if err := fixturemigrate.Canonicalize(ctx, tx, cfg, head, tables); err != nil {
			t.Fatal(err)
		}
		if len(head.Findings) != 0 {
			t.Fatalf("findings: %+v", head.Findings)
		}
		database, err := fixturemigrate.DatabaseSnapshot(ctx, tx, cfg, tables,
			fixturemigrate.SnapshotOptions{Columns: head.Columns, Order: head.Order})
		if err != nil {
			t.Fatal(err)
		}
		res, err := fixturemigrate.Check(cfg, database, head)
		if err != nil {
			t.Fatal(err)
		}
		if res.Drifted() {
			t.Fatalf("the file and the database hold the same values:\n%s", strings.Join(res.Lines(), "\n"))
		}

		// And an export writes them down so dbfixture loads them back as they are.
		all, err := fixturemigrate.DatabaseSnapshot(ctx, tx, cfg, tables, fixturemigrate.SnapshotOptions{})
		if err != nil {
			t.Fatal(err)
		}
		data, err := fixturemigrate.Export(cfg, all, tables, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range []string{`code: "01234"`, `label: "  padded  "`, "big: 9007199254740993", "octal: 15"} {
			if !strings.Contains(string(data), line) {
				t.Fatalf("the export is missing %q:\n%s", line, data)
			}
		}
		exported = data
	})
	shopTable(t, db)
	loadFixture(t, db, string(exported))
	if got := shopState(t, db); got != want {
		t.Fatalf("the export does not load back as the database it came from\n got %s\nwant %s", got, want)
	}
}

// A migration writes the values the way dbfixture would have: the timestamp
// without a zone is UTC, the octal is 15, the long numbers are whole.
func TestAMigrationWritesWhatDbfixtureWould(t *testing.T) {
	db := awayFromUTC(t)
	cfg := shopConfig(t)
	ctx := context.Background()
	shopTable(t, db)
	loadFixture(t, db, shopFixture)
	want := shopState(t, db)

	shopTable(t, db)
	placeholder := "- model: Shop\n  rows:\n    - _id: other\n      id: 2\n      code: \"x\"\n      label: x\n" +
		"      big: 1\n      octal: 1\n      price: \"1\"\n      ratio: 1\n      opens: 2020-01-01 00:00:00\n" +
		"      day: 2020-01-01\n      active: false\n      token: \"00000000-0000-0000-0000-000000000001\"\n"
	loadFixture(t, db, placeholder)
	// Without the database, 017, True and a timestamp without a zone are each
	// two values: what a string field gets and what any other field gets.
	old, next := fixtureSnapshot(t, cfg, placeholder, "old"), fixtureSnapshot(t, cfg, placeholder+shopRow, "new")
	res, err := fixturemigrate.Compute(cfg, old, next)
	if err != nil || len(res.Changes) != 0 || len(res.Refusals) != 1 ||
		!strings.Contains(res.Refusals[0].Reason, "octal is written 017") {
		t.Fatalf("%v %+v %+v", err, res.Changes, res.Refusals)
	}
	// With it, as generate reads it, the column types decide.
	readOnlyDo(t, db, func(tx bun.Tx, tables map[string]*dbschema.Table) {
		for _, s := range []*fixturemigrate.Snapshot{old, next} {
			if err := fixturemigrate.Canonicalize(ctx, tx, cfg, s, tables); err != nil {
				t.Fatal(err)
			}
		}
	})
	res, err = fixturemigrate.Compute(cfg, old, next)
	if err != nil || len(res.Refusals) != 0 {
		t.Fatalf("%v %+v", err, res.Refusals)
	}
	set := fixturechange.Set{Name: "values", SeedGuardTable: "shops", Tables: res.Tables, Changes: res.Changes}
	if err := fixtureapply.Apply(ctx, db, set, quiet()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM shops WHERE code = 'x'"); err != nil {
		t.Fatal(err)
	}
	if got := shopState(t, db); got != want {
		t.Fatalf("the migration wrote something else than dbfixture\n got %s\nwant %s", got, want)
	}
	// The caller's session is left as it was.
	if got := scan[string](t, db, "SHOW TimeZone"); got != "America/New_York" {
		t.Fatalf("TimeZone is now %s", got)
	}
}

type Release struct {
	bun.BaseModel `bun:"table:releases"`

	ID      int64    `bun:"id,pk"`
	Name    string   `bun:"name,notnull,unique"`
	Version string   `bun:"version,notnull"`
	Zip     string   `bun:"zip,notnull"`
	Flag    string   `bun:"flag,notnull"`
	At      string   `bun:"at,notnull"`
	Octal   int64    `bun:"octal,notnull"`
	Ratio   float64  `bun:"ratio,notnull"`
	Tags    []string `bun:"tags,array"`
	Nums    []int64  `bun:"nums,array"`
}

const releaseRow = `    - id: 1
      name: a
      version: 1.10
      zip: 01234
      flag: True
      at: 2026-01-01 10:00:00
      octal: 017
      ratio: 1.50
      tags: [1.10, x, 017]
      nums: [017, 2]
`

// yaml.v3 hands a string field a plain scalar as it is written and a number
// field the number it resolves to, so dbfixture stores 1.10 as "1.10" in a
// text column and 017 as 15 in an integer one. Before, the tool took every
// plain scalar by what it resolves to: it saw drift in a database seeded
// from the file itself, and a migration wrote 1.1 where dbfixture wrote 1.10.
func TestAStringColumnHoldsTheValueAsWritten(t *testing.T) {
	db := connect(t)
	db.RegisterModel((*Release)(nil))
	ctx := context.Background()
	cfg := &fixturemigrate.Config{Schema: "public",
		Models: map[string]*fixturemigrate.Model{"Release": {Table: "releases", Key: []string{"name"}}}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	reset := func() {
		t.Helper()
		if _, err := db.NewDropTable().Model((*Release)(nil)).IfExists().Exec(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := db.NewCreateTable().Model((*Release)(nil)).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	state := func() string {
		t.Helper()
		return scan[string](t, db, `SELECT concat_ws('|', version, zip, flag, at, octal, ratio, tags::text, nums::text)
			FROM releases WHERE name = 'a'`)
	}
	file := "- model: Release\n  rows:\n" + releaseRow
	reset()
	loadFixture(t, db, file)
	const want = "1.10|01234|True|2026-01-01 10:00:00|15|1.5|{1.10,x,017}|{15,2}"
	if got := state(); got != want {
		t.Fatalf("this documents what dbfixture stores; if it changed, so did the premise\n got %s\nwant %s", got, want)
	}

	canonical := func(snaps ...*fixturemigrate.Snapshot) {
		t.Helper()
		readOnlyDo(t, db, func(tx bun.Tx, tables map[string]*dbschema.Table) {
			for _, s := range snaps {
				if err := fixturemigrate.Canonicalize(ctx, tx, cfg, s, tables); err != nil {
					t.Fatal(err)
				}
				if len(s.Findings) != 0 {
					t.Fatalf("findings: %+v", s.Findings)
				}
			}
		})
	}

	// The database seeded from the file agrees with the file.
	head := fixtureSnapshot(t, cfg, file, "fixture.yml")
	canonical(head)
	database := databaseSnapshot(t, db, cfg, fixturemigrate.SnapshotOptions{Columns: head.Columns, Order: head.Order})
	check, err := fixturemigrate.Check(cfg, database, head)
	if err != nil {
		t.Fatal(err)
	}
	if check.Drifted() {
		t.Fatalf("the database is the file:\n%s", strings.Join(check.Lines(), "\n"))
	}

	// A migration from one version to the next writes what dbfixture would
	// have loaded from the new file.
	next := strings.NewReplacer("version: 1.10", "version: 1.20", "zip: 01234", "zip: 01235",
		"tags: [1.10, x, 017]", "tags: [1.20, x, 017]").Replace(file)
	old, head := fixtureSnapshot(t, cfg, file, "old"), fixtureSnapshot(t, cfg, next, "new")
	canonical(old, head)
	res, err := fixturemigrate.Compute(cfg, old, head)
	if err != nil || len(res.Refusals) != 0 || len(res.Changes) != 1 {
		t.Fatalf("%v %+v %+v", err, res.Changes, res.Refusals)
	}
	if got := res.Changes[0].New["version"].Lit; got != "1.20" {
		t.Fatalf("the migration would write version %q", got)
	}
	set := fixturechange.Set{Name: "release", Tables: res.Tables, Changes: res.Changes}
	if err := fixtureapply.Apply(ctx, db, set, quiet()); err != nil {
		t.Fatal(err)
	}
	migrated := state()
	reset()
	loadFixture(t, db, next)
	if seeded := state(); migrated != seeded {
		t.Fatalf("the migration wrote something else than dbfixture\nmigrated %s\n  seeded %s", migrated, seeded)
	}
}

// A value the column cannot hold is a finding before anything is generated,
// not a failed deploy.
func TestAValueTheColumnCannotHoldIsAFinding(t *testing.T) {
	db := awayFromUTC(t)
	cfg := shopConfig(t)
	shopTable(t, db)
	text := strings.Replace(shopFixture, "      day: 2026-03-04\n", "      day: \"2026-02-30\"\n", 1)
	text = strings.Replace(text, `token: "A0EEBC99`, `token: "Z0EEBC99`, 1)
	head := fixtureSnapshot(t, cfg, text, "fixture.yml")
	readOnlyDo(t, db, func(tx bun.Tx, tables map[string]*dbschema.Table) {
		if err := fixturemigrate.Canonicalize(context.Background(), tx, cfg, head, tables); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, f := range head.Findings {
			if f.Kind == fixturemigrate.FindingInvalidValue {
				got = append(got, f.Detail)
			}
		}
		if len(got) != 2 || !strings.Contains(fmt.Sprint(got), "day is \"2026-02-30\"") ||
			!strings.Contains(fmt.Sprint(got), "token is \"Z0EEBC99") {
			t.Fatalf("expected the date and the uuid to be reported, got %q", got)
		}
		// The transaction is still usable after the failed casts.
		if _, err := dbschema.Load(context.Background(), tx, "public"); err != nil {
			t.Fatalf("the transaction was poisoned: %v", err)
		}
	})
}
