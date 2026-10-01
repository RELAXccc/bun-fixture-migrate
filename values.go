package fixturemigrate

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"

	"gopkg.in/yaml.v3"
)

// A value's text is what gets compared and what a migration writes, so it has
// to be exactly the value, spelled one way.
//
// For a fixture scalar the YAML type decides, because that is what decides
// what dbfixture hands bun: a string is kept byte for byte, so "01234" stays a
// postcode and "  x  " keeps its spaces; an integer is resolved the way YAML
// resolves it, so 017 is 15 and 0x1F is 31, as dbfixture stores them; a
// decimal is written out exactly, never through float64, so 9007199254740993
// stays itself. On the database side the column type decides: numbers are
// written the same way, everything else is PostgreSQL's own text.

// scalarText is the text of a fixture scalar by its YAML type.
func scalarText(c Cell) string {
	switch c.Tag {
	case "!!int":
		if s, ok := yamlInt(c.Text); ok {
			return s
		}
	case "!!float":
		if s, ok := yamlFloat(c.Text); ok {
			return s
		}
	case "!!bool":
		return strings.ToLower(c.Text)
	case "!!timestamp":
		if s, ok := yamlTimestamp(c.Text); ok {
			return s
		}
	case "!!binary":
		// yaml.v3 hands a string field the bytes the base64 stands for,
		// and so dbfixture stores those. Text that is not base64 fails to
		// load at all, and is kept as written for the database to refuse.
		if b, err := base64.StdEncoding.DecodeString(c.Text); err == nil {
			return string(b)
		}
	}
	return c.Text
}

// yamlTimestampLayouts are yaml.v3's (resolve.go, allowedTimestampFormats).
var yamlTimestampLayouts = []string{
	"2006-1-2T15:4:5.999999999Z07:00",
	"2006-1-2t15:4:5.999999999Z07:00",
	"2006-1-2 15:4:5.999999999",
	"2006-1-2",
}

// yamlTimestamp resolves a YAML timestamp the way yaml.v3 does and writes it
// one way: a date alone as 2006-01-02, anything with a time as RFC 3339 in
// UTC. A timestamp without a zone is UTC to yaml.v3, and so to dbfixture, and
// bun writes the time.Time it becomes with its offset: that instant is what
// the database holds.
func yamlTimestamp(text string) (string, bool) {
	for _, layout := range yamlTimestampLayouts {
		t, err := time.Parse(layout, text)
		if err != nil {
			continue
		}
		if layout == "2006-1-2" {
			return t.Format("2006-01-02"), true
		}
		return t.UTC().Format(time.RFC3339Nano), true
	}
	return "", false
}

// exportTimestamp writes a date or a timestamp from the database as a YAML
// timestamp, which yaml.v3 decodes into a time.Time field; a quoted string it
// only decodes when it is RFC 3339. The second result is false for a value
// that is not one, such as infinity, which stays a string.
func exportTimestamp(typ, text string) (string, bool) {
	switch typ {
	case "date":
		if t, err := time.Parse("2006-01-02", text); err == nil {
			return t.Format("2006-01-02"), true
		}
	case "timestamp":
		// Written in RFC 3339 with a Z, which is how a YAML timestamp without
		// a zone resolves anyway: then the value reads back as written, and
		// nothing has to ask the column's type what 2026-01-01 10:00:00 is.
		if t, err := time.Parse("2006-01-02 15:04:05.999999999", text); err == nil {
			return t.UTC().Format(time.RFC3339Nano), true
		}
	case "timestamptz":
		for _, layout := range []string{"2006-01-02 15:04:05.999999999Z07", "2006-01-02 15:04:05.999999999Z07:00"} {
			if t, err := time.Parse(layout, text); err == nil {
				return t.UTC().Format(time.RFC3339Nano), true
			}
		}
	}
	return "", false
}

// yamlInt resolves a YAML integer exactly: the sign, the 0x, 0o and 0b
// prefixes, a leading 0 as octal and the underscores yaml.v3 accepts, with no
// limit on the size. yaml.v3 parses with strconv.ParseInt(plain, 0, 64), which
// has the same rules as big.Int.SetString(plain, 0).
func yamlInt(text string) (string, bool) {
	plain := strings.ReplaceAll(text, "_", "")
	n, ok := new(big.Int).SetString(plain, 0)
	if !ok {
		return "", false
	}
	return n.String(), true
}

// yamlFloat resolves a YAML float exactly, and spells infinity and NaN the way
// PostgreSQL does, which is what bun writes for a float64 holding one.
func yamlFloat(text string) (string, bool) {
	switch text {
	case ".inf", ".Inf", ".INF", "+.inf", "+.Inf", "+.INF":
		return "Infinity", true
	case "-.inf", "-.Inf", "-.INF":
		return "-Infinity", true
	case ".nan", ".NaN", ".NAN":
		return "NaN", true
	}
	return canonicalDecimal(strings.ReplaceAll(text, "_", ""))
}

// maxExponent bounds the exponent canonicalDecimal expands, so "1e999999999"
// cannot make it write a billion zeros.
const maxExponent = 1000

// canonicalDecimal writes a decimal number in one spelling: no sign for zero
// or a positive number, no leading zeros, no trailing zeros after the point,
// no point for an integer, no exponent. It is exact: "1.0", "1e0" and "10e-1"
// are all "1", and 9007199254740993 stays 9007199254740993. The second result
// is false for anything that is not a decimal number.
func canonicalDecimal(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	neg := false
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		neg, s = true, s[1:]
	}
	mantissa, exponent := s, 0
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mantissa = s[:i]
		e, err := strconv.Atoi(s[i+1:])
		if err != nil || e > maxExponent || e < -maxExponent {
			return "", false
		}
		exponent = e
	}
	intPart, fracPart := mantissa, ""
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		intPart, fracPart = mantissa[:i], mantissa[i+1:]
	}
	if intPart == "" && fracPart == "" {
		return "", false
	}
	for _, part := range []string{intPart, fracPart} {
		for i := 0; i < len(part); i++ {
			if part[i] < '0' || part[i] > '9' {
				return "", false
			}
		}
	}
	// digits × 10^scale, with the point moved into the exponent.
	digits := strings.TrimLeft(intPart+fracPart, "0")
	scale := exponent - len(fracPart)
	if digits == "" {
		return "0", true
	}
	for len(digits) > 1 && digits[len(digits)-1] == '0' {
		digits = digits[:len(digits)-1]
		scale++
	}
	var out string
	switch {
	case scale >= 0:
		out = digits + strings.Repeat("0", scale)
	case -scale < len(digits):
		out = digits[:len(digits)+scale] + "." + digits[len(digits)+scale:]
	default:
		out = "0." + strings.Repeat("0", -scale-len(digits)) + digits
	}
	if neg {
		out = "-" + out
	}
	return out, true
}

// numericType reports a PostgreSQL type whose values compare as numbers.
func numericType(typ string) bool {
	switch typ {
	case "int2", "int4", "int8", "numeric", "float4", "float8", "money", "oid":
		return true
	}
	return false
}

// columnText is how a value of a column is compared and written once
// PostgreSQL has spelled it: a number canonically, the numbers inside JSON and
// inside an array canonically, anything else as it is.
func columnText(c dbschema.Column, text string) string {
	switch {
	case numericType(c.Type):
		if s, ok := canonicalDecimal(text); ok {
			return s
		}
	case c.Type == "json" || c.Type == "jsonb" || c.Category == "A":
		return canonicalJSON(text)
	}
	return text
}

// sameScalar reports whether two texts are the same value when either could
// be a number: the zero checks compare a value with "0", "false" or "".
func sameScalar(a, b string) bool {
	if a == b {
		return true
	}
	ca, okA := canonicalDecimal(a)
	cb, okB := canonicalDecimal(b)
	return okA && okB && ca == cb
}

// yamlJSON writes a YAML mapping or sequence as JSON, one way: keys sorted
// the way encoding/json sorts a map's, no spaces, every scalar by its YAML
// type -- an integer exactly, a timestamp as RFC 3339 -- so two spellings of
// the same structure compare equal. It is what a jsonb column and an array
// column are compared and written as; PostgreSQL turns the JSON into either.
//
// Inside a mapping a value is what dbfixture's map[string]any hands
// encoding/json: a key as it is written, a timestamp as the time.Time yaml.v3
// makes of it (RFC 3339 in its own offset, a date at midnight UTC), !!binary
// as the text it encodes. A sequence that is not inside a mapping is an array
// column's, whose elements a slice field gets the way a column gets a
// scalar.
func yamlJSON(n *yaml.Node) (string, error) {
	var b strings.Builder
	if err := writeYAMLJSON(&b, n, false); err != nil {
		return "", err
	}
	return b.String(), nil
}

func writeYAMLJSON(b *strings.Builder, n *yaml.Node, inMapping bool) error {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) != 1 {
			return fmt.Errorf("an empty document")
		}
		return writeYAMLJSON(b, n.Content[0], inMapping)
	case yaml.AliasNode:
		return writeYAMLJSON(b, n.Alias, inMapping)
	case yaml.MappingNode:
		type pair struct {
			key   string
			value *yaml.Node
		}
		pairs := make([]pair, 0, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Kind == yaml.AliasNode && key.Alias != nil {
				key = key.Alias
			}
			if key.Kind != yaml.ScalarNode || key.ShortTag() == "!!merge" || key.ShortTag() == "!!null" {
				return fmt.Errorf("line %d: a mapping key JSON cannot hold", key.Line)
			}
			// A map[string]any gets a key as it is written, whatever it
			// resolves to: 017 stays "017".
			k := key.Value
			if key.ShortTag() == "!!binary" {
				k = scalarText(Cell{Text: key.Value, Tag: "!!binary"})
			}
			pairs = append(pairs, pair{k, n.Content[i+1]})
		}
		sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })
		b.WriteByte('{')
		for i, p := range pairs {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(jsonString(p.key))
			b.WriteByte(':')
			if err := writeYAMLJSON(b, p.value, true); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case yaml.SequenceNode:
		b.WriteByte('[')
		for i, item := range n.Content {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeYAMLJSON(b, item, inMapping); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case yaml.ScalarNode:
		text := scalarText(Cell{Text: n.Value, Tag: n.ShortTag()})
		switch n.ShortTag() {
		case "!!null":
			b.WriteString("null")
		case "!!bool", "!!int":
			b.WriteString(text)
		case "!!float":
			if _, ok := canonicalDecimal(text); !ok {
				return fmt.Errorf("line %d: %s has no JSON spelling", n.Line, n.Value)
			}
			if inMapping {
				// map[string]any holds a float as a float64, so
				// 0.1234567890123456789 is stored as 0.12345678901234568.
				f, err := strconv.ParseFloat(strings.ReplaceAll(n.Value, "_", ""), 64)
				if err != nil {
					return fmt.Errorf("line %d: %s has no JSON spelling", n.Line, n.Value)
				}
				j, err := json.Marshal(f)
				if err != nil {
					return fmt.Errorf("line %d: %s has no JSON spelling", n.Line, n.Value)
				}
				b.Write(j)
				return nil
			}
			b.WriteString(text)
		case "!!timestamp":
			if inMapping {
				if t, ok := yamlTime(n.Value); ok {
					j, err := t.MarshalJSON()
					if err != nil {
						return fmt.Errorf("line %d: %s: %w", n.Line, n.Value, err)
					}
					b.Write(j)
					return nil
				}
			}
			b.WriteString(jsonString(text))
		default:
			b.WriteString(jsonString(text))
		}
	default:
		return fmt.Errorf("line %d: a YAML node JSON cannot hold", n.Line)
	}
	return nil
}

// yamlTime is the time.Time yaml.v3 makes of a timestamp.
func yamlTime(text string) (time.Time, bool) {
	for _, layout := range yamlTimestampLayouts {
		if t, err := time.Parse(layout, text); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// jsonString quotes a string for JSON, without encoding/json's escaping of
// <, > and &, which is for HTML and would only make the text harder to read.
func jsonString(s string) string {
	if !utf8.ValidString(s) {
		// encoding/json writes U+FFFD for every byte that is not UTF-8.
		var v strings.Builder
		for i := 0; i < len(s); {
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				v.WriteRune(utf8.RuneError)
			} else {
				v.WriteString(s[i : i+size])
			}
			i += size
		}
		s = v.String()
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

// canonicalJSON writes every number of a JSON text the way canonicalDecimal
// does and leaves the rest -- key order, spacing, strings -- as it is. jsonb
// keeps the scale a number was written with, so {"a": 1.0} written by SQL
// reads back as 1.0 where the fixture file's {a: 1} reads back as 1; jsonb's
// equality, and every application reading it, hold them the same, and so,
// after this, does the text. Text that is not JSON comes back unchanged.
func canonicalJSON(text string) string {
	if !json.Valid([]byte(text)) {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		c := text[i]
		switch {
		case c == '"':
			j := i + 1
			for j < len(text) && text[j] != '"' {
				if text[j] == '\\' {
					j++
				}
				j++
			}
			b.WriteString(text[i:min(j+1, len(text))])
			i = j + 1
		case c == '-' || (c >= '0' && c <= '9'):
			j := i + 1
			for j < len(text) && strings.IndexByte("+-.eE0123456789", text[j]) >= 0 {
				j++
			}
			if canon, ok := canonicalDecimal(text[i:j]); ok {
				b.WriteString(canon)
			} else {
				b.WriteString(text[i:j])
			}
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}
