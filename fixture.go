package fixturemigrate

import (
	"bytes"
	"encoding/json"
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
	// Structured is true when the value was a mapping or a sequence. Text is
	// then its canonical JSON, which is what a jsonb, json or array column is
	// compared and written as.
	Structured bool
	// StringText is what a Go string field gets from this value when that is
	// not what the value resolves to, and "" otherwise. yaml.v3 decodes a
	// plain scalar into a string field as it is written: 1.10 stays "1.10"
	// there, and 017 stays "017", while an integer or a float field gets 15 or
	// 1.1. For a sequence it is the JSON array of its elements as written,
	// which is what a []string field gets. Which one the database holds
	// depends on the column's type.
	StringText string
	// Unsure says why the value is one thing to one Go field type and
	// another to another, which no column type settles and this tool cannot
	// see: a sequence holding a null, which yaml.v3 drops for a []string or
	// []int64 field and keeps for a []*string. "" for any other value.
	Unsure string
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
	// An alias (*name) is the node it names, to yaml.v3 and so to dbfixture,
	// with one difference: dbfixture evaluates a template only in a scalar
	// tagged !!str, and an alias has no tag. The text of a template reached
	// through an alias is stored as it is, which no other value of the file
	// does, so it is refused rather than read either way.
	if node.Kind == yaml.AliasNode {
		target := resolveAlias(&node)
		if target == nil {
			return Cell{}, fmt.Errorf("line %d: an alias of nothing", node.Line)
		}
		if target.Kind == yaml.ScalarNode && anyTemplate.MatchString(target.Value) {
			return Cell{}, fmt.Errorf("line %d: *%s stands for %s, which dbfixture stores as that text instead of "+
				"evaluating it, because it does not evaluate a template reached through an alias: write the "+
				"template itself here", node.Line, node.Value, target.Value)
		}
		return cellOf(*target)
	}
	switch {
	case node.Tag == "!!null":
		return Cell{IsNull: true}, nil
	case node.Kind == yaml.ScalarNode:
		c := Cell{Text: node.Value, Tag: node.ShortTag()}
		if scalarText(c) != c.Text {
			c.StringText = c.Text
		}
		return c, nil
	case node.Kind == 0:
		return Cell{IsNull: true}, nil
	default:
		text, err := yamlJSON(&node)
		if err != nil {
			return Cell{}, err
		}
		c := Cell{Text: text, Structured: true, StringText: sequenceAsWritten(&node)}
		if node.Kind == yaml.SequenceNode {
			for _, e := range node.Content {
				if e := resolveAlias(e); e != nil && e.ShortTag() == "!!null" {
					c.Unsure = "it is a sequence holding a null, which yaml.v3 leaves out of a []string or []int64 " +
						"field and keeps in a []*string one, and only the model says which it has: leave the null " +
						"out, which every field reads the same way"
					break
				}
			}
		}
		return c, nil
	}
}

// resolveAlias is the node an alias names, through any number of aliases,
// and any other node itself; nil for an alias of nothing.
func resolveAlias(n *yaml.Node) *yaml.Node {
	for i := 0; n != nil && n.Kind == yaml.AliasNode; i++ {
		if i > 100 {
			return nil
		}
		n = n.Alias
	}
	return n
}

// sequenceAsWritten is a sequence of scalars as a []string field gets it, as a
// JSON array, when that differs from the sequence's resolved JSON: "" for a
// mapping, for a sequence holding anything but plain scalars, and for one
// whose every element resolves to its own text.
func sequenceAsWritten(n *yaml.Node) string {
	if n.Kind != yaml.SequenceNode {
		return ""
	}
	texts := make([]string, 0, len(n.Content))
	differs := false
	for _, e := range n.Content {
		if e = resolveAlias(e); e == nil || e.Kind != yaml.ScalarNode || e.ShortTag() == "!!null" {
			return ""
		}
		if scalarText(Cell{Text: e.Value, Tag: e.ShortTag()}) != e.Value {
			differs = true
		}
		texts = append(texts, e.Value)
	}
	if !differs {
		return ""
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(texts); err != nil {
		return ""
	}
	return strings.TrimSuffix(b.String(), "\n")
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
