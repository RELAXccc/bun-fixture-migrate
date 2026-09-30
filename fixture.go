package fixturemigrate

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// anchorColumn is dbfixture's row anchor: not a database column, just the name
// other rows use in a template reference.
const anchorColumn = "_id"

// Cell is one column of a fixture row.
type Cell struct {
	// Text is the scalar as it was written, with no conversion. Numbers keep
	// the notation of the file; scalarText resolves them.
	Text string
	// Tag is the YAML type the scalar resolved to, "!!str", "!!int",
	// "!!float", "!!bool", "!!timestamp", and "" for a value that did not come
	// from YAML, such as a configured default. It decides what the text
	// means: 017 is the integer 15, "017" is a string.
	Tag string
	// IsNull is true for an explicit YAML null.
	IsNull bool
	// Structured is true when the value was a mapping or a sequence. The
	// generator cannot turn one into a column value and says so if such a
	// column takes part in a diff.
	Structured bool
}

// Row is one fixture row.
type Row map[string]Cell

// DocModel is one "- model: X / rows: [...]" block.
type DocModel struct {
	Name string
	Rows []Row
}

// Doc is a parsed fixture file, in file order.
type Doc []DocModel

// ParseDoc reads a dbfixture YAML file.
func ParseDoc(data []byte) (Doc, error) {
	var blocks []struct {
		Model string                 `yaml:"model"`
		Rows  []map[string]yaml.Node `yaml:"rows"`
	}
	if err := yaml.Unmarshal(data, &blocks); err != nil {
		return nil, fmt.Errorf("parse fixture file: %w", err)
	}
	doc := make(Doc, 0, len(blocks))
	for _, b := range blocks {
		if b.Model == "" {
			return nil, fmt.Errorf("parse fixture file: a block has no model name")
		}
		dm := DocModel{Name: b.Model, Rows: make([]Row, 0, len(b.Rows))}
		for _, raw := range b.Rows {
			row := make(Row, len(raw))
			for col, node := range raw {
				cell, err := cellOf(node)
				if err != nil {
					return nil, fmt.Errorf("parse fixture file: %s.%s: %w", b.Model, col, err)
				}
				row[col] = cell
			}
			dm.Rows = append(dm.Rows, row)
		}
		doc = append(doc, dm)
	}
	return doc, nil
}

func cellOf(node yaml.Node) (Cell, error) {
	switch {
	case node.Tag == "!!null":
		return Cell{IsNull: true}, nil
	case node.Kind == yaml.ScalarNode:
		return Cell{Text: node.Value, Tag: node.ShortTag()}, nil
	case node.Kind == 0:
		return Cell{IsNull: true}, nil
	default:
		out, err := yaml.Marshal(&node)
		if err != nil {
			return Cell{}, err
		}
		return Cell{Text: strings.TrimSpace(string(out)), Structured: true}, nil
	}
}

// Str returns a column's text, "" when the column is absent or null.
func (r Row) Str(col string) string { return r[col].Text }

// template matches a whole-value dbfixture reference, "{{ $.Model.row.Field }}".
// The delimiters are dbfixture's own: it only evaluates a value holding
// "{{ " and " }}" with the spaces (dbfixture/fixture.go, tplRE), so
// "{{$.Model.row.ID}}" is not a template to it and is not one here.
//
// The row is an identifier too: text/template reads "$.Model.row.Field" as a
// chain of field names, so an anchor such as "my-row" or "1_month" cannot be
// named in a template at all, however dbfixture registered it.
var template = regexp.MustCompile(`^\{\{ \s*\$\.([A-Za-z_][A-Za-z0-9_]*)\.([A-Za-z_][A-Za-z0-9_]*)\.([A-Za-z_][A-Za-z0-9_]*)\s* \}\}$`)

// looseTemplate is the same reference with any row name, so a row name
// text/template cannot parse gets a message about that rather than about
// templates in general.
var looseTemplate = regexp.MustCompile(`^\{\{ \s*\$\.([A-Za-z_][A-Za-z0-9_]*)\.([^\s{}]+)\.([A-Za-z_][A-Za-z0-9_]*)\s* \}\}$`)

// anyTemplate is dbfixture's test for "evaluate this value as a template"
// (tplRE). A value it matches never reaches the database as written: dbfixture
// replaces it with whatever the template produces.
var anyTemplate = regexp.MustCompile(`\{\{ .+ \}\}`)

// underscore is bun's default column name for a Go field name, so a template
// that names a field ("ID", "GroupName") can be matched against a column.
func underscore(s string) string {
	isUpper := func(c byte) bool { return c >= 'A' && c <= 'Z' }
	isLower := func(c byte) bool { return c >= 'a' && c <= 'z' }
	out := make([]byte, 0, len(s)+5)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isUpper(c) {
			out = append(out, c)
			continue
		}
		if i > 0 && i+1 < len(s) && (isLower(s[i-1]) || isLower(s[i+1])) {
			out = append(out, '_')
		}
		out = append(out, c-'A'+'a')
	}
	return string(out)
}
