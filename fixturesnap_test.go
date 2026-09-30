package fixturemigrate

import (
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// These pin the reading of a fixture file to dbfixture's own. A file this tool
// accepts has to be a file dbfixture loads, and a reference has to name the row
// dbfixture would have bound it to; dbtest/dbfixture_test.go shows each of
// these against the real loader.

func snapErr(t *testing.T, text string) error {
	t.Helper()
	_, err := FixtureSnapshot(testConfig(t), doc(t, text), "fixture.yml")
	return err
}

func planRef(t *testing.T, s *Snapshot, plan string) fixturechange.Value {
	t.Helper()
	for _, e := range s.Entries["Plan"] {
		if e.Cells["name"].Lit == plan {
			return e.Cells["currency_id"]
		}
	}
	t.Fatalf("no plan %q", plan)
	return fixturechange.Value{}
}

// A row without "_id" is registered as "pk" and its primary key.
func TestARowWithoutAnAnchorIsNamedByItsPrimaryKey(t *testing.T) {
	text := `- model: Currency
  rows:
    - id: 7
      code: EUR
      symbol: "E"
- model: Plan
  rows:
    - _id: team
      id: 2
      name: team
      currency_id: '{{ $.Currency.pk7.ID }}'
`
	got := planRef(t, snap(t, testConfig(t), text, "fixture.yml"), "team")
	if got.Ref == nil || got.Ref.Model != "Currency" || got.Ref.Key != "EUR" {
		t.Fatalf("got %+v", got)
	}
	// A row with an anchor is registered under the anchor only.
	withAnchor := strings.Replace(text, "    - id: 7\n", "    - _id: eur\n      id: 7\n", 1)
	if err := snapErr(t, withAnchor); err == nil || !strings.Contains(err.Error(), "names no row") {
		t.Fatalf("pk7 is not a name for a row that has an _id: %v", err)
	}
}

// dbfixture resolves a template while it loads the file, against the rows it
// has inserted so far.
func TestAReferenceToARowFurtherDownIsAnError(t *testing.T) {
	err := snapErr(t, `- model: Plan
  rows:
    - _id: team
      id: 2
      name: team
      currency_id: '{{ $.Currency.eur.ID }}'
- model: Currency
  rows:
    - _id: eur
      id: 1
      code: EUR
      symbol: "E"
`)
	if err == nil || !strings.Contains(err.Error(), "further down") {
		t.Fatalf("expected the forward reference to be refused, got %v", err)
	}
}

// Two rows can share an anchor; each template names the latest one above it.
func TestTheLatestRowOfAnAnchorIsTheOneNamed(t *testing.T) {
	s := snap(t, testConfig(t), `- model: Currency
  rows:
    - _id: main
      id: 1
      code: EUR
      symbol: "E"
- model: Plan
  rows:
    - _id: early
      id: 1
      name: early
      currency_id: '{{ $.Currency.main.ID }}'
- model: Currency
  rows:
    - _id: main
      id: 2
      code: USD
      symbol: "$"
- model: Plan
  rows:
    - _id: late
      id: 2
      name: late
      currency_id: '{{ $.Currency.main.ID }}'
`, "fixture.yml")
	if got := planRef(t, s, "early"); got.Ref == nil || got.Ref.Key != "EUR" {
		t.Fatalf("early: %+v", got)
	}
	if got := planRef(t, s, "late"); got.Ref == nil || got.Ref.Key != "USD" {
		t.Fatalf("late: %+v", got)
	}
}

// dbfixture only evaluates "{{ " ... " }}" with the spaces. Without them the
// text is not a template, and in a reference column it names no id.
func TestATemplateNeedsDbfixturesDelimiters(t *testing.T) {
	err := snapErr(t, strings.Replace(base, `'{{ $.Currency.eur.ID }}'`, `'{{$.Currency.eur.ID}}'`, 1))
	if err == nil || !strings.Contains(err.Error(), "no row of Currency") {
		t.Fatalf("expected the unspaced template to be read as text, got %v", err)
	}
}

// A template this tool cannot evaluate never reaches the database as written,
// so it can neither be compared nor written into a migration.
func TestATemplateThisToolCannotEvaluateIsAnError(t *testing.T) {
	text := strings.Replace(base, "      seats: 10\n", "      seats: 10\n      created_at: '{{ now }}'\n", 1)
	err := snapErr(t, text)
	if err == nil || !strings.Contains(err.Error(), "put created_at in ignore") {
		t.Fatalf("expected the template to be refused, got %v", err)
	}
	cfg := testConfig(t)
	cfg.Models["Plan"].Ignore = []string{"created_at"}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	if _, err := FixtureSnapshot(cfg, doc(t, text), "fixture.yml"); err != nil {
		t.Fatalf("an ignored column is not read at all: %v", err)
	}
}
