package fixturemigrate

import (
	"go/format"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

func TestRenderIsValidGoAndGofmtClean(t *testing.T) {
	cfg := testConfig(t)
	res := compute(t, base, replace(t, base, "      price_cents: 2000\n", "      price_cents: 2500\n"))
	src, err := Render(cfg, "plan prices", "20260921120000", res)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "gen.go", src, parser.AllErrors); err != nil {
		t.Fatalf("the generated file does not parse: %v\n%s", err, src)
	}
	formatted, err := format.Source(src)
	if err != nil {
		t.Fatal(err)
	}
	if string(formatted) != string(src) {
		t.Fatal("the generated file is not gofmt-clean")
	}
	text := string(src)
	for _, want := range []string{
		"package migrations",
		"Migrations.MustRegister",
		"fixtureapply.Apply(ctx, db, fixtureChanges20260921120000PlanPrices)",
		"fixtureapply.Revert(ctx, db, fixtureChanges20260921120000PlanPrices)",
		`SeedGuardTable: "plans"`,
		`"Plan": {Name: "plans", ID: "id"}`,
		`fixturechange.Lit("2500")`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the generated file is missing %q:\n%s", want, text)
		}
	}
}

// The change set a generated file carries has to pass the runtime's own check,
// otherwise the migration fails at deploy time rather than here.
func TestRenderedSetPassesValidate(t *testing.T) {
	res := compute(t, base, replace(t, base, "      price_cents: 2000\n", "      price_cents: 2500\n"))
	set := fixturechange.Set{Name: "test", Tables: res.Tables, Changes: res.Changes}
	if err := fixtureapply.Validate(set); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestRenderListsWhatItRefused(t *testing.T) {
	cfg := testConfig(t)
	next := replace(t, base, "      price_cents: 2000\n", "      price_cents: 2500\n")
	next = replace(t, next, "      seats: 1\n", "      seats: 1\n      trial_days: 14\n")
	res := compute(t, base, next)
	if len(res.Refusals) == 0 {
		t.Fatal("expected a refusal to report")
	}
	src, err := Render(cfg, "prices", "20260921120000", res)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "Left out, write these yourself:") ||
		!strings.Contains(string(src), "trial_days") {
		t.Fatalf("the refusals belong in the file:\n%s", src)
	}
}

func TestRenderRefusesAnEmptyChangeSet(t *testing.T) {
	if _, err := Render(testConfig(t), "nothing", "20260921120000", &Result{}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestNames(t *testing.T) {
	for _, tc := range []struct{ in, slug, file, ident string }{
		{"plan prices", "plan_prices", "20260921120000_fixture_plan_prices.go", "fixtureChanges20260921120000PlanPrices"},
		{"  Raise-Seats!  ", "raise_seats", "20260921120000_fixture_raise_seats.go", "fixtureChanges20260921120000RaiseSeats"},
		{"???", "changes", "20260921120000_fixture_changes.go", "fixtureChanges20260921120000Changes"},
	} {
		if got := Slug(tc.in); got != tc.slug {
			t.Errorf("Slug(%q) = %q, want %q", tc.in, got, tc.slug)
		}
		if got := FileName("20260921120000", tc.in); got != tc.file {
			t.Errorf("FileName(%q) = %q, want %q", tc.in, got, tc.file)
		}
		if got := VarName("20260921120000", tc.in); got != tc.ident {
			t.Errorf("VarName(%q) = %q, want %q", tc.in, got, tc.ident)
		}
	}
}
