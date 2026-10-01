package fixturemigrate

import (
	"fmt"
	"sort"
	"strings"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// index lets one fixture document resolve the references inside it, the way
// dbfixture resolves them while it loads the file.
//
// dbfixture evaluates a row's templates when it reaches the row, against the
// rows it has inserted so far, and registers each row under its "_id" -- or,
// for a row without one whose model has a single primary key, under "pk" and
// that key ("pk3") -- replacing whatever was registered under that name
// before. So a template names the latest row above it with that anchor, and a
// template naming a row further down fails to load. byAnchor is filled in the
// same order, which is what makes a file this tool accepts a file dbfixture
// can load.
type index struct {
	cfg *Config
	// byAnchor holds the rows read so far, per model, by anchor.
	byAnchor map[string]map[string]Row
	// defined holds every anchor of the file, so a forward reference can be
	// told apart from a reference to nothing.
	defined map[string]map[string]bool
	// byID holds every row of the file by its id, for a reference column that
	// holds a plain id. That is the database's lookup, not dbfixture's, and it
	// does not depend on the order.
	byID map[string]map[string]Row
}

func newIndex(cfg *Config, doc Doc) *index {
	ix := &index{
		cfg:      cfg,
		byAnchor: map[string]map[string]Row{},
		defined:  map[string]map[string]bool{},
		byID:     map[string]map[string]Row{},
	}
	for _, dm := range doc {
		if ix.defined[dm.Name] == nil {
			ix.defined[dm.Name] = map[string]bool{}
			ix.byID[dm.Name] = map[string]Row{}
			ix.byAnchor[dm.Name] = map[string]Row{}
		}
		m := cfg.Models[dm.Name]
		for _, row := range dm.Rows {
			if a := ix.anchorOf(m, row); a != "" {
				ix.defined[dm.Name][a] = true
			}
			if m == nil {
				continue
			}
			if id := idText(m, row); id != "" {
				if _, dup := ix.byID[dm.Name][id]; !dup {
					ix.byID[dm.Name][id] = row
				}
			}
		}
	}
	return ix
}

// anchorOf is the name dbfixture registers a row under: its "_id", else "pk"
// and its primary key when the row sets one. A row that leaves a serial key to
// the database gets its "pk" name from the id the database hands out, which
// nothing reading the file can know.
func (ix *index) anchorOf(m *Model, row Row) string {
	if a := row.Str(anchorColumn); a != "" {
		return a
	}
	if m == nil {
		return ""
	}
	if id := idText(m, row); id != "" {
		return "pk" + id
	}
	return ""
}

// idText is a row's primary key as text, "" when the row leaves it to the
// database: absent, null, or empty; see zeroID for a zero.
func idText(m *Model, row Row) string {
	c, ok := row[m.ID]
	if !ok || c.IsNull {
		return ""
	}
	id := scalarText(c)
	if zeroID(m, id) {
		return ""
	}
	return id
}

// zeroID reports an id that stands for "the database numbers this row": an
// empty one, and a zero in a serial model, whose autoincrement field bun
// writes as DEFAULT when it holds zero. Anywhere else a zero is an id like
// any other -- a status table keyed by "0", a row numbered 0 by hand -- and
// rows point at it.
func zeroID(m *Model, id string) bool {
	return id == "" || (m.Serial && sameScalar(id, "0"))
}

// loaded registers a row the way dbfixture does after inserting it: later
// rows can name it now, and it replaces an earlier row of the same anchor.
func (ix *index) loaded(model string, m *Model, row Row) {
	if a := ix.anchorOf(m, row); a != "" {
		ix.byAnchor[model][a] = row
	}
}

// cell returns a column of a row, falling back to the configured default. The
// second result is false when the column is neither in the row nor in the
// defaults, which the caller has to handle rather than guess at.
func (ix *index) cell(m *Model, col string, row Row) (Cell, bool) {
	if c, ok := row[col]; ok {
		return c, true
	}
	if def, ok := m.Defaults[col]; ok {
		if def == NullDefault {
			return Cell{IsNull: true}, true
		}
		return Cell{Text: def}, true
	}
	return Cell{}, false
}

// reading is one column of a fixture row as this tool reads it.
type reading struct {
	fixturechange.Value
	// written is what a Go string field gets in place of the value, when
	// that is something else; see Cell.StringText.
	written string
	// from is, for a value another row supplies, that row's column: the ref
	// column of the row a reference names, or the field a template copies.
	// dbfixture hands on what that field holds, so its type, not this
	// column's, decides which of the two readings the database holds.
	from *source
}

// source names a column of a model.
type source struct{ model, column string }

// value turns one column of a row into the value a migration carries.
func (ix *index) value(model, col string, row Row) (reading, bool, error) {
	m := ix.cfg.Models[model]
	cell, ok := ix.cell(m, col, row)
	if !ok {
		return reading{}, false, nil
	}
	if cell.Structured {
		// A mapping or a sequence is a jsonb, json or array value, carried as
		// its JSON. It cannot be a reference.
		if _, isRef := m.References[col]; isRef {
			return reading{}, false, fmt.Errorf(
				"%s.%s is a reference and holds a mapping or a sequence", model, col)
		}
		return reading{Value: fixturechange.Lit(cell.Text), written: cell.StringText}, true, nil
	}
	if cell.IsNull {
		return reading{Value: fixturechange.Null()}, true, nil
	}
	text := strings.TrimSpace(cell.Text)
	target, isRef := m.References[col]

	if match := template.FindStringSubmatch(text); match != nil {
		r, err := ix.resolveTemplate(model, col, text, match, target, isRef)
		return r, err == nil, err
	}
	if match := looseTemplate.FindStringSubmatch(text); match != nil {
		return reading{}, false, fmt.Errorf(
			"%s.%s is %s, but %q is not a name text/template can follow, so dbfixture cannot load this "+
				"file: give that row an _id made of letters, digits and underscores, not starting with a digit",
			model, col, text, match[2])
	}
	// Any other template is evaluated by dbfixture at load time, so the
	// database never holds this text. Comparing it, or writing it into a
	// migration, would be comparing and writing something that is not there.
	if anyTemplate.MatchString(text) {
		return reading{}, false, fmt.Errorf(
			"%s.%s is %s, a template dbfixture evaluates when it loads the file and this tool cannot; "+
				"the database does not hold this text, so put %s in ignore", model, col, text, col)
	}
	// The value itself, exactly: a string as written, a number as YAML
	// resolves it.
	lit := scalarText(cell)
	if !isRef {
		return reading{Value: fixturechange.Lit(lit), written: cell.StringText}, true, nil
	}
	// A reference column holding nothing or 0 points at no row, unless a row
	// has that id.
	if _, ok := ix.byID[target][lit]; !ok && (lit == "" || sameScalar(lit, "0")) {
		return reading{Value: fixturechange.Lit(lit)}, true, nil
	}
	r, err := ix.refByID(model, col, target, lit)
	return r, err == nil, err
}

// resolveTemplate turns a "{{ $.Model.row.Field }}" value into either a
// reference, when the column is configured as one and the template names the
// target's id, or into the literal the target row holds in that field.
func (ix *index) resolveTemplate(model, col, text string, match []string, target string, isRef bool) (reading, error) {
	tmodel, anchor, field := match[1], match[2], match[3]
	tm, err := ix.cfg.model(tmodel)
	if err != nil {
		return reading{}, fmt.Errorf("%s.%s: %s: %w", model, col, text, err)
	}
	trow, ok := ix.byAnchor[tmodel][anchor]
	if !ok {
		if ix.defined[tmodel][anchor] {
			return reading{}, fmt.Errorf(
				"%s.%s: %s names a row of %s that the file only defines further down; dbfixture loads the "+
					"file top to bottom and cannot load this: move that row above this one", model, col, text, tmodel)
		}
		return reading{}, fmt.Errorf("%s.%s: %s names no row of %s", model, col, text, tmodel)
	}
	column := underscore(field)
	if isRef {
		if tmodel != target {
			return reading{}, fmt.Errorf(
				"%s.%s: the configuration says it references %s but %s points at %s", model, col, target, text, tmodel)
		}
		if column == tm.ID {
			return ix.refTo(model, col, text, target, tm, trow)
		}
	}
	tcell, ok := trow[column]
	if !ok {
		return reading{}, fmt.Errorf("%s.%s: %s names no column %q of %s", model, col, text, column, tmodel)
	}
	if tcell.IsNull {
		return reading{Value: fixturechange.Null()}, nil
	}
	if !tcell.Structured && anyTemplate.MatchString(tcell.Text) {
		return reading{}, fmt.Errorf(
			"%s.%s: %s copies %s, which is itself a template in that row; dbfixture copies what that template "+
				"made of it, which this tool does not follow: write the value here", model, col, text, column)
	}
	return reading{Value: fixturechange.Lit(scalarText(tcell)), written: tcell.StringText,
		from: &source{tmodel, column}}, nil
}

// refByID turns the id a reference column holds into a reference by key, using
// the row the same document declares under that id.
func (ix *index) refByID(model, col, target, id string) (reading, error) {
	tm := ix.cfg.Models[target]
	trow, ok := ix.byID[target][id]
	if !ok {
		return reading{}, fmt.Errorf(
			"%s.%s = %s: no row of %s in this file has that %s, so the generator cannot name the row it points at",
			model, col, id, target, tm.ID)
	}
	return ix.refTo(model, col, id, target, tm, trow)
}

// refTo is a reference to a row: the row's ref value, which is what the
// database holds in that row's ref column and what a migration finds it by.
// It is read the way the row's own column is, with both readings of a value
// such as 0012 kept, and the ref column as the one whose type decides.
func (ix *index) refTo(model, col, text, target string, tm *Model, trow Row) (reading, error) {
	c, ok := trow[tm.Ref]
	if !ok || c.IsNull || scalarText(c) == "" {
		return reading{}, fmt.Errorf("%s.%s: %s points at a row of %s without a %s", model, col, text, target, tm.Ref)
	}
	if c.Structured || anyTemplate.MatchString(c.Text) {
		return reading{}, fmt.Errorf(
			"%s.%s: %s points at a row of %s whose %s is not a plain value but %s, and a reference can only "+
				"name a row by a plain value: write the value there", model, col, text, target, tm.Ref, c.Text)
	}
	return reading{Value: fixturechange.RefTo(target, scalarText(c)), written: c.StringText,
		from: &source{target, tm.Ref}}, nil
}

// keyValues is the natural key of a row.
func (ix *index) keyValues(model string, row Row) (fixturechange.Values, error) {
	m := ix.cfg.Models[model]
	out := fixturechange.Values{}
	for _, col := range m.Key {
		r, present, err := ix.value(model, col, row)
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, fmt.Errorf("%s: key column %q is missing from a row and has no default", model, col)
		}
		out[col] = r.Value
	}
	for _, group := range m.KeyAnyOf {
		chosen, value := group[0], fixturechange.Lit("")
		for _, col := range group {
			r, present, err := ix.value(model, col, row)
			if err != nil {
				return nil, err
			}
			if present && !isZero(r.Value) {
				chosen, value = col, r.Value
				break
			}
			if col == group[0] && present {
				value = r.Value
			}
		}
		out[chosen] = value
	}
	return out, nil
}

func isZero(v fixturechange.Value) bool {
	if v.Ref != nil {
		return false
	}
	return v.IsNull || v.Lit == "" || sameScalar(v.Lit, "0")
}

// FixtureSnapshot resolves a parsed fixture file into a snapshot. Every value
// is resolved here rather than at comparison time, so a template that names no
// row is an error when the file is read and not a difference that happens to
// look plausible.
func FixtureSnapshot(cfg *Config, doc Doc, source string) (*Snapshot, error) {
	snap := &Snapshot{Source: source, Entries: map[string][]*Entry{}, Columns: map[string][]string{}}
	ix := newIndex(cfg, doc)
	for _, dm := range doc {
		m, err := cfg.model(dm.Name)
		if err != nil {
			return nil, err
		}
		if _, seen := snap.Entries[dm.Name]; !seen {
			snap.Order = append(snap.Order, dm.Name)
			snap.Entries[dm.Name] = nil
		}
		for _, row := range dm.Rows {
			e, err := ix.entry(dm.Name, m, row)
			if err != nil {
				return nil, err
			}
			snap.Entries[dm.Name] = append(snap.Entries[dm.Name], e)
			ix.loaded(dm.Name, m, row)
		}
	}
	for _, model := range snap.Order {
		snap.Columns[model] = unionColumns(snap.Entries[model])
		snap.reportDuplicates(model)
		snap.reportDuplicateIDs(cfg, model)
	}
	return snap, nil
}

// entry resolves one fixture row. The compared columns are what the row spells
// out plus every column the model's defaults name, so a column left out of one
// row and written in the next is still comparable.
func (ix *index) entry(model string, m *Model, row Row) (*Entry, error) {
	key, err := ix.keyValues(model, row)
	if err != nil {
		return nil, err
	}
	e := &Entry{
		Anchor: row.Str(anchorColumn),
		ID:     idText(m, row),
		Cells:  fixturechange.Values{},
	}
	cols := map[string]bool{}
	for col := range row {
		cols[col] = true
	}
	for col := range m.Defaults {
		cols[col] = true
	}
	// In name order, so a row with two faults is refused for the same one on
	// every run.
	names := make([]string, 0, len(cols))
	for col := range cols {
		names = append(names, col)
	}
	sort.Strings(names)
	for _, col := range names {
		if m.skip(col) {
			continue
		}
		r, present, err := ix.value(model, col, row)
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		e.Cells[col] = r.Value
		e.record(col, r)
	}
	if e.ID != "" {
		e.record(m.ID, reading{written: row[m.ID].StringText})
	}
	e.setKey(model, key)
	return e, nil
}

// record keeps what a reading says beyond its value: the text a string field
// gets when that is something else, and the column whose type decides which
// of the two the database holds when that is not the entry's own.
func (e *Entry) record(col string, r reading) {
	if r.written == "" {
		return
	}
	if e.AsWritten == nil {
		e.AsWritten = map[string]string{}
	}
	e.AsWritten[col] = r.written
	if r.from != nil {
		if e.from == nil {
			e.from = map[string]source{}
		}
		e.from[col] = *r.from
	}
}

// LintZeroDefaults reports every value in the snapshot that is the column
// type's zero while the column's database default is something else.
//
// This is the defect that is worth more than the rest of this tool put
// together. bun's INSERT writes DEFAULT, not the value, for a zero in a field
// that carries a default (query_insert.go, marshalsToDefault). So a fixture row
// saying "production_max: 0" on such a column loads as 1, the file and the
// database disagree from the first seed onwards, and nothing says so. In the
// other direction an export that writes that zero does not reproduce the
// database it was taken from, which makes the export a lie.
//
// It needs no Go types: the catalog says the column has a default, and the
// column type says what its zero is.
func LintZeroDefaults(cfg *Config, snap *Snapshot, tables map[string]*dbschema.Table) {
	for _, model := range snap.Order {
		m := cfg.Models[model]
		if m == nil {
			continue
		}
		table := tables[cfg.QualifiedTable(m)]
		if table == nil {
			continue
		}
		for _, e := range snap.Entries[model] {
			for _, col := range sortedColumns(e.Cells) {
				v := e.Cells[col]
				if v.Ref != nil || v.IsNull {
					continue
				}
				column, ok := table.Column(col)
				if !ok {
					continue
				}
				zero, known := column.ZeroText()
				if !known || !sameScalar(v.Lit, zero) {
					continue
				}
				hazard, stored := column.ZeroIsNotDefault()
				if !hazard {
					continue
				}
				snap.Findings = append(snap.Findings, Finding{
					Kind: FindingZeroDefault, Model: model, Row: e.label(model),
					Detail: fmt.Sprintf(
						"%s is %s, but the column defaults to %s and bun writes DEFAULT for a zero, "+
							"so the database will hold %s and not %s: write %s here, or drop the column default",
						col, zero, stored, stored, zero, stored),
				})
			}
		}
	}
}

// LintNullDefaults reports every explicit null in the snapshot written into a
// column that has a default.
//
// It is the same line of bun as LintZeroDefaults, read from the other end:
// marshalsToDefault is true for a nil pointer as well as for a zero in a
// nullzero or default-tagged field, and a pointer or nullzero is how a bun
// model spells a nullable column. So "note: ~" on a column with a default
// loads as that default, and an export that writes ~ there does not reproduce
// the database it came from. Only a field of a type such as sql.NullString,
// with neither tag, writes the NULL; this check cannot see the Go type, which
// is why it has a policy of its own.
func LintNullDefaults(cfg *Config, snap *Snapshot, tables map[string]*dbschema.Table) {
	for _, model := range snap.Order {
		m := cfg.Models[model]
		if m == nil {
			continue
		}
		table := tables[cfg.QualifiedTable(m)]
		if table == nil {
			continue
		}
		for _, e := range snap.Entries[model] {
			for _, col := range sortedColumns(e.Cells) {
				if !e.Cells[col].IsNull {
					continue
				}
				column, ok := table.Column(col)
				if !ok {
					continue
				}
				def, ok := column.NonNullDefault()
				if !ok {
					continue
				}
				snap.Findings = append(snap.Findings, Finding{
					Kind: FindingNullDefault, Model: model, Row: e.label(model),
					Detail: fmt.Sprintf(
						"%s is null, but the column defaults to %s and bun writes DEFAULT for a nil pointer or a "+
							"nullzero field, so the database will hold %s and not NULL: write the value you mean, "+
							"or drop the column default", col, def, def),
				})
			}
		}
	}
}

// LintColumns reports every column of the fixture file the table does not have.
// Without it the mistake surfaces when the generated migration runs, which is
// the worst moment for it to surface.
func LintColumns(cfg *Config, snap *Snapshot, tables map[string]*dbschema.Table) {
	for _, model := range snap.Order {
		m := cfg.Models[model]
		if m == nil {
			continue
		}
		table := tables[cfg.QualifiedTable(m)]
		if table == nil {
			snap.Findings = append(snap.Findings, Finding{
				Kind: FindingUnknownColumn, Model: model,
				Detail: "the configuration says this model lives in " + cfg.QualifiedTable(m) + ", which does not exist",
			})
			continue
		}
		if idCol, ok := table.Column(m.ID); ok && idCol.IdentityAlways {
			for _, e := range snap.Entries[model] {
				if e.ID != "" {
					snap.Findings = append(snap.Findings, Finding{
						Kind: FindingUnknownColumn, Model: model, Row: e.label(model),
						Detail: fmt.Sprintf("%s is %s, but %s.%s is an identity GENERATED ALWAYS, which refuses "+
							"an explicit value from dbfixture as from a migration: leave %s out and name the row "+
							"by its _id", m.ID, e.ID, table.Qualified(), m.ID, m.ID),
					})
				}
			}
		}
		for _, col := range snap.Columns[model] {
			column, ok := table.Column(col)
			if !ok {
				snap.Findings = append(snap.Findings, Finding{
					Kind: FindingUnknownColumn, Model: model, Row: col,
					Detail: "the fixture file writes this column, " + table.Qualified() + " does not have it",
				})
				continue
			}
			if column.Generated {
				snap.Findings = append(snap.Findings, Finding{
					Kind: FindingUnknownColumn, Model: model, Row: col,
					Detail: "the fixture file writes this column, but " + table.Qualified() +
						" generates it and nothing can write into it: put it in derived",
				})
			}
		}
	}
}
