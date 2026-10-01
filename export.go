package fixturemigrate

import (
	"container/heap"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/template/parse"
	"time"
	"unicode/utf8"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// Export writes a snapshot as a dbfixture YAML file.
//
// The models come out in dependency order, so dbfixture can resolve every
// reference as it loads the file top to bottom; a reference is written as the
// "{{ $.Model.row.ID }}" template that points at the target's anchor, never as
// the raw id, because the ids of the database it was taken from mean nothing in
// another one; and every value is written in the notation its column type reads
// back as the same value.
//
// The comment block at the top says where the file came from and what was
// wrong with it. An export that does not reproduce the database it was taken
// from is worse than no export, so the round-trip hazards are named in the file
// itself as well as on the terminal.
func Export(cfg *Config, snap *Snapshot, tables map[string]*dbschema.Table, header []string) ([]byte, error) {
	return exportModels(cfg, snap, tables, header, snap.Order)
}

// ExportFiles writes a snapshot into several fixture files, the way an
// application loading them with one fixture.Load keeps its master data.
//
// Each model goes into the file of current that holds it now, a model none of
// them holds into the last, and every file lists its models in dependency
// order. dbfixture loads the files in order, so a row can only name a row of
// its own file or an earlier one: an assignment where a model points at one in
// a later file is refused rather than written. A file with no model left is
// written as an empty list, which dbfixture loads; an empty file it does not.
func ExportFiles(cfg *Config, snap *Snapshot, tables map[string]*dbschema.Table, header []string,
	current []FixtureFile) ([][]byte, error) {

	if len(current) == 0 {
		return nil, fmt.Errorf("no fixture files to export into")
	}
	where := map[string]int{}
	for i, f := range current {
		doc, err := ParseDoc(f.Data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		for _, dm := range doc {
			if _, ok := where[dm.Name]; !ok {
				where[dm.Name] = i
			}
		}
	}
	for _, model := range snap.Order {
		if _, ok := where[model]; !ok {
			where[model] = len(current) - 1
		}
	}
	for _, model := range snap.Order {
		m := cfg.Models[model]
		for _, col := range sortedKeysOf(m.References) {
			target := m.References[col]
			if m.skip(col) || target == model || where[target] <= where[model] {
				continue
			}
			return nil, fmt.Errorf("%s in %s points at %s, which is in %s, and dbfixture loads that file later: "+
				"move %s into %s or an earlier file", model, current[where[model]].Path, target,
				current[where[target]].Path, target, current[where[model]].Path)
		}
	}
	out := make([][]byte, len(current))
	for i := range current {
		var models []string
		for _, model := range snap.Order {
			if where[model] == i {
				models = append(models, model)
			}
		}
		data, err := exportModels(cfg, snap, tables, header, models)
		if err != nil {
			return nil, err
		}
		if len(models) == 0 {
			data = append(data, "[]\n"...)
		}
		out[i] = data
	}
	return out, nil
}

func sortedKeysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// exportModels writes some of a snapshot's models. References may name rows of
// any model of the snapshot: in another file they are still one scope.
func exportModels(cfg *Config, snap *Snapshot, tables map[string]*dbschema.Table, header []string,
	models []string) ([]byte, error) {

	var b strings.Builder
	for _, line := range header {
		for _, part := range commentLines(line) {
			if part == "" {
				b.WriteString("#\n")
				continue
			}
			b.WriteString("# " + part + "\n")
		}
	}
	if len(header) > 0 {
		b.WriteString("\n")
	}

	anchors := map[string]map[string]string{} // model -> ref value -> anchor
	for _, model := range snap.Order {
		m := cfg.Models[model]
		anchors[model] = map[string]string{}
		for _, e := range snap.Entries[model] {
			if v := e.refValue(m); v != "" {
				anchors[model][v] = e.Anchor
			}
		}
	}

	var written []writtenRow
	for _, model := range models {
		m := cfg.Models[model]
		table := tables[cfg.QualifiedTable(m)]
		if table == nil {
			return nil, fmt.Errorf("model %q: %s is not a table in this database", model, cfg.QualifiedTable(m))
		}
		entries, err := loadOrder(cfg, model, snap.Entries[model])
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			continue
		}
		fmt.Fprintf(&b, "- model: %s\n  rows:\n", model)
		cols := exportColumns(m, table, snap.Columns[model])
		for _, e := range entries {
			first := true
			row := writtenRow{model: model, cells: map[string]writtenCell{}}
			write := func(col, value, comment string, want writtenCell) {
				prefix := "      "
				if first {
					prefix = "    - "
					first = false
				}
				b.WriteString(prefix + col + ": " + value)
				if comment != "" {
					b.WriteString("  # " + comment)
				}
				b.WriteString("\n")
				row.cells[col] = want
			}
			write(anchorColumn, yamlAnchor(e.Anchor), "", writtenCell{text: e.Anchor, exact: true})
			// An identity GENERATED ALWAYS refuses an explicit id from
			// dbfixture as from anybody, so the file names rows by anchor
			// only and the database numbers them.
			if idCol, ok := table.Column(m.ID); e.ID != "" && !idCol.IdentityAlways {
				if !ok {
					idCol = dbschema.Column{Type: "text"}
				}
				text, _, err := exportLiteral(model, m.ID, e.ID, idCol)
				if err != nil {
					return nil, err
				}
				write(m.ID, text, "", writtenCell{value: fixturechange.Lit(e.ID), column: idCol})
			}
			for _, col := range cols {
				v, ok := e.Cells[col]
				if !ok {
					continue
				}
				column, _ := table.Column(col)
				if jsonNullByDefault(m, column, v) {
					// No YAML spelling is the JSON null to both this tool and
					// a map field; a row that leaves the column out is, by the
					// model's defaults and by a nil map.
					continue
				}
				text, note, err := exportValue(cfg, model, col, v, column, anchors)
				if err != nil {
					return nil, err
				}
				comment := hazardComment(v, column)
				if note != "" {
					comment = strings.TrimPrefix(comment+"; "+note, "; ")
				}
				want := writtenCell{value: v, column: column}
				if v.Ref != nil {
					want = writtenCell{text: strings.Trim(text, "'"), exact: true}
				}
				write(col, text, comment, want)
			}
			written = append(written, row)
		}
		b.WriteString("\n")
	}
	out := []byte(strings.TrimRight(b.String(), "\n") + "\n")
	if err := verifyExport(out, written); err != nil {
		return nil, err
	}
	return out, nil
}

// commentLines is a header line as the lines of a YAML comment: a line break
// in it, which a finding quoting a value can hold, starts a comment line of
// its own instead of ending the comment, and a character YAML refuses even
// in a comment is written as an escape.
func commentLines(line string) []string {
	var out []string
	var b strings.Builder
	for i := 0; i < len(line); {
		r, size := utf8.DecodeRuneInString(line[i:])
		i += size
		switch {
		case r == '\r' && strings.HasPrefix(line[i:], "\n"):
		case r == '\n' || r == '\r' || r == 0x85 || r == 0x2028 || r == 0x2029:
			out = append(out, b.String())
			b.Reset()
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, line[i-1])
		case r == '\t' || yamlPrintable(r):
			b.WriteRune(r)
		default:
			writeYAMLEscape(&b, r)
		}
	}
	return append(out, b.String())
}

// writtenRow is one row of an export, as it was meant to read back.
type writtenRow struct {
	model string
	cells map[string]writtenCell
}

// writtenCell is one value of an export: the database's value and its
// column, or, exact, the text an anchor or a reference has to read back as.
type writtenCell struct {
	value  fixturechange.Value
	column dbschema.Column
	text   string
	exact  bool
}

// verifyExport parses an export back the way dbfixture and this tool read it,
// with yaml.v3, and holds every value against the database's value it was
// written from. Escaping a string, choosing a notation, turning text that
// looks like a template into one that is not: a mistake in any of them is an
// export that loads as something else, and this is where it is caught,
// before the file is written rather than at the next seed.
func verifyExport(data []byte, rows []writtenRow) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("the export would not read back as the database it was taken from, so it was not "+
			"written: "+format, args...)
	}
	doc, err := ParseDoc(data)
	if err != nil {
		return fail("%v", err)
	}
	i := 0
	for _, dm := range doc {
		for _, row := range dm.Rows {
			if i >= len(rows) || rows[i].model != dm.Name {
				return fail("it parses back into other rows than it holds")
			}
			want := rows[i]
			i++
			if len(row) != len(want.cells) {
				return fail("a row of %s parses back with %d columns, not %d", dm.Name, len(row), len(want.cells))
			}
			for col, w := range want.cells {
				cell, ok := row[col]
				if !ok {
					return fail("a row of %s parses back without %s", dm.Name, col)
				}
				if err := w.check(cell); err != nil {
					return fail("%s.%s %v", dm.Name, col, err)
				}
			}
		}
	}
	if i != len(rows) {
		return fail("it parses back into fewer rows than it holds")
	}
	return nil
}

func (w writtenCell) check(cell Cell) error {
	switch {
	case w.exact:
		if cell.IsNull || cell.Structured || cell.Text != w.text {
			return fmt.Errorf("reads back as %q, not %q", cell.Text, w.text)
		}
	case w.value.IsNull:
		if !cell.IsNull {
			return fmt.Errorf("reads back as %q, not as null", cell.Text)
		}
	case cell.IsNull:
		return fmt.Errorf("reads back as null, not %q", w.value.Lit)
	case cell.Structured && w.column.Category == "A" && !isJSONElem(w.column) && nestedArray(cell.Text):
		return fmt.Errorf("is a sequence of sequences, and %s", multidimensionalReason)
	default:
		got, want := exportedReading(w.column, cell), databaseReading(w.column, w.value.Lit)
		if got != want {
			return fmt.Errorf("reads back as %q, not %q", got, want)
		}
	}
	return nil
}

// exportedReading is what the tool, and a model field of the column's type,
// read from a value of an exported file, spelled as databaseReading spells
// the database's.
func exportedReading(c dbschema.Column, cell Cell) string {
	if cell.Structured {
		if c.Type == "bytea" {
			hex, msg := byteaOf(cell.Text)
			if msg != "" {
				return msg
			}
			return hex
		}
		return normalJSON(cell.Text)
	}
	text := scalarText(cell)
	if cell.Tag == "!!str" {
		if lit, ok := quotedTemplate(text); ok {
			text = lit
		}
	}
	if isJSON(c) && !json.Valid([]byte(text)) {
		text = jsonString(text)
	}
	return databaseReading(c, text)
}

// databaseReading is a value as the database holds it, in one spelling for
// each type, so an exported value read back can be held against it without
// asking the database: numbers canonically, JSON with its keys sorted, an
// instant in UTC.
func databaseReading(c dbschema.Column, text string) string {
	switch {
	case isJSON(c) || c.Category == "A":
		return normalJSON(text)
	case numericType(c.Type):
		if s, ok := canonicalDecimal(text); ok {
			return s
		}
	case c.Type == "bool":
		switch strings.ToLower(text) {
		case "t", "true":
			return "true"
		case "f", "false":
			return "false"
		}
	case c.Type == "timestamp" || c.Type == "timestamptz":
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07",
			"2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999"} {
			if t, err := time.Parse(layout, text); err == nil {
				if c.Type == "timestamp" {
					return t.Format("2006-01-02T15:04:05.999999999")
				}
				return t.UTC().Format(time.RFC3339Nano)
			}
		}
	}
	return text
}

// loadOrder is the order dbfixture can load a model's rows in. It resolves a
// template against the rows above it, so a row that points at a row of its
// own model has to come after that row: parents before their children, and
// otherwise the order the snapshot holds them in, which for a database is id
// order. A tree whose root was added after its leaves needs that, and a model
// without a reference to itself keeps its order as it is. Rows that point at
// each other in a circle load in no order, and are refused.
func loadOrder(cfg *Config, model string, entries []*Entry) ([]*Entry, error) {
	m := cfg.Models[model]
	var self []string
	for _, col := range sortedKeysOf(m.References) {
		if m.References[col] == model && !m.skip(col) {
			self = append(self, col)
		}
	}
	if len(self) == 0 {
		return entries, nil
	}
	// The row a reference names is the one whose anchor it is written with,
	// which is the last row holding that ref value.
	byRef := map[string]int{}
	for i, e := range entries {
		if v := e.refValue(m); v != "" {
			byRef[v] = i
		}
	}
	waiting := make([]int, len(entries))
	children := make([][]int, len(entries))
	for i, e := range entries {
		for _, col := range self {
			ref := e.Cells[col].Ref
			if ref == nil {
				continue
			}
			// A target that is not in the snapshot is exportValue's to report.
			if parent, ok := byRef[ref.Key]; ok {
				waiting[i]++
				children[parent] = append(children[parent], i)
			}
		}
	}
	// Of the rows whose parents are all written, the one first in the
	// snapshot goes next, so the result is the snapshot's order wherever that
	// order loads.
	ready := &positions{}
	for i := range entries {
		if waiting[i] == 0 {
			heap.Push(ready, i)
		}
	}
	out := make([]*Entry, 0, len(entries))
	for ready.Len() > 0 {
		i := heap.Pop(ready).(int)
		out = append(out, entries[i])
		for _, child := range children[i] {
			if waiting[child]--; waiting[child] == 0 {
				heap.Push(ready, child)
			}
		}
	}
	if len(out) < len(entries) {
		var circle []string
		for i, e := range entries {
			if waiting[i] > 0 {
				circle = append(circle, e.label(model))
			}
		}
		return nil, fmt.Errorf("rows of %s point at each other, or at themselves, through %s in a circle, and dbfixture "+
			"cannot load such rows in any order: %s. Break the circle in the database, or put %s in ignore",
			model, strings.Join(self, ", "), strings.Join(circle, "; "), strings.Join(self, ", "))
	}
	return out, nil
}

// positions is a min-heap of row positions.
type positions []int

func (p positions) Len() int           { return len(p) }
func (p positions) Less(i, j int) bool { return p[i] < p[j] }
func (p positions) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
func (p *positions) Push(x any)        { *p = append(*p, x.(int)) }
func (p *positions) Pop() any {
	old := *p
	x := old[len(old)-1]
	*p = old[:len(old)-1]
	return x
}

// exportColumns is the column order of one model: the table's own order, so the
// file reads like the table, with anything the snapshot did not read left out.
func exportColumns(m *Model, table *dbschema.Table, have []string) []string {
	present := set(have)
	var out []string
	for _, c := range table.Columns {
		if c.Name == m.ID || m.skip(c.Name) || c.Generated || !present[c.Name] {
			continue
		}
		out = append(out, c.Name)
	}
	for _, col := range have {
		if _, ok := table.Column(col); !ok {
			out = append(out, col)
		}
	}
	return out
}

func exportValue(cfg *Config, model, col string, v fixturechange.Value, column dbschema.Column,
	anchors map[string]map[string]string) (string, string, error) {

	switch {
	case v.IsNull:
		if isJSON(column) && column.Default == "" && cfg.Policy.NullDefault == ModeError {
			return "", "", fmt.Errorf("%s.%s is NULL, which a fixture file can only write as ~, and a ~ in a %s "+
				"column is the JSON null to a map, slice or any field and NULL only to a nil pointer or a "+
				"nullzero one: set policy.null_default to warn if this model writes NULL here, and the export "+
				"writes ~", model, col, column.Type)
		}
		return "~", "", nil
	case v.Ref != nil:
		target := cfg.Models[v.Ref.Model]
		anchor, ok := anchors[v.Ref.Model][v.Ref.Key]
		if !ok {
			return "", "", fmt.Errorf("%s.%s points at %s %q, which is not in the export", model, col, v.Ref.Model, v.Ref.Key)
		}
		return fmt.Sprintf("'{{ $.%s.%s.%s }}'", v.Ref.Model, anchor, camel(target.ID)), "", nil
	}
	if column.Category == "A" && cfg.arrayNulls(cfg.Models[model]) != ArrayNullsKeep && jsonNullElement(v.Lit) {
		return "", "", fmt.Errorf("%s.%s holds %s, an array with a NULL element, which a YAML sequence writes as "+
			"null and a []string or []int64 field leaves out, so the file would read as something else and not "+
			"load as this: set array_nulls: keep on the model if its array fields keep a null, as a []*string "+
			"does, or put %s in ignore", model, col, jsonbText(v.Lit), col)
	}
	return exportLiteral(model, col, v.Lit, column)
}

// jsonNullElement reports a JSON array holding a null, or holding an array
// that does, as an array column holds a NULL element.
func jsonNullElement(text string) bool {
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		return false
	}
	var holds func(v any) bool
	holds = func(v any) bool {
		elems, ok := v.([]any)
		if !ok {
			return false
		}
		for _, e := range elems {
			if e == nil || holds(e) {
				return true
			}
		}
		return false
	}
	return holds(v)
}

// nestedArray reports a JSON array that holds an array, which is how an
// array of more than one dimension is read.
func nestedArray(text string) bool {
	var elems []any
	if err := json.Unmarshal([]byte(text), &elems); err != nil {
		return false
	}
	for _, e := range elems {
		if _, ok := e.([]any); ok {
			return true
		}
	}
	return false
}

// multidimensionalReason is why an array of more than one dimension is no
// value of a fixture file, which can only write it as a sequence of
// sequences: bun v1.2.18 writes a nested slice as text PostgreSQL refuses.
const multidimensionalReason = "bun cannot write a nested slice into an array column, so dbfixture fails to load it"

// exportLiteral writes a value the way the column's type reads back as the
// same value through the Go field a bun model has for that type -- an int64,
// a float64, a bool, a time.Time, a []byte, a map or a slice, a string -- and
// the way this tool reads it back too. The second result is a note for the
// comment on the line. A value no fixture file can write so that both read it
// back as itself is refused with the reason, because an export that does not
// reproduce the database it was taken from is worse than no export.
func exportLiteral(model, col, lit string, column dbschema.Column) (string, string, error) {
	refuse := func(why string, args ...any) (string, string, error) {
		return "", "", fmt.Errorf("%s.%s holds %s, %s", model, col, strconv.Quote(lit), fmt.Sprintf(why, args...))
	}
	switch {
	case isJSON(column):
		return exportJSON(model, col, lit)
	case column.Category == "A":
		if !jsonArray(lit) {
			return refuse("an array whose lower bound is not 1, which no YAML sequence loads as: " +
				"renumber it from 1 in the database, or ignore the column")
		}
		if !isJSONElem(column) && nestedArray(lit) {
			return refuse("an array of more than one dimension, which a fixture file can only write as a sequence " +
				"of sequences, and " + multidimensionalReason + ": put the column in ignore")
		}
		if column.ElemCategory == "N" && strings.Contains(lit, `"`) {
			return refuse("an array of numbers with NaN or Infinity in it, which a YAML sequence " +
				"cannot spell so that this tool reads it back")
		}
		return yamlSafeJSON(jsonbText(lit)), "", nil
	}
	switch column.Type {
	case "int2", "int4", "int8", "oid":
		if _, ok := yamlInt(lit); ok && !strings.ContainsAny(lit, "xXoObB_") {
			return lit, "", nil
		}
	case "float4", "float8":
		switch lit {
		case "NaN":
			return ".nan", "", nil
		case "Infinity":
			return ".inf", "", nil
		case "-Infinity":
			return "-.inf", "", nil
		}
		if _, ok := canonicalDecimal(lit); ok {
			return lit, "", nil
		}
	case "numeric", "money":
		switch lit {
		case "NaN", "Infinity", "-Infinity":
			return refuse("which a string field loads only from %q and a float64 field only from %s: no "+
				"spelling reads back as itself through both", lit, map[string]string{
				"NaN": ".nan", "Infinity": ".inf", "-Infinity": "-.inf"}[lit])
		}
		if _, ok := canonicalDecimal(lit); ok {
			return lit, "", nil
		}
	case "bool":
		switch strings.ToLower(lit) {
		case "true", "t":
			return "true", "", nil
		case "false", "f":
			return "false", "", nil
		}
	case "date", "timestamp", "timestamptz":
		if s, ok := exportTimestamp(column.Type, lit); ok {
			return s, "", nil
		}
		if lit == "infinity" || lit == "-infinity" {
			return yamlString(lit), "a time.Time field cannot hold " + lit + ": dbfixture loads it into a " +
				"string field or another type that reads it", nil
		}
	case "bytea":
		return exportBytea(model, col, lit)
	}
	return exportString(lit), "", nil
}

// exportString writes text as a YAML string that dbfixture stores exactly. A
// value holding "{{ " and " }}" is a template to dbfixture, which evaluates it
// instead of storing it; written as a template whose only action is that text
// as a Go string literal, '{{ "Hello {{ name }}" }}', it evaluates to itself.
func exportString(s string) string {
	if anyTemplate.MatchString(s) {
		return yamlString("{{ " + strconv.Quote(s) + " }}")
	}
	return yamlString(s)
}

// quotedTemplate is the text of a template whose only action is a Go string
// literal, which is what dbfixture stores for it; the second result is false
// for any other text.
func quotedTemplate(s string) (string, bool) {
	if !strings.HasPrefix(s, "{{ ") || !strings.HasSuffix(s, " }}") {
		return "", false
	}
	tree, err := parse.Parse("", s, "{{", "}}")
	if err != nil {
		return "", false
	}
	root := tree[""]
	if root == nil || len(root.Root.Nodes) != 1 {
		return "", false
	}
	action, ok := root.Root.Nodes[0].(*parse.ActionNode)
	if !ok || len(action.Pipe.Decl) != 0 || len(action.Pipe.Cmds) != 1 || len(action.Pipe.Cmds[0].Args) != 1 {
		return "", false
	}
	str, ok := action.Pipe.Cmds[0].Args[0].(*parse.StringNode)
	if !ok {
		return "", false
	}
	return str.Text, true
}

// exportJSON writes a json or jsonb value. A document is a flow mapping or
// sequence, which dbfixture decodes into a map, a slice or an any field, and
// whose numbers it decodes into float64. A string is a YAML string to an any
// field; a number, true and false are themselves.
func exportJSON(model, col, lit string) (string, string, error) {
	refuse := func(why string) (string, string, error) {
		return "", "", fmt.Errorf("%s.%s holds the JSON %s, %s", model, col, jsonbText(lit), why)
	}
	dec := json.NewDecoder(strings.NewReader(lit))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return refuse("which is not JSON this tool can read")
	}
	if n := beyondFloat64(v); n != "" {
		return refuse(fmt.Sprintf("and %s in it has more digits than the float64 dbfixture decodes a YAML number "+
			"into, so a fresh seed would store %s: store that, or keep the number as a JSON string", n,
			float64Text(n)))
	}
	switch v := v.(type) {
	case nil:
		return refuse("null, which a fixture file can only write as ~, and a ~ is NULL to this tool: " +
			"store NULL instead, or give the model defaults: {" + col + ": 'null'}, and the export leaves " +
			"the column out of these rows, which a map, slice or any field loads as the JSON null")
	case string:
		if json.Valid([]byte(v)) {
			return refuse("a string that is itself JSON: a YAML string is that JSON to this tool and to a " +
				"string field, and a JSON string only to an any field")
		}
		return exportString(v), "", nil
	case map[string]any, []any:
		return yamlSafeJSON(jsonbText(lit)), "", nil
	}
	return lit, "", nil
}

// jsonNullByDefault reports a JSON null in a json or jsonb column of a model
// whose defaults say a row without the column holds the JSON null.
func jsonNullByDefault(m *Model, c dbschema.Column, v fixturechange.Value) bool {
	def, ok := m.Defaults[c.Name]
	return ok && isJSON(c) && !v.IsNull && v.Ref == nil && strings.TrimSpace(v.Lit) == "null" &&
		strings.TrimSpace(def) == "null"
}

// beyondFloat64 is the first number in a decoded JSON value that does not
// survive what yaml.v3 decodes it into for an any field, "" when every one
// does: a whole number of 64 bits stays an integer, anything else becomes a
// float64.
func beyondFloat64(v any) string {
	switch v := v.(type) {
	case json.Number:
		canon, ok := canonicalDecimal(v.String())
		if !ok {
			return ""
		}
		if _, err := strconv.ParseInt(canon, 10, 64); err == nil {
			return ""
		}
		if _, err := strconv.ParseUint(canon, 10, 64); err == nil {
			return ""
		}
		if float64Text(v.String()) != canon {
			return v.String()
		}
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if n := beyondFloat64(v[k]); n != "" {
				return n
			}
		}
	case []any:
		for _, e := range v {
			if n := beyondFloat64(e); n != "" {
				return n
			}
		}
	}
	return ""
}

// float64Text is a number as a float64 holds it, written canonically.
func float64Text(n string) string {
	f, err := strconv.ParseFloat(n, 64)
	if err != nil {
		return ""
	}
	canon, _ := canonicalDecimal(strconv.FormatFloat(f, 'g', -1, 64))
	return canon
}

// exportBytea writes a bytea value as the sequence of byte values a []byte
// field loads, which is the only YAML yaml.v3 decodes into one.
func exportBytea(model, col, lit string) (string, string, error) {
	data, err := hex.DecodeString(strings.TrimPrefix(lit, `\x`))
	if err != nil || !strings.HasPrefix(lit, `\x`) {
		return "", "", fmt.Errorf("%s.%s holds %q, which is not bytea in hex", model, col, lit)
	}
	parts := make([]string, len(data))
	for i, c := range data {
		parts[i] = strconv.Itoa(int(c))
	}
	return "[" + strings.Join(parts, ", ") + "]", "", nil
}

// hazardComment is the warning that goes next to a value the fixture loader
// will not write back. See LintZeroDefaults: bun sends DEFAULT for a zero in a
// column that has one, so this exact line, loaded back, produces something
// else.
func hazardComment(v fixturechange.Value, column dbschema.Column) string {
	if v.IsNull {
		def, ok := column.NonNullDefault()
		if !ok {
			if isJSON(column) {
				return "ROUND-TRIP HAZARD: a map, slice or any field loads ~ as the JSON null, and only a nil " +
					"pointer or a nullzero field as the NULL this column holds"
			}
			return ""
		}
		return fmt.Sprintf("ROUND-TRIP HAZARD: the column defaults to %s and bun writes DEFAULT for a nil pointer "+
			"or a nullzero field, so loading this file stores %s here, not NULL", def, def)
	}
	if v.Ref != nil {
		return ""
	}
	zero, known := column.ZeroText()
	if !known || !sameScalar(v.Lit, zero) {
		return ""
	}
	hazard, stored := column.ZeroIsNotDefault()
	if !hazard {
		return ""
	}
	return fmt.Sprintf("ROUND-TRIP HAZARD: the column defaults to %s and bun writes DEFAULT for a zero, "+
		"so loading this file stores %s here, not %s", stored, stored, zero)
}

// yamlScalar writes a value the way its column type reads back; see
// exportLiteral. Anything it cannot write is written as a string.
func yamlScalar(text, typ string) string {
	out, _, err := exportLiteral("", "", text, dbschema.Column{Type: typ})
	if err != nil {
		return yamlString(text)
	}
	return out
}

// yamlAnchor writes a row anchor. It is a slug, so it is written plain unless
// YAML would read it as something other than a string.
func yamlAnchor(anchor string) string {
	if anchor == "" || anchor[0] < 'a' || anchor[0] > 'z' {
		return yamlString(anchor)
	}
	switch anchor {
	case "true", "false", "null", "yes", "no", "on", "off", "y", "n":
		return yamlString(anchor)
	}
	return anchor
}

// yamlString writes a double-quoted YAML scalar. Double quotes because the
// escapes are the ones every reader agrees on. Everything YAML does not take
// as printable is escaped, and so are the characters it reads as line breaks
// and folds into a space: NEL, U+2028 and U+2029.
func yamlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case 0x85:
			b.WriteString(`\N`)
		case 0x2028:
			b.WriteString(`\L`)
		case 0x2029:
			b.WriteString(`\P`)
		default:
			if !yamlPrintable(r) {
				writeYAMLEscape(&b, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// yamlPrintable is YAML 1.2's c-printable, less the byte-order mark, which a
// reader may take for the start of a stream.
func yamlPrintable(r rune) bool {
	switch {
	case r == 0x09 || r == 0x0A || r == 0x0D:
		return true
	case r >= 0x20 && r <= 0x7E:
		return true
	case r == 0x85:
		return true
	case r >= 0xA0 && r <= 0xD7FF:
		return true
	case r >= 0xE000 && r <= 0xFFFD:
		return r != 0xFEFF
	case r >= 0x10000 && r <= 0x10FFFF:
		return true
	}
	return false
}

func writeYAMLEscape(b *strings.Builder, r rune) {
	switch {
	case r < 0x100:
		fmt.Fprintf(b, `\x%02x`, r)
	case r < 0x10000:
		fmt.Fprintf(b, `\u%04x`, r)
	default:
		fmt.Fprintf(b, `\U%08x`, r)
	}
}

// yamlSafeJSON makes JSON text safe to write as YAML flow: inside a string,
// every character YAML does not print as itself is escaped the way both JSON
// and YAML read it, \uXXXX. jsonb leaves DEL, the C1 controls, NEL and the
// line separators as they are, and YAML refuses or folds them.
func yamlSafeJSON(text string) string {
	var b strings.Builder
	for _, r := range text {
		if r == 0x85 || r == 0x2028 || r == 0x2029 || !yamlPrintable(r) {
			// Below U+10000: everything YAML does not print is.
			fmt.Fprintf(&b, `\u%04x`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// camel turns a column name back into the Go field name a dbfixture template
// has to spell, which is the inverse of bun's default naming.
func camel(col string) string {
	parts := strings.Split(col, "_")
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		if up, ok := initialisms[p]; ok {
			b.WriteString(up)
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]) + p[1:])
	}
	return b.String()
}

// initialisms are the column names bun's own naming turns into all-caps field
// names. A project that spells them differently has to say so, which is what
// the id column of the configuration is for.
var initialisms = map[string]string{"id": "ID", "url": "URL", "uri": "URI", "api": "API", "uuid": "UUID"}

// FindingsByKind groups a snapshot's findings for a report.
func FindingsByKind(findings []Finding) map[FindingKind][]Finding {
	out := map[FindingKind][]Finding{}
	for _, f := range findings {
		out[f.Kind] = append(out[f.Kind], f)
	}
	for _, list := range out {
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].Model != list[j].Model {
				return list[i].Model < list[j].Model
			}
			return list[i].Row < list[j].Row
		})
	}
	return out
}
