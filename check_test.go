package fixturemigrate

import (
	"strings"
	"testing"
)

// check reads the same diff the generator computes, with the database as the
// base state. Both sides are snapshots, so the report can be tested without a
// database by handing one of two fixture files the database's name.
func checkOf(t *testing.T, cfg *Config, database, fixture string) *CheckResult {
	t.Helper()
	res, err := Check(cfg, snap(t, cfg, database, "the database"), snap(t, cfg, fixture, "fixtures/fixture.yml"))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return res
}

func TestCheckSaysSoWhenThereIsNothingToSay(t *testing.T) {
	res := checkOf(t, testConfig(t), base, base)
	if res.Drifted() {
		t.Fatalf("expected no drift, got %+v", res.Changes)
	}
	if lines := res.Lines(); len(lines) != 1 || !strings.Contains(lines[0], "agree") {
		t.Fatalf("unexpected report: %v", lines)
	}
}

// Which side is which is the whole report. A row the file has and the database
// does not is an insert in the diff, and it has to be printed as the file
// having it — getting that backwards sends somebody to fix the wrong end.
func TestCheckReportsEachSideAsItself(t *testing.T) {
	cfg := testConfig(t)
	cfg.Models["Plan"].Deletes = DeleteAllow

	const database = `- model: Currency
  rows:
    - _id: eur
      id: 1
      code: EUR
      symbol: "E"
- model: Plan
  rows:
    - _id: free
      id: 1
      name: free
      currency_id: '{{ $.Currency.eur.ID }}'
      price_cents: 0
      seats: 1
    - _id: team
      id: 2
      name: team
      currency_id: '{{ $.Currency.eur.ID }}'
      price_cents: 2500
      seats: 10
`
	const fixture = `- model: Currency
  rows:
    - _id: eur
      id: 1
      code: EUR
      symbol: "E"
- model: Plan
  rows:
    - _id: team
      id: 2
      name: team
      currency_id: '{{ $.Currency.eur.ID }}'
      price_cents: 2000
      seats: 10
    - _id: pro
      id: 3
      name: pro
      currency_id: '{{ $.Currency.eur.ID }}'
      price_cents: 9000
      seats: 20
`
	res := checkOf(t, cfg, database, fixture)
	if !res.Drifted() {
		t.Fatal("expected drift")
	}
	report := strings.Join(res.Lines(), "\n")
	for _, want := range []string{
		"In the fixture file, not in the database:\n  Plan name=pro",
		"In the database, not in the fixture file:\n  Plan name=free",
		"Different in the database and the fixture file:\n  Plan name=team\n    price_cents: database 2500, file 2000",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("the report is missing\n%s\ngot:\n%s", want, report)
		}
	}
}

// A refusal is not drift in a column, it is a difference nothing can migrate.
// It belongs in the report under its own heading, because the answer to it is
// a hand-written migration and not an edit to the file.
func TestCheckReportsWhatCouldNotBeMigrated(t *testing.T) {
	cfg := testConfig(t)
	database := base
	fixture := replace(t, base, "      name: team\n", "      name: crew\n")
	res := checkOf(t, cfg, database, fixture)
	report := strings.Join(res.Lines(), "\n")
	if !strings.Contains(report, "Cannot be migrated as it stands:") {
		t.Fatalf("expected the heading:\n%s", report)
	}
	if !strings.Contains(report, "renamed from") {
		t.Fatalf("expected the rename to be named:\n%s", report)
	}
}

// The findings of both snapshots end up in one report: they are what is wrong
// with the file or the database on its own, before the two are compared.
func TestCheckCarriesTheFindingsOfBothSides(t *testing.T) {
	cfg := testConfig(t)
	withZero := replace(t, base, "      seats: 10\n", "      seats: 0\n")
	database := snap(t, cfg, base, "the database")
	fixture := snap(t, cfg, withZero, "fixtures/fixture.yml")
	LintZeroDefaults(cfg, fixture, testTables())

	res, err := Check(cfg, database, fixture)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(res.Findings) != 1 || res.Findings[0].Kind != FindingZeroDefault {
		t.Fatalf("unexpected findings: %+v", res.Findings)
	}
	if !res.Drifted() {
		t.Fatal("a finding on its own is drift: the file does not load as written")
	}
	report := strings.Join(res.Lines(), "\n")
	if !strings.Contains(report, "Zero against a default:") {
		t.Fatalf("the heading names the kind:\n%s", report)
	}
}

// Worst is what the exit code is made of: the strictest policy any surviving
// finding calls for, with the ignored ones dropped rather than counted.
func TestWorstFollowsThePolicyPerKind(t *testing.T) {
	cfg := testConfig(t)
	findings := []Finding{
		{Kind: FindingZeroDefault, Model: "Plan", Row: "name=team", Detail: "seats"},
		{Kind: FindingDuplicateKey, Model: "Feature", Row: "code=api", Detail: "two rows"},
	}

	mode, kept := cfg.Worst(findings)
	if mode != ModeError || len(kept) != 2 {
		t.Fatalf("the defaults are strict: %q, %d kept", mode, len(kept))
	}

	cfg.Policy.ZeroDefault = ModeIgnore
	cfg.Policy.DuplicateKey = ModeWarn
	mode, kept = cfg.Worst(findings)
	if mode != ModeWarn {
		t.Fatalf("nothing calls for an error any more: %q", mode)
	}
	if len(kept) != 1 || kept[0].Kind != FindingDuplicateKey {
		t.Fatalf("an ignored finding is not reported either: %+v", kept)
	}

	// A kind with no policy of its own is an error: a column the table does
	// not have is never something to carry on past.
	if got := cfg.FindingMode(FindingUnknownColumn); got != ModeError {
		t.Fatalf("FindingMode(unknown column) = %q", got)
	}
}
