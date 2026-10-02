package fixtureapply

import (
	"strings"
	"testing"
	"time"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

func auditSet() fixturechange.Set {
	return fixturechange.Set{
		Name:            "20260921120000_fixture_prices",
		SeedGuardTable:  "plans",
		MigrationsTable: "bun_migrations",
		AuditTable:      "bun_fixture_audit",
		Tables: fixturechange.Tables{
			"Plan":    {Name: "plans", ID: "id", Key: "name", Serial: true},
			"Feature": {Name: "features", ID: "id", Policy: &fixturechange.Policy{ChangedRow: "error"}},
		},
		Policy: fixturechange.Policy{MissingRow: "error", ChangedRow: "warn", IDDrift: "error", DuplicateKey: "error"},
		Changes: []fixturechange.Change{
			{Model: "Plan", Kind: fixturechange.Update,
				Key: fixturechange.Values{"name": fixturechange.Lit("team")},
				Old: fixturechange.Values{"price_cents": fixturechange.Lit("2000"), "note": fixturechange.Null()},
				New: fixturechange.Values{"price_cents": fixturechange.Lit("2500"), "note": fixturechange.Lit("")}},
			{Model: "Feature", Kind: fixturechange.Insert,
				Key: fixturechange.Values{"code": fixturechange.Lit("sso"), "plan_id": fixturechange.RefTo("Plan", "team")},
				New: fixturechange.Values{"code": fixturechange.Lit("sso"), "plan_id": fixturechange.RefTo("Plan", "team")}},
		},
	}
}

func TestSetSHA256IsTheSameForTheSameSet(t *testing.T) {
	want := SetSHA256(auditSet())
	if len(want) != 64 || strings.Trim(want, "0123456789abcdef") != "" {
		t.Fatalf("not a hex SHA-256: %q", want)
	}
	// Maps are filled in another order every time; the hash does not care.
	for i := 0; i < 20; i++ {
		if got := SetSHA256(auditSet()); got != want {
			t.Fatalf("the hash changed between two runs over one set: %s, %s", got, want)
		}
	}
	// Format 0 and 1 mean the same, and so does an empty table policy.
	set := auditSet()
	set.Format = 1
	set.Tables["Plan"] = fixturechange.Table{Name: "plans", ID: "id", Key: "name", Serial: true,
		Policy: &fixturechange.Policy{}}
	if got := SetSHA256(set); got != want {
		t.Fatalf("Format 1 and an empty table policy changed the hash")
	}
}

// Every field that changes what the set does changes the hash, and so do the
// values that only differ in kind: a NULL, an empty string, the text NULL.
func TestSetSHA256SeesEveryChange(t *testing.T) {
	base := SetSHA256(auditSet())
	for name, edit := range map[string]func(*fixturechange.Set){
		"name":        func(s *fixturechange.Set) { s.Name = "x" },
		"format":      func(s *fixturechange.Set) { s.Format = 2 },
		"lock":        func(s *fixturechange.Set) { s.LockTimeout = "5s" },
		"audit table": func(s *fixturechange.Set) { s.AuditTable = "other" },
		"policy":      func(s *fixturechange.Set) { s.Policy.MissingRow = "warn" },
		"table policy": func(s *fixturechange.Set) {
			s.Tables["Feature"] = fixturechange.Table{Name: "features", ID: "id"}
		},
		"where": func(s *fixturechange.Set) {
			s.Tables["Plan"] = fixturechange.Table{Name: "plans", ID: "id", Key: "name", Serial: true, Where: "true"}
		},
		// A delete of a model with a soft delete keeps the row: a file
		// that gained it after it ran here does something else.
		"soft delete": func(s *fixturechange.Set) {
			s.Tables["Plan"] = fixturechange.Table{Name: "plans", ID: "id", Key: "name", Serial: true,
				SoftDelete: "deleted_at"}
		},
		"dropped change": func(s *fixturechange.Set) { s.Changes = s.Changes[:1] },
		"reordered":      func(s *fixturechange.Set) { s.Changes[0], s.Changes[1] = s.Changes[1], s.Changes[0] },
		"value":          func(s *fixturechange.Set) { s.Changes[0].New["price_cents"] = fixturechange.Lit("2600") },
		"null to empty":  func(s *fixturechange.Set) { s.Changes[0].Old["note"] = fixturechange.Lit("") },
		"null to NULL":   func(s *fixturechange.Set) { s.Changes[0].Old["note"] = fixturechange.Lit("NULL") },
		"empty to null":  func(s *fixturechange.Set) { s.Changes[0].New["note"] = fixturechange.Null() },
		"reference": func(s *fixturechange.Set) {
			s.Changes[1].New["plan_id"] = fixturechange.Lit("Plan(team)")
		},
		"id guard": func(s *fixturechange.Set) { s.Changes[0].ID = "2" },
		"invalid UTF-8": func(s *fixturechange.Set) {
			s.Changes[0].New["note"] = fixturechange.Lit("\xff")
		},
	} {
		set := auditSet()
		edit(&set)
		if SetSHA256(set) == base {
			t.Errorf("%s: the hash did not change", name)
		}
	}
	a, b := auditSet(), auditSet()
	a.Changes[0].New["note"] = fixturechange.Lit("\xfe")
	b.Changes[0].New["note"] = fixturechange.Lit("\xff")
	if SetSHA256(a) == SetSHA256(b) {
		t.Error("two bytes that are not UTF-8 hash the same")
	}
}

func TestAppliesSayWhichChangesWereMade(t *testing.T) {
	set := auditSet()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	first := AuditRecord{ID: 4, Set: set.Name, Direction: DirectionUp, AppliedAt: at, Outcomes: []AuditOutcome{
		{Index: 0, Model: "Plan", Key: "name=team", Kind: fixturechange.Update, Status: StatusApplied},
		{Index: 1, Model: "Feature", Key: "code=sso,plan_id=Plan(team)", Kind: fixturechange.Insert,
			Status: StatusSkipped, Problem: ProblemChangedRow},
	}}
	// A Revert that failed left the changes made, and the Apply after it
	// found them all made.
	again := AuditRecord{ID: 7, Set: set.Name, Direction: DirectionUp, AppliedAt: at.Add(time.Hour),
		Outcomes: []AuditOutcome{
			{Index: 0, Model: "Plan", Key: "name=team", Kind: fixturechange.Update, Status: StatusUnchanged},
			{Index: 1, Model: "Feature", Key: "code=sso,plan_id=Plan(team)", Kind: fixturechange.Insert,
				Status: StatusUnchanged},
		}}
	base := Applies{again, first}
	if made, _, row := base.Made(0, set.Changes[0]); !made || row.ID != 4 {
		t.Fatalf("the first Apply made the update: %v %+v", made, row)
	}
	made, done, row := base.Made(1, set.Changes[1])
	if made || row == nil || row.ID != 7 || done.Status != StatusUnchanged {
		t.Fatalf("no Apply made the insert, and the newest says what it found: %v %+v %+v", made, done, row)
	}
	if msg := notMade(base, done, row); !strings.Contains(msg, "found it unchanged") ||
		!strings.Contains(msg, "(row 7)") {
		t.Fatalf("notMade: %s", msg)
	}

	// The set was edited: a change moved, one was added.
	edited := auditSet()
	added := fixturechange.Change{Model: "Plan", Kind: fixturechange.Insert,
		Key: fixturechange.Values{"name": fixturechange.Lit("pro")},
		New: fixturechange.Values{"name": fixturechange.Lit("pro")}}
	edited.Changes = append([]fixturechange.Change{added}, edited.Changes...)
	if made, _, _ := (Applies{first}).Made(1, edited.Changes[1]); !made {
		t.Fatal("a change that moved is found by its model, key and kind")
	}
	made, _, row = (Applies{first}).Made(0, edited.Changes[0])
	if made || row != nil {
		t.Fatal("a change the run did not have was not made by it")
	}
	if msg := notMade(Applies{first}, AuditOutcome{}, nil); !strings.Contains(msg, "did not hold this change") {
		t.Fatalf("notMade: %s", msg)
	}

	// A run that found the database unseeded changed nothing, whatever the
	// set holds.
	unseeded := AuditRecord{ID: 9, Set: set.Name, Direction: DirectionUp, AppliedAt: at,
		Outcomes: []AuditOutcome{{Index: -1, Status: StatusUnseeded}}}
	if !unseeded.Unseeded() || first.Unseeded() {
		t.Fatal("Unseeded")
	}
	made, done, row = (Applies{unseeded}).Made(0, set.Changes[0])
	if made || row != nil {
		t.Fatal("an unseeded run made nothing")
	}
	if msg := notMade(Applies{unseeded}, done, row); !strings.Contains(msg, "the run here was unseeded (audit row 9)") ||
		strings.Contains(msg, "did not hold") {
		t.Fatalf("notMade of an unseeded run: %s", msg)
	}
	// Seeded since and run again: the run that ran is the one to name.
	if msg := notMade(Applies{first, unseeded}, AuditOutcome{}, nil); !strings.Contains(msg, "(row 4)") {
		t.Fatalf("notMade: %s", msg)
	}
}

func TestAuditRecordCountsTheChangesOnly(t *testing.T) {
	r := AuditRecord{Outcomes: []AuditOutcome{
		{Index: 0, Status: StatusApplied}, {Index: 1, Status: StatusApplied}, {Index: 2, Status: StatusSkipped},
		{Index: -1, Status: StatusSequence}, {Index: 3, Status: StatusUnchanged},
	}}
	if r.Count(StatusApplied) != 2 || r.Count(StatusSkipped) != 1 || r.Count(StatusUnchanged) != 1 ||
		r.Count(StatusSequence) != 0 {
		t.Fatalf("counts: %+v", r)
	}
}
