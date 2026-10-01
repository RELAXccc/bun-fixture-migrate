package dbtest_test

// What a review of the value fidelity found, each held against the real
// dbfixture: a fresh seed of a file has to read as that file, and a
// migration or a sync has to store what the seed stores.

import (
	"context"
	"testing"

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
