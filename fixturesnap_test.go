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

// bun writes DEFAULT for a zero only in an autoincrement field, so a zero is
// "no id" in a serial model and an id like any other elsewhere.
func TestAZeroIDIsAnIDUnlessTheModelIsSerial(t *testing.T) {
	cfg := testConfig(t)
	text := `- model: Currency
  rows:
    - {id: 0, code: XXX}
- model: Plan
  rows:
    - {id: 0, name: free, currency_id: 0}
- model: Feature
  rows:
    - {id: 0, plan_id: 0, code: api}
`
	s := snap(t, cfg, text, "fixture.yml")
	if got := s.Entries["Currency"][0].ID; got != "0" {
		t.Fatalf("a zero in a model that is not serial is its id, got %q", got)
	}
	if got := s.Entries["Feature"][0].ID; got != "" {
		t.Fatalf("a zero in a serial model is left to the sequence, got %q", got)
	}
	// And a reference holding 0 names the row whose id is 0, where there is
	// one.
	if got := planRef(t, s, "free"); got.Ref == nil || got.Ref.Key != "XXX" {
		t.Fatalf("expected Currency XXX, got %+v", got)
	}
	if got := s.Entries["Feature"][0].Cells["plan_id"]; got.Ref == nil || got.Ref.Key != "free" {
		t.Fatalf("expected Plan free, got %+v", got)
	}
}

// A reference carries its row's ref value, and 0012 there is the integer 10
// or the text 0012 depending on the ref column's type. Both readings travel
// with the reference, and the ref column is the one whose type decides,
// whether the reference is a template or a plain id.
func TestAReferenceKeepsBothReadingsOfItsRow(t *testing.T) {
	cfg := testConfig(t)
	text := `- model: Currency
  rows:
    - {_id: odd, id: 1, code: 0012}
    - {_id: ten, id: 2, code: "10"}
- model: Plan
  rows:
    - {id: 1, name: a, currency_id: '{{ $.Currency.odd.ID }}', note: '{{ $.Currency.odd.Code }}'}
    - {id: 2, name: b, currency_id: 1}
`
	s := snap(t, cfg, text, "fixture.yml")
	for _, e := range s.Entries["Plan"] {
		if ref := e.Cells["currency_id"].Ref; ref == nil || ref.Key != "10" {
			t.Fatalf("expected the resolved reading, got %+v", e.Cells["currency_id"])
		}
		if e.AsWritten["currency_id"] != "0012" || e.from["currency_id"] != (source{"Currency", "code"}) {
			t.Fatalf("expected the reading as written and its column, got %q %+v", e.AsWritten, e.from)
		}
	}
	// A template copying a field hands on what that field holds, so the
	// field's type decides there too.
	a := s.Entries["Plan"][0]
	if a.Cells["note"].Lit != "10" || a.AsWritten["note"] != "0012" || a.from["note"] != (source{"Currency", "code"}) {
		t.Fatalf("got %+v %q %+v", a.Cells["note"], a.AsWritten, a.from)
	}
	// Until a type decides, 0012 and "10" are not known to be one key, so
	// neither is reported as a duplicate of the other.
	if len(s.Findings) != 0 {
		t.Fatalf("expected no finding, got %+v", s.Findings)
	}

	// And a change that needs to know is refused without the database.
	res := computeWith(t, cfg, text, text+"    - {id: 3, name: c, currency_id: '{{ $.Currency.odd.ID }}'}\n")
	if len(res.Changes) != 0 || len(res.Refusals) != 1 ||
		!strings.Contains(res.Refusals[0].Reason, "currency_id points at the Currency whose code is written 0012") {
		t.Fatalf("expected the insert to be refused, got %+v / %+v", res.Changes, res.Refusals)
	}
	// So is a reference whose row only changed its spelling.
	res = computeWith(t, cfg, text, strings.Replace(text, "code: 0012", "code: 012", 1))
	for _, r := range res.Refusals {
		if r.Model == "Plan" && strings.Contains(r.Reason, "written 0012 before and 012 after") {
			return
		}
	}
	t.Fatalf("expected the respelled reference to be refused, got %+v / %+v", res.Changes, res.Refusals)
}

// A template copying a field that is itself a template copies whatever
// dbfixture made of that one, which this tool does not follow.
func TestACopyOfATemplateIsRefused(t *testing.T) {
	err := snapErr(t, `- model: Currency
  rows:
    - {_id: eur, id: 1, code: EUR}
- model: Plan
  rows:
    - {_id: a, id: 1, name: a, currency_id: '{{ $.Currency.eur.ID }}'}
    - {id: 2, name: b, currency_id: '{{ $.Currency.eur.ID }}', note: '{{ $.Plan.a.CurrencyID }}'}
`)
	if err == nil || !strings.Contains(err.Error(), "which is itself a template") {
		t.Fatalf("expected the copy to be refused, got %v", err)
	}
}

// A key value whose spelling alone changed is the same key in a numeric
// column and a rename in a text one, and says so.
func TestARespelledKeyIsNeitherARenameNorNothing(t *testing.T) {
	text := "- model: Currency\n  rows:\n    - {_id: odd, id: 1, code: 0012}\n"
	res := computeWith(t, testConfig(t), text, strings.Replace(text, "code: 0012", "code: 012", 1))
	if len(res.Changes) != 0 || len(res.Refusals) != 1 ||
		!strings.Contains(res.Refusals[0].Reason, "code is written 0012 before and 012 after") {
		t.Fatalf("expected one refusal about the spelling, got %+v / %+v", res.Changes, res.Refusals)
	}
}
