package dbtest_test

// What a review of the value fidelity found, each held against the real
// dbfixture: a fresh seed of a file has to read as that file, and a
// migration or a sync has to store what the seed stores.

import (
	"context"
	"testing"
	"time"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"

	"github.com/uptrace/bun"
)

type RvJ struct {
	bun.BaseModel `bun:"table:rv_j"`
	ID            int64          `bun:"id,pk"`
	Code          string         `bun:"code,notnull"`
	Doc           map[string]any `bun:"doc,type:jsonb"`
}

// A finding names its row by the key's label, as every other one does, not
// by the encoding two keys are compared in.
func TestReviewFindingsNameTheRowByItsLabel(t *testing.T) {
	db := connect(t)
	if _, err := db.ExecContext(context.Background(), "CREATE EXTENSION IF NOT EXISTS citext"); err != nil {
		t.Skipf("citext: %v", err)
	}
	l := newLab(t, map[string]*fixturemigrate.Model{"RvJ": {Table: "rv_j", Key: []string{"code"}}}, "rv_j",
		[]string{"DROP TABLE IF EXISTS rv_j", "CREATE TABLE rv_j (id bigint PRIMARY KEY, code citext NOT NULL, doc jsonb)"},
		`SELECT ''`, (*RvJ)(nil))
	head := l.read("- model: RvJ\n  rows:\n    - {id: 1, code: Go, doc: ~}\n    - {id: 2, code: GO, doc: {}}\n")
	kinds := map[fixturemigrate.FindingKind]bool{}
	for _, f := range head.Findings {
		kinds[f.Kind] = true
		if f.Row != "RvJ/code=Go" {
			t.Errorf("%s names its row %q", f.Kind, f.Row)
		}
	}
	if !kinds[fixturemigrate.FindingNullDefault] || !kinds[fixturemigrate.FindingDuplicateKey] {
		t.Fatalf("expected a null and a duplicate key, got %+v", head.Findings)
	}
}

type RvAny struct {
	bun.BaseModel `bun:"table:rv_any"`
	ID            int64       `bun:"id,pk"`
	Name          string      `bun:"name,notnull"`
	Doc           any         `bun:"doc,type:jsonb"`
	List          []any       `bun:"list,type:jsonb"`
	At            time.Time   `bun:"at,nullzero"`
	Ats           []time.Time `bun:"ats,array"`
}

// A sequence or a scalar at the top of a jsonb column is what an any or a
// slice field makes of it, as it is inside a mapping: a timestamp keeps its
// offset, a date alone is midnight UTC, a float is a float64. And a date alone
// in a timestamptz column is the midnight UTC a time.Time field makes of it,
// not a value the seeding session decides.
func TestReviewTopLevelJSONAndDatesReadAsDbfixtureStoresThem(t *testing.T) {
	l := newLab(t, map[string]*fixturemigrate.Model{"RvAny": {Table: "rv_any", Key: []string{"name"}}}, "rv_any",
		[]string{"DROP TABLE IF EXISTS rv_any", "CREATE TABLE rv_any (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, " +
			"doc jsonb, list jsonb, at timestamptz, ats timestamptz[])"},
		`SELECT string_agg(concat_ws('|', name, doc::text, list::text, at AT TIME ZONE 'UTC', ats::text), E'\n' ORDER BY name) FROM rv_any`,
		(*RvAny)(nil))
	v1 := "- model: RvAny\n  rows:\n    - {id: 1, name: a, doc: 1, list: [1], at: 2025-06-01T00:00:00Z, ats: [2025-06-01T00:00:00Z]}\n"
	for _, row := range []string{
		`{id: 1, name: a, doc: 1, list: [2026-01-01T10:00:00+02:00], at: 2026-01-01, ats: [2026-01-01]}`,
		`{id: 1, name: a, doc: 1, list: [2026-01-01], at: 2026-01-01T10:00:00+02:00, ats: [2026-01-01, 2026-01-02T10:00:00+02:00]}`,
		`{id: 1, name: a, doc: 1, list: [0.1234567890123456789, {k: 2026-01-01}], at: 2026-01-01, ats: [2026-01-01]}`,
		`{id: 1, name: a, doc: 2026-01-01T10:00:00+02:00, list: [1], at: 2026-01-01, ats: [2026-01-01]}`,
		`{id: 1, name: a, doc: 0.1234567890123456789, list: [1], at: 2026-01-01, ats: [2026-01-01]}`,
		`{id: 1, name: a, doc: 2026-01-01, list: [1], at: 2026-01-01, ats: [2026-01-01]}`,
		`{id: 1, name: a, doc: 2026-01-01 10:00:00, list: [2026-01-01 10:00:00], at: 2026-01-01 10:00:00, ats: [2026-01-01]}`,
	} {
		t.Run(row, func(t *testing.T) {
			l.t = t
			l.fidelity(v1, "- model: RvAny\n  rows:\n    - "+row+"\n")
		})
	}
}
