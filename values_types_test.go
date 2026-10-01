package fixturemigrate

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"

	"gopkg.in/yaml.v3"
)

// A !!binary scalar is the text it encodes, which is what yaml.v3 hands a
// string field and so what dbfixture stores.
func TestScalarTextDecodesBinary(t *testing.T) {
	if got := scalarText(Cell{Text: "SGVsbG8=", Tag: "!!binary"}); got != "Hello" {
		t.Fatal(got)
	}
	if got := scalarText(Cell{Text: "not base64!", Tag: "!!binary"}); got != "not base64!" {
		t.Fatal(got)
	}
}

// Inside a mapping a value is what encoding/json makes of what yaml.v3 puts
// into a map[string]any; a sequence outside one is an array column's.
func TestYAMLJSONIsWhatAMapMarshalsTo(t *testing.T) {
	for in, want := range map[string]string{
		`{launch: 2026-01-01, at: 2026-01-01T10:00:00+02:00, n: 2026-01-01 10:00:00.1234567}`:  `{"at":"2026-01-01T10:00:00+02:00","launch":"2026-01-01T00:00:00Z","n":"2026-01-01T10:00:00.1234567Z"}`,
		`{big: 123456789012345678901234567890, prec: 0.1234567890123456789, one: 1.0, e: 1e3}`: `{"big":1.2345678901234568e+29,"e":1000,"one":1,"prec":0.12345678901234568}`,
		`{017: 017, 0x1F: 0x1F, true: yes}`:                                                    `{"017":15,"0x1F":31,"true":"yes"}`,
		`{bin: !!binary SGk=, list: [2026-01-02, 1.50]}`:                                       `{"bin":"Hi","list":["2026-01-02T00:00:00Z",1.5]}`,
		`[2026-01-02, 1.50, 017, 12345678901234567890123]`:                                     `["2026-01-02",1.5,15,12345678901234567890123]`,
		`[[1, 2], [3, ~]]`:   `[[1,2],[3,null]]`,
		`[{at: 2026-01-01}]`: `[{"at":"2026-01-01T00:00:00Z"}]`,
	} {
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(in), &n); err != nil {
			t.Fatal(err)
		}
		got, err := yamlJSON(&n)
		if err != nil || got != want {
			t.Errorf("%s:\n got %s %v\nwant %s", in, got, err, want)
		}
	}
}

// jsonb keeps the scale of a number; both sides of a comparison are written
// without it.
func TestCanonicalJSON(t *testing.T) {
	for in, want := range map[string]string{
		`{"a": 1.0, "b": 1.50, "c": [1.0, -0.0, 1e3], "d": "1.0"}`: `{"a": 1, "b": 1.5, "c": [1, 0, 1000], "d": "1.0"}`,
		`["a\"1.0", 2.50]`:               `["a\"1.0", 2.5]`,
		`"str"`:                          `"str"`,
		`not json 1.0`:                   `not json 1.0`,
		`123456789012345680000000000000`: `123456789012345680000000000000`,
	} {
		if got := canonicalJSON(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

// encoding/json writes U+FFFD for each byte that is not UTF-8.
func TestJSONStringReplacesInvalidUTF8(t *testing.T) {
	if got := jsonString("a\xffb\xfe"); got != `"a`+"\uFFFD"+`b`+"\uFFFD"+`"` {
		t.Fatal(got)
	}
}

// A JSON value is compared with its numbers canonical; a number column as a
// number; anything else as it is.
func TestColumnTextOfStructuredColumns(t *testing.T) {
	for _, tc := range []struct {
		col      dbschema.Column
		in, want string
	}{
		{dbschema.Column{Type: "jsonb"}, `{"a": 1.0}`, `{"a": 1}`},
		{dbschema.Column{Type: "_numeric", Category: "A"}, `[1.50, null]`, `[1.5, null]`},
		{dbschema.Column{Type: "int4", Domain: "qty"}, `05`, `5`},
		{dbschema.Column{Type: "text"}, `1.0`, `1.0`},
	} {
		if got := columnText(tc.col, tc.in); got != tc.want {
			t.Errorf("%+v %q: %q, want %q", tc.col, tc.in, got, tc.want)
		}
	}
}

// At the top of a json or jsonb column a value is what encoding/json makes of
// what yaml.v3 puts into an any field, as it is inside a mapping: the reading
// a cell keeps for such a column, where it differs from the array column's.
func TestTheJSONReadingIsWhatAnAnyFieldMarshalsTo(t *testing.T) {
	marshal := func(t *testing.T, in string) string {
		var v any
		if err := yaml.Unmarshal([]byte(in), &v); err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSuffix(b.String(), "\n")
	}
	for _, in := range []string{
		`[2026-01-01T10:00:00+02:00, 2026-01-01, 2026-01-01 10:00:00.5, 0.1234567890123456789, 017, x, "y", true]`,
		`[[2026-01-01], {a: [1.5, 2026-01-02]}]`,
		`{a: [2026-01-01T10:00:00-05:00], b: <b>}`,
	} {
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(in), &n); err != nil {
			t.Fatal(err)
		}
		got, err := yamlAnyJSON(&n)
		if err != nil {
			t.Fatal(err)
		}
		if want := marshal(t, in); got != want {
			t.Errorf("%s:\n got %s\nwant %s", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"2026-01-01T10:00:00+02:00": "2026-01-01T10:00:00+02:00",
		"2026-01-01":                "2026-01-01T00:00:00Z",
		"2026-01-01 10:00:00":       "",
		"2026-01-01T10:00:00Z":      "",
		"0.1234567890123456789":     "0.12345678901234568",
		"1.50":                      "",
		"017":                       "",
		`"2026-01-01"`:              "",
	} {
		doc, err := ParseDoc([]byte("- model: M\n  rows:\n    - {v: " + in + "}\n"))
		if err != nil {
			t.Fatal(err)
		}
		if got := doc[0].Rows[0]["v"].JSONText; got != want {
			t.Errorf("%s: JSONText %q, want %q", in, got, want)
		}
		if want != "" && strings.Trim(marshal(t, in), `"`) != want {
			t.Errorf("%s: an any field marshals to %s", in, marshal(t, in))
		}
	}
	// A sequence whose any reading is the array column's has none.
	doc, err := ParseDoc([]byte("- model: M\n  rows:\n    - {v: [1, a, {k: 2026-01-01}]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := doc[0].Rows[0]["v"].JSONText; got != "" {
		t.Errorf("JSONText %q", got)
	}
}
