package fixturemigrate

import (
	"container/heap"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// Refusal is one difference the generator will not write.
type Refusal struct {
	Model  string
	Key    string
	Reason string
}

func (r Refusal) String() string { return r.Model + " " + r.Key + ": " + r.Reason }

// Result is what Compute found.
type Result struct {
	// Changes are in apply order: renames first, then deletes, then updates,
	// then inserts, then the updates that point at a row inserted here, with
	// every change after the ones it depends on, so no row is written before
	// the row it points at, none is removed while a row still points at it,
	// and a value one row gives up is free before another takes it. See
	// orderChanges.
	Changes []fixturechange.Change
	// Refusals are the differences that need a hand-written migration.
	Refusals []Refusal
	// Warnings are what the policy says to report and carry on with: a row
	// under another id than the one it had, with policy.id_drift set to warn.
	// The rest of such a row is migrated. Nothing in them stops a migration
	// from being written, or a sync from running.
	Warnings []Refusal
	// Tables covers every model a change touches or points at.
	Tables fixturechange.Tables
	// Order is the model order that was used: every model after the models
	// it points at, and otherwise in the order of the files.
	Order []string
	// Base and Head name the two snapshots, for the generated file's comment.
	Base, Head string
}

// Totals counts the changes by kind.
func (r *Result) Totals() (ins, upd, del int) {
	for _, c := range r.Changes {
		switch c.Kind {
		case fixturechange.Insert:
			ins++
		case fixturechange.Update:
			upd++
		case fixturechange.Delete:
			del++
		}
	}
	return
}

// Summary is one line per model, "Plan: 1 insert, 2 updates", in model order.
func (r *Result) Summary() []string {
	type counts struct{ ins, upd, del int }
	per := map[string]*counts{}
	for _, c := range r.Changes {
		if per[c.Model] == nil {
			per[c.Model] = &counts{}
		}
		switch c.Kind {
		case fixturechange.Insert:
			per[c.Model].ins++
		case fixturechange.Update:
			per[c.Model].upd++
		case fixturechange.Delete:
			per[c.Model].del++
		}
	}
	var out []string
	for _, model := range r.Order {
		k := per[model]
		if k == nil {
			continue
		}
		var parts []string
		if k.ins > 0 {
			parts = append(parts, plural(k.ins, "insert"))
		}
		if k.upd > 0 {
			parts = append(parts, plural(k.upd, "update"))
		}
		if k.del > 0 {
			parts = append(parts, plural(k.del, "delete"))
		}
		out = append(out, model+": "+strings.Join(parts, ", "))
	}
	return out
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func sameValue(a, b fixturechange.Value) bool {
	if a.IsNull != b.IsNull {
		return false
	}
	if (a.Ref == nil) != (b.Ref == nil) {
		return false
	}
	if a.Ref != nil {
		return a.Ref.Model == b.Ref.Model && a.Ref.Key == b.Ref.Key
	}
	// Both sides are already canonical: a string as written, a number in one
	// spelling (see scalarText and columnText).
	return a.Lit == b.Lit
}

// undecidedReason is why a value only the column's type can settle stops a
// change.
const undecidedReason = "which a column written from a Go string holds as written and any other column as the " +
	"value it resolves to, and only the column's type says which: with the database configured (and without " +
	"-no-lint) the tool reads the types and decides. Or write it so both agree: quoted for a string column; " +
	"for any other the way it resolves, as 1.1, 15, true or 2026-01-01T10:00:00Z"

// writtenAs is a column of an entry as resolved and as written: a literal, or
// for a reference the ref value it names its row by. ok is false for a column
// that is absent or NULL.
func writtenAs(m *Model, e *Entry, col string) (resolved, written string, ok bool) {
	if e == nil {
		return "", "", false
	}
	if col == m.ID {
		resolved = e.ID
	} else {
		v, present := e.Cells[col]
		switch {
		case !present || v.IsNull:
			return "", "", false
		case v.Ref != nil:
			resolved = v.Ref.Key
		default:
			resolved = v.Lit
		}
	}
	written = resolved
	if w, ok := e.AsWritten[col]; ok {
		written = w
	}
	return resolved, written, true
}

// undecidedColumns describes the columns of prev and cur, either of which
// may be nil, that are still in an entry's AsWritten and that keep reports
// true for.
func undecidedColumns(m *Model, prev, cur *Entry, keep func(col, prevResolved, curResolved string) bool) []string {
	var out []string
	seen := map[string]bool{}
	for _, e := range []*Entry{prev, cur} {
		if e == nil {
			continue
		}
		for col := range e.AsWritten {
			if seen[col] {
				continue
			}
			seen[col] = true
			pr, pw, okP := writtenAs(m, prev, col)
			cr, cw, okC := writtenAs(m, cur, col)
			if !keep(col, pr, cr) {
				continue
			}
			subject := col + " is"
			if v := e.Cells[col]; v.Ref != nil {
				subject = fmt.Sprintf("%s points at the %s whose %s is", col, v.Ref.Model, e.from[col].column)
			}
			switch {
			case okP && okC && pw != cw:
				out = append(out, fmt.Sprintf("%s written %s before and %s after", subject, pw, cw))
			case okP:
				out = append(out, fmt.Sprintf("%s written %s", subject, pw))
			case okC:
				out = append(out, fmt.Sprintf("%s written %s", subject, cw))
			}
		}
	}
	sort.Strings(out)
	return out
}

// undecided refuses a change that carries a value still in an entry's
// AsWritten: 1.10, 017, 0x1F, True, a timestamp. Canonicalize settles them
// when the database's columns are at hand; without them the migration would
// write one of two values, and could write the wrong one. label names the
// row in the refusal.
func undecided(label string, m *Model, c fixturechange.Change, prev, cur *Entry) (Refusal, bool) {
	cols := undecidedColumns(m, prev, cur, func(col, _, _ string) bool {
		_, inKey := c.Key[col]
		_, inOld := c.Old[col]
		_, inNew := c.New[col]
		return inKey || inOld || inNew
	})
	if len(cols) == 0 {
		return Refusal{}, false
	}
	return Refusal{c.Model, label, strings.Join(cols, "; ") + ", " + undecidedReason}, true
}

// respelled refuses a row whose value resolves the same on both sides but is
// written differently, 1.10 before and 1.1 after: no change for a number
// column, a change for a string column, and without the column's type there is
// no telling which.
func respelled(model string, m *Model, prev, cur *Entry) (Refusal, bool) {
	cols := undecidedColumns(m, prev, cur, func(col, pr, cr string) bool {
		_, pw, okP := writtenAs(m, prev, col)
		_, cw, okC := writtenAs(m, cur, col)
		return okP && okC && pr == cr && pw != cw
	})
	if len(cols) == 0 {
		return Refusal{}, false
	}
	return Refusal{model, cur.label(model), strings.Join(cols, "; ") + ", " + undecidedReason}, true
}

// Compute diffs two snapshots. Both sides are the same shape whether they came
// from a fixture file or from a database, so this one function serves the diff
// between two revisions of the file and the diff between the database and the
// file.
func Compute(cfg *Config, old, next *Snapshot) (*Result, error) {
	order, err := modelOrder(cfg, old, next)
	if err != nil {
		return nil, err
	}
	// An accepted rename rewrites the base state: the renamed row starts
	// carrying its new key, and every row that named it by the old one follows.
	// That happens on a copy. A caller that hands the same snapshot to two
	// comparisons — check reads the database once and could diff it against
	// more than one file — must get the same answer both times.
	old = old.clone()
	res := &Result{Tables: fixturechange.Tables{}, Order: order, Base: old.Source, Head: next.Source}
	var renames, inserts, updates, deletes []fixturechange.Change

	// First pass over every model: decide the identity questions before any
	// value is compared. A rename and a renumbering both make a whole row
	// disappear from the value diff, and a row in another model may point at
	// the renamed one, so all of them have to be known first.
	//
	// skipped holds, per model, the natural keys the value diff leaves alone;
	// renamed holds the reference values that are in the middle of a rename,
	// across all models, because a change in one model can point at a row of
	// another.
	skipped := map[string]map[string]bool{}
	renamed := map[string]bool{}
	shared := sharedRefs(cfg, old, next)
	for _, model := range order {
		skip := map[string]bool{}
		if err := identity(cfg, model, old, next, res, skip, renamed, shared, &renames); err != nil {
			return nil, err
		}
		skipped[model] = skip
	}
	// refused is every reason the diff has to leave a change out because of
	// what it points at, or the id it writes.
	refused := func(c fixturechange.Change) (Refusal, bool) {
		if r, ok := refusedByRename(renamed, c); ok {
			return r, true
		}
		if r, ok := refusedByShared(shared, c); ok {
			return r, true
		}
		return refusedBySharedID(cfg, next, c)
	}

	for _, model := range order {
		m := cfg.Models[model]
		skip := skipped[model]
		oldGroups, oldOrder := byKey(old.Entries[model])
		newGroups, newOrder := byKey(next.Entries[model])

		for _, k := range newOrder {
			if skip[k] {
				continue
			}
			cur, prev := newGroups[k], oldGroups[k]
			label := cur[0].label(model)
			// A key that is not unique cannot be turned into a WHERE clause
			// that hits the right row. As long as the group did not change
			// that costs nothing; once it does, it has to be hand-written.
			if len(cur) > 1 || len(prev) > 1 {
				if !sameRowSet(prev, cur) {
					res.Refusals = append(res.Refusals, Refusal{model, label, fmt.Sprintf(
						"the natural key is not unique (%s before, %s after) and the rows differ: "+
							"hand-write the migration, and give the table a unique index",
						plural(len(prev), "row"), plural(len(cur), "row"))})
				}
				continue
			}
			if len(prev) == 0 {
				change := fixturechange.Change{
					Model: model, Kind: fixturechange.Insert, Key: cur[0].Key, New: cur[0].Full(m)}
				if r, ok := refused(change); ok {
					res.Refusals = append(res.Refusals, r)
					continue
				}
				if r, ok := undecided(label, m, change, nil, cur[0]); ok {
					res.Refusals = append(res.Refusals, r)
					continue
				}
				inserts = append(inserts, change)
				continue
			}
			change, refusal := diffRow(model, prev[0], cur[0])
			if refusal != nil {
				res.Refusals = append(res.Refusals, *refusal)
				continue
			}
			if r, ok := respelled(model, m, prev[0], cur[0]); ok {
				res.Refusals = append(res.Refusals, r)
				continue
			}
			if change == nil {
				continue
			}
			if r, ok := refused(*change); ok {
				res.Refusals = append(res.Refusals, r)
				continue
			}
			if r, ok := undecided(label, m, *change, prev[0], cur[0]); ok {
				res.Refusals = append(res.Refusals, r)
				continue
			}
			updates = append(updates, *change)
		}

		for _, k := range oldOrder {
			if skip[k] {
				continue
			}
			if _, stillThere := newGroups[k]; stillThere {
				continue
			}
			prev := oldGroups[k]
			label := prev[0].label(model)
			if m.Deletes == DeleteRefuse {
				res.Refusals = append(res.Refusals, Refusal{model, label,
					"deletes of this model are refused by the configuration because other rows may point at it: " +
						"hand-write the migration"})
				continue
			}
			if len(prev) > 1 {
				res.Refusals = append(res.Refusals, Refusal{model, label, fmt.Sprintf(
					"delete of %s sharing one natural key: hand-write the migration", plural(len(prev), "row"))})
				continue
			}
			change := fixturechange.Change{
				Model: model, Kind: fixturechange.Delete, Key: prev[0].Key, Old: prev[0].Full(m)}
			if r, ok := refused(change); ok {
				res.Refusals = append(res.Refusals, r)
				continue
			}
			if r, ok := undecided(label, m, change, prev[0], nil); ok {
				res.Refusals = append(res.Refusals, r)
				continue
			}
			deletes = append(deletes, change)
		}
	}

	for i, j := 0, len(deletes)-1; i < j; i, j = i+1, j-1 {
		deletes[i], deletes[j] = deletes[j], deletes[i]
	}
	res.Changes = orderChanges(cfg, renames, deletes, updates, inserts)
	res.Tables = tablesFor(cfg, res.Changes)
	for _, list := range [][]Refusal{res.Refusals, res.Warnings} {
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].Model != list[j].Model {
				return list[i].Model < list[j].Model
			}
			return list[i].Key < list[j].Key
		})
	}
	return res, nil
}

// identity settles what happened to a row's name and its id before anything
// compares values.
//
// Two things can happen and neither is an ordinary update. A row can keep its
// id and change its natural key, which is a rename: written as an insert plus a
// delete it would break every row that points at the old one and everything
// outside the database that knows the old name. And a row can keep its natural
// key and change its id, which is a renumbering: live data points at the old id.
// Policy.Renames and Policy.IDDrift decide what to do with each; the rows
// involved are left out of the value diff either way, because their keys no
// longer line up.
func identity(cfg *Config, model string, old, next *Snapshot, res *Result,
	skip map[string]bool, renamed map[string]bool, shared map[string][]string, renames *[]fixturechange.Change) error {

	m := cfg.Models[model]
	// An id two rows of one snapshot share names neither of them: it is a
	// fault of the file (FindingDuplicateID), not a rename or a renumbering,
	// and which of the two came first in the file decides nothing.
	sharedID := map[string]bool{}
	for _, snap := range []*Snapshot{old, next} {
		seen := map[string]bool{}
		for _, e := range snap.Entries[model] {
			if e.ID != "" && seen[e.ID] {
				sharedID[e.ID] = true
			}
			seen[e.ID] = true
		}
	}
	byID := func(entries []*Entry) map[string]*Entry {
		out := map[string]*Entry{}
		for _, e := range entries {
			if e.ID != "" && !sharedID[e.ID] {
				out[e.ID] = e
			}
		}
		return out
	}
	newByID := byID(next.Entries[model])
	oldByKey := map[string]*Entry{}
	for _, e := range old.Entries[model] {
		if _, dup := oldByKey[e.KeyStr]; !dup {
			oldByKey[e.KeyStr] = e
		}
	}

	for _, prev := range old.Entries[model] {
		if prev.ID == "" || sharedID[prev.ID] {
			continue
		}
		cur, ok := newByID[prev.ID]
		if !ok || cur.KeyStr == prev.KeyStr {
			continue
		}
		if keyString(model, cur.Key) == keyString(model, prev.Key) {
			// Only the spelling of a key value changed, 0012 to 012: the
			// same key in a numeric column and a rename in a text one.
			skip[prev.KeyStr], skip[cur.KeyStr] = true, true
			if r, ok := respelled(model, m, prev, cur); ok {
				res.Refusals = append(res.Refusals, r)
			}
			continue
		}
		// A rename into a name another row still holds cannot be written in
		// any order this tool can work out: two rows swapping names need one
		// of them parked somewhere first.
		occupied := false
		if other, ok := oldByKey[cur.KeyStr]; ok && other.ID != prev.ID {
			occupied = true
		}
		if cfg.Policy.Renames == RenameUpdate && !occupied {
			change, err := renameChange(m, model, prev, cur)
			if err != nil {
				return err
			}
			if r, ok := refusedByShared(shared, change); ok {
				res.Refusals = append(res.Refusals, r)
				skip[prev.KeyStr], skip[cur.KeyStr] = true, true
				for _, e := range []*Entry{prev, cur} {
					if v := e.refValue(m); v != "" {
						renamed[model+"\x00"+v] = true
					}
				}
				continue
			}
			*renames = append(*renames, change)
			// Everything that points at this row named it by its old value.
			// The id behind that name does not change, so no dependent row
			// needs a statement of its own; what it needs is for the base
			// state to stop calling the row by a name that will not exist by
			// the time the rest of the migration runs.
			if from, to := prev.refValue(m), cur.refValue(m); from != to && from != "" && to != "" {
				rewriteRefs(cfg, old, model, from, to)
			}
			// The rename covers the key columns. The base row now carries the
			// new key, so the ordinary diff lines the two up and writes
			// whatever else about the row changed.
			for col := range cur.Key {
				if v, ok := cur.Cells[col]; ok {
					prev.Cells[col] = v
				}
			}
			prev.Key, prev.KeyStr = cur.Key, cur.KeyStr
			continue
		}
		skip[prev.KeyStr] = true
		skip[cur.KeyStr] = true
		for _, e := range []*Entry{prev, cur} {
			if v := e.refValue(m); v != "" {
				renamed[model+"\x00"+v] = true
			}
		}
		if occupied && cfg.Policy.Renames == RenameUpdate {
			res.Refusals = append(res.Refusals, Refusal{model, m.ID + " " + prev.ID, fmt.Sprintf(
				"renamed from %s to %s, but another row still holds %s in the base state. Two rows cannot swap "+
					"names in one step: park one of them under a third name first, in a migration of its own",
				prev.label(model), cur.label(model), cur.label(model))})
			continue
		}
		res.Refusals = append(res.Refusals, Refusal{model, m.ID + " " + prev.ID, fmt.Sprintf(
			"renamed from %s to %s. An insert plus a delete is not a rename: rows elsewhere point at this one "+
				"and so does whatever knows the old name outside the database. Hand-write the migration, or set "+
				"policy.renames to update and run this again", prev.label(model), cur.label(model))})
	}

	if cfg.Policy.IDDrift == ModeIgnore {
		return nil
	}
	newByKey := map[string]*Entry{}
	for _, e := range next.Entries[model] {
		if _, dup := newByKey[e.KeyStr]; !dup {
			newByKey[e.KeyStr] = e
		}
	}
	for _, prev := range old.Entries[model] {
		if skip[prev.KeyStr] || prev.ID == "" || sharedID[prev.ID] {
			continue
		}
		cur, ok := newByKey[prev.KeyStr]
		if !ok || cur.ID == "" || cur.ID == prev.ID || sharedID[cur.ID] {
			continue
		}
		if cfg.Policy.IDDrift == ModeWarn {
			// The row still gets its value diff; only the id is left alone,
			// which an update never writes anyway.
			res.Warnings = append(res.Warnings, Refusal{model, prev.label(model), fmt.Sprintf(
				"its %s changed from %s to %s. This tool does not renumber a primary key, so wherever the row "+
					"is it keeps the %s it has; policy.id_drift is warn, so the rest of the row is migrated",
				m.ID, prev.ID, cur.ID, m.ID)})
			continue
		}
		res.Refusals = append(res.Refusals, Refusal{model, prev.label(model), fmt.Sprintf(
			"its %s changed from %s to %s. This tool does not renumber a primary key: live data points at %s, "+
				"and so does anything outside the database that was given an id. Put %s back, or set "+
				"policy.id_drift to warn if nothing outside this database names these ids",
			m.ID, prev.ID, cur.ID, prev.ID, prev.ID)})
		skip[prev.KeyStr] = true
	}
	return nil
}

// rewriteRefs renames a row inside a snapshot: every reference to it, and
// every natural key made out of one, starts calling it by its new name.
func rewriteRefs(cfg *Config, snap *Snapshot, model, from, to string) {
	for _, other := range snap.Order {
		m := cfg.Models[other]
		if m == nil {
			continue
		}
		for _, e := range snap.Entries[other] {
			touched := false
			for col, v := range e.Cells {
				if v.Ref != nil && v.Ref.Model == model && v.Ref.Key == from {
					e.Cells[col] = fixturechange.RefTo(model, to)
					touched = true
				}
			}
			if !touched {
				continue
			}
			if key, err := keyOf(cfg, m, other, e.Full(m)); err == nil {
				e.setKey(other, key)
			}
		}
	}
}

// refValue is the value a reference to this row carries: its ref column, or
// its id when the ref column is the id, and "" when it has none.
func (e *Entry) refValue(m *Model) string {
	if m.Ref == m.ID {
		return e.ID
	}
	v, ok := e.Cells[m.Ref]
	if !ok || v.Ref != nil || v.IsNull {
		return ""
	}
	return v.Lit
}

// renameChange writes a rename as what it is: an update of the key columns,
// guarded by the id as well as by the old key, so it cannot land on a row that
// merely happens to carry the old name.
func renameChange(m *Model, model string, prev, cur *Entry) (fixturechange.Change, error) {
	if prev.ID == "" {
		return fixturechange.Change{}, fmt.Errorf("%s %s: a rename needs the row's %s", model, prev.label(model), m.ID)
	}
	oldVals, newVals := fixturechange.Values{}, fixturechange.Values{}
	for _, col := range sortedColumns(cur.Key) {
		ov, hadOld := prev.Key[col]
		nv := cur.Key[col]
		if hadOld && sameValue(ov, nv) {
			continue
		}
		if !hadOld {
			ov = fixturechange.Null()
		}
		oldVals[col], newVals[col] = ov, nv
	}
	// A key_any_of group can move the key to a different column, which leaves
	// the old one holding a value nothing clears.
	for _, col := range sortedColumns(prev.Key) {
		if _, ok := cur.Key[col]; ok {
			continue
		}
		return fixturechange.Change{}, fmt.Errorf(
			"%s %s: the natural key moved from column %q to another column; hand-write this one",
			model, prev.label(model), col)
	}
	return fixturechange.Change{
		Model: model, Kind: fixturechange.Update, Key: prev.Key, ID: prev.ID,
		Old: oldVals, New: newVals}, nil
}

// modelOrder is the order changes are made in, model by model: every model
// after the models it points at, so an insert finds the row it names, and in
// reverse for deletes, so a row goes before the row it points at. Of the
// models free to go next, the one that comes first in the new snapshot, then
// in the old one, does: the file's own order wherever that order works. A
// model only the old snapshot has -- every row of it deleted -- still goes
// before the models pointing at it, and so its deletes after theirs.
//
// Models pointing at each other in a circle cannot be put in such an order;
// the one first in the files goes first, and orderChanges sorts out the rows.
func modelOrder(cfg *Config, old, next *Snapshot) ([]string, error) {
	var files []string
	position := map[string]int{}
	for _, snap := range []*Snapshot{next, old} {
		for _, model := range snap.Order {
			if _, err := cfg.model(model); err != nil {
				return nil, err
			}
			if _, seen := position[model]; seen {
				continue
			}
			position[model] = len(files)
			files = append(files, model)
		}
	}
	waiting := make([]int, len(files))
	pointedAt := make([][]int, len(files))
	for i, model := range files {
		m := cfg.Models[model]
		targets := map[int]bool{}
		for col, target := range m.References {
			if j, ok := position[target]; ok && target != model && !m.skip(col) {
				targets[j] = true
			}
		}
		for j := range targets {
			waiting[i]++
			pointedAt[j] = append(pointedAt[j], i)
		}
	}
	var order []string
	for _, i := range topological(waiting, pointedAt, func(i int) int { return i }) {
		order = append(order, files[i])
	}
	return order, nil
}

// topological sorts the nodes 0..n-1 of a graph so every node comes after the
// nodes it waits for: waiting[i] is how many it waits for, next[i] the nodes
// waiting for i. Of the nodes whose wait is over, the one of lowest rank goes
// next. In a circle nothing's wait is ever over, and then the lowest-ranked
// node left goes anyway, so the result is always every node once.
func topological(waiting []int, next [][]int, rank func(int) int) []int {
	waiting = append([]int(nil), waiting...)
	ready := &rankedHeap{rank: rank}
	for i, w := range waiting {
		if w == 0 {
			heap.Push(ready, i)
		}
	}
	done := make([]bool, len(waiting))
	out := make([]int, 0, len(waiting))
	for len(out) < len(waiting) {
		if ready.Len() == 0 {
			stuck := -1
			for i := range waiting {
				if !done[i] && (stuck < 0 || rank(i) < rank(stuck)) {
					stuck = i
				}
			}
			waiting[stuck] = 0
			heap.Push(ready, stuck)
		}
		i := heap.Pop(ready).(int)
		if done[i] {
			continue
		}
		done[i] = true
		out = append(out, i)
		for _, j := range next[i] {
			if waiting[j]--; waiting[j] == 0 && !done[j] {
				heap.Push(ready, j)
			}
		}
	}
	return out
}

// rankedHeap is a min-heap of nodes by rank, and by number between equal ranks.
type rankedHeap struct {
	nodes []int
	rank  func(int) int
}

func (h *rankedHeap) Len() int { return len(h.nodes) }
func (h *rankedHeap) Less(i, j int) bool {
	a, b := h.nodes[i], h.nodes[j]
	if ra, rb := h.rank(a), h.rank(b); ra != rb {
		return ra < rb
	}
	return a < b
}
func (h *rankedHeap) Swap(i, j int) { h.nodes[i], h.nodes[j] = h.nodes[j], h.nodes[i] }
func (h *rankedHeap) Push(x any)    { h.nodes = append(h.nodes, x.(int)) }
func (h *rankedHeap) Pop() any {
	x := h.nodes[len(h.nodes)-1]
	h.nodes = h.nodes[:len(h.nodes)-1]
	return x
}

// orderChanges puts a change set in an order the database accepts.
//
// The base order is renames, then deletes, then updates, then inserts, and
// last the updates that point at a row inserted in the same set. A delete or
// an update can free what an insert takes: a value of a unique column, or the
// open end of a price that an exclusion constraint or a partial unique index
// allows only once, so the old price has to be closed before the new one is
// opened. Inserts follow the model order and the file's row order, deletes the
// reverse of both.
//
// On top of that, a change waits for every change it depends on:
//
//   - a change naming a row that another change inserts, or renames into
//     that name, waits for it: parents before children, in one model too;
//   - a change removing a row, or the name a row is found by, waits for
//     every change that still names it in its key or its old values: the
//     child deleted or moved elsewhere goes first;
//   - a change taking a value of a column that another change of the same
//     model gives up waits for it, so a unique value can move from one row to
//     another in one set. Nothing here knows which columns are unique; a
//     column whose values would have to wait for each other in a circle, two
//     rows trading values, cannot be one, and waits for nothing.
//
// Of the changes whose wait is over, the one first in the base order goes
// next, so the base order stands wherever nothing forces another.
func orderChanges(cfg *Config, renames, deletes, updates, inserts []fixturechange.Change) []fixturechange.Change {
	refOf := func(model string, values fixturechange.Values) (string, bool) {
		v, ok := values[cfg.Models[model].Ref]
		if !ok || v.Ref != nil || v.IsNull {
			return "", false
		}
		return model + "\x00" + v.Lit, true
	}
	refsIn := func(sets ...fixturechange.Values) []string {
		var out []string
		for _, values := range sets {
			for _, col := range sortedColumns(values) {
				if ref := values[col].Ref; ref != nil {
					out = append(out, ref.Model+"\x00"+ref.Key)
				}
			}
		}
		return out
	}

	inserted := map[string]bool{}
	for _, c := range inserts {
		if ref, ok := refOf(c.Model, c.New); ok {
			inserted[ref] = true
		}
	}
	var early, late []fixturechange.Change
	for _, c := range updates {
		later := false
		for _, ref := range refsIn(c.New) {
			later = later || inserted[ref]
		}
		if later {
			late = append(late, c)
		} else {
			early = append(early, c)
		}
	}
	var changes []fixturechange.Change
	for _, part := range [][]fixturechange.Change{renames, deletes, early, inserts, late} {
		changes = append(changes, part...)
	}
	n := len(changes)

	// What each change gives a name to, and takes one away from.
	created, removed := map[string][]int{}, map[string][]int{}
	for i, c := range changes {
		switch c.Kind {
		case fixturechange.Insert:
			if ref, ok := refOf(c.Model, c.New); ok {
				created[ref] = append(created[ref], i)
			}
		case fixturechange.Update:
			if ref, ok := refOf(c.Model, c.New); ok {
				created[ref] = append(created[ref], i)
				if ref, ok := refOf(c.Model, c.Old); ok {
					removed[ref] = append(removed[ref], i)
				}
			}
		case fixturechange.Delete:
			if ref, ok := refOf(c.Model, c.Old); ok {
				removed[ref] = append(removed[ref], i)
			}
		}
	}
	type edge struct{ from, to int }
	var edges []edge
	for j, c := range changes {
		for _, ref := range refsIn(c.Key, c.Old, c.New) {
			for _, i := range created[ref] {
				if i != j {
					edges = append(edges, edge{i, j})
				}
			}
		}
		for _, ref := range refsIn(c.Key, c.Old) {
			for _, i := range removed[ref] {
				if i != j {
					edges = append(edges, edge{j, i})
				}
			}
		}
	}

	// The values each change gives up and takes, per model and column. A
	// value is a node of its own between the changes freeing and taking it,
	// which keeps the edges to one per change and value.
	type column struct{ model, col string }
	type moves struct{ freed, taken map[string][]int }
	byColumn := map[column]*moves{}
	note := func(c fixturechange.Change, col string, v fixturechange.Value, i int, free bool) {
		if v.IsNull {
			return
		}
		k := column{c.Model, col}
		if byColumn[k] == nil {
			byColumn[k] = &moves{freed: map[string][]int{}, taken: map[string][]int{}}
		}
		if free {
			byColumn[k].freed[valueKey(v)] = append(byColumn[k].freed[valueKey(v)], i)
		} else {
			byColumn[k].taken[valueKey(v)] = append(byColumn[k].taken[valueKey(v)], i)
		}
	}
	for i, c := range changes {
		switch c.Kind {
		case fixturechange.Insert:
			for col, v := range c.New {
				note(c, col, v, i, false)
			}
		case fixturechange.Update:
			for col, v := range c.New {
				note(c, col, c.Old[col], i, true)
				note(c, col, v, i, false)
			}
		case fixturechange.Delete:
			for col, v := range c.Old {
				note(c, col, v, i, true)
			}
		}
	}
	columns := make([]column, 0, len(byColumn))
	for k := range byColumn {
		columns = append(columns, k)
	}
	sort.Slice(columns, func(i, j int) bool {
		if columns[i].model != columns[j].model {
			return columns[i].model < columns[j].model
		}
		return columns[i].col < columns[j].col
	})
	nodes := n
	graph := func(edges []edge) ([]int, [][]int) {
		waiting, next := make([]int, nodes), make([][]int, nodes)
		for _, e := range edges {
			waiting[e.to]++
			next[e.from] = append(next[e.from], e.to)
		}
		return waiting, next
	}
	circular := func(edges []edge) bool {
		waiting, next := graph(edges)
		var ready []int
		for i, w := range waiting {
			if w == 0 {
				ready = append(ready, i)
			}
		}
		seen := 0
		for len(ready) > 0 {
			i := ready[len(ready)-1]
			ready = ready[:len(ready)-1]
			seen++
			for _, j := range next[i] {
				if waiting[j]--; waiting[j] == 0 {
					ready = append(ready, j)
				}
			}
		}
		return seen < nodes
	}
	for _, k := range columns {
		mv := byColumn[k]
		var more []edge
		for _, value := range sortedKeysOfInts(mv.freed) {
			taken := mv.taken[value]
			if len(taken) == 0 {
				continue
			}
			via := nodes
			nodes++
			for _, i := range mv.freed[value] {
				more = append(more, edge{i, via})
			}
			for _, j := range taken {
				more = append(more, edge{via, j})
			}
		}
		if len(more) == 0 {
			continue
		}
		if candidate := append(append([]edge(nil), edges...), more...); !circular(candidate) {
			edges = candidate
		}
	}

	waiting, next := graph(edges)
	rank := func(i int) int {
		if i >= n {
			// A value between two changes: on its way as soon as it is free.
			return -1
		}
		return i
	}
	out := make([]fixturechange.Change, 0, n)
	for _, i := range topological(waiting, next, rank) {
		if i < n {
			out = append(out, changes[i])
		}
	}
	return out
}

func sortedKeysOfInts(m map[string][]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// refusedByRename reports a change that points at a row whose rename was
// refused. Until that rename is written by hand, neither name is where the
// change expects it.
func refusedByRename(renamed map[string]bool, c fixturechange.Change) (Refusal, bool) {
	if len(renamed) == 0 {
		return Refusal{}, false
	}
	for _, values := range []fixturechange.Values{c.Key, c.Old, c.New} {
		for _, col := range sortedColumns(values) {
			ref := values[col].Ref
			if ref == nil || !renamed[ref.Model+"\x00"+ref.Key] {
				continue
			}
			return Refusal{c.Model, keyLabel(c.Model, c.Key), fmt.Sprintf(
				"it points at %s %q, whose rename was refused above: write that migration first, then run this again",
				ref.Model, ref.Key)}, true
		}
	}
	return Refusal{}, false
}

// sharedRefs lists, for every ref value more than one row of a model holds in
// either snapshot, those rows, by "Model\x00value". A value still read two
// ways (AsWritten) is shared only by rows that read the same both ways:
// without the column's type, 0012 and "10" are not known to be one value,
// and a reference to either is refused for that reason instead.
func sharedRefs(cfg *Config, snaps ...*Snapshot) map[string][]string {
	out := map[string][]string{}
	for _, snap := range snaps {
		for _, model := range snap.Order {
			m := cfg.Models[model]
			holders := map[[2]string][]string{}
			for _, e := range snap.Entries[model] {
				v := e.refValue(m)
				if v == "" {
					continue
				}
				written, ok := e.AsWritten[m.Ref]
				if !ok {
					written = v
				}
				holders[[2]string{v, written}] = append(holders[[2]string{v, written}], e.label(model))
			}
			readings := make([][2]string, 0, len(holders))
			for v := range holders {
				readings = append(readings, v)
			}
			sort.Slice(readings, func(i, j int) bool {
				return readings[i][0]+"\x00"+readings[i][1] < readings[j][0]+"\x00"+readings[j][1]
			})
			for _, v := range readings {
				if rows := holders[v]; len(rows) > 1 && len(rows) > len(out[model+"\x00"+v[0]]) {
					out[model+"\x00"+v[0]] = rows
				}
			}
		}
	}
	return out
}

// refusedByShared reports a change that points at a row by a ref value more
// than one row holds. A migration finds the row a reference names with
// "WHERE <ref> = ?", which would match all of them: categories named
// "Accessories" under two parents, say. Nothing the change could carry says
// which one is meant.
func refusedByShared(shared map[string][]string, c fixturechange.Change) (Refusal, bool) {
	if len(shared) == 0 {
		return Refusal{}, false
	}
	for _, values := range []fixturechange.Values{c.Key, c.Old, c.New} {
		for _, col := range sortedColumns(values) {
			ref := values[col].Ref
			if ref == nil {
				continue
			}
			rows := shared[ref.Model+"\x00"+ref.Key]
			if len(rows) == 0 {
				continue
			}
			return Refusal{c.Model, keyLabel(c.Model, c.Key), fmt.Sprintf(
				"%s points at %s %q, which %s hold (%s). A migration finds the row a reference names by that "+
					"value alone, so it cannot tell them apart: make the ref column of %s unique, or hand-write "+
					"this change", col, ref.Model, ref.Key, plural(len(rows), "row"), strings.Join(rows, ", "), ref.Model)}, true
		}
	}
	return Refusal{}, false
}

// refusedBySharedID reports an insert writing an id another row of the new
// snapshot has too, which two branches each adding "the next id" leave
// behind. Neither row can be inserted under it, and dbfixture cannot load the
// file at all.
func refusedBySharedID(cfg *Config, next *Snapshot, c fixturechange.Change) (Refusal, bool) {
	m := cfg.Models[c.Model]
	id, ok := c.New[m.ID]
	if c.Kind != fixturechange.Insert || !ok || id.Ref != nil || id.IsNull {
		return Refusal{}, false
	}
	var others []string
	for _, e := range next.Entries[c.Model] {
		if e.ID == id.Lit && keyString(c.Model, e.Key) != keyString(c.Model, c.Key) {
			others = append(others, e.label(c.Model))
		}
	}
	if len(others) == 0 {
		return Refusal{}, false
	}
	return Refusal{c.Model, keyLabel(c.Model, c.Key), fmt.Sprintf(
		"its %s, %s, is the %s of %s too. dbfixture cannot load a file in which two rows share one, and "+
			"a migration cannot insert both: give each row its own %s", m.ID, id.Lit, m.ID,
		strings.Join(others, ", "), m.ID)}, true
}

// diffRow compares two revisions of one row. A column that is spelled out on
// one side and missing on the other with no configured default is refused: the
// generator would have to invent what the missing one means.
func diffRow(model string, prev, cur *Entry) (*fixturechange.Change, *Refusal) {
	oldVals, newVals := fixturechange.Values{}, fixturechange.Values{}
	cols := map[string]bool{}
	for col := range prev.Cells {
		cols[col] = true
	}
	for col := range cur.Cells {
		cols[col] = true
	}
	names := make([]string, 0, len(cols))
	for col := range cols {
		names = append(names, col)
	}
	sort.Strings(names)
	for _, col := range names {
		ov, oldPresent := prev.Cells[col]
		nv, newPresent := cur.Cells[col]
		if oldPresent != newPresent {
			return nil, &Refusal{model, cur.label(model), fmt.Sprintf(
				"column %q is written on one side and left out on the other, and has no entry in defaults: "+
					"say what an omitted %q means and run this again", col, col)}
		}
		if sameValue(ov, nv) {
			continue
		}
		oldVals[col], newVals[col] = ov, nv
	}
	if len(newVals) == 0 {
		return nil, nil
	}
	return &fixturechange.Change{
		Model: model, Kind: fixturechange.Update, Key: cur.Key, Old: oldVals, New: newVals}, nil
}

// sameRowSet compares two groups of rows that share one natural key, as
// multisets.
func sameRowSet(old, cur []*Entry) bool {
	signatures := func(entries []*Entry) []string {
		out := make([]string, 0, len(entries))
		for _, e := range entries {
			var parts []string
			for _, col := range sortedColumns(e.Cells) {
				parts = append(parts, strconv.Quote(col)+"="+valueKey(e.Cells[col]))
			}
			out = append(out, strings.Join(parts, " "))
		}
		sort.Strings(out)
		return out
	}
	a, b := signatures(old), signatures(cur)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// tablesFor collects the models the change set touches and the models it points
// at, so the generated file carries nothing it does not need.
func tablesFor(cfg *Config, changes []fixturechange.Change) fixturechange.Tables {
	out := fixturechange.Tables{}
	add := func(model string, referenced bool) {
		m := cfg.Models[model]
		t := fixturechange.Table{Name: m.Table, ID: m.ID, Serial: m.Serial, Cascade: m.Deletes == DeleteCascade}
		if referenced {
			t.Key = m.Ref
		} else if have, ok := out[model]; ok {
			t.Key = have.Key
		}
		out[model] = t
	}
	for _, c := range changes {
		add(c.Model, false)
		for _, values := range []fixturechange.Values{c.Key, c.Old, c.New} {
			for _, v := range values {
				if v.Ref != nil {
					add(v.Ref.Model, true)
				}
			}
		}
	}
	return out
}
