package fixturemigrate

import (
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
)

func itemsConfig(t *testing.T) *Config {
	t.Helper()
	cfg := &Config{Models: map[string]*Model{"Item": {Table: "items", Key: []string{"name"}}}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

const (
	itemsInOrder = "- model: Item\n  rows:\n    - {id: 1, name: a, grp: g, position: 1}\n" +
		"    - {id: 2, name: b, grp: g, position: 2}\n    - {id: 3, name: c, grp: g, position: 3}\n"
	itemsRotated = "- model: Item\n  rows:\n    - {id: 1, name: a, grp: g, position: 2}\n" +
		"    - {id: 2, name: b, grp: g, position: 3}\n    - {id: 3, name: c, grp: g, position: 1}\n"
)

// computeUnder diffs two files with the catalog's word on the items table, as
// generate does with the database configured.
func computeUnder(t *testing.T, cfg *Config, table *dbschema.Table, oldText, newText string) *Result {
	t.Helper()
	old, next := snap(t, cfg, oldText, "old"), snap(t, cfg, newText, "new")
	old.noteUniques("Item", table)
	next.noteUniques("Item", table)
	res, err := Compute(cfg, old, next)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A list rotated under UNIQUE (grp, position), which is checked after every
// statement: whichever item moves first finds its new position held, in any
// order. Before, the circle made the index order nothing, and generate wrote
// a migration that could never run, without a word. Each of the three
// updates is refused now, with what to do.
func TestARotationUnderAUniqueIndexIsRefused(t *testing.T) {
	cfg := itemsConfig(t)
	table := &dbschema.Table{Uniques: [][]string{{"id"}, {"name"}, {"grp", "position"}}}
	res := computeUnder(t, cfg, table, itemsInOrder, itemsRotated)
	if len(res.Changes) != 0 || len(res.Refusals) != 3 {
		t.Fatalf("expected the three updates refused, got %s / %+v", kindsOf(res), res.Refusals)
	}
	for _, r := range res.Refusals {
		if !strings.Contains(r.Reason, "moves (grp, position) from") ||
			!strings.Contains(r.Reason, "trade values with it in a circle") ||
			!strings.Contains(r.Reason, "UNIQUE constraint DEFERRABLE INITIALLY IMMEDIATE") ||
			!strings.Contains(r.Reason, "a value no row holds, in a migration of its own") {
			t.Fatalf("the refusal has to say why and what to do: %s", r)
		}
	}
	if r := res.Refusals[0]; r.Key != "Item/name=a" ||
		!strings.Contains(r.Reason, "from (g, 1) to (g, 2) while Item/name=b and Item/name=c trade") {
		t.Fatalf("got %s", r)
	}

	// Two rows swapping the value of a unique column are the smallest
	// circle.
	res = computeUnder(t, cfg, &dbschema.Table{Uniques: [][]string{{"id"}, {"name"}, {"position"}}},
		"- model: Item\n  rows:\n    - {id: 1, name: a, position: 1}\n    - {id: 2, name: b, position: 2}\n",
		"- model: Item\n  rows:\n    - {id: 1, name: a, position: 2}\n    - {id: 2, name: b, position: 1}\n")
	if len(res.Changes) != 0 || len(res.Refusals) != 2 ||
		!strings.Contains(res.Refusals[0].Reason, "moves position from 1 to 2 while Item/name=b trade") {
		t.Fatalf("expected the swap refused, got %s / %+v", kindsOf(res), res.Refusals)
	}

	// The rest of the set is ordered without them, as before.
	res = computeUnder(t, cfg, table, itemsInOrder+"    - {id: 4, name: d, grp: h, position: 1}\n",
		itemsRotated+"    - {id: 4, name: d, grp: h, position: 2}\n    - {id: 5, name: e, grp: h, position: 1}\n")
	if got := kindsOf(res); got != "update Item/name=d; insert Item/name=e" || len(res.Refusals) != 3 {
		t.Fatalf("got %s / %+v", got, res.Refusals)
	}
}

// Under a DEFERRABLE constraint the change set checks the index at its end,
// so any order gets through, a circle's too: nothing is refused.
func TestARotationUnderADeferrableConstraintIsWritten(t *testing.T) {
	cfg := itemsConfig(t)
	table := &dbschema.Table{Uniques: [][]string{{"id"}, {"name"}, {"grp", "position"}},
		Deferrable: [][]string{{"grp", "position"}}}
	res := computeUnder(t, cfg, table, itemsInOrder, itemsRotated)
	if len(res.Refusals) != 0 || len(res.Warnings) != 0 || len(res.Changes) != 3 {
		t.Fatalf("got %s / %+v / %+v", kindsOf(res), res.Refusals, res.Warnings)
	}
	// A deferrable index orders nothing, not even a move a plain order
	// would get through: it is checked at the end.
	read := snap(t, cfg, itemsInOrder, "old")
	read.noteUniques("Item", table)
	u := uniqueIndexes(cfg, read)
	if !u.deferrable[indexName("Item", []string{"grp", "position"})] || u.deferrable[indexName("Item", []string{"name"})] {
		t.Fatalf("got %+v", u.deferrable)
	}
	// One catalog read that says otherwise, a constraint made deferrable
	// since, or a second index over the same columns that is not, keeps
	// the index checked after each statement.
	other := &dbschema.Table{Uniques: [][]string{{"id"}, {"name"}, {"grp", "position"}, {"grp", "position"}},
		Deferrable: [][]string{{"grp", "position"}}}
	if res := computeUnder(t, cfg, other, itemsInOrder, itemsRotated); len(res.Refusals) != 3 {
		t.Fatalf("expected the refusals, got %+v", res.Refusals)
	}
}

// Two indexes ordering the same two updates in opposite ways leave no order
// either: a takes b's code, and b takes a's slot.
func TestTwoUniqueIndexesOrderingInOppositeWaysAreRefused(t *testing.T) {
	cfg := itemsConfig(t)
	table := &dbschema.Table{Uniques: [][]string{{"id"}, {"name"}, {"code"}, {"slot"}}}
	res := computeUnder(t, cfg, table,
		"- model: Item\n  rows:\n    - {id: 1, name: a, code: x, slot: 1}\n    - {id: 2, name: b, code: y, slot: 2}\n",
		"- model: Item\n  rows:\n    - {id: 1, name: a, code: y, slot: 3}\n    - {id: 2, name: b, code: z, slot: 1}\n")
	if len(res.Changes) != 0 || len(res.Refusals) != 2 ||
		!strings.Contains(res.Refusals[0].Reason, "the unique indexes on code and slot are checked after every statement") {
		t.Fatalf("got %s / %+v", kindsOf(res), res.Refusals)
	}
}

// Where the files do not write every column of the index, the rows may not
// share what they leave out, a tenant say, and the circle may be none: a
// warning, and the updates are written.
func TestACircleThroughAColumnTheFilesDoNotWriteIsAWarning(t *testing.T) {
	cfg := itemsConfig(t)
	table := &dbschema.Table{Uniques: [][]string{{"id"}, {"name"}, {"tenant", "position"}}}
	res := computeUnder(t, cfg, table,
		"- model: Item\n  rows:\n    - {id: 1, name: a, position: 1}\n    - {id: 2, name: b, position: 2}\n",
		"- model: Item\n  rows:\n    - {id: 1, name: a, position: 2}\n    - {id: 2, name: b, position: 1}\n")
	if len(res.Refusals) != 0 || len(res.Changes) != 2 || len(res.Warnings) != 1 ||
		!strings.Contains(res.Warnings[0].Reason, "they do not write every column of it") {
		t.Fatalf("got %s / %+v / %+v", kindsOf(res), res.Refusals, res.Warnings)
	}
}

// Without the database, which columns are unique is a guess: a rotation is a
// warning that says so, and the updates are written.
func TestARotationWithoutTheCatalogIsAWarning(t *testing.T) {
	cfg := itemsConfig(t)
	res := computeWith(t, cfg, itemsInOrder, itemsRotated)
	if len(res.Refusals) != 0 || len(res.Changes) != 3 || len(res.Warnings) != 1 {
		t.Fatalf("got %s / %+v / %+v", kindsOf(res), res.Refusals, res.Warnings)
	}
	if w := res.Warnings[0].String(); !strings.Contains(w, "Item: Item/name=a, Item/name=b and Item/name=c trade their "+
		"values of position in a circle") || !strings.Contains(w, "Run generate with the database configured") {
		t.Fatalf("got %s", w)
	}
}
