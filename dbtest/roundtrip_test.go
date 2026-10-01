package dbtest_test

// A round trip against a real PostgreSQL. It is skipped unless
// BUN_FIXTURE_MIGRATE_POSTGRES holds a DSN, so `go test ./...` passes on a
// clean checkout:
//
//	podman run --rm -d -p 55433:5432 -e POSTGRES_PASSWORD=pg --name bfm-test postgres:18
//	BUN_FIXTURE_MIGRATE_POSTGRES=postgres://postgres:pg@127.0.0.1:55433/postgres?sslmode=disable go test ./...

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
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
	db := openDB(t, dsn, nil)
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
	var outcomes []fixtureapply.Outcome
	err := fixtureapply.Apply(ctx, db, changeSet(), quiet(),
		fixtureapply.WithReport(func(o fixtureapply.Outcome) { outcomes = append(outcomes, o) }))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := scan[int64](t, db, `SELECT price_cents FROM plans WHERE name = 'team'`); got != 3333 {
		t.Fatalf("the hand-made edit should have survived, price_cents = %d", got)
	}
	reported := map[int]fixtureapply.Status{}
	for _, o := range outcomes {
		if o.Index >= 0 {
			reported[o.Index] = o.Status
		}
	}
	if len(reported) != len(changeSet().Changes) {
		t.Fatalf("every change should be reported, got %v", reported)
	}
	if reported[2] != fixtureapply.StatusSkipped {
		t.Fatalf("the edited row's update should be skipped, got %v", reported)
	}
}

// A row that exists under another id is not inserted a second time, and is not
// the row the fixture file describes either, even when every other value
// agrees: it is id drift, under policy.id_drift. It used to be reported as a
// row already there, nothing to do.
func TestAnInsertOfANameHeldUnderAnotherIDIsIDDrift(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	ctx := context.Background()
	// Every value the insert writes, under another id.
	if _, err := db.ExecContext(ctx, `INSERT INTO plans (id, name, price_cents, rating, public, note)
		VALUES (99, 'pro', 9000, 4.9, true, NULL)`); err != nil {
		t.Fatal(err)
	}
	set := changeSet()
	set.Changes = set.Changes[:1]
	for _, tc := range []struct {
		policy fixturechange.Mode
		fails  bool
		status fixtureapply.Status
	}{
		{"", true, fixtureapply.StatusFailed},
		{fixturechange.ModeWarn, false, fixtureapply.StatusSkipped},
		{fixturechange.ModeIgnore, false, fixtureapply.StatusUnchanged},
	} {
		set.Policy.IDDrift = tc.policy
		outcomes, err := applyReporting(t, db, set)
		if (err != nil) != tc.fails {
			t.Fatalf("id_drift %q: %v", tc.policy, err)
		}
		if len(outcomes) != 1 || outcomes[0].Status != tc.status {
			t.Fatalf("id_drift %q: %+v", tc.policy, outcomes)
		}
		if tc.policy != fixturechange.ModeIgnore && (outcomes[0].Problem != fixtureapply.ProblemIDDrift ||
			!strings.Contains(outcomes[0].Message, "exists, but under id 99 and not 3")) {
			t.Fatalf("id_drift %q: %+v", tc.policy, outcomes)
		}
		if got := scan[int64](t, db, `SELECT count(*) FROM plans WHERE name = 'pro'`); got != 1 {
			t.Fatalf("expected the row to be left alone, got %d rows named pro", got)
		}
		if got := scan[int64](t, db, `SELECT id FROM plans WHERE name = 'pro'`); got != 99 {
			t.Fatalf("the existing row should keep its id, got %d", got)
		}
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
	var outcomes []fixtureapply.Outcome
	err := fixtureapply.Apply(context.Background(), db, set, quiet(),
		fixtureapply.WithReport(func(o fixtureapply.Outcome) { outcomes = append(outcomes, o) }))
	if err == nil || !strings.Contains(err.Error(),
		`plan_id is to point at Plan "ghost", and no row of plans has name = "ghost"`) {
		t.Fatalf("expected a failure naming the missing row, got %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].Problem != fixtureapply.ProblemError {
		t.Fatalf("a reference the change writes fails whatever the policy says: %+v", outcomes)
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
	// Dropping the change from the file would keep it from every other
	// database too; the policy of this one migration is the remedy.
	if !strings.Contains(err.Error(), `set MissingRow to "warn" in this migration's Policy`) ||
		strings.Contains(err.Error(), "drop this change") {
		t.Fatalf("the error has to name a remedy that keeps the change for other databases: %v", err)
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
	if !strings.Contains(strings.Join(log, "\n"), "it was changed in this database") {
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

	// id_drift warn and ignore are what a database whose ids are not the
	// file's runs under: the rename finds its row by the old name alone,
	// warns about the id under warn, and a second run finds it made. Guarded
	// by the file's id, it was skipped and recorded.
	for _, policy := range []fixturechange.Mode{fixturechange.ModeWarn, fixturechange.ModeIgnore} {
		run(t, db2, "UPDATE plans SET name = 'team' WHERE id = 5")
		set.Policy.IDDrift = policy
		outcomes, err := applyReporting(t, db2, set)
		if err != nil || len(outcomes) != 1 || outcomes[0].Status != fixtureapply.StatusApplied {
			t.Fatalf("%s: %v %+v", policy, err, outcomes)
		}
		if warned := strings.Contains(outcomes[0].Message, "is under id 5, not 2"); warned != (policy == fixturechange.ModeWarn) {
			t.Fatalf("%s: %+v", policy, outcomes[0])
		}
		if got := scan[int64](t, db2, `SELECT id FROM plans WHERE name = 'crew'`); got != 5 {
			t.Fatalf("%s: crew is %d", policy, got)
		}
		if outcomes, err = applyReporting(t, db2, set); err != nil || outcomes[0].Status != fixtureapply.StatusUnchanged {
			t.Fatalf("%s: a second run: %v %+v", policy, err, outcomes)
		}
	}
}

// A rename written as an update (renames: update) behaves like every other
// change: a second run finds it made, a revert puts the old name back, and so
// does a second revert. Keyed on the old name alone, the second run, the
// revert and a plan of the applied migration all failed as a missing row.
func TestARenameRunsTwiceAndReverts(t *testing.T) {
	db := connect(t)
	db.RegisterModel((*Currency)(nil), (*Plan)(nil), (*Feature)(nil))
	ctx := context.Background()
	cfg := pipelineConfig(t)
	cfg.Policy.Renames = fixturemigrate.RenameUpdate
	renamed := replaceOnce(t, oldFixture, "      name: team\n      currency_id: '{{ $.Currency.eur.ID }}'\n      price_cents: 2000\n",
		"      name: crew\n      currency_id: '{{ $.Currency.eur.ID }}'\n      price_cents: 2500\n")
	renamed = replaceOnce(t, renamed, "      code: sso\n      quota: 1\n", "      code: sso\n      quota: 2\n")
	res, err := fixturemigrate.Compute(cfg, fixtureSnapshot(t, cfg, oldFixture, "base"),
		fixtureSnapshot(t, cfg, renamed, "head"))
	if err != nil || len(res.Refusals) != 0 {
		t.Fatalf("Compute: %v %+v", err, res.Refusals)
	}
	if len(res.Changes) == 0 || res.Changes[0].ID == "" {
		t.Fatalf("expected a rename first, got %+v", res.Changes)
	}
	set := fixturechange.Set{Name: "rename", SeedGuardTable: "plans", Tables: res.Tables, Changes: res.Changes}

	resetSchema(t, db)
	load(t, db, renamed)
	want := snapshot(t, db)
	resetSchema(t, db)
	load(t, db, oldFixture)
	before := snapshot(t, db)

	run := func(what string, fn func(context.Context, bun.IDB, fixturechange.Set, ...fixtureapply.Option) error,
		set fixturechange.Set, wantStatus fixtureapply.Status, wantState string) {
		t.Helper()
		var outcomes []fixtureapply.Outcome
		if err := fn(ctx, db, set, quiet(), fixtureapply.WithReport(func(o fixtureapply.Outcome) {
			outcomes = append(outcomes, o)
		})); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		for _, o := range outcomes {
			if o.Index >= 0 && o.Status != wantStatus {
				t.Fatalf("%s: %s %s %s is %s, want %s: %s", what, o.Model, o.Key, o.Kind, o.Status, wantStatus, o.Message)
			}
		}
		if got := snapshot(t, db); got != wantState {
			t.Fatalf("%s left\n%s\nwant\n%s", what, got, wantState)
		}
	}
	run("Apply", fixtureapply.Apply, set, fixtureapply.StatusApplied, want)
	if got := scan[int64](t, db, `SELECT id FROM plans WHERE name = 'crew'`); got != 2 {
		t.Fatalf("the renamed row keeps its id, got %d", got)
	}
	run("a second Apply", fixtureapply.Apply, set, fixtureapply.StatusUnchanged, want)
	run("Revert", fixtureapply.Revert, set, fixtureapply.StatusApplied, before)
	// The changes after the rename find their rows under the new name, which
	// a revert takes away; the rename itself finds its row either way. bun
	// never rolls one migration back twice, so only the rename is run again.
	rename := set
	rename.Changes = set.Changes[:1]
	run("a second Revert of the rename", fixtureapply.Revert, rename, fixtureapply.StatusUnchanged, before)
	run("Apply after the revert", fixtureapply.Apply, set, fixtureapply.StatusApplied, want)

	// Another row took the old name after the rename: the rename is still the
	// one this change made, and says so.
	if _, err := db.ExecContext(ctx, `INSERT INTO plans (name, currency_id, price_cents, seats, rating, public)
		VALUES ('team', 1, 1, 1, 1, true)`); err != nil {
		t.Fatal(err)
	}
	var outcomes []fixtureapply.Outcome
	if err := fixtureapply.Apply(ctx, db, set, quiet(),
		fixtureapply.WithReport(func(o fixtureapply.Outcome) { outcomes = append(outcomes, o) })); err != nil {
		t.Fatalf("Apply with the old name taken again: %v", err)
	}
	if outcomes[0].Status != fixtureapply.StatusUnchanged || !strings.Contains(outcomes[0].Message, "as name=crew") {
		t.Fatalf("the rename is made already: %+v", outcomes[0])
	}
}

// A reference in a guard whose row an admin renamed or removed is a row that no
// longer holds what the change was generated against, and goes through the
// policy like any other. It used to fail the deploy outright, whatever
// changed_row said, which is the admin-UI case the policy exists for.
func TestAGuardReferenceToARenamedRowFollowsThePolicy(t *testing.T) {
	db := connect(t)
	db.RegisterModel((*Currency)(nil), (*Plan)(nil), (*Feature)(nil))
	reset := func() {
		t.Helper()
		resetSchema(t, db)
		load(t, db, oldFixture)
		run(t, db, "INSERT INTO currencies (id, code, symbol) VALUES (2, 'USD', '$')",
			"UPDATE currencies SET code = 'EURO' WHERE code = 'EUR'", // an admin renamed it
			"UPDATE plans SET name = 'crew' WHERE name = 'team'")     // and this one
	}
	tables := fixturechange.Tables{
		"Currency": {Name: "currencies", ID: "id", Key: "code"},
		"Plan":     {Name: "plans", ID: "id", Key: "name", Serial: true},
		"Feature":  {Name: "features", ID: "id", Serial: true},
	}
	currency := fixturechange.Change{Model: "Plan", Kind: fixturechange.Update,
		Key: fixturechange.Values{"name": fixturechange.Lit("free")},
		Old: fixturechange.Values{"currency_id": fixturechange.RefTo("Currency", "EUR")},
		New: fixturechange.Values{"currency_id": fixturechange.RefTo("Currency", "USD")}}
	feature := fixturechange.Change{Model: "Feature", Kind: fixturechange.Update,
		Key: fixturechange.Values{"plan_id": fixturechange.RefTo("Plan", "team"), "code": fixturechange.Lit("api")},
		Old: fixturechange.Values{"quota": fixturechange.Lit("5000")},
		New: fixturechange.Values{"quota": fixturechange.Lit("6000")}}
	gone := fixturechange.Change{Model: "Feature", Kind: fixturechange.Delete,
		Key: fixturechange.Values{"plan_id": fixturechange.RefTo("Plan", "team"), "code": fixturechange.Lit("sso")},
		Old: fixturechange.Values{"plan_id": fixturechange.RefTo("Plan", "team"), "code": fixturechange.Lit("sso"),
			"quota": fixturechange.Lit("1")}}

	for _, tc := range []struct {
		name    string
		change  fixturechange.Change
		policy  fixturechange.Policy
		fails   bool
		status  fixtureapply.Status
		problem fixtureapply.Problem
		says    string
	}{
		{"an old value, changed_row warn", currency, fixturechange.Policy{}, false,
			fixtureapply.StatusSkipped, fixtureapply.ProblemChangedRow, `Currency "EUR" is not in this database`},
		{"an old value, changed_row error", currency, fixturechange.Policy{ChangedRow: fixturechange.ModeError}, true,
			fixtureapply.StatusFailed, fixtureapply.ProblemChangedRow, `Currency "EUR" is not in this database`},
		{"the natural key, missing_row error", feature, fixturechange.Policy{}, true,
			fixtureapply.StatusFailed, fixtureapply.ProblemMissingRow, `Plan "team" is not in this database`},
		{"the natural key, missing_row warn", feature, fixturechange.Policy{MissingRow: fixturechange.ModeWarn}, false,
			fixtureapply.StatusSkipped, fixtureapply.ProblemMissingRow, `Plan "team" is not in this database`},
		{"the natural key of a delete", gone, fixturechange.Policy{}, false,
			fixtureapply.StatusUnchanged, "", `Plan "team" is not in this database`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reset()
			before := snapshot(t, db)
			set := fixturechange.Set{Name: "refs", SeedGuardTable: "plans", Tables: tables, Policy: tc.policy,
				Changes: []fixturechange.Change{tc.change}}
			outcomes, err := applyReporting(t, db, set)
			if (err != nil) != tc.fails {
				t.Fatalf("Apply: %v", err)
			}
			if len(outcomes) != 1 || outcomes[0].Status != tc.status || outcomes[0].Problem != tc.problem ||
				!strings.Contains(outcomes[0].Message, tc.says) {
				t.Fatalf("outcomes %+v", outcomes)
			}
			if after := snapshot(t, db); after != before {
				t.Fatalf("nothing may change:\n%s\nwas\n%s", after, before)
			}
		})
	}
}

// Global tags and each tenant's own share one table, and the model's where says
// which rows are master data. Every statement, lookup and reference has to stay
// inside it: before, the relabel of a global tag relabelled a tenant's tag too,
// the insert of a global tag was skipped because a tenant had one by that code,
// and a new rule was bound to a tenant's private tag.
func TestAModelsWhereLimitsEveryStatement(t *testing.T) {
	db := connect(t)
	ctx := context.Background()
	run(t, db,
		"DROP TABLE IF EXISTS scoped_tag_rules", "DROP TABLE IF EXISTS scoped_tags",
		`CREATE TABLE scoped_tags (id bigserial PRIMARY KEY, tenant_id bigint, code text NOT NULL, label text NOT NULL,
			flags jsonb NOT NULL DEFAULT '{}')`,
		"CREATE UNIQUE INDEX ON scoped_tags (code) WHERE tenant_id IS NULL",
		`CREATE TABLE scoped_tag_rules (id bigserial PRIMARY KEY, name text NOT NULL UNIQUE,
			tag_id bigint NOT NULL REFERENCES scoped_tags (id))`,
		"INSERT INTO scoped_tags (id, code, label) VALUES (1, 'urgent', 'Urgent'), (2, 'later', 'Later'), (3, 'done', 'Done')",
		"INSERT INTO scoped_tag_rules (id, name, tag_id) VALUES (1, 'escalate', 1)",
		// Tenant 1's own tags, with the codes and labels of global ones.
		`INSERT INTO scoped_tags (id, tenant_id, code, label) VALUES (5, 1, 'urgent', 'Urgent'), (6, 1, 'blocked', 'Blocked'),
			(7, 1, 'later', 'Later')`,
		`INSERT INTO scoped_tags (id, code, label, flags) VALUES (8, 'secret', 'Secret', '{"private": true}')`)
	dump := func() string {
		return scan[string](t, db, `SELECT string_agg(concat_ws(' ', id, tenant_id, code, label), ', ' ORDER BY id) FROM scoped_tags`) +
			" | " + scan[string](t, db, `SELECT string_agg(concat_ws(' ', id, name, tag_id), ', ' ORDER BY id) FROM scoped_tag_rules`)
	}
	before := dump()
	tag := func(code string) fixturechange.Values { return fixturechange.Values{"code": fixturechange.Lit(code)} }
	set := fixturechange.Set{
		Name:           "20260921120000_fixture_tags",
		SeedGuardTable: "scoped_tags",
		Tables: fixturechange.Tables{
			// A ? that bun must not take for a placeholder, and a comment that
			// must not swallow the rest of the statement.
			"Tag":     {Name: "scoped_tags", ID: "id", Key: "code", Serial: true, Where: "tenant_id IS NULL AND NOT flags ? 'private' -- global rows"},
			"TagRule": {Name: "scoped_tag_rules", ID: "id", Key: "name", Serial: true},
		},
		Changes: []fixturechange.Change{
			{Model: "Tag", Kind: fixturechange.Update, Key: tag("urgent"),
				Old: fixturechange.Values{"label": fixturechange.Lit("Urgent")},
				New: fixturechange.Values{"label": fixturechange.Lit("URGENT")}},
			{Model: "Tag", Kind: fixturechange.Insert, Key: tag("blocked"),
				New: fixturechange.Values{"id": fixturechange.Lit("100"), "code": fixturechange.Lit("blocked"),
					"label": fixturechange.Lit("Blocked globally")}},
			{Model: "TagRule", Kind: fixturechange.Insert, Key: fixturechange.Values{"name": fixturechange.Lit("page")},
				New: fixturechange.Values{"id": fixturechange.Lit("3"), "name": fixturechange.Lit("page"),
					"tag_id": fixturechange.RefTo("Tag", "urgent")}},
			{Model: "Tag", Kind: fixturechange.Delete, Key: tag("later"),
				Old: fixturechange.Values{"id": fixturechange.Lit("2"), "code": fixturechange.Lit("later"),
					"label": fixturechange.Lit("Later")}},
		},
	}
	outcomes, err := applyReporting(t, db, set)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, o := range outcomes {
		if o.Index >= 0 && (o.Status != fixtureapply.StatusApplied || o.Rows != 1) {
			t.Fatalf("every change applies to exactly the global row: %+v", o)
		}
	}
	const after = "1 urgent URGENT, 3 done Done, 5 1 urgent Urgent, 6 1 blocked Blocked, 7 1 later Later, " +
		"8 secret Secret, 100 blocked Blocked globally | 1 escalate 1, 3 page 1"
	if got := dump(); got != after {
		t.Fatalf("after Apply\n got %s\nwant %s", got, after)
	}
	outcomes, err = applyReporting(t, db, set)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	for _, o := range outcomes {
		if o.Index >= 0 && o.Status != fixtureapply.StatusUnchanged {
			t.Fatalf("a second run finds every change made: %+v", o)
		}
	}
	if err := fixtureapply.Revert(ctx, db, set, quiet()); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := dump(); got != before {
		t.Fatalf("after Revert\n got %s\nwant %s", got, before)
	}

	// A row the change writes has to be master data afterwards, or nothing
	// would find it again.
	set.Changes = []fixturechange.Change{{Model: "Tag", Kind: fixturechange.Insert, Key: tag("mine"),
		New: fixturechange.Values{"code": fixturechange.Lit("mine"), "label": fixturechange.Lit("Mine"),
			"tenant_id": fixturechange.Lit("1")}}}
	if _, err := applyReporting(t, db, set); err == nil || !strings.Contains(err.Error(), "does not hold the model's where") {
		t.Fatalf("want the row outside the where refused, got %v", err)
	}
	if got := dump(); got != before {
		t.Fatalf("nothing may change\n got %s\nwant %s", got, before)
	}
}

// Something other than the data can stop a guarded statement: a row-level
// security policy, a BEFORE trigger that returns NULL, a rule. Each made the
// statement change nothing, which was read as a row somebody had changed: the
// change was skipped and the migration recorded as applied. A policy that hid
// the seed guard table's rows made the whole set a recorded no-op. Each is an
// error now.
func TestAStatementSomethingElseStoppedFails(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	ctx := context.Background()
	update := changeSet()
	update.Changes = update.Changes[2:3] // team's price, rating, flag and note
	insert := changeSet()
	insert.Changes = insert.Changes[:1] // pro

	t.Run("a trigger", func(t *testing.T) {
		run(t, db, `CREATE OR REPLACE FUNCTION bfm_refuse() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RETURN NULL; END$$`,
			"CREATE TRIGGER bfm_refuse BEFORE UPDATE OR INSERT ON plans FOR EACH ROW EXECUTE FUNCTION bfm_refuse()")
		defer run(t, db, "DROP TRIGGER bfm_refuse ON plans")
		for _, set := range []fixturechange.Set{update, insert} {
			outcomes, err := applyReporting(t, db, set)
			if err == nil || !strings.Contains(err.Error(), "a BEFORE trigger that returned NULL, a rule, or a row-level security policy stopped it") {
				t.Fatalf("want the stopped statement named, got %v", err)
			}
			if len(outcomes) != 1 || outcomes[0].Status != fixtureapply.StatusFailed {
				t.Fatalf("outcomes %+v", outcomes)
			}
		}
	})

	// The policies apply to a role that does not own the table, as an
	// application role often does not.
	run(t, db,
		`DO $$BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'bfm_rls') THEN CREATE ROLE bfm_rls; END IF; END$$`,
		"GRANT USAGE ON SCHEMA public TO bfm_rls",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON plans, features TO bfm_rls",
		"GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO bfm_rls",
		"ALTER TABLE plans ENABLE ROW LEVEL SECURITY",
		"CREATE POLICY plans_read ON plans FOR SELECT USING (true)",
		"CREATE POLICY plans_write ON plans FOR UPDATE USING (name <> 'team')",
		"CREATE POLICY plans_add ON plans FOR INSERT WITH CHECK (true)")
	asRole := func(set fixturechange.Set) error {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE bfm_rls"); err != nil {
			t.Fatal(err)
		}
		return fixtureapply.Apply(ctx, tx, set, quiet())
	}
	t.Run("an update policy", func(t *testing.T) {
		err := asRole(update)
		if err == nil || !strings.Contains(err.Error(), "a row-level security policy applies to it") {
			t.Fatalf("want the policy named, got %v", err)
		}
	})
	t.Run("a policy that hides the seed guard table", func(t *testing.T) {
		run(t, db, "DROP POLICY plans_read ON plans", "CREATE POLICY plans_read ON plans FOR SELECT USING (false)")
		err := asRole(update)
		if err == nil || !strings.Contains(err.Error(), "a row-level security policy applies to it") {
			t.Fatalf("want the policy named rather than an unseeded database, got %v", err)
		}
	})
	if got := scan[int64](t, db, "SELECT price_cents FROM plans WHERE name = 'team'"); got != 2000 {
		t.Fatalf("nothing may change, price_cents = %d", got)
	}
}

// A char(n) column compared through "character", which is char(1): 'EUR'
// became 'E'. An update of a char(5) column never matched its guard and was
// skipped as a changed row, a char(3)[] value was written as {"E  ","E  "},
// every change keyed on a char(3) code failed as a missing row, and an insert
// of a row already there failed as an id held by another row.
func TestACharColumnKeepsItsLength(t *testing.T) {
	db := connect(t)
	run(t, db, "DROP TABLE IF EXISTS char_currencies",
		"CREATE TABLE char_currencies (id bigserial PRIMARY KEY, code char(3) NOT NULL UNIQUE, label char(5) NOT NULL, aliases char(3)[])",
		"INSERT INTO char_currencies (id, code, label, aliases) VALUES (1, 'EUR', 'Euro', '{EUR,EWR}'), (2, 'USD', 'Dolr', NULL)")
	code := func(c string) fixturechange.Values { return fixturechange.Values{"code": fixturechange.Lit(c)} }
	set := fixturechange.Set{
		Name:   "20260921120000_fixture_chars",
		Tables: fixturechange.Tables{"Currency": {Name: "char_currencies", ID: "id", Key: "code", Serial: true}},
		Changes: []fixturechange.Change{
			{Model: "Currency", Kind: fixturechange.Update, Key: code("EUR"),
				Old: fixturechange.Values{"label": fixturechange.Lit("Euro"), "aliases": fixturechange.Lit(`["EUR","EWR"]`)},
				New: fixturechange.Values{"label": fixturechange.Lit("Euros"), "aliases": fixturechange.Lit(`["EUR","EWR","ECU"]`)}},
			{Model: "Currency", Kind: fixturechange.Insert, Key: code("GBP"),
				New: fixturechange.Values{"id": fixturechange.Lit("3"), "code": fixturechange.Lit("GBP"),
					"label": fixturechange.Lit("Pound"), "aliases": fixturechange.Lit(`["GBP","STG"]`)}},
			{Model: "Currency", Kind: fixturechange.Delete, Key: code("USD"),
				Old: fixturechange.Values{"id": fixturechange.Lit("2"), "code": fixturechange.Lit("USD"),
					"label": fixturechange.Lit("Dolr"), "aliases": fixturechange.Null()}},
		},
	}
	dump := func() string {
		return scan[string](t, db, `SELECT string_agg(concat_ws(' ', id, code, label, aliases::text), ', ' ORDER BY id) FROM char_currencies`)
	}
	before := dump()
	for _, want := range []fixtureapply.Status{fixtureapply.StatusApplied, fixtureapply.StatusUnchanged} {
		outcomes, err := applyReporting(t, db, set)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		for _, o := range outcomes {
			if o.Index >= 0 && o.Status != want {
				t.Fatalf("want every change %s: %+v", want, o)
			}
		}
		if got := dump(); got != "1 EUR Euros {EUR,EWR,ECU}, 3 GBP Pound {GBP,STG}" {
			t.Fatalf("after Apply: %s", got)
		}
	}
	if err := fixtureapply.Revert(context.Background(), db, set, quiet()); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := dump(); got != before {
		t.Fatalf("after Revert: %s, want %s", got, before)
	}
}

// box and circle have an = that compares areas. A hand edit to another box of
// the same area passed the guard and was overwritten; it is a changed row.
func TestAGeometricValueIsComparedAsItself(t *testing.T) {
	db := connect(t)
	run(t, db, "DROP TABLE IF EXISTS zones",
		"CREATE TABLE zones (id bigserial PRIMARY KEY, name text NOT NULL UNIQUE, area box, c circle)",
		"INSERT INTO zones (name, area, c) VALUES ('zone', '(1,1),(0,0)', '<(0,0),1>')")
	set := fixturechange.Set{
		Name:   "20260921120000_fixture_zones",
		Tables: fixturechange.Tables{"Zone": {Name: "zones", ID: "id", Key: "name"}},
		Changes: []fixturechange.Change{{Model: "Zone", Kind: fixturechange.Update,
			Key: fixturechange.Values{"name": fixturechange.Lit("zone")},
			Old: fixturechange.Values{"area": fixturechange.Lit("(1,1),(0,0)"), "c": fixturechange.Lit("<(0,0),1>")},
			New: fixturechange.Values{"area": fixturechange.Lit("(3,3),(0,0)"), "c": fixturechange.Lit("<(5,5),2>")}}},
	}
	// A hand edit: another box and another circle, of the same areas.
	run(t, db, "UPDATE zones SET area = '(4,0.25),(0,0)', c = '<(9,9),1>'")
	outcomes, err := applyReporting(t, db, set)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 1 || outcomes[0].Problem != fixtureapply.ProblemChangedRow {
		t.Fatalf("the edit is a changed row: %+v", outcomes)
	}
	if got := scan[string](t, db, "SELECT area::text || ' ' || c::text FROM zones"); got != "(4,0.25),(0,0) <(9,9),1>" {
		t.Fatalf("the hand edit has to survive, got %s", got)
	}
	// Without the edit the change is made, and a second run finds it made.
	run(t, db, "UPDATE zones SET area = '(1,1),(0,0)', c = '<(0,0),1>'")
	for _, want := range []fixtureapply.Status{fixtureapply.StatusApplied, fixtureapply.StatusUnchanged} {
		outcomes, err := applyReporting(t, db, set)
		if err != nil || len(outcomes) != 1 || outcomes[0].Status != want {
			t.Fatalf("want %s: %v %+v", want, err, outcomes)
		}
	}
}

// Revert compares each row with what the migration writes, and nothing records
// whether the migration wrote it on this database. Where the row does not hold
// it, the change is not reverted, and the outcome says so in those terms
// rather than as a row the change was generated against.
func TestARevertSaysWhatItLeftAlone(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	ctx := context.Background()
	set := changeSet()
	if err := fixtureapply.Apply(ctx, db, set, quiet()); err != nil {
		t.Fatal(err)
	}
	run(t, db, "UPDATE plans SET price_cents = 3333 WHERE name = 'team'")
	var outcomes []fixtureapply.Outcome
	if err := fixtureapply.Revert(ctx, db, set, quiet(),
		fixtureapply.WithReport(func(o fixtureapply.Outcome) { outcomes = append(outcomes, o) })); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	var team fixtureapply.Outcome
	for _, o := range outcomes {
		if o.Index == 2 {
			team = o
		}
	}
	if team.Status != fixtureapply.StatusSkipped ||
		!strings.Contains(team.Message, "does not hold what the migration writes, so this change was not reverted") {
		t.Fatalf("the revert has to say why it left the row alone: %+v", team)
	}
	if got := scan[int64](t, db, "SELECT price_cents FROM plans WHERE name = 'team'"); got != 3333 {
		t.Fatalf("price_cents = %d", got)
	}
}

// The application inserts into a table the migration writes explicit ids into,
// while the migration runs. The sequence was moved past those ids only once
// the whole set was done, so the application drew one of them meanwhile, and
// one of the two inserts failed on the primary key.
func TestTheSequenceMovesBeforeAnExplicitIDIsWritten(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	ctx := context.Background()
	set := changeSet()
	// pro, with id 3 while the sequence stands at 2, then team's update,
	// which waits for a lock while the application inserts.
	set.Changes = set.Changes[:3]
	admin, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Rollback()
	if _, err := admin.ExecContext(ctx, "SELECT 1 FROM plans WHERE name = 'team' FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	var outcomes []fixtureapply.Outcome
	go func() {
		done <- fixtureapply.Apply(ctx, db, set, quiet(),
			fixtureapply.WithReport(func(o fixtureapply.Outcome) { outcomes = append(outcomes, o) }))
	}()
	for i := 0; scan[int64](t, db, "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' "+
		"AND datname = current_database()") == 0; i++ {
		if i == 100 {
			t.Fatal("the change set never waited for the row")
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := scan[int64](t, db, "SELECT nextval(pg_get_serial_sequence('plans', 'id'))")
	if err := admin.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got <= 3 {
		t.Fatalf("the application drew id %d while the migration was writing id 3", got)
	}
	// The move is reported as it was when it came after the set.
	if last := outcomes[len(outcomes)-1]; last.Status != fixtureapply.StatusSequence || last.Model != "Plan" {
		t.Fatalf("the sequence move has to be reported: %+v", outcomes)
	}
}

// An id held by a row of a model nobody points at, which has no key column to
// name it by, is named by the change's natural key.
func TestAnIDHeldByAnotherRowIsNamedByItsKey(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	set := changeSet()
	set.Tables["Feature"] = fixturechange.Table{Name: "features", ID: "id", Serial: true}
	set.Changes = []fixturechange.Change{{Model: "Feature", Kind: fixturechange.Insert,
		Key: fixturechange.Values{"plan_id": fixturechange.RefTo("Plan", "team"), "code": fixturechange.Lit("sso")},
		New: fixturechange.Values{"id": fixturechange.Lit("1"), "plan_id": fixturechange.RefTo("Plan", "team"),
			"code": fixturechange.Lit("sso")}}}
	_, err := applyReporting(t, db, set)
	if err == nil || !strings.Contains(err.Error(), "already held by the row id = 1 (code=api,plan_id=1)") {
		t.Fatalf("the row holding the id has to be named, got %v", err)
	}
}
