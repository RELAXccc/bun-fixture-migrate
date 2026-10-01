package fixturemigrate

import (
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
