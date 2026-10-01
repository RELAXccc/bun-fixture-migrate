package dbtest_test

// What a change set does about rows it cannot tell apart, and about locks
// other sessions hold: never touch two rows for one change, and never wait
// behind somebody's open transaction for longer than it was told to.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
)

func dupPlans(t *testing.T) *bun.DB {
	t.Helper()
	db := connect(t)
	// No unique index on name: an admin tool inserted "team" twice.
	run(t, db,
		"DROP TABLE IF EXISTS dup_plans",
		"CREATE TABLE dup_plans (id bigserial PRIMARY KEY, name text NOT NULL, price int NOT NULL)",
		"INSERT INTO dup_plans (name, price) VALUES ('team', 10), ('team', 10), ('solo', 5)")
	return db
}

func dupSet(policy fixturechange.Mode, changes ...fixturechange.Change) fixturechange.Set {
	return fixturechange.Set{
		Name:    "20260921120000_fixture_duplicates",
		Tables:  fixturechange.Tables{"Plan": {Name: "dup_plans", ID: "id", Key: "name", Serial: true}},
		Policy:  fixturechange.Policy{DuplicateKey: policy},
		Changes: changes,
	}
}

func applyReporting(t *testing.T, db bun.IDB, set fixturechange.Set) ([]fixtureapply.Outcome, error) {
	t.Helper()
	var outcomes []fixtureapply.Outcome
	err := fixtureapply.Apply(context.Background(), db, set, quiet(),
		fixtureapply.WithReport(func(o fixtureapply.Outcome) { outcomes = append(outcomes, o) }))
	return outcomes, err
}

// Before: an update guarded by a natural key two rows share changed both and
// reported "applied (2 rows)", and a delete removed both.
func TestAChangeIsNeverMadeToTwoRows(t *testing.T) {
	team := fixturechange.Values{"name": fixturechange.Lit("team")}
	for _, c := range []fixturechange.Change{
		{Model: "Plan", Kind: fixturechange.Update, Key: team,
			Old: fixturechange.Values{"price": fixturechange.Lit("10")},
			New: fixturechange.Values{"price": fixturechange.Lit("20")}},
		{Model: "Plan", Kind: fixturechange.Delete, Key: team,
			Old: fixturechange.Values{"name": fixturechange.Lit("team"), "price": fixturechange.Lit("10")}},
		{Model: "Plan", Kind: fixturechange.Insert, Key: team,
			New: fixturechange.Values{"name": fixturechange.Lit("team"), "price": fixturechange.Lit("30")}},
	} {
		t.Run(string(c.Kind), func(t *testing.T) {
			db := dupPlans(t)
			outcomes, err := applyReporting(t, db, dupSet("", c))
			if err == nil || !strings.Contains(err.Error(), "2 rows of dup_plans hold name=team") {
				t.Fatalf("the duplicate key has to fail the change set by default, got %v", err)
			}
			if len(outcomes) != 1 || outcomes[0].Problem != fixtureapply.ProblemDuplicateKey {
				t.Fatalf("outcomes %+v", outcomes)
			}
			if n := scan[int64](t, db, "SELECT count(*) FROM dup_plans WHERE name = 'team' AND price = 10"); n != 2 {
				t.Fatalf("both rows have to be left as they were, %d are", n)
			}

			// warn leaves both alone and carries on with the rest.
			solo := fixturechange.Change{Model: "Plan", Kind: fixturechange.Update,
				Key: fixturechange.Values{"name": fixturechange.Lit("solo")},
				Old: fixturechange.Values{"price": fixturechange.Lit("5")},
				New: fixturechange.Values{"price": fixturechange.Lit("6")}}
			outcomes, err = applyReporting(t, db, dupSet(fixturechange.ModeWarn, c, solo))
			if err != nil {
				t.Fatal(err)
			}
			if len(outcomes) != 2 || outcomes[0].Status != fixtureapply.StatusSkipped ||
				outcomes[1].Status != fixtureapply.StatusApplied {
				t.Fatalf("outcomes %+v", outcomes)
			}
			if n := scan[int64](t, db, "SELECT count(*) FROM dup_plans WHERE name = 'team' AND price = 10"); n != 2 {
				t.Fatalf("both rows have to be left as they were, %d are", n)
			}
			if p := scan[int64](t, db, "SELECT price FROM dup_plans WHERE name = 'solo'"); p != 6 {
				t.Fatalf("the unambiguous change has to be made, price %d", p)
			}
		})
	}
}

// An id guard does not make a shared key safe: the key is what the change
// set's author meant, and two rows hold it.
func TestARenameOfADuplicatedKeyIsRefused(t *testing.T) {
	db := dupPlans(t)
	id := scan[string](t, db, "SELECT min(id)::text FROM dup_plans WHERE name = 'team'")
	_, err := applyReporting(t, db, dupSet("", fixturechange.Change{Model: "Plan", Kind: fixturechange.Update, ID: id,
		Key: fixturechange.Values{"name": fixturechange.Lit("team")},
		Old: fixturechange.Values{"name": fixturechange.Lit("team")},
		New: fixturechange.Values{"name": fixturechange.Lit("crew")}}))
	if err == nil || !strings.Contains(err.Error(), "2 rows") {
		t.Fatalf("want the duplicate key refused, got %v", err)
	}
}

func lockedPlans(t *testing.T) *bun.DB {
	t.Helper()
	db := connect(t)
	run(t, db,
		"DROP TABLE IF EXISTS locked_plans",
		"CREATE TABLE locked_plans (id bigserial PRIMARY KEY, name text UNIQUE NOT NULL, price int NOT NULL)",
		"INSERT INTO locked_plans (name, price) VALUES ('team', 10)")
	return db
}

func lockedSet(timeout string) fixturechange.Set {
	return fixturechange.Set{
		Name:        "20260921120000_fixture_locked",
		LockTimeout: timeout,
		Tables:      fixturechange.Tables{"Plan": {Name: "locked_plans", ID: "id", Key: "name"}},
		Changes: []fixturechange.Change{{Model: "Plan", Kind: fixturechange.Update,
			Key: fixturechange.Values{"name": fixturechange.Lit("team")},
			Old: fixturechange.Values{"price": fixturechange.Lit("10")},
			New: fixturechange.Values{"price": fixturechange.Lit("20")}}},
	}
}

// An admin's open transaction holds the row. Without a lock timeout the
// deploy waits for as long as the admin does, and the application's writes to
// that row queue up behind the deploy.
func TestALockedRowFailsTheChangeSetAfterTheLockTimeout(t *testing.T) {
	db := lockedPlans(t)
	ctx := context.Background()
	admin, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Rollback()
	if _, err := admin.ExecContext(ctx, "SELECT * FROM locked_plans WHERE name = 'team' FOR UPDATE"); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	outcomes, err := applyReporting(t, db, lockedSet("200ms"))
	if err == nil || !strings.Contains(err.Error(), "lock timeout of 200ms") {
		t.Fatalf("want a lock timeout, got %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the change set waited %s", took)
	}
	if len(outcomes) != 1 || outcomes[0].Problem != fixtureapply.ProblemLockTimeout {
		t.Fatalf("outcomes %+v", outcomes)
	}
	var ce *fixtureapply.ChangeError
	if !errors.As(err, &ce) || ce.Outcome.Problem != fixtureapply.ProblemLockTimeout || errors.Unwrap(ce) == nil {
		t.Fatalf("the lock timeout has to be a *ChangeError around PostgreSQL's error: %v", err)
	}

	// Once the admin is done, the same change set goes through.
	if err := admin.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := applyReporting(t, db, lockedSet("200ms")); err != nil {
		t.Fatal(err)
	}
	if p := scan[int64](t, db, "SELECT price FROM locked_plans"); p != 20 {
		t.Fatalf("price %d", p)
	}
}

// An ALTER TABLE or a VACUUM FULL holds the audit table. Its row was written
// once the session's lock_timeout was back, often 0, so the migration waited
// for as long as that lock lasted, holding every row it had changed. It waits
// for the set's lock timeout, as for a row, writing the row or reading what to
// revert.
func TestALockOnTheAuditTableFailsTheChangeSetAfterTheLockTimeout(t *testing.T) {
	db := lockedPlans(t)
	ctx := context.Background()
	run(t, db, "DROP TABLE IF EXISTS bfm_locked_audit")
	t.Cleanup(func() { run(t, db, "DROP TABLE IF EXISTS bfm_locked_audit") })
	set := lockedSet("200ms")
	set.AuditTable = "bfm_locked_audit"
	if err := fixtureapply.Apply(ctx, db, set, quiet()); err != nil {
		t.Fatal(err)
	}
	run(t, db, "UPDATE locked_plans SET price = 10")
	locked := func(fn func(context.Context) error) error {
		t.Helper()
		admin, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer admin.Rollback()
		run(t, admin, "LOCK TABLE bfm_locked_audit IN ACCESS EXCLUSIVE MODE")
		bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		start := time.Now()
		err = fn(bounded)
		if took := time.Since(start); took > 4*time.Second {
			t.Fatalf("the change set waited %s for the lock on the audit table: %v", took, err)
		}
		return err
	}
	for _, c := range []struct {
		name string
		run  func(context.Context, bun.IDB, fixturechange.Set, ...fixtureapply.Option) error
		want string
	}{
		{"Apply", fixtureapply.Apply, "recording the run in the audit table bfm_locked_audit waited for a lock"},
		{"Revert", fixtureapply.Revert, "reading what to revert from the audit table bfm_locked_audit waited"},
	} {
		if c.name == "Revert" {
			if err := fixtureapply.Apply(ctx, db, set, quiet()); err != nil {
				t.Fatal(err)
			}
		}
		before := scan[int64](t, db, "SELECT price FROM locked_plans")
		err := locked(func(ctx context.Context) error { return c.run(ctx, db, set, quiet()) })
		var ce *fixtureapply.ChangeError
		if !errors.As(err, &ce) || ce.Outcome.Problem != fixtureapply.ProblemLockTimeout ||
			!strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "lock timeout of 200ms") {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := scan[int64](t, db, "SELECT price FROM locked_plans"); got != before {
			t.Fatalf("%s: price %d after a failed run", c.name, got)
		}
	}
}

// Waiting for another replica's change set is not what the lock timeout is
// about: the second replica waits its turn and then finds the work done.
func TestTheLockTimeoutDoesNotCutShortTheWaitForAnotherChangeSet(t *testing.T) {
	db := lockedPlans(t)
	ctx := context.Background()
	other, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback()
	// The key every change set serialises on.
	if _, err := other.ExecContext(ctx, "SELECT pg_advisory_xact_lock(?)", int64(0x62666d0001)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := applyReporting(t, db, lockedSet("100ms"))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the change set did not wait for the other one: %v", err)
	case <-time.After(700 * time.Millisecond):
	}
	if err := other.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("once the other change set finished this one has to go through: %v", err)
	}
}

// A lock_timeout the connection has, from the DSN's options or the role's
// settings, cut short the wait for another change set all the same, and the
// second replica failed instead of finding the work done. Inside a caller's
// transaction the caller's lock_timeout still bounds that wait: plan sets one
// so as not to wait behind a deploy while it holds locks of its own.
func TestASessionLockTimeoutDoesNotCutShortTheWaitForAnotherChangeSet(t *testing.T) {
	lockedPlans(t)
	db := openDB(t, os.Getenv("BUN_FIXTURE_MIGRATE_POSTGRES"), map[string]string{"lock_timeout": "100ms"})
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	hold := func() bun.Tx {
		t.Helper()
		other, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := other.ExecContext(ctx, "SELECT pg_advisory_xact_lock(?)", int64(0x62666d0001)); err != nil {
			t.Fatal(err)
		}
		return other
	}

	other := hold()
	done := make(chan error, 1)
	go func() {
		_, err := applyReporting(t, db, lockedSet(""))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the change set did not wait for the other one: %v", err)
	case <-time.After(700 * time.Millisecond):
	}
	if err := other.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("once the other change set finished this one has to go through: %v", err)
	}
	var got string
	if err := db.QueryRowContext(ctx, "SHOW lock_timeout").Scan(&got); err != nil || got != "100ms" {
		t.Fatalf("the session's lock_timeout is %q after Apply: %v", got, err)
	}

	other = hold()
	defer other.Rollback()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	start := time.Now()
	_, err = applyReporting(t, tx, lockedSet(""))
	if err == nil || !strings.Contains(err.Error(), "wait for another change set to finish") {
		t.Fatalf("inside a caller's transaction its lock_timeout applies, got %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("waited %s", took)
	}
}

// Apply inside the caller's transaction leaves the caller's lock_timeout as it
// found it.
func TestTheLockTimeoutIsPutBack(t *testing.T) {
	db := lockedPlans(t)
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SET LOCAL lock_timeout = '7s'"); err != nil {
		t.Fatal(err)
	}
	if _, err := applyReporting(t, tx, lockedSet("200ms")); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := tx.QueryRowContext(ctx, "SHOW lock_timeout").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "7s" {
		t.Fatalf("lock_timeout is %q after Apply, want the caller's 7s", got)
	}
}
