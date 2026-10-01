package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// status says per migration what its last run did here, by the audit table:
// the counts, the changes it skipped and why, and a file edited since.
func TestStatusReadsWhatTheAuditTableSays(t *testing.T) {
	set := fixturechange.Set{Name: "20260921120000_fixture_prices", AuditTable: "bun_fixture_audit",
		Tables: fixturechange.Tables{"Plan": {Name: "plans", ID: "id", Key: "name"}},
		Changes: []fixturechange.Change{{Model: "Plan", Kind: fixturechange.Update,
			Key: fixturechange.Values{"name": fixturechange.Lit("team")},
			Old: fixturechange.Values{"price": fixturechange.Lit("1")},
			New: fixturechange.Values{"price": fixturechange.Lit("2")}}}}
	other := set
	other.Name = "20260922120000_fixture_other"
	all := []fixturemigrate.MigrationFile{
		{Name: "20260921120000", Comment: "fixture_prices", Fixture: &set},
		{Name: "20260922120000", Comment: "fixture_other", Fixture: &other},
		{Name: "20260923120000", Comment: "schema"},
	}
	r := &statusReport{Database: &databaseInfo{Audit: &auditTableInfo{Table: "bun_fixture_audit", Exists: true}}}
	for _, m := range all {
		r.Migrations = append(r.Migrations, migrationInfo{ID: m.ID(), Name: m.Name, Fixture: m.Fixture != nil})
	}
	at := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	edited := set
	edited.Changes = nil
	audit := map[string]fixtureapply.AuditRecord{
		set.Name: {Set: set.Name, Direction: fixtureapply.DirectionUp, AppliedAt: at, AppliedBy: "deploy",
			SetSHA256: fixtureapply.SetSHA256(set), Outcomes: []fixtureapply.AuditOutcome{
				{Index: 0, Model: "Plan", Key: "name=team", Kind: "update", Status: "skipped", Problem: "changed row"},
				{Index: 1, Model: "Plan", Key: "name=pro", Kind: "insert", Status: "applied"},
				{Index: 2, Model: "Plan", Key: "name=solo", Kind: "update", Status: "unchanged"},
				{Index: -1, Model: "Plan", Status: "sequence"},
			}},
		other.Name: {Set: other.Name, Direction: fixtureapply.DirectionDown, AppliedAt: at, AppliedBy: "ops",
			SetSHA256: fixtureapply.SetSHA256(edited)},
	}
	markAudit(r, all, audit)
	a := r.Migrations[0].Audit
	if a == nil || a.Applied != 1 || a.Unchanged != 1 || a.Skipped != 1 || len(a.SkippedChanges) != 1 ||
		a.SkippedChanges[0].Key != "name=team" || a.Edited || a.By != "deploy" {
		t.Fatalf("the first migration: %+v", a)
	}
	if b := r.Migrations[1].Audit; b == nil || b.Direction != "down" || !b.Edited {
		t.Fatalf("the second migration: %+v", b)
	}
	if r.Migrations[2].Audit != nil {
		t.Fatal("a migration with no row has none")
	}
	if len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "20260922120000_fixture_other was edited after it ran here") {
		t.Fatalf("notes: %v", r.Notes)
	}
	var out bytes.Buffer
	printAudit(streams{stdout: &out}, r)
	for _, want := range []string{
		"what the fixture migrations did here, according to bun_fixture_audit",
		"20260921120000_fixture_prices: applied 2026-09-21 12:00:00 by deploy: 1 applied, 1 unchanged, 1 skipped",
		"    skipped Plan name=team update [changed row]",
		"20260922120000_fixture_other: reverted 2026-09-21 12:00:00 by ops: 0 reverted, 0 unchanged, 0 skipped",
		"edited after it ran here",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q:\n%s", want, out.String())
		}
	}
	r.Database.Audit.Exists = false
	out.Reset()
	printAudit(streams{stdout: &out}, r)
	if !strings.Contains(out.String(), "bun_fixture_audit does not exist yet") {
		t.Fatalf("a table not there yet:\n%s", out.String())
	}
}
