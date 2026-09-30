package fixturemigrate

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

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
	var b strings.Builder
	for _, line := range header {
		if line == "" {
			b.WriteString("#\n")
			continue
		}
		b.WriteString("# " + line + "\n")
	}
	if len(header) > 0 {
		b.WriteString("\n")
	}

	anchors := map[string]map[string]string{} // model -> ref value -> anchor
	for _, model := range snap.Order {
		m := cfg.Models[model]
		anchors[model] = map[string]string{}
		for _, e := range snap.Entries[model] {
			if v, ok := e.Cells[m.Ref]; ok && v.Ref == nil && !v.IsNull {
				anchors[model][v.Lit] = e.Anchor
			}
		}
	}

	for _, model := range snap.Order {
		m := cfg.Models[model]
		table := tables[cfg.QualifiedTable(m)]
		if table == nil {
			return nil, fmt.Errorf("model %q: %s is not a table in this database", model, cfg.QualifiedTable(m))
		}
		entries := snap.Entries[model]
		if len(entries) == 0 {
			continue
		}
		fmt.Fprintf(&b, "- model: %s\n  rows:\n", model)
		cols := exportColumns(m, table, snap.Columns[model])
		for _, e := range entries {
			first := true
			write := func(col, value, comment string) {
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
			}
			write(anchorColumn, yamlAnchor(e.Anchor), "")
			// An identity GENERATED ALWAYS refuses an explicit id from
			// dbfixture as from anybody, so the file names rows by anchor
			// only and the database numbers them.
			if idCol, _ := table.Column(m.ID); e.ID != "" && !idCol.IdentityAlways {
				write(m.ID, yamlScalar(e.ID, idType(table, m.ID)), "")
			}
			for _, col := range cols {
				v, ok := e.Cells[col]
				if !ok {
					continue
				}
				column, _ := table.Column(col)
				text, err := exportValue(cfg, model, col, v, column, anchors)
				if err != nil {
					return nil, err
				}
				write(col, text, hazardComment(v, column))
			}
		}
		b.WriteString("\n")
	}
	return []byte(strings.TrimRight(b.String(), "\n") + "\n"), nil
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

func idType(table *dbschema.Table, id string) string {
	if c, ok := table.Column(id); ok {
		return c.Type
	}
	return "text"
}

func exportValue(cfg *Config, model, col string, v fixturechange.Value, column dbschema.Column,
	anchors map[string]map[string]string) (string, error) {

	switch {
	case v.IsNull:
		return "~", nil
	case v.Ref != nil:
		target := cfg.Models[v.Ref.Model]
		anchor, ok := anchors[v.Ref.Model][v.Ref.Key]
		if !ok {
			return "", fmt.Errorf("%s.%s points at %s %q, which is not in the export", model, col, v.Ref.Model, v.Ref.Key)
		}
		return fmt.Sprintf("'{{ $.%s.%s.%s }}'", v.Ref.Model, anchor, camel(target.ID)), nil
	}
	// dbfixture evaluates any string holding "{{ ... }}" as a template when it
	// loads the file, whatever the column, so this value cannot be written
	// down in a way that loads back as itself.
	if anyTemplate.MatchString(v.Lit) {
		return "", fmt.Errorf("%s.%s holds %q, which dbfixture would evaluate as a template when it loads the "+
			"file instead of storing it; the export cannot reproduce it: change the value, or ignore the column",
			model, col, v.Lit)
	}
	return yamlScalar(v.Lit, column.Type), nil
}

// hazardComment is the warning that goes next to a value the fixture loader
// will not write back. See LintZeroDefaults: bun sends DEFAULT for a zero in a
// column that has one, so this exact line, loaded back, produces something
// else.
func hazardComment(v fixturechange.Value, column dbschema.Column) string {
	if v.IsNull {
		def, ok := column.NonNullDefault()
		if !ok {
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

// yamlScalar writes a value the way its column type reads back. A number stays
// a number, a boolean stays a boolean, and everything else is quoted, because
// an unquoted "yes", "01" or "1.0" is not the string it looks like.
func yamlScalar(text, typ string) string {
	switch typ {
	case "int2", "int4", "int8":
		if _, err := strconv.ParseInt(text, 10, 64); err == nil {
			return text
		}
	case "float4", "float8", "numeric", "money":
		if _, err := strconv.ParseFloat(text, 64); err == nil {
			return text
		}
	case "bool":
		switch strings.ToLower(text) {
		case "true", "t":
			return "true"
		case "false", "f":
			return "false"
		}
	case "date", "timestamp", "timestamptz":
		if s, ok := exportTimestamp(typ, text); ok {
			return s
		}
	}
	return yamlString(text)
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
// escapes are the ones every reader agrees on.
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
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\x%02x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
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
