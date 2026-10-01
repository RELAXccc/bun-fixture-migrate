package fixtureapply

import (
	"errors"
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

func softDeleteSet() fixturechange.Set {
	return fixturechange.Set{
		Name: "20261001000000_fixture_retire",
		Tables: fixturechange.Tables{
			"Plan": {Name: "plans", ID: "id", Key: "name", Serial: true, SoftDelete: "deleted_at"},
		},
		Changes: []fixturechange.Change{
			{Model: "Plan", Kind: fixturechange.Delete,
				Key: fixturechange.Values{"name": fixturechange.Lit("team")},
				Old: fixturechange.Values{"name": fixturechange.Lit("team"), "price_cents": fixturechange.Lit("2500")}},
			{Model: "Plan", Kind: fixturechange.Insert,
				Key: fixturechange.Values{"name": fixturechange.Lit("legacy")},
				New: fixturechange.Values{"name": fixturechange.Lit("legacy"), "price_cents": fixturechange.Lit("900")}},
		},
	}
}

func TestValidateAcceptsASoftDelete(t *testing.T) {
	if err := Validate(softDeleteSet()); err != nil {
		t.Fatal(err)
	}
}

// The column says whether a row is live and nothing else: no change names
// it, and it is no column the set finds rows by.
func TestValidateRefusesASoftDeleteItCannotRun(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*fixturechange.Set)
		want string
	}{
		"not an identifier": {func(s *fixturechange.Set) {
			s.Tables["Plan"] = fixturechange.Table{Name: "plans", ID: "id", Key: "name", SoftDelete: "deleted_at; --"}
		}, "not a plain SQL identifier"},
		"qualified": {func(s *fixturechange.Set) {
			s.Tables["Plan"] = fixturechange.Table{Name: "plans", ID: "id", Key: "name", SoftDelete: "plans.deleted_at"}
		}, "not a plain SQL identifier"},
		"the id": {func(s *fixturechange.Set) {
			s.Tables["Plan"] = fixturechange.Table{Name: "plans", ID: "id", Key: "name", SoftDelete: "id"}
		}, "is the id column"},
		"the key": {func(s *fixturechange.Set) {
			s.Tables["Plan"] = fixturechange.Table{Name: "plans", ID: "id", Key: "name", SoftDelete: "name"}
		}, "is the key column"},
		"cascade": {func(s *fixturechange.Set) {
			s.Tables["Plan"] = fixturechange.Table{Name: "plans", ID: "id", Key: "name", Cascade: true,
				SoftDelete: "deleted_at"}
		}, "reaches no row through a foreign key"},
		"a change writes it": {func(s *fixturechange.Set) {
			s.Changes[1].New["deleted_at"] = fixturechange.Null()
		}, "never writes or compares"},
		"a delete guards on it": {func(s *fixturechange.Set) {
			s.Changes[0].Old["deleted_at"] = fixturechange.Null()
		}, "never writes or compares"},
	} {
		set := softDeleteSet()
		tc.edit(&set)
		if err := Validate(set); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", name, err, tc.want)
		}
	}
}

// Without a soft delete, a scope adds nothing but the where; with one, it
// adds whether the row is live.
func TestScopedSeesLiveRowsOfASoftDeleteModel(t *testing.T) {
	r := &runner{set: fixturechange.Set{Tables: fixturechange.Tables{
		"Plan":     {Name: "plans", SoftDelete: "deleted_at", Where: "tenant_id IS NULL"},
		"Currency": {Name: "currencies"},
	}}}
	if got, args := r.scoped("Currency", "code = ?", []any{"EUR"}); got != "code = ?" || len(args) != 1 {
		t.Fatalf("a model without soft delete: %q %v", got, args)
	}
	got, args := r.scoped("Plan", "name = ?", []any{"team"})
	if got != "name = ? AND (?\n) AND \"deleted_at\" IS NULL" || len(args) != 2 {
		t.Fatalf("live: %q %v", got, args)
	}
	got, _ = r.scopedDeleted("Plan", "name = ?", []any{"team"})
	if got != "name = ? AND (?\n) AND \"deleted_at\" IS NOT NULL" {
		t.Fatalf("soft-deleted: %q", got)
	}
}

func TestAnAppliedChangeSaysWhatItDid(t *testing.T) {
	for _, tc := range []struct {
		out  outcome
		kind fixturechange.Kind
		want Action
	}{
		{outcome{}, fixturechange.Insert, ActionInserted},
		{outcome{}, fixturechange.Update, ActionUpdated},
		{outcome{}, fixturechange.Delete, ActionDeleted},
		{outcome{action: ActionSoftDeleted}, fixturechange.Delete, ActionSoftDeleted},
		{outcome{action: ActionRestored}, fixturechange.Insert, ActionRestored},
	} {
		if got := tc.out.actionOf(tc.kind); got != tc.want {
			t.Errorf("%s %+v: got %s, want %s", tc.kind, tc.out, got, tc.want)
		}
	}
	if got := rowsDone(1, ActionRestored); got != "1 row, restored" {
		t.Errorf("rowsDone: %q", got)
	}
	if got := rowsDone(2, ActionDeleted); got != "2 rows" {
		t.Errorf("rowsDone: %q", got)
	}
}

// A unique violation names its constraint, whichever driver wrapped it.
func TestConstraintOf(t *testing.T) {
	err := errors.New(`ERROR: duplicate key value violates unique constraint "plans_name_key" (SQLSTATE 23505)`)
	if got := constraintOf(err); got != `constraint "plans_name_key"` {
		t.Fatalf("got %q", got)
	}
	if got := constraintOf(errors.New("something else")); got != "a unique index" {
		t.Fatalf("got %q", got)
	}
}

func TestADeletedRowSaysWhichItIs(t *testing.T) {
	if got := (deleted{id: "3", at: "T"}).label(); got != " (id 3, deleted at T)" {
		t.Fatalf("got %q", got)
	}
	if got := (deleted{at: "T"}).label(); got != " (deleted at T)" {
		t.Fatalf("got %q", got)
	}
}
