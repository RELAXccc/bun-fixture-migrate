package fixturemigrate

import (
	"fmt"
	"math/big"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCanonicalDecimal(t *testing.T) {
	for in, want := range map[string]string{
		"0": "0", "-0": "0", "+0.000": "0", "00": "0",
		"1": "1", "1.0": "1", "1.50": "1.5", "01.5": "1.5", "+1": "1", "-1.500": "-1.5",
		".5": "0.5", "5.": "5", "-.25": "-0.25",
		"1e3": "1000", "1E3": "1000", "1.5e1": "15", "10e-1": "1", "1e-3": "0.001", "123e-2": "1.23",
		"1e+20":                 "100000000000000000000",
		"9007199254740993":      "9007199254740993",
		"12345678901234567890":  "12345678901234567890",
		"123456789.123456789":   "123456789.123456789",
		"0.1000000000000000055": "0.1000000000000000055",
	} {
		got, ok := canonicalDecimal(in)
		if !ok || got != want {
			t.Errorf("canonicalDecimal(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "+", "-", ".", "e3", "1e", "1e3.5", "1.2.3", "0x1F", "1_000", "abc", "1 2",
		" 1", "Infinity", "NaN", "1e99999"} {
		if got, ok := canonicalDecimal(in); ok {
			t.Errorf("canonicalDecimal(%q) = %q, want no number", in, got)
		}
	}
}

// Whatever yaml.v3 decodes into an int64 -- and dbfixture decodes into an
// integer field -- yamlInt writes as that integer.
func TestYamlIntAgreesWithYaml(t *testing.T) {
	for _, in := range []string{"0", "7", "-7", "+7", "017", "0o17", "0x1F", "0X1f", "0b101", "-0b101", "1_000",
		"-0x10", "9223372036854775807", "-9223372036854775808", "0777"} {
		var node yaml.Node
		if err := yaml.Unmarshal([]byte(in), &node); err != nil {
			t.Fatal(err)
		}
		scalar := node.Content[0]
		if scalar.ShortTag() != "!!int" {
			t.Fatalf("%s is %s to YAML", in, scalar.ShortTag())
		}
		var n int64
		if err := scalar.Decode(&n); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := scalarText(Cell{Text: in, Tag: "!!int"}); got != fmt.Sprint(n) {
			t.Errorf("%s: %s, YAML says %d", in, got, n)
		}
	}
	// Beyond int64 YAML gives up on an integer field; the text stays exact.
	if got := scalarText(Cell{Text: "18446744073709551616", Tag: "!!int"}); got != "18446744073709551616" {
		t.Fatal(got)
	}
}

func TestScalarTextByYamlType(t *testing.T) {
	for _, tc := range []struct {
		cell Cell
		want string
	}{
		// A string is kept byte for byte: a postcode, a padded label.
		{Cell{Text: "01234", Tag: "!!str"}, "01234"},
		{Cell{Text: "  padded  ", Tag: "!!str"}, "  padded  "},
		{Cell{Text: "1.0", Tag: "!!str"}, "1.0"},
		{Cell{Text: "0x1F", Tag: "!!str"}, "0x1F"},
		{Cell{Text: "017", Tag: "!!int"}, "15"},
		{Cell{Text: "1.50", Tag: "!!float"}, "1.5"},
		{Cell{Text: "1_000.5", Tag: "!!float"}, "1000.5"},
		{Cell{Text: ".inf", Tag: "!!float"}, "Infinity"},
		{Cell{Text: "-.Inf", Tag: "!!float"}, "-Infinity"},
		{Cell{Text: ".NaN", Tag: "!!float"}, "NaN"},
		{Cell{Text: "True", Tag: "!!bool"}, "true"},
		{Cell{Text: "2026-01-01", Tag: "!!timestamp"}, "2026-01-01"},
		// A configured default did not come from YAML and is taken as written.
		{Cell{Text: "01"}, "01"},
	} {
		if got := scalarText(tc.cell); got != tc.want {
			t.Errorf("scalarText(%+v) = %q, want %q", tc.cell, got, tc.want)
		}
	}
}

func TestColumnTextComparesOnlyNumbersAsNumbers(t *testing.T) {
	for _, tc := range []struct{ typ, in, want string }{
		{"numeric", "1.50", "1.5"},
		{"int8", "9007199254740993", "9007199254740993"},
		{"float8", "1e+20", "100000000000000000000"},
		{"float8", "Infinity", "Infinity"},
		{"text", "1.0", "1.0"},
		{"text", "01234", "01234"},
		{"varchar", " x ", " x "},
	} {
		if got := columnText(tc.typ, tc.in); got != tc.want {
			t.Errorf("columnText(%s, %q) = %q, want %q", tc.typ, tc.in, got, tc.want)
		}
	}
}

// The regression, end to end through the fixture reader: a postcode, a padded
// label, a snowflake id and an octal-looking integer, each as dbfixture would
// store it.
func TestTheFixtureReaderKeepsValuesExact(t *testing.T) {
	cfg := &Config{Models: map[string]*Model{"Zip": {Table: "zips", Key: []string{"code"}, Ref: "code"}}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	s := snap(t, cfg, `- model: Zip
  rows:
    - code: "01234"
      label: "  padded  "
      big: 9007199254740993
      octal: 017
      hex: 0x1F
      price: 1.50
`, "fixture.yml")
	e := s.Entries["Zip"][0]
	for col, want := range map[string]string{"code": "01234", "label": "  padded  ", "big": "9007199254740993",
		"octal": "15", "hex": "31", "price": "1.5"} {
		if got := e.Cells[col].Lit; got != want {
			t.Errorf("%s = %q, want %q", col, got, want)
		}
	}
}

// A change of a postcode is an update from one exact string to another. Before,
// both sides lost the leading zero, and the guard looked for a value the
// database did not hold.
func TestALeadingZeroSurvivesTheDiff(t *testing.T) {
	cfg := &Config{Models: map[string]*Model{"Shop": {Table: "shops", Key: []string{"name"}}}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	text := "- model: Shop\n  rows:\n    - name: a\n      zip: \"0012\"\n"
	res, err := Compute(cfg, snap(t, cfg, text, "old"), snap(t, cfg, strings.Replace(text, "0012", "0013", 1), "new"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 1 || res.Changes[0].Old["zip"].Lit != "0012" || res.Changes[0].New["zip"].Lit != "0013" {
		t.Fatalf("%+v", res.Changes)
	}
}

// Canonical text is a fixed point and means the same number.
func FuzzCanonicalDecimal(f *testing.F) {
	for _, seed := range []string{"0", "1.50", "-0.001", "1e3", "9007199254740993", ".5", "5.", "1e-1000"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out, ok := canonicalDecimal(in)
		if !ok {
			return
		}
		again, ok := canonicalDecimal(out)
		if !ok || again != out {
			t.Fatalf("%q -> %q -> %q, %v", in, out, again, ok)
		}
		a, okA := new(big.Rat).SetString(strings.TrimPrefix(in, "+"))
		b, okB := new(big.Rat).SetString(out)
		if okA && okB && a.Cmp(b) != 0 {
			t.Fatalf("%q is %s, canonical %q is %s", in, a, out, b)
		}
	})
}

func TestYamlTimestampIsOneInstant(t *testing.T) {
	for in, want := range map[string]string{
		"2026-01-01 10:00:00":       "2026-01-01T10:00:00Z",
		"2026-01-01T10:00:00Z":      "2026-01-01T10:00:00Z",
		"2026-01-01t12:00:00+02:00": "2026-01-01T10:00:00Z",
		"2026-1-1 10:0:0.5":         "2026-01-01T10:00:00.5Z",
		"2026-3-4":                  "2026-03-04",
	} {
		if got := scalarText(Cell{Text: in, Tag: "!!timestamp"}); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

// An export writes a date or a timestamp the way YAML reads one, which is the
// way yaml.v3 decodes into a time.Time; quoted, only RFC 3339 would decode.
func TestExportWritesTimesAsYamlTimestamps(t *testing.T) {
	for _, tc := range []struct{ typ, in, want string }{
		{"date", "2026-03-04", "2026-03-04"},
		{"timestamp", "2026-01-01 10:00:00.5", "2026-01-01 10:00:00.5"},
		{"timestamptz", "2026-01-01 10:00:00+00", "2026-01-01T10:00:00Z"},
		{"timestamptz", "2026-01-01 10:00:00+05:30", "2026-01-01T04:30:00Z"},
		{"timestamptz", "infinity", `"infinity"`},
		{"time", "10:00:00", `"10:00:00"`},
	} {
		got := yamlScalar(tc.in, tc.typ)
		if got != tc.want {
			t.Errorf("%s %s: %s, want %s", tc.typ, tc.in, got, tc.want)
		}
		if strings.HasPrefix(got, `"`) {
			continue
		}
		var node yaml.Node
		if err := yaml.Unmarshal([]byte(got), &node); err != nil || node.Content[0].ShortTag() != "!!timestamp" {
			t.Errorf("%s is not a YAML timestamp: %v", got, err)
		}
	}
}

func TestYamlJSONIsOneSpelling(t *testing.T) {
	json := func(src string) (string, error) {
		var node yaml.Node
		if err := yaml.Unmarshal([]byte(src), &node); err != nil {
			t.Fatal(err)
		}
		return yamlJSON(node.Content[0])
	}
	for src, want := range map[string]string{
		"{b: 1, a: 2}":                    `{"a":2,"b":1}`,
		"{a: 1.50, b: 017, c: 0x1F}":      `{"a":1.5,"b":15,"c":31}`,
		`{s: "a <b> & c", n: ~, t: true}`: `{"n":null,"s":"a <b> & c","t":true}`,
		`[x, "01", 1, [a]]`:               `["x","01",1,["a"]]`,
		"{when: 2026-01-01 10:00:00}":     `{"when":"2026-01-01T10:00:00Z"}`,
		"{1: one}":                        `{"1":"one"}`,
		"[]":                              `[]`,
		"{}":                              `{}`,
		"{a: &x [1], b: *x}":              `{"a":[1],"b":[1]}`,
	} {
		got, err := json(src)
		if err != nil || got != want {
			t.Errorf("%s: %s (%v), want %s", src, got, err, want)
		}
	}
	for _, src := range []string{"[.nan]", "{a: .inf}", "{[1]: x}"} {
		if got, err := json(src); err == nil {
			t.Errorf("%s: %s, JSON cannot hold it", src, got)
		}
	}
	// Two spellings of one structure are one text.
	a, _ := json("{rollout: 50, regions: [eu, us]}")
	b, _ := json("regions:\n  - eu\n  - us\nrollout: 50.0\n")
	if a != b {
		t.Fatalf("%s != %s", a, b)
	}
}
