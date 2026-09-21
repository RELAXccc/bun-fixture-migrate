package dbtest_test

// A round trip against a real PostgreSQL. It is skipped unless
// BUN_FIXTURE_MIGRATE_POSTGRES holds a DSN, so `go test ./...` passes on a
// clean checkout:
//
//	podman run --rm -d -p 55433:5432 -e POSTGRES_PASSWORD=pg --name bfm-test postgres:18
//	BUN_FIXTURE_MIGRATE_POSTGRES=postgres://postgres:pg@127.0.0.1:55433/postgres?sslmode=disable go test ./...

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

const schema = `
DROP TABLE IF EXISTS features;
DROP TABLE IF EXISTS plans;
CREATE TABLE plans (
	id          bigserial PRIMARY KEY,
	name        text NOT NULL UNIQUE,
	price_cents bigint NOT NULL DEFAULT 0,
	rating      double precision NOT NULL DEFAULT 0,
	public      boolean NOT NULL DEFAULT false,
	note        text
);
CREATE TABLE features (
	id      bigserial PRIMARY KEY,
	plan_id bigint NOT NULL REFERENCES plans (id),
	code    text NOT NULL,
	quota   bigint NOT NULL DEFAULT 0
);
`

// connect opens the throwaway database, or skips the test.
func connect(t *testing.T) *bun.DB {
	t.Helper()
	dsn := os.Getenv("BUN_FIXTURE_MIGRATE_POSTGRES")
	if dsn == "" {
		t.Skip("set BUN_FIXTURE_MIGRATE_POSTGRES to a PostgreSQL DSN to run the round trip")
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	t.Cleanup(func() { db.Close() })
	return db
}

func testDB(t *testing.T) *bun.DB {
	t.Helper()
	db := connect(t)
	ctx := context.Background()
	for _, stmt := range strings.Split(schema, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return db
}

func seed(t *testing.T, db *bun.DB) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`INSERT INTO plans (id, name, price_cents, rating, public, note) VALUES
			(1, 'free', 0, 0, true, NULL), (2, 'team', 2000, 4.5, true, 'old note')`,
		`INSERT INTO features (plan_id, code, quota) VALUES (1, 'api', 100), (2, 'api', 5000)`,
		`SELECT setval(pg_get_serial_sequence('plans', 'id'), (SELECT MAX(id) FROM plans))`,
		`SELECT setval(pg_get_serial_sequence('features', 'id'), (SELECT MAX(id) FROM features))`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

func tables() fixturechange.Tables {
	return fixturechange.Tables{
		"Plan":    {Name: "plans", ID: "id", Key: "name", Serial: true},
		"Feature": {Name: "features", ID: "id", Key: "code", Serial: true},
	}
}

// changeSet exercises every column type and every kind of change at once: an
// int, a float, a bool, a text and a NULL update, an insert with an explicit
// id, an insert with a reference and a serial id, and a delete.
func changeSet() fixturechange.Set {
	return fixturechange.Set{
		Name:           "20260921120000_fixture_round_trip",
		SeedGuardTable: "plans",
		Tables:         tables(),
		Changes: []fixturechange.Change{
			{Model: "Plan", Kind: fixturechange.Insert,
				Key: fixturechange.Values{"name": fixturechange.Lit("pro")},
				New: fixturechange.Values{
					"id": fixturechange.Lit("3"), "name": fixturechange.Lit("pro"),
					"price_cents": fixturechange.Lit("9000"), "rating": fixturechange.Lit("4.9"),
					"public": fixturechange.Lit("true"), "note": fixturechange.Null()}},
			{Model: "Feature", Kind: fixturechange.Insert,
				Key: fixturechange.Values{"plan_id": fixturechange.RefTo("Plan", "pro"), "code": fixturechange.Lit("sso")},
				New: fixturechange.Values{
					"plan_id": fixturechange.RefTo("Plan", "pro"),
					"code":    fixturechange.Lit("sso"), "quota": fixturechange.Lit("1")}},
			{Model: "Plan", Kind: fixturechange.Update,
				Key: fixturechange.Values{"name": fixturechange.Lit("team")},
				Old: fixturechange.Values{
					"price_cents": fixturechange.Lit("2000"), "rating": fixturechange.Lit("4.5"),
					"public": fixturechange.Lit("true"), "note": fixturechange.Lit("old note")},
				New: fixturechange.Values{
					"price_cents": fixturechange.Lit("2500"), "rating": fixturechange.Lit("4.75"),
					"public": fixturechange.Lit("false"), "note": fixturechange.Null()}},
			{Model: "Feature", Kind: fixturechange.Delete,
				Key: fixturechange.Values{"plan_id": fixturechange.RefTo("Plan", "free"), "code": fixturechange.Lit("api")},
				Old: fixturechange.Values{
					"plan_id": fixturechange.RefTo("Plan", "free"),
					"code":    fixturechange.Lit("api"), "quota": fixturechange.Lit("100")}},
		},
	}
}

func quiet() fixtureapply.Option { return fixtureapply.WithLogger(func(string, ...any) {}) }

func scan[T any](t *testing.T, db *bun.DB, query string, args ...any) T {
	t.Helper()
	var v T
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return v
}

func TestRoundTrip(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	ctx := context.Background()
	set := changeSet()

	if err := fixtureapply.Apply(ctx, db, set, quiet()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := scan[int64](t, db, `SELECT id FROM plans WHERE name = 'pro'`); got != 3 {
		t.Fatalf("the explicit id should have been kept, got %d", got)
	}
	if got := scan[bool](t, db, `SELECT note IS NULL FROM plans WHERE name = 'team'`); !got {
		t.Fatal("the update should have written NULL")
	}
	if got := scan[float64](t, db, `SELECT rating FROM plans WHERE name = 'team'`); got != 4.75 {
		t.Fatalf("rating = %v", got)
	}
	if got := scan[bool](t, db, `SELECT public FROM plans WHERE name = 'team'`); got {
		t.Fatal("public should be false")
	}
	if got := scan[int64](t, db, `SELECT price_cents FROM plans WHERE name = 'team'`); got != 2500 {
		t.Fatalf("price_cents = %d", got)
	}
	if got := scan[int64](t, db, `SELECT count(*) FROM features WHERE code = 'api' AND plan_id = 1`); got != 0 {
		t.Fatal("the feature should have been deleted")
	}
	// The inserted feature took its id from the sequence and the reference was
	// resolved against the plan the same change set had just inserted.
	if got := scan[int64](t, db, `SELECT plan_id FROM features WHERE code = 'sso'`); got != 3 {
		t.Fatalf("plan_id = %d", got)
	}

	// Running it again must change nothing.
	if err := fixtureapply.Apply(ctx, db, set, quiet()); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if got := scan[int64](t, db, `SELECT count(*) FROM plans`); got != 3 {
		t.Fatalf("a second run duplicated rows: %d plans", got)
	}

	// The plan was inserted with an explicit id of 3 while the sequence still
	// stood at 2. An ordinary insert now has to land on 4; without the setval
	// after the change set it would try 3 and trip the primary key.
	if _, err := db.ExecContext(ctx, `INSERT INTO plans (name) VALUES ('scratch')`); err != nil {
		t.Fatalf("the sequence was left behind: %v", err)
	}
	if got := scan[int64](t, db, `SELECT id FROM plans WHERE name = 'scratch'`); got != 4 {
		t.Fatalf("expected the sequence to continue at 4, got %d", got)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM plans WHERE name = 'scratch'`); err != nil {
		t.Fatal(err)
	}

	if err := fixtureapply.Revert(ctx, db, set, quiet()); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := scan[int64](t, db, `SELECT count(*) FROM plans WHERE name = 'pro'`); got != 0 {
		t.Fatal("the inserted plan should be gone again")
	}
	if got := scan[string](t, db, `SELECT note FROM plans WHERE name = 'team'`); got != "old note" {
		t.Fatalf("note = %q", got)
	}
	if got := scan[int64](t, db, `SELECT quota FROM features WHERE code = 'api' AND plan_id = 1`); got != 100 {
		t.Fatalf("the deleted feature should be back, quota = %d", got)
	}
}

// Somebody edited the row by hand after the fixture file was written. Their
// value wins; the migration reports that it skipped the row.
func TestAnEditedRowIsLeftAlone(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `UPDATE plans SET price_cents = 3333 WHERE name = 'team'`); err != nil {
		t.Fatal(err)
	}
	var log []string
	err := fixtureapply.Apply(ctx, db, changeSet(),
		fixtureapply.WithLogger(func(f string, a ...any) { log = append(log, f) }))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := scan[int64](t, db, `SELECT price_cents FROM plans WHERE name = 'team'`); got != 3333 {
		t.Fatalf("the hand-made edit should have survived, price_cents = %d", got)
	}
	if len(log) != len(changeSet().Changes) {
		t.Fatalf("every change should be reported, got %d lines", len(log))
	}
}

// A row that exists under another id is not inserted a second time.
func TestAnInsertSkipsANameHeldUnderAnotherID(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO plans (id, name) VALUES (99, 'pro')`); err != nil {
		t.Fatal(err)
	}
	if err := fixtureapply.Apply(ctx, db, changeSet(), quiet()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := scan[int64](t, db, `SELECT count(*) FROM plans WHERE name = 'pro'`); got != 1 {
		t.Fatalf("expected the row to be left alone, got %d rows named pro", got)
	}
	if got := scan[int64](t, db, `SELECT id FROM plans WHERE name = 'pro'`); got != 99 {
		t.Fatalf("the existing row should keep its id, got %d", got)
	}
}

// A reference that matches no row fails the migration. It used to write a NULL
// foreign key and report success.
func TestAMissingReferenceFailsInsteadOfWritingNull(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	set := changeSet()
	set.Changes = []fixturechange.Change{
		{Model: "Feature", Kind: fixturechange.Update,
			Key: fixturechange.Values{"plan_id": fixturechange.RefTo("Plan", "free"), "code": fixturechange.Lit("api")},
			Old: fixturechange.Values{"plan_id": fixturechange.RefTo("Plan", "free")},
			New: fixturechange.Values{"plan_id": fixturechange.RefTo("Plan", "ghost")}},
	}
	err := fixtureapply.Apply(context.Background(), db, set, quiet())
	if err == nil || !strings.Contains(err.Error(), "no row in plans") {
		t.Fatalf("expected a failure naming the missing row, got %v", err)
	}
	if got := scan[int64](t, db, `SELECT plan_id FROM features WHERE code = 'api' AND plan_id = 1`); got != 1 {
		t.Fatalf("the transaction should have rolled back, plan_id = %d", got)
	}
}

// Two rows with the same key cannot be told apart, so the migration stops
// rather than pick one.
func TestAnAmbiguousReferenceFails(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `ALTER TABLE features ADD CONSTRAINT c UNIQUE (plan_id, code)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO features (plan_id, code, quota) VALUES (2, 'sso', 1)`); err != nil {
		t.Fatal(err)
	}
	set := changeSet()
	set.Changes = []fixturechange.Change{
		{Model: "Plan", Kind: fixturechange.Update,
			Key: fixturechange.Values{"name": fixturechange.Lit("team")},
			Old: fixturechange.Values{"price_cents": fixturechange.Lit("2000")},
			New: fixturechange.Values{"price_cents": fixturechange.RefTo("Feature", "api")}},
	}
	err := fixtureapply.Apply(ctx, db, set, quiet())
	if err == nil || !strings.Contains(err.Error(), "more than one row") {
		t.Fatalf("expected a failure about the ambiguous reference, got %v", err)
	}
}

// Before the fixture loader has run there is nothing to migrate, and inserting
// the new rows now would collide with the seed that follows.
func TestAnUnseededDatabaseIsLeftAlone(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := fixtureapply.Apply(ctx, db, changeSet(), quiet()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := scan[int64](t, db, `SELECT count(*) FROM plans`); got != 0 {
		t.Fatalf("expected an untouched database, got %d plans", got)
	}
}

// The subtlest of the lot, and the reason the run time diagnoses a zero row
// count instead of shrugging at it. bun's migrator records a migration as
// applied the moment the function returns nil, so an update that quietly
// matched nothing is a change that will never be attempted again. It has to
// fail.
func TestAnUpdateOfAMissingRowFailsInsteadOfBeingRecorded(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DELETE FROM features`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM plans WHERE name = 'team'`); err != nil {
		t.Fatal(err)
	}
	set := changeSet()
	err := fixtureapply.Apply(ctx, db, set, quiet())
	if err == nil {
		t.Fatal("a change that could not be made must not report success")
	}
	if !strings.Contains(err.Error(), "no row of plans has name=team") {
		t.Fatalf("the error has to say which row: %v", err)
	}
	// And the transaction rolled back, so the insert that came before the
	// failing update is gone too.
	if got := scan[int64](t, db, `SELECT count(*) FROM plans WHERE name = 'pro'`); got != 0 {
		t.Fatalf("expected a rollback, found %d rows named pro", got)
	}
}

// Set missing_row to warn and the same run is a warning instead. This is the
// setting that can lose a change, which is why it is not the default.
func TestMissingRowCanBeDowngradedToAWarning(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DELETE FROM features`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM plans WHERE name = 'team'`); err != nil {
		t.Fatal(err)
	}
	set := changeSet()
	set.Policy.MissingRow = "warn"
	var log []string
	err := fixtureapply.Apply(ctx, db, set,
		fixtureapply.WithLogger(func(f string, a ...any) { log = append(log, fmt.Sprintf(f, a...)) }))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var skipped int
	for _, line := range log {
		if strings.Contains(line, "SKIPPED") {
			skipped++
		}
	}
	if skipped == 0 {
		t.Fatalf("the skip has to be reported: %v", log)
	}
}

// A second run of the same change set finds the database already in the state
// it wanted. That is not drift and not a failure.
func TestASecondRunIsBenign(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	ctx := context.Background()
	set := changeSet()
	if err := fixtureapply.Apply(ctx, db, set, quiet()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var log []string
	if err := fixtureapply.Apply(ctx, db, set,
		fixtureapply.WithLogger(func(f string, a ...any) { log = append(log, fmt.Sprintf(f, a...)) })); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	for _, line := range log {
		if strings.Contains(line, "SKIPPED") {
			t.Fatalf("an already-applied change is not a skip worth shouting about: %s", line)
		}
	}
	if !strings.Contains(strings.Join(log, "\n"), "already holds these values") {
		t.Fatalf("expected the benign wording: %v", log)
	}
}

// Somebody changed the row here. The default keeps their change and says so;
// setting changed_row to error stops the deploy instead.
func TestAChangedRowIsAWarningOrAnError(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `UPDATE plans SET price_cents = 3333 WHERE name = 'team'`); err != nil {
		t.Fatal(err)
	}
	var log []string
	if err := fixtureapply.Apply(ctx, db, changeSet(),
		fixtureapply.WithLogger(func(f string, a ...any) { log = append(log, fmt.Sprintf(f, a...)) })); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(strings.Join(log, "\n"), "somebody changed it in this database") {
		t.Fatalf("the operator has to be told what happened: %v", log)
	}
	if got := scan[int64](t, db, `SELECT price_cents FROM plans WHERE name = 'team'`); got != 3333 {
		t.Fatalf("their change should survive, got %d", got)
	}

	strict := changeSet()
	strict.Policy.ChangedRow = "error"
	err := fixtureapply.Apply(ctx, db, strict, quiet())
	if err == nil || !strings.Contains(err.Error(), "no longer holds the values") {
		t.Fatalf("expected the strict policy to stop, got %v", err)
	}
}

// The fixture file's id is held by a different row. Inserting anyway either
// trips the primary key or, with no unique index, leaves two rows nothing can
// tell apart. The check runs before the statement, so the message names the
// row rather than arriving as a constraint violation.
func TestAnInsertWhoseIDIsTakenIsReported(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO plans (id, name) VALUES (3, 'something else')`); err != nil {
		t.Fatal(err)
	}
	err := fixtureapply.Apply(ctx, db, changeSet(), quiet())
	if err == nil || !strings.Contains(err.Error(), "already held by the row") {
		t.Fatalf("expected the conflict to be named, got %v", err)
	}
	if !strings.Contains(err.Error(), "something else") {
		t.Fatalf("the message has to name the row holding the id: %v", err)
	}
}

// A rename is an update of the key columns, guarded by the id as well, so it
// cannot land on a row that merely carries the old name.
func TestARenameUpdatesTheKeyColumnUnderItsIDGuard(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	ctx := context.Background()
	set := changeSet()
	set.Changes = []fixturechange.Change{
		{Model: "Plan", Kind: fixturechange.Update, ID: "2",
			Key: fixturechange.Values{"name": fixturechange.Lit("team")},
			Old: fixturechange.Values{"name": fixturechange.Lit("team")},
			New: fixturechange.Values{"name": fixturechange.Lit("crew")}},
	}
	if err := fixtureapply.Apply(ctx, db, set, quiet()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := scan[int64](t, db, `SELECT id FROM plans WHERE name = 'crew'`); got != 2 {
		t.Fatalf("the row kept its id, got %d", got)
	}

	// On a database where that name belongs to another row, the id guard stops
	// it rather than renaming the wrong one.
	db2 := testDB(t)
	seed(t, db2)
	if _, err := db2.ExecContext(ctx, `DELETE FROM features`); err != nil {
		t.Fatal(err)
	}
	if _, err := db2.ExecContext(ctx, `UPDATE plans SET id = 5 WHERE name = 'team'`); err != nil {
		t.Fatal(err)
	}
	err := fixtureapply.Apply(ctx, db2, set, quiet())
	if err == nil || !strings.Contains(err.Error(), "under id 5 and not 2") {
		t.Fatalf("expected the id drift to be named, got %v", err)
	}
}
