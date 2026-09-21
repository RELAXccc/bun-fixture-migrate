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
	// Changes are in apply order: inserts in the order the models appear in
	// the fixture file, then updates in that order, then deletes in reverse,
	// so no row is written before the row it points at.
	Changes []fixturechange.Change
	// Refusals are the differences that need a hand-written migration.
	Refusals []Refusal
	// Tables covers every model a change touches or points at.
	Tables fixturechange.Tables
	// Order is the model order that was used.
	Order []string
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

// index lets one document resolve the references inside it.
type index struct {
	cfg      *Config
	rows     map[string][]Row
	byAnchor map[string]map[string]Row
	byID     map[string]map[string]Row
}

func newIndex(cfg *Config, doc Doc) *index {
	ix := &index{
		cfg:      cfg,
		rows:     map[string][]Row{},
		byAnchor: map[string]map[string]Row{},
		byID:     map[string]map[string]Row{},
	}
	for _, dm := range doc {
		ix.rows[dm.Name] = append(ix.rows[dm.Name], dm.Rows...)
		if ix.byAnchor[dm.Name] == nil {
			ix.byAnchor[dm.Name] = map[string]Row{}
			ix.byID[dm.Name] = map[string]Row{}
		}
		for _, row := range dm.Rows {
			if a := row.Str(anchorColumn); a != "" {
				if _, dup := ix.byAnchor[dm.Name][a]; !dup {
					ix.byAnchor[dm.Name][a] = row
				}
			}
			if m := cfg.Models[dm.Name]; m != nil {
				if id := normalize(row.Str(m.ID)); id != "" && id != "0" {
					if _, dup := ix.byID[dm.Name][id]; !dup {
						ix.byID[dm.Name][id] = row
					}
				}
			}
		}
	}
	return ix
}

// cell returns a column of a row, falling back to the configured default. The
// second result is false when the column is neither in the row nor in the
// defaults, which the caller has to handle rather than guess at.
func (ix *index) cell(m *Model, col string, row Row) (Cell, bool) {
	if c, ok := row[col]; ok {
		return c, true
	}
	if def, ok := m.Defaults[col]; ok {
		return Cell{Text: def}, true
	}
	return Cell{}, false
}

// value turns one column of a row into the value a migration carries.
func (ix *index) value(model, col string, row Row) (fixturechange.Value, bool, error) {
	m := ix.cfg.Models[model]
	cell, ok := ix.cell(m, col, row)
	if !ok {
		return fixturechange.Value{}, false, nil
	}
	if cell.Structured {
		return fixturechange.Value{}, false, fmt.Errorf(
			"%s.%s is a mapping or a sequence; this tool only handles scalar columns, put it in ignore", model, col)
	}
	if cell.IsNull {
		return fixturechange.Null(), true, nil
	}
	text := strings.TrimSpace(cell.Text)
	target, isRef := m.References[col]

	if match := template.FindStringSubmatch(text); match != nil {
		v, err := ix.resolveTemplate(model, col, text, match, target, isRef)
		return v, err == nil, err
	}
	if !isRef {
		return fixturechange.Lit(normalize(text)), true, nil
	}
	// A reference column holding nothing, 0 or NULL points at no row.
	if n := normalize(text); n == "" || n == "0" {
		return fixturechange.Lit(n), true, nil
	}
	v, err := ix.refByID(model, col, target, normalize(text))
	return v, err == nil, err
}

// resolveTemplate turns a "{{ $.Model.row.Field }}" value into either a
// reference, when the column is configured as one and the template names the
// target's id, or into the literal the target row holds in that field.
func (ix *index) resolveTemplate(model, col, text string, match []string, target string, isRef bool) (fixturechange.Value, error) {
	tmodel, anchor, field := match[1], match[2], match[3]
	tm, err := ix.cfg.model(tmodel)
	if err != nil {
		return fixturechange.Value{}, fmt.Errorf("%s.%s: %s: %w", model, col, text, err)
	}
	trow, ok := ix.byAnchor[tmodel][anchor]
	if !ok {
		return fixturechange.Value{}, fmt.Errorf("%s.%s: %s names no row of %s", model, col, text, tmodel)
	}
	column := underscore(field)
	if isRef {
		if tmodel != target {
			return fixturechange.Value{}, fmt.Errorf(
				"%s.%s: the configuration says it references %s but %s points at %s", model, col, target, text, tmodel)
		}
		if column == tm.ID {
			key := trow.Str(tm.Ref)
			if key == "" {
				return fixturechange.Value{}, fmt.Errorf(
					"%s.%s: %s points at a row of %s without a %s", model, col, text, tmodel, tm.Ref)
			}
			return fixturechange.RefTo(target, key), nil
		}
	}
	tcell, ok := trow[column]
	if !ok {
		return fixturechange.Value{}, fmt.Errorf("%s.%s: %s names no column %q of %s", model, col, text, column, tmodel)
	}
	if tcell.IsNull {
		return fixturechange.Null(), nil
	}
	return fixturechange.Lit(normalize(tcell.Text)), nil
}

// refByID turns the id a reference column holds into a reference by key, using
// the row the same document declares under that id.
func (ix *index) refByID(model, col, target, id string) (fixturechange.Value, error) {
	tm := ix.cfg.Models[target]
	trow, ok := ix.byID[target][id]
	if !ok {
		return fixturechange.Value{}, fmt.Errorf(
			"%s.%s = %s: no row of %s in this file has that %s, so the generator cannot name the row it points at",
			model, col, id, target, tm.ID)
	}
	key := trow.Str(tm.Ref)
	if key == "" {
		return fixturechange.Value{}, fmt.Errorf("%s.%s = %s: that row of %s has no %s", model, col, id, target, tm.Ref)
	}
	return fixturechange.RefTo(target, key), nil
}

func isZero(v fixturechange.Value) bool {
	if v.Ref != nil {
		return false
	}
	return v.IsNull || v.Lit == "" || normalize(v.Lit) == "0"
}

// keyValues is the natural key of a row.
func (ix *index) keyValues(model string, row Row) (fixturechange.Values, error) {
	m := ix.cfg.Models[model]
	out := fixturechange.Values{}
	for _, col := range m.Key {
		v, present, err := ix.value(model, col, row)
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, fmt.Errorf("%s: key column %q is missing from a row and has no default", model, col)
		}
		out[col] = v
	}
	for _, group := range m.KeyAnyOf {
		chosen, value := group[0], fixturechange.Lit("")
		for _, col := range group {
			v, present, err := ix.value(model, col, row)
			if err != nil {
				return nil, err
			}
			if present && !isZero(v) {
				chosen, value = col, v
				break
			}
			if col == group[0] && present {
				value = v
			}
		}
		out[chosen] = value
	}
	return out, nil
}

func keyString(model string, key fixturechange.Values) string {
	cols := make([]string, 0, len(key))
	for c := range key {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		parts = append(parts, c+"="+key[c].String())
	}
	return model + "/" + strings.Join(parts, "/")
}

// comparedColumns is the union of the rows' columns without the anchor, the
// id, the ignored and the derived ones.
func comparedColumns(m *Model, rows ...Row) []string {
	seen := map[string]bool{}
	var out []string
	for _, row := range rows {
		for col := range row {
			if m.skip(col) || seen[col] {
				continue
			}
			seen[col] = true
			out = append(out, col)
		}
	}
	sort.Strings(out)
	return out
}

// fullRowColumns is the whole row as the fixture loader would have written it:
// what the row spells out, what the defaults say about the rest, and the id
// when the row has one. An insert writes these, a delete guards on them.
func fullRowColumns(m *Model, row Row) []string {
	cols := comparedColumns(m, row)
	for col := range m.Defaults {
		if _, ok := row[col]; ok || m.skip(col) {
			continue
		}
		cols = append(cols, col)
	}
	sort.Strings(cols)
	if _, ok := row[m.ID]; ok {
		cols = append(cols, m.ID)
	}
	return cols
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

type keyedRow struct {
	key    fixturechange.Values
	keyStr string
	row    Row
}

// Compute diffs two revisions of a fixture file.
func Compute(cfg *Config, old, next Doc) (*Result, error) {
	order, err := modelOrder(cfg, old, next)
	if err != nil {
		return nil, err
	}
	oldIx, newIx := newIndex(cfg, old), newIndex(cfg, next)
	res := &Result{Tables: fixturechange.Tables{}, Order: order}
	var inserts, updates, deletes []fixturechange.Change

	// First pass: key every row and take the refusals that make a whole row
	// disappear from the diff. A rename and a renumbering are the same
	// refusal seen from two sides, and both have to be known before any
	// change is built, because a row elsewhere may point at the renamed one.
	type state struct {
		old, cur []keyedRow
		skip     map[string]bool
	}
	states := make(map[string]*state, len(order))
	renamed := map[string]bool{}
	for _, model := range order {
		st := &state{}
		var err error
		if st.old, err = keyRows(oldIx, model); err != nil {
			return nil, err
		}
		if st.cur, err = keyRows(newIx, model); err != nil {
			return nil, err
		}
		st.skip = refuseRenames(cfg.Models[model], model, st.old, st.cur, res, renamed)
		states[model] = st
	}

	for _, model := range order {
		m := cfg.Models[model]
		st := states[model]
		skipKeys := st.skip
		oldGroups, oldOrder := groupRows(st.old)
		newGroups, newOrder := groupRows(st.cur)

		for _, k := range newOrder {
			if skipKeys[k] {
				continue
			}
			cur, prev := newGroups[k], oldGroups[k]
			// A key that is not unique cannot be turned into a WHERE clause
			// that hits the right row. As long as the group did not change
			// that costs nothing; once it does, it has to be hand-written.
			if len(cur) > 1 || len(prev) > 1 {
				same, err := sameRowSet(m, model, oldIx, newIx, prev, cur)
				if err != nil {
					return nil, err
				}
				if !same {
					res.Refusals = append(res.Refusals, Refusal{model, k, fmt.Sprintf(
						"the natural key is not unique (%d row(s) before, %d after) and the rows differ: hand-write the migration",
						len(prev), len(cur))})
				}
				continue
			}
			if len(prev) == 0 {
				values, err := rowValues(newIx, model, fullRowColumns(m, cur[0].row), cur[0].row)
				if err != nil {
					return nil, err
				}
				change := fixturechange.Change{
					Model: model, Kind: fixturechange.Insert, Key: cur[0].key, New: values}
				if r, ok := refusedByRename(renamed, change); ok {
					res.Refusals = append(res.Refusals, r)
					continue
				}
				inserts = append(inserts, change)
				continue
			}
			change, refusal, err := diffRow(oldIx, newIx, m, model, prev[0], cur[0])
			if err != nil {
				return nil, err
			}
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
			if skipKeys[k] {
				continue
			}
			if _, stillThere := newGroups[k]; stillThere {
				continue
			}
			prev := oldGroups[k]
			if m.NoDelete {
				res.Refusals = append(res.Refusals, Refusal{model, k,
					"deletes of this model are refused because other rows may point at it: hand-write the migration"})
				continue
			}
			if len(prev) > 1 {
				res.Refusals = append(res.Refusals, Refusal{model, k, fmt.Sprintf(
					"delete of %d rows sharing one natural key: hand-write the migration", len(prev))})
				continue
			}
			values, err := rowValues(oldIx, model, fullRowColumns(m, prev[0].row), prev[0].row)
			if err != nil {
				return nil, err
			}
			change := fixturechange.Change{
				Model: model, Kind: fixturechange.Delete, Key: prev[0].key, Old: values}
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
	res.Changes = append(append(append([]fixturechange.Change{}, inserts...), updates...), deletes...)
	res.Tables = tablesFor(cfg, res.Changes)
	sort.SliceStable(res.Refusals, func(i, j int) bool {
		if res.Refusals[i].Model != res.Refusals[j].Model {
			return res.Refusals[i].Model < res.Refusals[j].Model
		}
		return res.Refusals[i].Key < res.Refusals[j].Key
	})
	return res, nil
}

// modelOrder is the order the models appear in the new file, with models only
// the old file had appended. dbfixture loads a file top to bottom, so that
// order already satisfies the foreign keys.
func modelOrder(cfg *Config, old, cur Doc) ([]string, error) {
	var order []string
	seen := map[string]bool{}
	for _, doc := range []Doc{cur, old} {
		for _, dm := range doc {
			if _, err := cfg.model(dm.Name); err != nil {
				return nil, err
			}
			if seen[dm.Name] {
				continue
			}
			seen[dm.Name] = true
			order = append(order, dm.Name)
		}
	}
	return order, nil
}

func keyRows(ix *index, model string) ([]keyedRow, error) {
	var out []keyedRow
	for _, row := range ix.rows[model] {
		key, err := ix.keyValues(model, row)
		if err != nil {
			return nil, err
		}
		out = append(out, keyedRow{key: key, keyStr: keyString(model, key), row: row})
	}
	return out, nil
}

func groupRows(rows []keyedRow) (map[string][]keyedRow, []string) {
	groups := map[string][]keyedRow{}
	var order []string
	for _, kr := range rows {
		if _, seen := groups[kr.keyStr]; !seen {
			order = append(order, kr.keyStr)
		}
		groups[kr.keyStr] = append(groups[kr.keyStr], kr)
	}
	return groups, order
}

// refuseRenames reports the rows whose stable id kept but whose natural key
// changed, and the rows whose natural key kept but whose stable id changed.
// Neither can be migrated: the first would insert the new row and delete the
// old one, which is not a rename, and the second would renumber a primary key
// other tables point at. The returned keys are left out of the diff.
func refuseRenames(m *Model, model string, old, cur []keyedRow, res *Result, renamed map[string]bool) map[string]bool {
	skip := map[string]bool{}
	if m.StableID == "" {
		return skip
	}
	byStable := func(rows []keyedRow) map[string]keyedRow {
		out := map[string]keyedRow{}
		for _, kr := range rows {
			id := normalize(kr.row.Str(m.StableID))
			if id == "" || id == "0" {
				continue
			}
			if _, dup := out[id]; !dup {
				out[id] = kr
			}
		}
		return out
	}
	newByStable := byStable(cur)
	for _, kr := range old {
		id := normalize(kr.row.Str(m.StableID))
		nkr, ok := newByStable[id]
		if !ok || nkr.keyStr == kr.keyStr {
			continue
		}
		res.Refusals = append(res.Refusals, Refusal{model, m.StableID + " " + id, fmt.Sprintf(
			"renamed from %s to %s: an insert plus a delete is not a rename, and rows elsewhere may point at it "+
				"— hand-write the migration, then run this again", kr.keyStr, nkr.keyStr)})
		skip[kr.keyStr] = true
		skip[nkr.keyStr] = true
		renamed[model+"\x00"+kr.row.Str(m.Ref)] = true
		renamed[model+"\x00"+nkr.row.Str(m.Ref)] = true
	}
	if m.StableID == m.ID {
		newByKey := make(map[string]keyedRow, len(cur))
		for _, kr := range cur {
			if _, dup := newByKey[kr.keyStr]; !dup {
				newByKey[kr.keyStr] = kr
			}
		}
		for _, kr := range old {
			if skip[kr.keyStr] {
				continue
			}
			nkr, ok := newByKey[kr.keyStr]
			if !ok {
				continue
			}
			o, n := normalize(kr.row.Str(m.ID)), normalize(nkr.row.Str(m.ID))
			if o == n || o == "" || n == "" || o == "0" || n == "0" {
				continue
			}
			res.Refusals = append(res.Refusals, Refusal{model, kr.keyStr, fmt.Sprintf(
				"its %s changed from %s to %s: this tool does not renumber primary keys, "+
					"put %s back or hand-write the migration", m.ID, o, n, o)})
			skip[kr.keyStr] = true
		}
	}
	return skip
}

// refusedByRename reports a change that points at a row whose rename was
// refused. Until that rename is written by hand, neither name is where the
// change expects it.
func refusedByRename(renamed map[string]bool, c fixturechange.Change) (Refusal, bool) {
	if len(renamed) == 0 {
		return Refusal{}, false
	}
	for _, values := range []fixturechange.Values{c.Key, c.Old, c.New} {
		for _, col := range sortedValueKeys(values) {
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

// rowValues renders the given columns of a row. A column that does not
// resolve is an error: guessing here is how a generator writes a migration
// that looks right and is not.
func rowValues(ix *index, model string, cols []string, row Row) (fixturechange.Values, error) {
	out := fixturechange.Values{}
	for _, col := range cols {
		v, present, err := ix.value(model, col, row)
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		out[col] = v
	}
	return out, nil
}

// diffRow compares two revisions of one row. A column that is spelled out on
// one side and missing on the other with no configured default is refused: the
// generator would have to invent what the missing one means.
func diffRow(oldIx, newIx *index, m *Model, model string, prev, cur keyedRow) (*fixturechange.Change, *Refusal, error) {
	oldVals, newVals := fixturechange.Values{}, fixturechange.Values{}
	for _, col := range comparedColumns(m, prev.row, cur.row) {
		ov, oldPresent, err := oldIx.value(model, col, prev.row)
		if err != nil {
			return nil, nil, err
		}
		nv, newPresent, err := newIx.value(model, col, cur.row)
		if err != nil {
			return nil, nil, err
		}
		if !oldPresent && !newPresent {
			continue
		}
		if oldPresent != newPresent {
			return nil, &Refusal{model, cur.keyStr, fmt.Sprintf(
				"column %q is written in one revision and left out in the other, and has no entry in defaults: "+
					"say what an omitted %q means and run this again", col, col)}, nil
		}
		if sameValue(ov, nv) {
			continue
		}
		oldVals[col], newVals[col] = ov, nv
	}
	if len(newVals) == 0 {
		return nil, nil, nil
	}
	return &fixturechange.Change{
		Model: model, Kind: fixturechange.Update, Key: cur.key, Old: oldVals, New: newVals}, nil, nil
}

// sameRowSet compares two groups of rows that share one natural key, as
// multisets. A column that does not resolve is an error, never a placeholder:
// two rows that both failed to resolve would otherwise look equal and the
// group would count as unchanged.
func sameRowSet(m *Model, model string, oldIx, newIx *index, old, cur []keyedRow) (bool, error) {
	signatures := func(ix *index, rows []keyedRow) ([]string, error) {
		out := make([]string, 0, len(rows))
		for _, kr := range rows {
			var parts []string
			for _, col := range comparedColumns(m, kr.row) {
				v, present, err := ix.value(model, col, kr.row)
				if err != nil {
					return nil, err
				}
				if !present {
					continue
				}
				parts = append(parts, col+"="+v.String())
			}
			out = append(out, strings.Join(parts, "\x00"))
		}
		sort.Strings(out)
		return out, nil
	}
	// Both sides are rendered even when the lengths differ, so an unresolvable
	// reference is reported instead of hidden behind a "they differ".
	a, err := signatures(oldIx, old)
	if err != nil {
		return false, err
	}
	b, err := signatures(newIx, cur)
	if err != nil {
		return false, err
	}
	if len(a) != len(b) {
		return false, nil
	}
	for i := range a {
		if a[i] != b[i] {
			return false, nil
		}
	}
	return true, nil
}

// tablesFor collects the models the change set touches and the models it
// points at, so the generated file carries nothing it does not need.
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
