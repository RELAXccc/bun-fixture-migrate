package fixtureapply

import (
	"strings"
	"testing"
)

func TestArrayLiteral(t *testing.T) {
	for in, want := range map[string]string{
		`[]`:                        `{}`,
		`[1, 2, 3]`:                 `{1,2,3}`,
		`[[1,2],[3,4]]`:             `{{1,2},{3,4}}`,
		`[[[1],[2]],[[3],[4]]]`:     `{{{1},{2}},{{3},{4}}}`,
		`["a", "b c", "", "NULL"]`:  `{"a","b c","","NULL"}`,
		`["say \"hi\"", "back\\"]`:  `{"say \"hi\"","back\\"}`,
		`[null, true, 1.50, -2e3]`:  `{NULL,true,1.50,-2e3}`,
		`[{"b": 1, "a": [1, 2]}]`:   `{"{\"b\":1,\"a\":[1,2]}"}`,
		` [ ["x"] , ["y"] ] `:       `{{"x"},{"y"}}`,
		`["{a,b}", "{\"c\"}", ","]`: `{"{a,b}","{\"c\"}",","}`,
	} {
		got, ok, err := arrayLiteral(in)
		if err != nil || !ok || got != want {
			t.Errorf("%s: %s, %v, %v; want %s", in, got, ok, err, want)
		}
	}
	// Not JSON: PostgreSQL's own spelling, which it reads itself.
	for _, in := range []string{`[0:1]={7,8}`, `[1,`, `{1,2}`} {
		if _, ok, err := arrayLiteral(in); ok || err != nil {
			t.Errorf("%s: %v, %v", in, ok, err)
		}
	}
	// PostgreSQL stores no array whose rows differ in length.
	for _, in := range []string{`[[1,2],[3]]`, `[[1],2]`, `[1,[2]]`, `[[[1]],[[1,2]]]`} {
		if _, ok, err := arrayLiteral(in); !ok || err == nil || !strings.Contains(err.Error(), "same length") {
			t.Errorf("%s: %v, %v", in, ok, err)
		}
	}
	// PostgreSQL stores none of these, and said so only at deploy time, or
	// not at all: bun dropped the NUL and stored {ab}.
	for in, want := range map[string]string{
		`["a\u0000b"]`:             "NUL",
		`[{"note": "x\u0000"}]`:    "",
		`[[[[[[1]]]]]]`:            "",
		`[[[[[[[1]]]]]]]`:          "at most 6 dimensions",
		`[[]]`:                     "empty list inside a list",
		`[[], []]`:                 "empty list inside a list",
		`[]`:                       "",
		`[[1, 2], ["x\u0000", 3]]`: "NUL",
	} {
		_, ok, err := arrayLiteral(in)
		if !ok || (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v, %v; want %q", in, ok, err, want)
		}
	}
}
