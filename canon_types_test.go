package fixturemigrate

import (
	"errors"
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
)

// What PostgreSQL is handed for a value, or why it never is.
func TestCastInput(t *testing.T) {
	ints := dbschema.Column{Type: "int4", FullType: "integer"}
	arr := dbschema.Column{Type: "_int4", FullType: "integer[]", Category: "A", ElemType: "int4"}
	jsonArr := dbschema.Column{Type: "_jsonb", FullType: "jsonb[]", Category: "A", ElemType: "jsonb"}
	bytea := dbschema.Column{Type: "bytea", FullType: "bytea"}
	jsonb := dbschema.Column{Type: "jsonb", FullType: "jsonb"}
	for _, tc := range []struct {
		col      dbschema.Column
		in, want string
		refused  string
	}{
		{ints, "15", "15", ""},
		{ints, "1.5", "", "an integer field holds as 1, because yaml.v3 drops the fraction"},
		{ints, "-0.5", "", "holds as 0"},
		{ints, "1e3", "1e3", ""},
		{arr, `[1,2]`, `{"1","2"}`, ""},
		{arr, `[[1,2],[3,null]]`, `{{"1","2"},{"3",NULL}}`, ""},
		{arr, `{1,2}`, `{1,2}`, ""},
		{arr, `[0:1]={7,8}`, `[0:1]={7,8}`, ""},
		{dbschema.Column{Type: "_text", Category: "A", ElemType: "text"}, `["a \"b\"","c\\d",true]`,
			`{"a \"b\"","c\\d","true"}`, ""},
		{jsonArr, `[{"a":1},[1,2]]`, `{"{\"a\":1}","[1,2]"}`, ""},
		{bytea, `[72,105]`, `\x4869`, ""},
		{bytea, `[]`, `\x`, ""},
		{bytea, `[256]`, "", "a []byte field cannot hold"},
		{bytea, `[1.5]`, "", "a []byte field cannot hold"},
		{bytea, `\x00ff`, `\x00ff`, ""},
		{jsonb, `{"a":1}`, `{"a":1}`, ""},
		{jsonb, `hello`, `"hello"`, ""},
		{jsonb, `"str"`, `"str"`, ""},
		{jsonb, `true`, `true`, ""},
	} {
		got, refused := castInput(tc.col, tc.in)
		if tc.refused != "" {
			if !strings.Contains(refused, tc.refused) {
				t.Errorf("%s %q: refused %q, want %q", tc.col.Type, tc.in, refused, tc.refused)
			}
			continue
		}
		if refused != "" || got != tc.want {
			t.Errorf("%s %q: %q %q, want %q", tc.col.Type, tc.in, got, refused, tc.want)
		}
	}
}

// The length an INSERT holds a value against, as SQL.
func TestTooLong(t *testing.T) {
	for _, tc := range []struct {
		col  dbschema.Column
		want string
	}{
		{dbschema.Column{Type: "text"}, "false"},
		{dbschema.Column{Type: "varchar"}, "false"},
		{dbschema.Column{Type: "varchar", Length: 5}, "(char_length(t.v) > 5 AND rtrim(substr(t.v, 5 + 1), ' ') <> '')"},
		{dbschema.Column{Type: "bpchar", Length: 3}, "(char_length(t.v) > 3 AND rtrim(substr(t.v, 3 + 1), ' ') <> '')"},
		{dbschema.Column{Type: "bit", Length: 3}, "(length((t.v)::varbit) <> 3)"},
		{dbschema.Column{Type: "varbit", Length: 3}, "(length((t.v)::varbit) > 3)"},
		{dbschema.Column{Type: "_bpchar", Category: "A", ElemType: "bpchar", Length: 3},
			"EXISTS (SELECT 1 FROM unnest((t.v)::varchar[]) AS e(v) WHERE char_length(e.v) > 3 AND rtrim(substr(e.v, 3 + 1), ' ') <> '')"},
	} {
		if got := tooLong(tc.col, "t.v"); got != tc.want {
			t.Errorf("%+v:\n got %s\nwant %s", tc.col, got, tc.want)
		}
	}
}

type stateErr string

func (e stateErr) Error() string    { return "ERROR: boom (SQLSTATE " + string(e) + ")" }
func (e stateErr) SQLState() string { return string(e) }

// Any error PostgreSQL gives about the value is the value's; one about the
// connection, the transaction or the server is not.
func TestValueError(t *testing.T) {
	for state, want := range map[string]bool{
		"22P02": true, "22001": true, "23514": true, "23502": true, "42601": true, "54000": true, "42883": true,
		"08006": false, "25P02": false, "40001": false, "42501": false, "53100": false, "55P03": false,
		"57014": false, "58030": false, "XX000": false,
	} {
		if got := valueError(stateErr(state)); got != want {
			t.Errorf("%s: %v, want %v", state, got, want)
		}
	}
	if valueError(errors.New("driver: bad connection")) {
		t.Fatal("an error without a SQLSTATE is not about a value")
	}
	if got := valueMessage(stateErr("22P02")); got != "boom" {
		t.Fatalf("pgx's decoration is stripped: %q", got)
	}
}

func TestSQLIdent(t *testing.T) {
	if got := sqlIdent(`a "b"`); got != `"a ""b"""` {
		t.Fatal(got)
	}
}
