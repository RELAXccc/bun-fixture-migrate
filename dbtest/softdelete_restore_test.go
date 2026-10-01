package dbtest_test

// Which soft-deleted row an insert restores: the copy that holds the change's
// values, not merely the newest with the key, locked until it is restored.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
)

func sdrPlans(t *testing.T) *bun.DB {
	t.Helper()
	db := connect(t)
	run(t, db, "DROP TABLE IF EXISTS sdr_subs, sdr_plans",
		"CREATE TABLE sdr_plans (id bigserial PRIMARY KEY, name text NOT NULL, price int NOT NULL, "+
			"deleted_at timestamptz)",
		"CREATE UNIQUE INDEX sdr_plans_name_live ON sdr_plans (name) WHERE deleted_at IS NULL",
		"CREATE TABLE sdr_subs (id bigserial PRIMARY KEY, plan_id bigint NOT NULL REFERENCES sdr_plans)")
	return db
}

func sdrTables() fixturechange.Tables {
	return fixturechange.Tables{"Plan": {Name: "sdr_plans", ID: "id", Key: "name", Serial: true,
		SoftDelete: "deleted_at"}}
}

func sdrRows(t *testing.T, db *bun.DB) string {
	t.Helper()
	return scan[string](t, db, "SELECT string_agg(id || ':' || name || '=' || price || CASE WHEN deleted_at IS NULL "+
		"THEN '' ELSE ' (deleted)' END, ', ' ORDER BY id) FROM sdr_plans")
}

// Before: M1 soft-deleted team, M2 added team back at another price beside
// it, and reverting both inserted a third team instead of restoring the
// first, which the subscriptions still point at: only the newest soft-deleted
// copy was looked at, and it held M2's price.
func TestARevertRestoresTheCopyThatHoldsItsValues(t *testing.T) {
	db := sdrPlans(t)
	run(t, db, "INSERT INTO sdr_plans (id, name, price) VALUES (1, 'free', 0), (2, 'team', 2500)",
		"SELECT setval('sdr_plans_id_seq', 2)", "INSERT INTO sdr_subs (plan_id) VALUES (2)")
	team := fixturechange.Values{"name": fixturechange.Lit("team")}
	retire := fixturechange.Set{Name: "20261001000000_fixture_retire_team", Tables: sdrTables(),
		Changes: []fixturechange.Change{{Model: "Plan", Kind: fixturechange.Delete, Key: team,
			Old: fixturechange.Values{"name": fixturechange.Lit("team"), "price": fixturechange.Lit("2500")}}}}
	readd := fixturechange.Set{Name: "20261002000000_fixture_new_team", Tables: sdrTables(),
		Changes: []fixturechange.Change{{Model: "Plan", Kind: fixturechange.Insert, Key: team,
			New: fixturechange.Values{"name": fixturechange.Lit("team"), "price": fixturechange.Lit("3000")}}}}
	ctx := context.Background()
	for _, step := range []func() error{
		func() error { return fixtureapply.Apply(ctx, db, retire, quiet()) },
		func() error { return fixtureapply.Apply(ctx, db, readd, quiet()) },
		func() error { return fixtureapply.Revert(ctx, db, readd, quiet()) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	var outcomes []fixtureapply.Outcome
	if err := fixtureapply.Revert(ctx, db, retire, quiet(),
		fixtureapply.WithReport(func(o fixtureapply.Outcome) { outcomes = append(outcomes, o) })); err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 1 || outcomes[0].Action != fixtureapply.ActionRestored ||
		!strings.Contains(outcomes[0].Message, "(id 2)") {
		t.Fatalf("the revert restores the team the migration soft-deleted, id 2: %+v", outcomes)
	}
	if got, want := sdrRows(t, db), "1:free=0, 2:team=2500, 3:team=3000 (deleted)"; got != want {
		t.Fatalf("plans\n got %s\nwant %s", got, want)
	}
}

// lockProbe runs fn on another connection before the first statement of the
// change set's own connection whose text holds match.
type lockProbe struct {
	match string
	fn    func()
	done  bool
}

func (p *lockProbe) BeforeQuery(ctx context.Context, e *bun.QueryEvent) context.Context {
	if !p.done && strings.Contains(e.Query, p.match) {
		p.done = true
		p.fn()
	}
	return ctx
}

func (p *lockProbe) AfterQuery(context.Context, *bun.QueryEvent) {}

// Before: the soft-deleted row was found by where it is and restored there
// later, without a lock. The application updating it in between, as bun's
// NewDelete of it again does, moved it, and the set failed blaming a
// trigger; deleting it for good and another row taking its place made that
// row live instead.
func TestACandidateIsLockedUntilItIsRestored(t *testing.T) {
	db := sdrPlans(t)
	run(t, db, "INSERT INTO sdr_plans (id, name, price, deleted_at) VALUES (1, 'legacy', 900, '2026-01-01Z'), "+
		"(2, 'free', 0, NULL)")
	app := connect(t)
	migrator := openDB(t, os.Getenv("BUN_FIXTURE_MIGRATE_POSTGRES"), nil)
	t.Cleanup(func() { migrator.Close() })
	var appErr error
	migrator.AddQueryHook(&lockProbe{match: `SET "deleted_at" = NULL WHERE tableoid`, fn: func() {
		appErr = app.RunInTx(context.Background(), nil, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.ExecContext(ctx, "SET LOCAL lock_timeout = '200ms'"); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, "UPDATE sdr_plans SET deleted_at = '2026-02-02Z' WHERE id = 1")
			return err
		})
	}})
	set := fixturechange.Set{Name: "20261001000000_fixture_legacy", Tables: sdrTables(),
		Changes: []fixturechange.Change{{Model: "Plan", Kind: fixturechange.Insert,
			Key: fixturechange.Values{"name": fixturechange.Lit("legacy")},
			New: fixturechange.Values{"name": fixturechange.Lit("legacy"), "price": fixturechange.Lit("900")}}}}
	outcomes, err := applyReporting(t, migrator, set)
	if err != nil {
		t.Fatalf("the restore fails: %v", err)
	}
	if pgState(appErr) != "55P03" {
		t.Fatalf("the application's update of the row being restored waits for the change set, and times out "+
			"here: %v", appErr)
	}
	if len(outcomes) != 1 || outcomes[0].Action != fixtureapply.ActionRestored {
		t.Fatalf("%+v", outcomes)
	}
	if got, want := sdrRows(t, db), "1:legacy=900, 2:free=0"; got != want {
		t.Fatalf("plans\n got %s\nwant %s", got, want)
	}
}
