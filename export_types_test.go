package fixturemigrate

import (
	"strings"
	"testing"
	gotemplate "text/template"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"gopkg.in/yaml.v3"
)

// Every value is written the way a model field of the column's type loads it
// back, a domain's included, or refused with the reason.
func TestExportLiteral(t *testing.T) {
	col := func(typ string) dbschema.Column { return dbschema.Column{Type: typ, FullType: typ} }
	arr := dbschema.Column{Type: "_int4", Category: "A", ElemType: "int4", ElemCategory: "N"}
	for _, tc := range []struct {
		col        dbschema.Column
		in, want   string
		note       string
		refusedFor string
	}{
		{dbschema.Column{Type: "int4", Domain: "qty"}, "5", "5", "", ""},
		{col("int8"), "9007199254740993", "9007199254740993", "", ""},
		{col("float8"), "NaN", ".nan", "", ""},
		{col("float4"), "Infinity", ".inf", "", ""},
		{col("float8"), "-Infinity", "-.inf", "", ""},
		{col("float8"), "0.1", "0.1", "", ""},
		{col("numeric"), "12345678901234567890.1234567890", "12345678901234567890.1234567890", "", ""},
		{col("numeric"), "NaN", "", "", "no spelling reads back as itself"},
		{col("money"), "1.5", "1.5", "", ""},
		{dbschema.Column{Type: "bool", Domain: "flag"}, "t", "true", "", ""},
		{col("date"), "2026-03-04", "2026-03-04", "", ""},
		{col("timestamptz"), "2026-01-01 10:00:00+00", "2026-01-01T10:00:00Z", "", ""},
		{col("timestamp"), "2026-01-01 10:00:00.5", "2026-01-01T10:00:00.5Z", "", ""},
		{col("timestamptz"), "infinity", `"infinity"`, "a time.Time field cannot hold infinity", ""},
		{col("date"), "-infinity", `"-infinity"`, "a time.Time field cannot hold -infinity", ""},
		{col("bytea"), `\x4869`, "[72, 105]", "", ""},
		{col("bytea"), `\x`, "[]", "", ""},
		{col("text"), "yes", `"yes"`, "", ""},
		{col("text"), "Hello {{ name }}", `"{{ \"Hello {{ name }}\" }}"`, "", ""},
		{col("uuid"), "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11", `"a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"`, "", ""},
		{arr, "[[1, 2], [3, 4]]", "[[1, 2], [3, 4]]", "", ""},
		{arr, "[0:1]={7,8}", "", "", "lower bound is not 1"},
		{arr, `["NaN", 1]`, "", "", "NaN or Infinity"},
		{dbschema.Column{Type: "_text", Category: "A", ElemType: "text", ElemCategory: "S"}, "[\"a\u2028b\"]",
			`["a\u2028b"]`, "", ""},
		{col("jsonb"), `{"a": 1, "t": "{{ x }}"}`, `{"a": 1, "t": "{{ x }}"}`, "", ""},
		{dbschema.Column{Type: "jsonb", Domain: "doc"}, `{"k": 1}`, `{"k": 1}`, "", ""},
		{col("jsonb"), `"str"`, `"str"`, "", ""},
		{col("jsonb"), `"Hi {{ .Name }}"`, `"{{ \"Hi {{ .Name }}\" }}"`, "", ""},
		{col("jsonb"), `1.5`, `1.5`, "", ""},
		{col("jsonb"), `true`, `true`, "", ""},
		{col("jsonb"), `"true"`, "", "", "a string that is itself JSON"},
		{col("jsonb"), `"{\"a\": 1}"`, "", "", "a string that is itself JSON"},
		{col("jsonb"), `null`, "", "", "null, which a fixture file can only write as ~"},
		{col("json"), `{"big": 123456789012345678901234567890}`, "", "", "a fresh seed would store 123456789012345680000000000000"},
		{col("jsonb"), `[0.1234567890123456789]`, "", "", "more digits than the float64"},
		{col("jsonb"), `{"u": 12345678901234567890, "i": -9223372036854775808}`,
			`{"u": 12345678901234567890, "i": -9223372036854775808}`, "", ""},
		{col("jsonb"), `[18446744073709551616]`, "", "", "more digits than the float64"},
	} {
		got, note, err := exportLiteral("M", "c", tc.in, tc.col)
		if tc.refusedFor != "" {
			if err == nil || !strings.Contains(err.Error(), tc.refusedFor) {
				t.Errorf("%s %q: %q %v, want it refused for %q", tc.col.Type, tc.in, got, err, tc.refusedFor)
			}
			continue
		}
		if err != nil || got != tc.want || !strings.HasPrefix(note, tc.note) {
			t.Errorf("%s %q: %q %q %v, want %q %q", tc.col.Type, tc.in, got, note, err, tc.want, tc.note)
		}
	}
}

// yamlString writes any text so that yaml.v3 reads it back as itself: the
// controls, DEL, the C1 range, NEL and the line separators, which YAML folds,
// a byte-order mark, U+FFFE, and everything else.
func TestYAMLStringReadsBackAsItself(t *testing.T) {
	texts := []string{"", " x ", "a\u0085b", "a\x7fb", "a\u0096b", "a\ufffeb", "a\u2028b\u2029", "\ufeffx",
		"\t\n\r\x01\x1b", `"a" \ 'b'`, "snow ☃ 𝄞", "yes", "~", "null", "{{ x }}", "# not a comment", "- x", ": x"}
	for r := rune(0); r < 0x3000; r++ {
		if r >= 0xD800 && r < 0xE000 {
			continue
		}
		texts = append(texts, "a"+string(r)+"b")
	}
	texts = append(texts, "a\uffffb", "a\U0001F600b", "a\U0010FFFFb")
	for _, s := range texts {
		var back map[string]string
		if err := yaml.Unmarshal([]byte("v: "+yamlString(s)+"\n"), &back); err != nil || back["v"] != s {
			t.Fatalf("%q: written %s, read back %q, %v", s, yamlString(s), back["v"], err)
		}
	}
}

// Text dbfixture would evaluate as a template is written as a template that
// evaluates to it, which text/template does exactly as dbfixture does.
func TestExportStringOfTemplateLikeText(t *testing.T) {
	for _, s := range []string{"Hello {{ name }}", "{{ now }}", "Hi {{ .Name }} \"x\" \\ }} {{", "{{ a }}\n{{ b }}",
		"tab\t{{ x }}\x01"} {
		written := exportString(s)
		var back map[string]string
		if err := yaml.Unmarshal([]byte("v: "+written+"\n"), &back); err != nil {
			t.Fatal(err)
		}
		if !anyTemplate.MatchString(back["v"]) {
			t.Fatalf("%q: %s is not a template to dbfixture", s, written)
		}
		tpl, err := gotemplate.New("").Parse(back["v"])
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		var out strings.Builder
		if err := tpl.Execute(&out, nil); err != nil || out.String() != s {
			t.Fatalf("%q: evaluates to %q, %v", s, out.String(), err)
		}
		if lit, ok := literalTemplate(back["v"]); !ok || lit != s {
			t.Fatalf("%q: literalTemplate %q %v", s, lit, ok)
		}
	}
	for _, s := range []string{"{{ $.Plan.free.ID }}", "{{ now }}", `{{ "a" | printf "%s" }}`, "x {{ \"a\" }}"} {
		if _, ok := literalTemplate(s); ok {
			t.Errorf("%q is not a string literal", s)
		}
	}
}

// jsonb leaves NEL, the line separators and DEL as they are, which YAML does
// not; written into a flow mapping they are escaped the way both read.
func TestYAMLSafeJSON(t *testing.T) {
	in := "{\"a\": \"x\u0085y\u2028z\x7f\u0096\"}"
	out := yamlSafeJSON(in)
	if out != `{"a": "x\u0085y\u2028z\u007f\u0096"}` {
		t.Fatal(out)
	}
	var back map[string]any
	if err := yaml.Unmarshal([]byte(out), &back); err != nil || back["a"] != "x\u0085y\u2028z\x7f\u0096" {
		t.Fatalf("%q %v", back, err)
	}
}

// The parse-back catches an export that would load as something else.
func TestVerifyExportCatchesAMisreading(t *testing.T) {
	text := col("text")
	rows := []writtenRow{{model: "M", cells: map[string]writtenCell{
		"_id": {text: "a", exact: true},
		"v":   {value: fixturechange.Lit("a\u0085b"), column: text},
	}}}
	if err := verifyExport([]byte("- model: M\n  rows:\n    - _id: a\n      v: \"a\u0085b\"\n"), rows); err == nil ||
		!strings.Contains(err.Error(), `M.v reads back as "a b"`) {
		t.Fatalf("a NEL folded into a space is caught: %v", err)
	}
	if err := verifyExport([]byte("- model: M\n  rows:\n    - _id: a\n      v: \"a\\Nb\"\n"), rows); err != nil {
		t.Fatal(err)
	}
	if err := verifyExport([]byte("- model: M\n  rows:\n    - _id: a\n      v: ~\n"), rows); err == nil {
		t.Fatal("a null where text was is caught")
	}
	if err := verifyExport([]byte("- model: M\n  rows:\n    - _id: a\n      v: \"x\"\n      w: 1\n"), rows); err == nil {
		t.Fatal("a column too many is caught")
	}
	// Readings that differ in spelling only are one value.
	for _, tc := range []struct {
		col          dbschema.Column
		yaml, lit    string
		structuredIn bool
	}{
		{col("timestamptz"), "2026-01-01T10:00:00Z", "2026-01-01 10:00:00+00", false},
		{col("timestamp"), "2026-01-01T10:00:00Z", "2026-01-01 10:00:00", false},
		{col("numeric"), "1.5", "1.50", false},
		{col("bool"), "true", "t", false},
		{col("jsonb"), `{"b": 1, "a": [1.0]}`, `{"a": [1], "b": 1}`, true},
		{col("jsonb"), `"str"`, `"str"`, false},
		{col("bytea"), "[72, 105]", `\x4869`, true},
		{col("text"), `"{{ \"Hello {{ x }}\" }}"`, "Hello {{ x }}", false},
	} {
		rows := []writtenRow{{model: "M", cells: map[string]writtenCell{"v": {value: fixturechange.Lit(tc.lit), column: tc.col}}}}
		if err := verifyExport([]byte("- model: M\n  rows:\n    - v: "+tc.yaml+"\n"), rows); err != nil {
			t.Errorf("%s %s: %v", tc.col.Type, tc.yaml, err)
		}
	}
}

func col(typ string) dbschema.Column { return dbschema.Column{Type: typ, FullType: typ} }

// An export of a NULL in a json column says what each kind of field makes of
// the ~ it writes, and refuses to write it while null_default is an error.
func TestExportOfANullJSONColumn(t *testing.T) {
	cfg := testConfig(t)
	jsonb := dbschema.Column{Name: "doc", Type: "jsonb", Nullable: true}
	if _, _, err := exportValue(cfg, "M", "doc", fixturechange.Null(), jsonb, nil); err == nil ||
		!strings.Contains(err.Error(), "set policy.null_default to warn") {
		t.Fatalf("%v", err)
	}
	cfg.Policy.NullDefault = ModeWarn
	if got, _, err := exportValue(cfg, "M", "doc", fixturechange.Null(), jsonb, nil); err != nil || got != "~" {
		t.Fatalf("%q %v", got, err)
	}
	if got := hazardComment(fixturechange.Null(), jsonb); !strings.Contains(got, "a map, slice or any field loads ~ as the JSON null") {
		t.Fatal(got)
	}
}
