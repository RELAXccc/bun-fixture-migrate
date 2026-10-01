package fixturemigrate

import (
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"reflect"
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
	// gofmt aligns a run of fields, so the spacing depends on the neighbours.
	text := strings.Join(strings.Fields(string(src)), " ")
	for _, want := range []string{
		"package migrations",
		"Migrations.MustRegister",
		"fixtureapply.Apply(ctx, db, fixtureChanges20260921120000PlanPrices)",
		"fixtureapply.Revert(ctx, db, fixtureChanges20260921120000PlanPrices)",
		`SeedGuardTable: "plans"`,
		`MigrationsTable: "bun_migrations"`,
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

// The migration runs under the application's search_path: a seed guard table
// in the configured schema has to say which schema it is in, or it is not
// found, or a table of the same name in public is taken for it.
func TestRenderQualifiesTheSeedGuardTable(t *testing.T) {
	res := compute(t, base, replace(t, base, "      price_cents: 2000\n", "      price_cents: 2500\n"))
	for _, tc := range []struct{ schema, table, want string }{
		{"public", "plans", `SeedGuardTable: "plans"`},
		{"app", "plans", `SeedGuardTable: "app.plans"`},
		{"app", "other.plans", `SeedGuardTable: "other.plans"`},
	} {
		cfg := testConfig(t)
		cfg.Schema, cfg.SeedGuardTable = tc.schema, tc.table
		src, err := Render(cfg, "x", "20260921120000", res)
		if err != nil {
			t.Fatal(err)
		}
		if text := strings.Join(strings.Fields(string(src)), " "); !strings.Contains(text, tc.want) {
			t.Fatalf("schema %s, table %s: want %s in\n%s", tc.schema, tc.table, tc.want, src)
		}
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

// hostile are strings a column can hold, and so a fixture file and, through
// generate -from-db, production data: each would end a // comment and become
// Go code in the application's migrations package, end a string literal, or
// make a reviewer read something other than what the file holds.
var hostile = []string{
	"b\nfunc init() { panic(1) }\n//",
	"x\r\nvar y = 1",
	"x\rfunc init() { panic(1) }",
	"*/ func init() {} /*",
	"`; func init() {}; var _ = `",
	`"; func init() {}; var _ = "`,
	"a\u2028func init() {}\u2029",
	"\u202e}{ )(tini cnuf",
	"\ufeffbom",
	"\x00nul",
	"\xff\xfe not UTF-8",
	"tab\there",
	"\x1b[31mred\x1b[0m",
	"\x85next line",
}

// hostileResult carries s in every value, refusal and name the file holds.
// PostgreSQL cannot store a NUL, so a value holding one is refused before
// anything is written; the comments still get it.
func hostileResult(value, text string) *Result {
	return &Result{
		Tables: fixturechange.Tables{
			"Plan":     {Name: "plans", ID: "id", Key: "name", Where: "note IS DISTINCT FROM 'x'"},
			"Currency": {Name: "currencies", ID: "id", Key: "code"},
		},
		Changes: []fixturechange.Change{
			{Model: "Plan", Kind: fixturechange.Update,
				Key: fixturechange.Values{"name": fixturechange.Lit(value)},
				Old: fixturechange.Values{"note": fixturechange.Lit(value), "currency_id": fixturechange.RefTo("Currency", value)},
				New: fixturechange.Values{"note": fixturechange.Lit(value + "!"), "currency_id": fixturechange.Null()}},
		},
		Refusals: []Refusal{{Model: "Plan", Key: "Plan/name=" + text, Reason: "renamed from " + text}},
		Head:     text,
		Base:     text,
	}
}

// checkDeclarations fails unless src parses and declares exactly what a
// generated migration declares: its imports, one init and the change set.
func checkDeclarations(t *testing.T, src []byte, ident string) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "gen.go", src, parser.AllErrors|parser.ParseComments)
	if err != nil {
		t.Fatalf("the generated file does not parse: %v\n%s", err, src)
	}
	var decls []string
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			decls = append(decls, "func "+d.Name.Name)
			if len(d.Body.List) != 1 {
				t.Fatalf("init holds %d statements:\n%s", len(d.Body.List), src)
			}
		case *ast.GenDecl:
			if d.Tok == token.IMPORT {
				continue
			}
			for _, spec := range d.Specs {
				for _, name := range spec.(*ast.ValueSpec).Names {
					decls = append(decls, d.Tok.String()+" "+name.Name)
				}
			}
		}
	}
	if got, want := strings.Join(decls, "; "), "func init; var "+ident; got != want {
		t.Fatalf("the generated file declares %q, want %q:\n%s", got, want, src)
	}
}

// Nothing a value, a refused row or a revision name holds gets out of the
// literal or the comment it is written into.
func TestRenderKeepsHostileTextInItsPlace(t *testing.T) {
	cfg := testConfig(t)
	for _, h := range hostile {
		value := h
		if strings.ContainsRune(value, 0) {
			value = "safe"
		}
		res := hostileResult(value, h)
		src, err := Render(cfg, "x", "20260921120000", res)
		if err != nil {
			t.Fatalf("%q: %v", h, err)
		}
		checkDeclarations(t, src, VarName("20260921120000", "x"))
		got, _, err := ReadChangeSet(src)
		if err != nil {
			t.Fatalf("%q: %v", h, err)
		}
		if !reflect.DeepEqual(got.Changes, res.Changes) || !reflect.DeepEqual(got.Tables, res.Tables) {
			t.Fatalf("%q reads back as\n%+v\nnot\n%+v", h, got.Changes, res.Changes)
		}
		for _, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, "panic(") || strings.HasPrefix(strings.TrimSpace(line), "var y") {
				if !strings.HasPrefix(strings.TrimSpace(line), "//") && !strings.Contains(line, "fixturechange.") {
					t.Fatalf("%q escaped into code: %q", h, line)
				}
			}
		}
	}
}

func TestRenderRefusesANameThatIsNotGo(t *testing.T) {
	for _, field := range []string{"package", "migrator"} {
		cfg := testConfig(t)
		bad := "migrations\nfunc init() { panic(1) }"
		if field == "package" {
			cfg.Package = bad
		} else {
			cfg.Migrator = bad
		}
		res := compute(t, base, replace(t, base, "      price_cents: 2000\n", "      price_cents: 2500\n"))
		if _, err := Render(cfg, "x", "20260921120000", res); err == nil || !strings.Contains(err.Error(), "not a Go identifier") {
			t.Fatalf("%s: %v", field, err)
		}
	}
}

// Whatever a value or a comment holds, the file Render writes declares only
// its own and reads back as the set it runs.
func FuzzRenderReadsBack(f *testing.F) {
	for _, h := range hostile {
		f.Add(h, h)
	}
	f.Add(`{"a":"b"}`, "HEAD:fixture.yml")
	cfg := &Config{Fixture: "fixture.yml", Out: "migrations", Models: map[string]*Model{
		"Plan": {Table: "plans", Key: []string{"name"}},
	}}
	if err := cfg.Prepare(); err != nil {
		f.Fatal(err)
	}
	ident := VarName("20260921120000", "x")
	f.Fuzz(func(t *testing.T, value, text string) {
		res := hostileResult(value, text)
		src, err := Render(cfg, "x", "20260921120000", res)
		if err != nil {
			if !strings.ContainsRune(value, 0) {
				t.Fatalf("%q: %v", value, err)
			}
			return
		}
		checkDeclarations(t, src, ident)
		got, ok, err := ReadChangeSet(src)
		if err != nil || !ok {
			t.Fatalf("%v %v\n%s", ok, err, src)
		}
		if !reflect.DeepEqual(got.Changes, res.Changes) || !reflect.DeepEqual(got.Tables, res.Tables) {
			t.Fatalf("reads back as\n%+v\nnot\n%+v", got.Changes, res.Changes)
		}
	})
}
