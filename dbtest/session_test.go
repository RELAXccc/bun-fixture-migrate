package dbtest_test

// What a change set does to the session it runs in, and what the session does
// to it: row-level security, deferred constraints, lock timeouts.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// The migration role owns the tables. rls_plans has no row-level security;
// an audit trigger on it writes into rls_audit, which has FORCE ROW LEVEL
// SECURITY and a policy that lets everything through, as the plain UPDATE
// shows. With row_security off, every fixture change to rls_plans failed
// with "query would be affected by row-level security policy".
func TestATriggerWritingIntoATableUnderRowLevelSecurityIsFine(t *testing.T) {
	db := connect(t)
	ctx := context.Background()
	run(t, db, "DROP TABLE IF EXISTS rls_features, rls_audit, rls_plans",
		"DROP FUNCTION IF EXISTS rls_audit_fn()",
		`DO $$BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'bfm_rls_owner') THEN CREATE ROLE bfm_rls_owner; END IF; END$$`,
		"GRANT USAGE ON SCHEMA public TO bfm_rls_owner",
		"CREATE TABLE rls_plans (id bigint PRIMARY KEY, name text UNIQUE NOT NULL, price int NOT NULL)",
		"CREATE TABLE rls_audit (id bigserial PRIMARY KEY, tenant text NOT NULL DEFAULT 'global', what text)",
		"CREATE TABLE rls_features (id bigint PRIMARY KEY, plan_id bigint NOT NULL REFERENCES rls_plans ON DELETE CASCADE, code text NOT NULL)",
		"ALTER TABLE rls_audit ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE rls_audit FORCE ROW LEVEL SECURITY",
		"CREATE POLICY everything ON rls_audit USING (true) WITH CHECK (true)",
		`CREATE FUNCTION rls_audit_fn() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN INSERT INTO rls_audit (what) VALUES (TG_OP || ' ' || NEW.name); RETURN NEW; END$$`,
		"CREATE TRIGGER rls_audit_trg AFTER INSERT OR UPDATE ON rls_plans FOR EACH ROW EXECUTE FUNCTION rls_audit_fn()",
		"ALTER TABLE rls_plans OWNER TO bfm_rls_owner",
		"ALTER TABLE rls_audit OWNER TO bfm_rls_owner",
		"ALTER TABLE rls_features OWNER TO bfm_rls_owner",
		"INSERT INTO rls_plans VALUES (1, 'team', 100), (2, 'solo', 50)",
		"INSERT INTO rls_features VALUES (1, 2, 'api')",
		"TRUNCATE rls_audit")
	t.Cleanup(func() {
		run(t, db, "DROP TABLE IF EXISTS rls_features, rls_audit, rls_plans", "DROP FUNCTION IF EXISTS rls_audit_fn()")
	})
	tables := fixturechange.Tables{"Plan": {Name: "rls_plans", ID: "id", Key: "name"}}
	asOwner := func(set fixturechange.Set) error {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE bfm_rls_owner"); err != nil {
			t.Fatal(err)
		}
		if err := fixtureapply.Apply(ctx, tx, set, quiet()); err != nil {
			return err
		}
		return tx.Commit()
	}

	price := fixturechange.Set{Name: "rls", Tables: tables, Changes: []fixturechange.Change{{Model: "Plan",
		Kind: fixturechange.Update,
		Key:  fixturechange.Values{"name": fixturechange.Lit("team")},
		Old:  fixturechange.Values{"price": fixturechange.Lit("100")},
		New:  fixturechange.Values{"price": fixturechange.Lit("120")}}}}
	if err := asOwner(price); err != nil {
		t.Fatalf("a change to a table without row-level security fails because a trigger writes into one: %v", err)
	}
	if got := scan[int64](t, db, "SELECT price FROM rls_plans WHERE name = 'team'"); got != 120 {
		t.Fatalf("price %d", got)
	}
	if got := scan[string](t, db, "SELECT string_agg(what, ', ') FROM rls_audit"); got != "UPDATE team" {
		t.Fatalf("the trigger wrote %q", got)
	}

	// A delete counts the rows pointing at its row. Under FORCE ROW LEVEL
	// SECURITY on the child table the count would leave out the rows a
	// policy hides, and the cascade would take them all the same.
	run(t, db, "ALTER TABLE rls_features ENABLE ROW LEVEL SECURITY", "ALTER TABLE rls_features FORCE ROW LEVEL SECURITY",
		"CREATE POLICY none ON rls_features USING (false)")
	tables["Plan"] = fixturechange.Table{Name: "rls_plans", ID: "id", Key: "name", Cascade: true}
	gone := fixturechange.Set{Name: "rls", Tables: tables, Changes: []fixturechange.Change{{Model: "Plan",
		Kind: fixturechange.Delete,
		Key:  fixturechange.Values{"name": fixturechange.Lit("solo")},
		Old:  fixturechange.Values{"name": fixturechange.Lit("solo"), "price": fixturechange.Lit("50")}}}}
	err := asOwner(gone)
	if err == nil || !strings.Contains(err.Error(), "row-level security is active on rls_features for the role running the migration") {
		t.Fatalf("want the delete refused for the child table's policy, got %v", err)
	}
	if got := scan[int64](t, db, "SELECT count(*) FROM rls_features"); got != 1 {
		t.Fatal("nothing may change")
	}
	// The same table under the policy does not stop a change that deletes
	// nothing.
	price.Changes[0].Old, price.Changes[0].New = price.Changes[0].New, price.Changes[0].Old
	if err := asOwner(price); err != nil {
		t.Fatalf("an update does not reach the child table: %v", err)
	}
}

// A change set checks every deferred constraint when it is done, with SET
// CONSTRAINTS ALL IMMEDIATE, and inside a caller's transaction that used to
// leave them all immediate: the caller's next statement relying on a
// DEFERRABLE INITIALLY DEFERRED foreign key, a child row inserted before its
// parent, failed where bun's migrator, committing each migration, succeeds.
func TestAConstraintKeepsTheModeItIsDeclaredWithAfterApply(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	ctx := context.Background()
	// Two constraints of one name in one schema, one INITIALLY DEFERRED:
	// SET CONSTRAINTS takes both by that name, so it is left immediate
	// rather than defer the other one.
	run(t, db, "DROP TABLE IF EXISTS mode_child, mode_strict, mode_twin, mode_parent",
		"CREATE TABLE mode_parent (id int PRIMARY KEY)",
		"CREATE TABLE mode_child (id int PRIMARY KEY, parent_id int CONSTRAINT mode_child_parent REFERENCES mode_parent DEFERRABLE INITIALLY DEFERRED)",
		"CREATE TABLE mode_strict (id int PRIMARY KEY, parent_id int CONSTRAINT mode_strict_parent REFERENCES mode_parent DEFERRABLE INITIALLY IMMEDIATE)",
		"CREATE TABLE mode_twin (id int PRIMARY KEY, a int CONSTRAINT mode_twin_fk REFERENCES mode_parent DEFERRABLE INITIALLY DEFERRED, "+
			"b int CONSTRAINT mode_twin_fk2 REFERENCES mode_parent DEFERRABLE INITIALLY IMMEDIATE)")
	t.Cleanup(func() { run(t, db, "DROP TABLE IF EXISTS mode_child, mode_strict, mode_twin, mode_parent") })
	price := func(plan, from, to string) fixturechange.Set {
		return fixturechange.Set{Name: "modes", Tables: tables(), Changes: []fixturechange.Change{{Model: "Plan",
			Kind: fixturechange.Update,
			Key:  fixturechange.Values{"name": fixturechange.Lit(plan)},
			Old:  fixturechange.Values{"price_cents": fixturechange.Lit(from)},
			New:  fixturechange.Values{"price_cents": fixturechange.Lit(to)}}}}
	}
	inTx := func(set fixturechange.Set, fails bool, then func(exec func(string) error)) {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := fixtureapply.Apply(ctx, tx, set, quiet()); (err != nil) != fails {
			t.Fatalf("Apply: %v", err)
		}
		then(func(q string) error {
			if _, err := tx.ExecContext(ctx, "SAVEPOINT probe"); err != nil {
				t.Fatal(err)
			}
			_, err := tx.ExecContext(ctx, q)
			if _, rerr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT probe"); rerr != nil {
				t.Fatal(rerr)
			}
			return err
		})
	}
	check := func(exec func(string) error) {
		t.Helper()
		if err := exec("INSERT INTO mode_child VALUES (1, 10)"); err != nil {
			t.Errorf("a DEFERRABLE INITIALLY DEFERRED foreign key is checked at once after Apply: %v", err)
		}
		if err := exec("INSERT INTO mode_strict VALUES (1, 10)"); err == nil {
			t.Error("a DEFERRABLE INITIALLY IMMEDIATE foreign key is left deferred after Apply")
		}
		if err := exec("INSERT INTO mode_twin VALUES (1, NULL, 10)"); err == nil {
			t.Error("a constraint sharing its name with an INITIALLY DEFERRED one is left deferred after Apply")
		}
	}
	inTx(price("team", "2000", "2100"), false, check)
	// A failed Apply rolls back to its savepoint, which puts the modes back
	// by itself.
	inTx(price("gone", "1", "2"), true, check)

	// The whole of it, committed: the caller inserts the child before its
	// parent, which the deferred foreign key allows until COMMIT.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := fixtureapply.Apply(ctx, tx, price("team", "2000", "2100"), quiet()); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"INSERT INTO mode_child VALUES (1, 10)", "INSERT INTO mode_parent VALUES (10)"} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// SET CONSTRAINTS looks a constraint's schema up as the role running it, and
// refuses a schema the role may not use: a deferred constraint in another
// application's schema failed every change set the role ran.
func TestADeferredConstraintInASchemaTheRoleCannotUseIsLeftAlone(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	ctx := context.Background()
	run(t, db, "DROP SCHEMA IF EXISTS bfm_hidden CASCADE", "CREATE SCHEMA bfm_hidden",
		"CREATE TABLE bfm_hidden.p (id int PRIMARY KEY)",
		"CREATE TABLE bfm_hidden.c (id int PRIMARY KEY, p int CONSTRAINT c_p REFERENCES bfm_hidden.p DEFERRABLE INITIALLY DEFERRED)",
		`DO $$BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'bfm_plain') THEN CREATE ROLE bfm_plain; END IF; END$$`,
		"GRANT USAGE ON SCHEMA public TO bfm_plain",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON plans, features TO bfm_plain",
		"GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO bfm_plain")
	t.Cleanup(func() { run(t, db, "DROP SCHEMA IF EXISTS bfm_hidden CASCADE") })
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE bfm_plain"); err != nil {
		t.Fatal(err)
	}
	set := fixturechange.Set{Name: "hidden", Tables: tables(), Changes: []fixturechange.Change{{Model: "Plan",
		Kind: fixturechange.Update,
		Key:  fixturechange.Values{"name": fixturechange.Lit("team")},
		Old:  fixturechange.Values{"price_cents": fixturechange.Lit("2000")},
		New:  fixturechange.Values{"price_cents": fixturechange.Lit("2100")}}}}
	if err := fixtureapply.Apply(ctx, tx, set, quiet()); err != nil {
		t.Fatalf("a schema the role cannot use stopped the change set: %v", err)
	}
}

// plan simulates a fixture migration and then a .tx.up.sql migration that
// inserts a child row before its parent, in one transaction. bun's migrator
// commits the first before the second and applies both; the plan said the SQL
// migration would fail, because the fixture migration had left the deferred
// foreign key immediate.
func TestPlanAgreesWithTheDeployOnADeferredKeyAfterAFixtureMigration(t *testing.T) {
	db := deferredDB(t)
	c := deferredCLI(t)
	c.write("fixtures/fixture.yml", deferredFixture+"    - {id: 2, name: hammer, region_id: 1}\n")
	c.must(0, "generate", "-name", "hammer", "-at", "20300101000000")
	c.write("migrations/20300101000001_eu.tx.up.sql",
		"INSERT INTO d_items VALUES (3, 'saw', 3);\n--bun:split\nINSERT INTO d_regions VALUES (3, 'eu');\n")
	code, out, errOut := c.run("plan", "-with-sql")
	if code != 0 || strings.Contains(out, "would FAIL") {
		t.Fatalf("plan exit %d, but the deploy succeeds:\n%s\n%s", code, out, errOut)
	}

	deferredDB(t)
	ok, deploy := runMigrator(t, projectMigrator(t, filepath.Join(c.dir, "migrations")), false)
	if !ok {
		t.Fatalf("the deploy has to succeed:\n%s", deploy)
	}
	if n := scan[int64](t, db, "SELECT count(*) FROM d_items"); n != 3 {
		t.Fatalf("%d items after the deploy", n)
	}
}

// The check of the deferred constraints at the end of a set takes locks like
// any statement: a foreign key's check locks the row it points at. Waiting
// past the set's lock timeout for a row an admin holds was reported as "a
// constraint did not hold".
func TestALockTimeoutWhileTheConstraintsAreCheckedIsALockTimeout(t *testing.T) {
	db := deferredDB(t)
	ctx := context.Background()
	admin, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Rollback()
	if _, err := admin.ExecContext(ctx, "SELECT * FROM d_regions WHERE id = 1 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	set := fixturechange.Set{Name: "hammer", LockTimeout: "200ms",
		Tables: fixturechange.Tables{"DItem": {Name: "d_items", ID: "id", Key: "name"}},
		Changes: []fixturechange.Change{{Model: "DItem", Kind: fixturechange.Insert,
			Key: fixturechange.Values{"name": fixturechange.Lit("hammer")},
			New: fixturechange.Values{"id": fixturechange.Lit("2"), "name": fixturechange.Lit("hammer"),
				"region_id": fixturechange.Lit("1")}}}}
	start := time.Now()
	err = fixtureapply.Apply(ctx, db, set, quiet())
	var ce *fixtureapply.ChangeError
	if !errors.As(err, &ce) || ce.Outcome.Problem != fixtureapply.ProblemLockTimeout || ce.Outcome.Index != -1 ||
		!strings.Contains(err.Error(), "longer than the lock timeout of 200ms") {
		t.Fatalf("want a lock timeout of the set, got %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the change set waited %s", took)
	}
	if n := scan[int64](t, db, "SELECT count(*) FROM d_items"); n != 1 {
		t.Fatal("nothing may change")
	}
	if err := admin.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := fixtureapply.Apply(ctx, db, set, quiet()); err != nil {
		t.Fatalf("once the admin is done the set goes through: %v", err)
	}
}
