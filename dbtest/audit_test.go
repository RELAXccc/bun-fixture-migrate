package dbtest_test

// The audit table: what a change set did in a database, written in the
// transaction that did it, and a Revert that undoes only that.
//
// Without it, Revert inverts every change of a set. A change the migration
// found made already, through another path, is reverted to an old value this
// database never held, and a delete it skipped because somebody had removed
// the row puts the row back. A long-running SaaS replay found both on a staging
// database.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
)

func auditDB(t *testing.T) *bun.DB {
	t.Helper()
	db := connect(t)
	run(t, db,
		"DROP TABLE IF EXISTS au_plans, au_translations, bfm_audit_test CASCADE",
		"DROP SCHEMA IF EXISTS bfm_audit_schema CASCADE",
		"CREATE TABLE au_plans (id bigserial PRIMARY KEY, name text UNIQUE NOT NULL, price int NOT NULL)",
		"CREATE TABLE au_translations (id bigserial PRIMARY KEY, key text UNIQUE NOT NULL, text text NOT NULL)",
		"INSERT INTO au_plans (name, price) VALUES ('free', 0), ('team', 20), ('solo', 5), ('gone', 1), ('old', 9)",
		"INSERT INTO au_translations (key, text) VALUES ('hello', 'Hello'), ('bye', 'Bye')")
	t.Cleanup(func() {
		run(t, db, "DROP TABLE IF EXISTS au_plans, au_translations, bfm_audit_test CASCADE",
			"DROP SCHEMA IF EXISTS bfm_audit_schema CASCADE")
	})
	return db
}

func price(name, from, to string) fixturechange.Change {
	return fixturechange.Change{Model: "Plan", Kind: fixturechange.Update,
		Key: fixturechange.Values{"name": fixturechange.Lit(name)},
		Old: fixturechange.Values{"price": fixturechange.Lit(from)},
		New: fixturechange.Values{"price": fixturechange.Lit(to)}}
}

// auditedSet changes five plans, which the tests put in different states
// before it runs.
func auditedSet() fixturechange.Set {
	return fixturechange.Set{
		Name:       "20260921120000_fixture_audited",
		AuditTable: "bfm_audit_test",
		Tables: fixturechange.Tables{
			"Plan":        {Name: "au_plans", ID: "id", Key: "name", Serial: true},
			"Translation": {Name: "au_translations", ID: "id", Key: "key", Serial: true},
		},
		Policy: fixturechange.Policy{MissingRow: "warn", ChangedRow: "warn"},
		Changes: []fixturechange.Change{
			price("team", "20", "25"),
			price("solo", "5", "6"),
			price("free", "0", "1"),
			{Model: "Plan", Kind: fixturechange.Delete,
				Key: fixturechange.Values{"name": fixturechange.Lit("gone")},
				Old: fixturechange.Values{"name": fixturechange.Lit("gone"), "price": fixturechange.Lit("1")}},
			{Model: "Plan", Kind: fixturechange.Insert,
				Key: fixturechange.Values{"name": fixturechange.Lit("pro")},
				New: fixturechange.Values{"name": fixturechange.Lit("pro"), "price": fixturechange.Lit("90")}},
		},
	}
}

func plans(t *testing.T, db *bun.DB) string {
	t.Helper()
	return scan[string](t, db, "SELECT coalesce(string_agg(name || '=' || price, ' ' ORDER BY name), '') FROM au_plans")
}

func TestTheAuditTableRecordsWhatTheRunDid(t *testing.T) {
	db := auditDB(t)
	ctx := context.Background()
	// This database got solo's new price another way, an admin edited free,
	// and somebody removed gone already.
	run(t, db, "UPDATE au_plans SET price = 6 WHERE name = 'solo'", "UPDATE au_plans SET price = 3 WHERE name = 'free'",
		"DELETE FROM au_plans WHERE name = 'gone'")
	set := auditedSet()
	outcomes, err := applyReporting(t, db, set)
	if err != nil {
		t.Fatal(err)
	}
	if got := plans(t, db); got != "free=3 old=9 pro=90 solo=6 team=25" {
		t.Fatalf("after Apply: %s", got)
	}
	var row struct {
		ID                                   int64
		SetName, Direction, Hash, By, Status string
		Outcomes                             string
	}
	if err := db.QueryRowContext(ctx, "SELECT id, set_name, direction, set_sha256, applied_by, outcomes::text, "+
		"(applied_by = current_user)::text FROM bfm_audit_test").Scan(&row.ID, &row.SetName, &row.Direction, &row.Hash,
		&row.By, &row.Outcomes, &row.Status); err != nil {
		t.Fatal(err)
	}
	if row.SetName != set.Name || row.Direction != "up" || row.Hash != fixtureapply.SetSHA256(set) ||
		row.Status != "true" {
		t.Fatalf("the audit row: %+v", row)
	}
	var recorded []fixtureapply.AuditOutcome
	if err := json.Unmarshal([]byte(row.Outcomes), &recorded); err != nil {
		t.Fatal(err)
	}
	if len(recorded) != len(outcomes) {
		t.Fatalf("%d outcomes recorded, %d reported:\n%s", len(recorded), len(outcomes), row.Outcomes)
	}
	want := []string{"0 applied ", "1 unchanged ", "2 skipped changed row", "3 unchanged ", "4 applied "}
	for i, o := range recorded {
		if got := strings.TrimSpace(strings.Join([]string{itoa(int64(o.Index)), string(o.Status), string(o.Problem)},
			" ")); got != strings.TrimSpace(want[i]) || o.Model != "Plan" || o.Key == "" || o.Kind == "" {
			t.Fatalf("outcome %d: %+v, want %s", i, o, want[i])
		}
	}
	// The table says what it is, to whoever finds it.
	if c := scan[string](t, db, "SELECT obj_description('bfm_audit_test'::regclass, 'pg_class')"); !strings.Contains(c,
		"bun-fixture-migrate") || !strings.Contains(c, "Revert") {
		t.Fatalf("the table's comment: %q", c)
	}

	// The Revert undoes the team update and the pro insert, which this run
	// made, and leaves solo, free and gone as they are: solo never held 5
	// here after this migration, free is the admin's, gone was removed by
	// somebody else.
	var reverted []fixtureapply.Outcome
	if err := fixtureapply.Revert(ctx, db, set, quiet(),
		fixtureapply.WithReport(func(o fixtureapply.Outcome) { reverted = append(reverted, o) })); err != nil {
		t.Fatal(err)
	}
	if got := plans(t, db); got != "free=3 old=9 solo=6 team=20" {
		t.Fatalf("after Revert: %s\n%+v", got, reverted)
	}
	for _, o := range reverted {
		if (o.Index == 0 || o.Index == 4) != (o.Status == fixtureapply.StatusApplied) {
			t.Fatalf("Revert of change %d: %+v", o.Index, o)
		}
		if o.Index == 1 && !strings.Contains(o.Message, "found it unchanged") {
			t.Fatalf("a change left alone says why: %+v", o)
		}
	}
	if got := scan[string](t, db, "SELECT string_agg(direction, ',' ORDER BY id) FROM bfm_audit_test"); got != "up,down" {
		t.Fatalf("directions: %s", got)
	}

	// Applied again, and reverted again: the second Revert follows the
	// second Apply, not the first.
	run(t, db, "UPDATE au_plans SET price = 0 WHERE name = 'free'")
	if err := fixtureapply.Apply(ctx, db, set, quiet()); err != nil {
		t.Fatal(err)
	}
	if got := plans(t, db); got != "free=1 old=9 pro=90 solo=6 team=25" {
		t.Fatalf("after the second Apply: %s", got)
	}
	if err := fixtureapply.Revert(ctx, db, set, quiet()); err != nil {
		t.Fatal(err)
	}
	if got := plans(t, db); got != "free=0 old=9 solo=6 team=20" {
		t.Fatalf("after the second Revert: %s", got)
	}
}

// A Revert that fails under bun's default migrator leaves the migration
// pending with its changes made; the next migrate runs Apply, which finds them
// all made. The Revert after that undoes what the first Apply made.
func TestARevertAfterAFailedRevertUndoesWhatTheFirstApplyMade(t *testing.T) {
	db := auditDB(t)
	ctx := context.Background()
	set := auditedSet()
	set.Changes = set.Changes[:1]
	set.Policy.ChangedRow = "error"
	if err := fixtureapply.Apply(ctx, db, set, quiet()); err != nil {
		t.Fatal(err)
	}
	run(t, db, "UPDATE au_plans SET price = 26 WHERE name = 'team'")
	if err := fixtureapply.Revert(ctx, db, set, quiet()); err == nil {
		t.Fatal("the Revert has to fail on a changed row under changed_row error")
	}
	run(t, db, "UPDATE au_plans SET price = 25 WHERE name = 'team'")
	if err := fixtureapply.Apply(ctx, db, set, quiet()); err != nil {
		t.Fatal(err)
	}
	if got := scan[string](t, db, "SELECT string_agg(direction, ',' ORDER BY id) FROM bfm_audit_test"); got != "up,up" {
		t.Fatalf("a failed Revert writes no row: %s", got)
	}
	if err := fixtureapply.Revert(ctx, db, set, quiet()); err != nil {
		t.Fatal(err)
	}
	if got := scan[int64](t, db, "SELECT price FROM au_plans WHERE name = 'team'"); got != 20 {
		t.Fatalf("team = %d, the first Apply's change was not reverted", got)
	}
}

func TestAFailedApplyRecordsNothing(t *testing.T) {
	db := auditDB(t)
	ctx := context.Background()
	set := auditedSet()
	set.Policy.MissingRow = "error"
	run(t, db, "DELETE FROM au_plans WHERE name = 'team'")
	if err := fixtureapply.Apply(ctx, db, set, quiet()); err == nil {
		t.Fatal("the update of a missing row has to fail under missing_row error")
	}
	if got := scan[bool](t, db, "SELECT to_regclass('bfm_audit_test') IS NULL"); !got {
		t.Fatal("a failed Apply created the audit table")
	}
	// Nor when the table is there already.
	run(t, db, "INSERT INTO au_plans (name, price) VALUES ('team', 20)")
	if err := fixtureapply.Apply(ctx, db, set, quiet()); err != nil {
		t.Fatal(err)
	}
	if err := fixtureapply.Revert(ctx, db, set, quiet()); err != nil {
		t.Fatal(err)
	}
	run(t, db, "DELETE FROM au_plans WHERE name = 'team'")
	if err := fixtureapply.Apply(ctx, db, set, quiet()); err == nil {
		t.Fatal("the second Apply has to fail")
	}
	if got := scan[int64](t, db, "SELECT count(*) FROM bfm_audit_test"); got != 2 {
		t.Fatalf("%d rows, a failed Apply wrote one", got)
	}
}

// Without a row, Revert does what it always did, and says so.
func TestARevertWithoutAnAuditRowRevertsEverything(t *testing.T) {
	db := auditDB(t)
	ctx := context.Background()
	set := auditedSet()
	set.AuditTable = ""
	if err := fixtureapply.Apply(ctx, db, set, quiet()); err != nil {
		t.Fatal(err)
	}
	set.AuditTable = "bfm_audit_test"
	var logged []string
	if err := fixtureapply.Revert(ctx, db, set, fixtureapply.WithLogger(func(f string, a ...any) {
		logged = append(logged, fmt.Sprintf(f, a...))
	})); err != nil {
		t.Fatal(err)
	}
	if got := plans(t, db); got != "free=0 gone=1 old=9 solo=5 team=20" {
		t.Fatalf("after Revert: %s", got)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "holds no Apply of this change set") {
		t.Fatalf("the fallback has to say so:\n%s", strings.Join(logged, "\n"))
	}
	if got := scan[string](t, db, "SELECT direction FROM bfm_audit_test"); got != "down" {
		t.Fatalf("the Revert is recorded: %s", got)
	}
}

// Creating the table takes CREATE on its schema, and a role without it gets a
// sentence saying so, and no change made.
func TestTheAuditTableNeedsTheRightToCreateIt(t *testing.T) {
	db := auditDB(t)
	ctx := context.Background()
	run(t, db, "CREATE SCHEMA bfm_audit_schema",
		`DO $$BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'bfm_auditor') THEN CREATE ROLE bfm_auditor; END IF; END$$`,
		"GRANT USAGE ON SCHEMA public, bfm_audit_schema TO bfm_auditor",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON au_plans, au_translations TO bfm_auditor",
		"GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO bfm_auditor")
	set := auditedSet()
	set.AuditTable = "bfm_audit_schema.audit"
	asAuditor := func(fn func(tx bun.Tx) error) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE bfm_auditor"); err != nil {
			t.Fatal(err)
		}
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	err := asAuditor(func(tx bun.Tx) error { return fixtureapply.Apply(ctx, tx, set, quiet()) })
	if err == nil || !strings.Contains(err.Error(), "may not create it, which takes CREATE on its schema") {
		t.Fatalf("the error has to say what is missing: %v", err)
	}
	if got := plans(t, db); got != "free=0 gone=1 old=9 solo=5 team=20" {
		t.Fatalf("a run that could not record itself changed the database: %s", got)
	}

	// Created by a role that may, and granted, it works.
	if err := fixtureapply.Apply(ctx, db, set, quiet()); err != nil {
		t.Fatal(err)
	}
	if err := fixtureapply.Revert(ctx, db, set, quiet()); err != nil {
		t.Fatal(err)
	}
	err = asAuditor(func(tx bun.Tx) error { return fixtureapply.Apply(ctx, tx, set, quiet()) })
	if err == nil || !strings.Contains(err.Error(), "may not write into the audit table") {
		t.Fatalf("without INSERT on it: %v", err)
	}
	run(t, db, "GRANT SELECT, INSERT ON bfm_audit_schema.audit TO bfm_auditor")
	if err := asAuditor(func(tx bun.Tx) error { return fixtureapply.Apply(ctx, tx, set, quiet()) }); err != nil {
		t.Fatal(err)
	}
	if got := scan[string](t, db, "SELECT applied_by FROM bfm_audit_schema.audit ORDER BY id DESC LIMIT 1"); got != "bfm_auditor" {
		t.Fatalf("applied_by = %q", got)
	}
}

// Translations at changed_row warn, prices at error: one model's edit is
// kept, the other's fails the migration.
func TestEachModelRunsUnderItsOwnPolicy(t *testing.T) {
	db := auditDB(t)
	ctx := context.Background()
	set := fixturechange.Set{
		Name: "20260921120000_fixture_policies",
		Tables: fixturechange.Tables{
			"Plan":        {Name: "au_plans", ID: "id", Key: "name"},
			"Translation": {Name: "au_translations", ID: "id", Key: "key", Policy: &fixturechange.Policy{ChangedRow: "warn"}},
		},
		Policy: fixturechange.Policy{ChangedRow: "error", MissingRow: "error"},
		Changes: []fixturechange.Change{
			price("team", "20", "25"),
			{Model: "Translation", Kind: fixturechange.Update,
				Key: fixturechange.Values{"key": fixturechange.Lit("hello")},
				Old: fixturechange.Values{"text": fixturechange.Lit("Hello")},
				New: fixturechange.Values{"text": fixturechange.Lit("Hello!")}},
		},
	}
	run(t, db, "UPDATE au_translations SET text = 'Hi' WHERE key = 'hello'")
	outcomes, err := applyReporting(t, db, set)
	if err != nil {
		t.Fatalf("a translation edited here is kept under its model's changed_row warn: %v", err)
	}
	if outcomes[1].Status != fixtureapply.StatusSkipped || outcomes[0].Status != fixtureapply.StatusApplied {
		t.Fatalf("%+v", outcomes)
	}
	run(t, db, "UPDATE au_plans SET price = 21 WHERE name = 'team'")
	err = fixtureapply.Revert(ctx, db, set, quiet())
	var ce *fixtureapply.ChangeError
	if !errors.As(err, &ce) || ce.Outcome.Model != "Plan" || ce.Outcome.Problem != fixtureapply.ProblemChangedRow {
		t.Fatalf("a plan edited here fails under the set's changed_row error: %v", err)
	}
	// A missing translation is still the set's missing_row error, and the
	// message says where to change that.
	run(t, db, "UPDATE au_plans SET price = 25 WHERE name = 'team'", "DELETE FROM au_translations WHERE key = 'hello'")
	err = fixtureapply.Revert(ctx, db, set, quiet())
	if !errors.As(err, &ce) || ce.Outcome.Problem != fixtureapply.ProblemMissingRow ||
		!strings.Contains(err.Error(), `in the Policy of "Translation" in this migration's Tables`) {
		t.Fatalf("missing row: %v", err)
	}
}
