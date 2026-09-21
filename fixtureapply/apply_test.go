package fixtureapply

import (
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

func tables() fixturechange.Tables {
	return fixturechange.Tables{
		"Plan":    {Name: "plans", ID: "id", Key: "name"},
		"Feature": {Name: "features", ID: "id", Key: "code", Serial: true},
	}
}

func TestValidateAcceptsAGoodSet(t *testing.T) {
	set := fixturechange.Set{
		Name:           "20260921120000_fixture_prices",
		SeedGuardTable: "plans",
		Tables:         tables(),
		Changes: []fixturechange.Change{
			{Model: "Plan", Kind: fixturechange.Update,
				Key: fixturechange.Values{"name": fixturechange.Lit("team")},
				Old: fixturechange.Values{"price_cents": fixturechange.Lit("2000")},
				New: fixturechange.Values{"price_cents": fixturechange.Lit("2500")}},
			{Model: "Feature", Kind: fixturechange.Insert,
				Key: fixturechange.Values{"code": fixturechange.Lit("sso")},
				New: fixturechange.Values{"code": fixturechange.Lit("sso"), "plan_id": fixturechange.RefTo("Plan", "team")}},
		},
	}
	if err := Validate(set); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		set  fixturechange.Set
		want string
	}{
		"unknown model": {
			fixturechange.Set{Tables: tables(), Changes: []fixturechange.Change{
				{Model: "Coupon", Kind: fixturechange.Update,
					Key: fixturechange.Values{"code": fixturechange.Lit("x")}},
			}}, "unknown model"},
		"unknown kind": {
			fixturechange.Set{Tables: tables(), Changes: []fixturechange.Change{
				{Model: "Plan", Kind: "replace", Key: fixturechange.Values{"name": fixturechange.Lit("x")}},
			}}, "unknown kind"},
		"no key": {
			fixturechange.Set{Tables: tables(), Changes: []fixturechange.Change{
				{Model: "Plan", Kind: fixturechange.Delete},
			}}, "no key"},
		"column that is not an identifier": {
			fixturechange.Set{Tables: tables(), Changes: []fixturechange.Change{
				{Model: "Plan", Kind: fixturechange.Delete,
					Key: fixturechange.Values{"name\"; DROP TABLE plans; --": fixturechange.Lit("x")}},
			}}, "plain SQL identifier"},
		"table that is not an identifier": {
			fixturechange.Set{Tables: fixturechange.Tables{"Plan": {Name: "plans; DROP TABLE x", ID: "id", Key: "name"}}},
			"plain SQL identifier"},
		// Without the old value the statement would overwrite whatever is
		// there, which is the one thing this tool promises not to do.
		"update without the old value": {
			fixturechange.Set{Tables: tables(), Changes: []fixturechange.Change{
				{Model: "Plan", Kind: fixturechange.Update,
					Key: fixturechange.Values{"name": fixturechange.Lit("team")},
					New: fixturechange.Values{"price_cents": fixturechange.Lit("2500")}},
			}}, "without its old value"},
		"insert with old values": {
			fixturechange.Set{Tables: tables(), Changes: []fixturechange.Change{
				{Model: "Plan", Kind: fixturechange.Insert,
					Key: fixturechange.Values{"name": fixturechange.Lit("team")},
					Old: fixturechange.Values{"price_cents": fixturechange.Lit("1")},
					New: fixturechange.Values{"price_cents": fixturechange.Lit("2")}},
			}}, "insert with old values"},
		"reference to an unknown model": {
			fixturechange.Set{Tables: tables(), Changes: []fixturechange.Change{
				{Model: "Plan", Kind: fixturechange.Delete,
					Key: fixturechange.Values{"name": fixturechange.RefTo("Coupon", "x")}},
			}}, "unknown model"},
		// The natural key alone would delete whatever carries the name now,
		// including a row somebody has since edited into something else.
		"delete without the row it removes": {
			fixturechange.Set{Tables: tables(), Changes: []fixturechange.Change{
				{Model: "Plan", Kind: fixturechange.Delete,
					Key: fixturechange.Values{"name": fixturechange.Lit("team")}},
			}}, "delete without the row it removes"},
		// A misspelled policy reads as one of the two settings, and which one
		// depends on the field: strict here, lenient in ChangedRow.
		"policy nobody can read": {
			fixturechange.Set{Tables: tables(), Policy: fixturechange.Policy{MissingRow: "warm"}},
			"policy MissingRow"},
		"policy that is not offered for this field": {
			fixturechange.Set{Tables: tables(), Policy: fixturechange.Policy{MissingRow: fixturechange.Ignore}},
			"policy MissingRow"},
	} {
		err := Validate(tc.set)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: expected an error containing %q, got %v", name, tc.want, err)
		}
	}
}

func TestQuoteIdent(t *testing.T) {
	for in, want := range map[string]string{
		"items":         `"items"`,
		"public.items":  `"public"."items"`,
		"_x1":           `"_x1"`,
		`a" OR "1`:      "",
		"":              "",
		"items; DROP x": "",
	} {
		got, err := quoteIdent(in)
		if want == "" {
			if err == nil {
				t.Errorf("quoteIdent(%q) should have failed, got %q", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("quoteIdent(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestInvert(t *testing.T) {
	ins := fixturechange.Change{Model: "Plan", Kind: fixturechange.Insert,
		Key: fixturechange.Values{"name": fixturechange.Lit("pro")},
		New: fixturechange.Values{"name": fixturechange.Lit("pro")}}
	back := invert(ins)
	if back.Kind != fixturechange.Delete || len(back.Old) != 1 || len(back.New) != 0 {
		t.Fatalf("an insert reverts to a guarded delete, got %+v", back)
	}
	upd := fixturechange.Change{Model: "Plan", Kind: fixturechange.Update,
		Key: fixturechange.Values{"name": fixturechange.Lit("pro")},
		Old: fixturechange.Values{"seats": fixturechange.Lit("1")},
		New: fixturechange.Values{"seats": fixturechange.Lit("2")}}
	back = invert(upd)
	if back.Old["seats"].Lit != "2" || back.New["seats"].Lit != "1" {
		t.Fatalf("an update reverts by swapping, got %+v", back)
	}
}

// A guard that matched nothing is not success. bun records a migration as
// applied the moment the function returns nil, so the default for every reason
// a statement could not do its job is to fail and roll back.
func TestModeForIsStrictByDefault(t *testing.T) {
	var strict fixturechange.Policy
	for _, pr := range []problem{problemMissing, problemIDDrift} {
		if got := modeFor(strict, pr); got != "error" {
			t.Errorf("modeFor(zero, %q) = %q, want error", pr, got)
		}
	}
	// Except a row somebody edited here: their edit is kept, and an update
	// that would overwrite it is skipped with a warning.
	if got := modeFor(strict, problemChanged); got != "warn" {
		t.Errorf("modeFor(zero, changed) = %q, want warn", got)
	}
	// And the database already holding what the change wanted is never a
	// problem at all.
	if got := modeFor(strict, problemBenign); got != "warn" {
		t.Errorf("modeFor(zero, benign) = %q, want warn", got)
	}
}

func TestModeForFollowsThePolicy(t *testing.T) {
	p := fixturechange.Policy{MissingRow: "warn", ChangedRow: "error", IDDrift: "ignore"}
	if got := modeFor(p, problemMissing); got != "warn" {
		t.Errorf("missing: %q", got)
	}
	if got := modeFor(p, problemChanged); got != "error" {
		t.Errorf("changed: %q", got)
	}
	if got := modeFor(p, problemIDDrift); got != "warn" {
		t.Errorf("id drift: %q", got)
	}
}

func TestValidateWantsAnIDColumnForAnIDGuard(t *testing.T) {
	set := fixturechange.Set{
		Name:   "rename",
		Tables: fixturechange.Tables{"Plan": {Name: "plans"}},
		Changes: []fixturechange.Change{{Model: "Plan", Kind: fixturechange.Update, ID: "2",
			Key: fixturechange.Values{"name": fixturechange.Lit("team")},
			Old: fixturechange.Values{"name": fixturechange.Lit("team")},
			New: fixturechange.Values{"name": fixturechange.Lit("crew")}}},
	}
	if err := Validate(set); err == nil || !strings.Contains(err.Error(), "id column") {
		t.Fatalf("a change guarded by an id needs the id column, got %v", err)
	}
	set.Tables["Plan"] = fixturechange.Table{Name: "plans", ID: "id"}
	if err := Validate(set); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRefusesAnInsertWithAnIDGuard(t *testing.T) {
	set := fixturechange.Set{
		Name:   "bad",
		Tables: tables(),
		Changes: []fixturechange.Change{{Model: "Plan", Kind: fixturechange.Insert, ID: "2",
			Key: fixturechange.Values{"name": fixturechange.Lit("team")},
			New: fixturechange.Values{"name": fixturechange.Lit("team")}}},
	}
	if err := Validate(set); err == nil {
		t.Fatal("an insert has nothing to guard on yet")
	}
}

// The empty policy is the one a hand-written set carries, and it has to pass.
func TestValidateAcceptsAnEmptyPolicy(t *testing.T) {
	if err := (fixturechange.Policy{}).Validate(); err != nil {
		t.Fatalf("the zero policy is the default, not a mistake: %v", err)
	}
	full := fixturechange.Policy{
		MissingRow: fixturechange.Error, ChangedRow: fixturechange.Warn, IDDrift: fixturechange.Ignore}
	if err := full.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestKeyLabelIsStableWhateverTheMapOrder(t *testing.T) {
	key := fixturechange.Values{
		"code":    fixturechange.Lit("api"),
		"plan_id": fixturechange.RefTo("Plan", "team"),
		"note":    fixturechange.Null(),
	}
	const want = "code=api,note=NULL,plan_id=Plan(team)"
	for i := 0; i < 20; i++ {
		if got := keyLabel(key); got != want {
			t.Fatalf("keyLabel = %q, want %q", got, want)
		}
	}
}

// The id is written by the insert but left out of the check for a row that is
// already there: the row is the same row whatever id this database gave it.
func TestWithoutColumn(t *testing.T) {
	values := fixturechange.Values{
		"id": fixturechange.Lit("3"), "name": fixturechange.Lit("pro")}
	out := withoutColumn(values, "id")
	if len(out) != 1 || out["name"].Lit != "pro" {
		t.Fatalf("withoutColumn dropped the wrong thing: %+v", out)
	}
	if len(values) != 2 {
		t.Fatal("the original has to be left alone")
	}
}

func TestRowCount(t *testing.T) {
	for n, want := range map[int64]string{0: "0 rows", 1: "1 row", 2: "2 rows"} {
		if got := rowCount(n); got != want {
			t.Errorf("rowCount(%d) = %q, want %q", n, got, want)
		}
	}
}
