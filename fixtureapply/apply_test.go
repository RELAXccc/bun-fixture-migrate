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
