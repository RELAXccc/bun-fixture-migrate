package fixturemigrate

import (
	"context"
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// A value is cast to the column's type with its length and its domain: an
// INSERT refuses a value that is too long, and tooLong says so before the
// cast can cut it.
func TestCastType(t *testing.T) {
	for _, tc := range []struct {
		col  dbschema.Column
		want string
	}{
		{dbschema.Column{Type: "numeric", FullType: "numeric(10,2)"}, "numeric(10,2)"},
		{dbschema.Column{Type: "varchar", FullType: "character varying(3)", Length: 3}, "character varying(3)"},
		{dbschema.Column{Type: "bpchar", FullType: "character(3)", Length: 3}, "character(3)"},
		{dbschema.Column{Type: "_bpchar", FullType: "character(3)[]", Category: "A", ElemType: "bpchar"},
			"character(3)[]"},
		{dbschema.Column{Type: "int4", Domain: "qty", FullType: "qty"}, "qty"},
	} {
		if got := castType(tc.col); got != tc.want {
			t.Errorf("%+v: %s, want %s", tc.col, got, tc.want)
		}
	}
}

func TestReadExpr(t *testing.T) {
	for _, tc := range []struct {
		col  dbschema.Column
		want string
	}{
		{dbschema.Column{Type: "int8"}, `("x")::text`},
		{dbschema.Column{Type: "json"}, `("x")::jsonb::text`},
		{dbschema.Column{Type: "json", Domain: "doc"}, `("x")::jsonb::text`},
		{dbschema.Column{Type: "money"}, `("x")::numeric::text`},
		{dbschema.Column{Type: "_text", Category: "A", ElemType: "text"},
			`CASE WHEN ("x")::text LIKE '[%' THEN ("x")::text ELSE to_jsonb(("x"))::text END`},
		{dbschema.Column{Type: "_bpchar", Category: "A", ElemType: "bpchar"},
			`CASE WHEN ("x")::text LIKE '[%' THEN ("x")::text ELSE to_jsonb(("x")::text[])::text END`},
	} {
		if got := readExpr(tc.col, `"x"`); got != tc.want {
			t.Errorf("%+v: %s, want %s", tc.col, got, tc.want)
		}
	}
}

func TestLiteralOf(t *testing.T) {
	m := &Model{ID: "id"}
	e := &Entry{ID: "7", Cells: fixturechange.Values{
		"a": fixturechange.Lit("x"), "b": fixturechange.Null(), "c": fixturechange.RefTo("M", "k")}}
	for col, want := range map[string]bool{"id": true, "a": true, "b": false, "c": false, "missing": false} {
		if _, ok := literalOf(e, m, col); ok != want {
			t.Errorf("%s: %v", col, ok)
		}
	}
	if _, ok := literalOf(&Entry{}, m, "id"); ok {
		t.Fatal("no id is nothing to cast")
	}
}

func TestFixtureLabel(t *testing.T) {
	c := &Config{Fixture: "a.yml"}
	if c.FixtureLabel() != "a.yml" {
		t.Fatal(c.FixtureLabel())
	}
	c.Fixtures = []string{"a.yml", "b.yml"}
	if c.FixtureLabel() != "a.yml, b.yml" {
		t.Fatal(c.FixtureLabel())
	}
}

// A snapshot built by hand can name a model the configuration does not have.
// That is the mistake FixtureSnapshot refuses, and it has to be an error here
// too rather than a nil pointer.
func TestCanonicalizeRefusesAModelMissingFromTheConfiguration(t *testing.T) {
	snap := &Snapshot{Order: []string{"Coupon"}, Entries: map[string][]*Entry{}}
	err := Canonicalize(context.Background(), nil, testConfig(t), snap, nil)
	if err == nil || !strings.Contains(err.Error(), `"Coupon"`) {
		t.Fatalf("expected an error naming the model, got %v", err)
	}
}
