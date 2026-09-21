package fixturemigrate

import (
	"fmt"
	"strings"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// index lets one fixture document resolve the references inside it.
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
			if _, ok := table.Column(col); !ok {
				snap.Findings = append(snap.Findings, Finding{
					Kind: FindingUnknownColumn, Model: model, Row: col,
					Detail: "the fixture file writes this column, " + table.Qualified() + " does not have it",
				})
			}
		}
	}
}
