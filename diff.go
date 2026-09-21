package fixturemigrate

import (
	"fmt"
	"sort"
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
	// Changes are in apply order: renames first, then inserts in model order,
	// then updates, then deletes in reverse model order, so no row is written
	// before the row it points at and none is removed before the rows that
	// point at it.
	Changes []fixturechange.Change
	// Refusals are the differences that need a hand-written migration.
	Refusals []Refusal
	// Tables covers every model a change touches or points at.
	Tables fixturechange.Tables
	// Order is the model order that was used.
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
	return normalize(a.Lit) == normalize(b.Lit)
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
	res := &Result{Tables: fixturechange.Tables{}, Order: order, Base: old.Source, Head: next.Source}
	var renames, inserts, updates, deletes []fixturechange.Change

	// First pass over every model: decide the identity questions before any
	// value is compared. A rename and a renumbering both make a whole row
	// disappear from the value diff, and a row in another model may point at
	// the renamed one, so all of them have to be known first.
	type state struct {
		skip    map[string]bool
		renamed map[string]bool // ref values that are in the middle of a rename
	}
	states := map[string]*state{}
	renamed := map[string]bool{}
	for _, model := range order {
		st := &state{skip: map[string]bool{}}
		if err := identity(cfg, model, old, next, res, st.skip, renamed, &renames); err != nil {
			return nil, err
		}
		states[model] = st
	}

	for _, model := range order {
		m := cfg.Models[model]
		skip := states[model].skip
		oldGroups, oldOrder := byKey(old.Entries[model])
		newGroups, newOrder := byKey(next.Entries[model])

		for _, k := range newOrder {
			if skip[k] {
				continue
			}
			cur, prev := newGroups[k], oldGroups[k]
			// A key that is not unique cannot be turned into a WHERE clause
			// that hits the right row. As long as the group did not change
			// that costs nothing; once it does, it has to be hand-written.
			if len(cur) > 1 || len(prev) > 1 {
				if !sameRowSet(m, prev, cur) {
					res.Refusals = append(res.Refusals, Refusal{model, k, fmt.Sprintf(
						"the natural key is not unique (%s before, %s after) and the rows differ: "+
							"hand-write the migration, and give the table a unique index",
						plural(len(prev), "row"), plural(len(cur), "row"))})
				}
				continue
			}
			if len(prev) == 0 {
				change := fixturechange.Change{
					Model: model, Kind: fixturechange.Insert, Key: cur[0].Key, New: cur[0].Full(m)}
				if r, ok := refusedByRename(renamed, change); ok {
					res.Refusals = append(res.Refusals, r)
					continue
				}
				inserts = append(inserts, change)
				continue
			}
			change, refusal := diffRow(m, model, prev[0], cur[0])
			if refusal != nil {
				res.Refusals = append(res.Refusals, *refusal)
				continue
			}
			if change == nil {
				continue
			}
			if r, ok := refusedByRename(renamed, *change); ok {
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
			if m.Deletes == DeleteRefuse {
				res.Refusals = append(res.Refusals, Refusal{model, k,
					"deletes of this model are refused by the configuration because other rows may point at it: " +
						"hand-write the migration"})
				continue
			}
			if len(prev) > 1 {
				res.Refusals = append(res.Refusals, Refusal{model, k, fmt.Sprintf(
					"delete of %s sharing one natural key: hand-write the migration", plural(len(prev), "row"))})
				continue
			}
			change := fixturechange.Change{
				Model: model, Kind: fixturechange.Delete, Key: prev[0].Key, Old: prev[0].Full(m)}
			if r, ok := refusedByRename(renamed, change); ok {
				res.Refusals = append(res.Refusals, r)
				continue
			}
			deletes = append(deletes, change)
		}
	}

	for i, j := 0, len(deletes)-1; i < j; i, j = i+1, j-1 {
		deletes[i], deletes[j] = deletes[j], deletes[i]
	}
	res.Changes = nil
	for _, part := range [][]fixturechange.Change{renames, inserts, updates, deletes} {
		res.Changes = append(res.Changes, part...)
	}
	res.Tables = tablesFor(cfg, res.Changes)
	sort.SliceStable(res.Refusals, func(i, j int) bool {
		if res.Refusals[i].Model != res.Refusals[j].Model {
			return res.Refusals[i].Model < res.Refusals[j].Model
		}
		return res.Refusals[i].Key < res.Refusals[j].Key
	})
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
	skip map[string]bool, renamed map[string]bool, renames *[]fixturechange.Change) error {

	m := cfg.Models[model]
	byID := func(entries []*Entry) map[string]*Entry {
		out := map[string]*Entry{}
		for _, e := range entries {
			if e.ID == "" {
				continue
			}
			if _, dup := out[e.ID]; !dup {
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
		if prev.ID == "" {
			continue
		}
		cur, ok := newByID[prev.ID]
		if !ok || cur.KeyStr == prev.KeyStr {
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
				prev.KeyStr, cur.KeyStr, cur.KeyStr)})
			continue
		}
		res.Refusals = append(res.Refusals, Refusal{model, m.ID + " " + prev.ID, fmt.Sprintf(
			"renamed from %s to %s. An insert plus a delete is not a rename: rows elsewhere point at this one "+
				"and so does whatever knows the old name outside the database. Hand-write the migration, or set "+
				"policy.renames to update and run this again", prev.KeyStr, cur.KeyStr)})
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
		if skip[prev.KeyStr] || prev.ID == "" {
			continue
		}
		cur, ok := newByKey[prev.KeyStr]
		if !ok || cur.ID == "" || cur.ID == prev.ID {
			continue
		}
		reason := fmt.Sprintf(
			"its %s changed from %s to %s. This tool does not renumber a primary key: live data points at %s, "+
				"and so does anything outside the database that was given an id. Put %s back, or set "+
				"policy.id_drift to warn if nothing outside this database names these ids",
			m.ID, prev.ID, cur.ID, prev.ID, prev.ID)
		if cfg.Policy.IDDrift == ModeWarn {
			// The row still gets its value diff; only the id is left alone,
			// which an update never writes anyway.
			res.Refusals = append(res.Refusals, Refusal{model, prev.KeyStr, "warning: " + reason})
			continue
		}
		res.Refusals = append(res.Refusals, Refusal{model, prev.KeyStr, reason})
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
			if key, err := keyOf(cfg, m, other, e.Cells); err == nil {
				e.Key, e.KeyStr = key, keyString(other, key)
			}
		}
	}
}

// refValue is the value a reference to this row carries.
func (e *Entry) refValue(m *Model) string {
	v, ok := e.Cells[m.Ref]
	if !ok || v.Ref != nil {
		return ""
	}
	return v.Lit
}

// renameChange writes a rename as what it is: an update of the key columns,
// guarded by the id as well as by the old key, so it cannot land on a row that
// merely happens to carry the old name.
func renameChange(m *Model, model string, prev, cur *Entry) (fixturechange.Change, error) {
	if prev.ID == "" {
		return fixturechange.Change{}, fmt.Errorf("%s %s: a rename needs the row's %s", model, prev.KeyStr, m.ID)
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
			model, prev.KeyStr, col)
	}
	return fixturechange.Change{
		Model: model, Kind: fixturechange.Update, Key: prev.Key, ID: prev.ID,
		Old: oldVals, New: newVals}, nil
}

// modelOrder is the order the models appear in the new snapshot, with models
// only the old one had appended.
func modelOrder(cfg *Config, old, next *Snapshot) ([]string, error) {
	var order []string
	seen := map[string]bool{}
	for _, snap := range []*Snapshot{next, old} {
		for _, model := range snap.Order {
			if _, err := cfg.model(model); err != nil {
				return nil, err
			}
			if seen[model] {
				continue
			}
			seen[model] = true
			order = append(order, model)
		}
	}
	return order, nil
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
			return Refusal{c.Model, keyString(c.Model, c.Key), fmt.Sprintf(
				"it points at %s %q, whose rename was refused above: write that migration first, then run this again",
				ref.Model, ref.Key)}, true
		}
	}
	return Refusal{}, false
}

// diffRow compares two revisions of one row. A column that is spelled out on
// one side and missing on the other with no configured default is refused: the
// generator would have to invent what the missing one means.
func diffRow(m *Model, model string, prev, cur *Entry) (*fixturechange.Change, *Refusal) {
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
			return nil, &Refusal{model, cur.KeyStr, fmt.Sprintf(
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
func sameRowSet(m *Model, old, cur []*Entry) bool {
	signatures := func(entries []*Entry) []string {
		out := make([]string, 0, len(entries))
		for _, e := range entries {
			var parts []string
			for _, col := range sortedColumns(e.Cells) {
				parts = append(parts, col+"="+e.Cells[col].String())
			}
			out = append(out, strings.Join(parts, "\x00"))
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
		t := fixturechange.Table{Name: m.Table, ID: m.ID, Serial: m.Serial}
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
