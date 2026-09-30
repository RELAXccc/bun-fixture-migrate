package fixturemigrate

import (
	"fmt"
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
			if id := normalize(row.Str(m.ID)); id != "" && id != "0" {
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
	if id := normalize(row.Str(m.ID)); id != "" && id != "0" && !row[m.ID].IsNull {
		return "pk" + id
	}
	return ""
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
	// Any other template is evaluated by dbfixture at load time, so the
	// database never holds this text. Comparing it, or writing it into a
	// migration, would be comparing and writing something that is not there.
	if anyTemplate.MatchString(text) {
		return fixturechange.Value{}, false, fmt.Errorf(
			"%s.%s is %s, a template dbfixture evaluates when it loads the file and this tool cannot; "+
				"the database does not hold this text, so put %s in ignore", model, col, text, col)
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
		if ix.defined[tmodel][anchor] {
			return fixturechange.Value{}, fmt.Errorf(
				"%s.%s: %s names a row of %s that the file only defines further down; dbfixture loads the "+
					"file top to bottom and cannot load this: move that row above this one", model, col, text, tmodel)
		}
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

func isZero(v fixturechange.Value) bool {
	if v.Ref != nil {
		return false
	}
	return v.IsNull || v.Lit == "" || normalize(v.Lit) == "0"
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
		ID:     normalize(row.Str(m.ID)),
		Key:    key,
		KeyStr: keyString(model, key),
		Cells:  fixturechange.Values{},
	}
	if e.ID == "0" {
		e.ID = ""
	}
	cols := map[string]bool{}
	for col := range row {
		cols[col] = true
	}
	for col := range m.Defaults {
		cols[col] = true
	}
	for col := range cols {
		if m.skip(col) {
			continue
		}
		v, present, err := ix.value(model, col, row)
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		e.Cells[col] = v
	}
	return e, nil
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
				if !known || normalize(v.Lit) != normalize(zero) {
					continue
				}
				hazard, stored := column.ZeroIsNotDefault()
				if !hazard {
					continue
				}
				snap.Findings = append(snap.Findings, Finding{
					Kind: FindingZeroDefault, Model: model, Row: e.KeyStr,
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
					Kind: FindingNullDefault, Model: model, Row: e.KeyStr,
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
