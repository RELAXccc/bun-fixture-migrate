package dbtest_test

// What a change set does to the session it runs in, and what the session does
// to it: row-level security, deferred constraints, lock timeouts.

import (
	"context"
	"strings"
	"testing"

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
