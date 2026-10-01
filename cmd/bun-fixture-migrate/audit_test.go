package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
)

// status says per migration what its last run did here, by the audit table:
// the counts, the changes it skipped and why, and a file edited since. (The
// library's tests read the table into the report.)
func TestStatusPrintsWhatTheAuditTableSays(t *testing.T) {
	at := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	r := &fixturemigrate.StatusReport{
		Database: &fixturemigrate.StatusDatabase{
			Audit: &fixturemigrate.StatusAuditTable{Table: "bun_fixture_audit", Exists: true}},
		Migrations: []fixturemigrate.StatusMigration{
			{ID: "20260921120000_fixture_prices", Name: "20260921120000", Fixture: true,
				Audit: &fixturemigrate.StatusAudit{Direction: "up", At: at, By: "deploy", Applied: 1, Unchanged: 1,
					Skipped: 1, SkippedChanges: []fixturemigrate.StatusAuditChange{
						{Index: 0, Model: "Plan", Key: "name=team", Kind: "update", Problem: "changed row"}}}},
			{ID: "20260922120000_fixture_other", Name: "20260922120000", Fixture: true,
				Audit: &fixturemigrate.StatusAudit{Direction: "down", At: at, By: "ops",
					SkippedChanges: []fixturemigrate.StatusAuditChange{}, Edited: true}},
			{ID: "20260923120000_schema", Name: "20260923120000"},
		},
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
