package dbschema

import (
	"strings"
	"testing"
)

func TestLiteralDefault(t *testing.T) {
	for _, tc := range []struct {
		def     string
		want    string
		literal bool
	}{
		{"", "", false},
		{"0", "0", true},
		{"1", "1", true},
		{"1.5", "1.5", true},
		{"false", "false", true},
		{"true", "true", true},
		{"NULL", "", false},
		{"'draft'::text", "draft", true},
		{"'a::b'::text", "a::b", true},
		{"''::text", "", true},
		{"-1", "-1", true},
		{"'{}'::jsonb", "{}", true},
		{"'it''s'::character varying", "it's", true},
		{"0::bigint", "0", true},
		{"now()", "", false},
		{"nextval('plans_id_seq'::regclass)", "", false},
		{"(now() + '1 day'::interval)", "", false},
	} {
		got, ok := Column{Default: tc.def}.LiteralDefault()
		if ok != tc.literal || got != tc.want {
			t.Errorf("LiteralDefault(%q) = %q, %v; want %q, %v", tc.def, got, ok, tc.want, tc.literal)
		}
	}
}

// What an INSERT that says DEFAULT puts into the column, for the null half of
// the round-trip hazard. A NULL default and a generation expression are not
// defaults of that kind.
func TestNonNullDefault(t *testing.T) {
	for _, tc := range []struct {
		col  Column
		want string
		has  bool
	}{
		{Column{}, "", false},
		{Column{Default: "NULL::text"}, "", false},
		{Column{Default: "NULL"}, "", false},
		{Column{Default: "'none'::text"}, "none", true},
		{Column{Default: "0"}, "0", true},
		{Column{Default: "now()"}, "now()", true},
		{Column{Default: "nextval('t_x_seq'::regclass)"}, "nextval('t_x_seq'::regclass)", true},
		{Column{Default: "(price * 2)", Generated: true}, "", false},
	} {
		got, ok := tc.col.NonNullDefault()
		if ok != tc.has || got != tc.want {
			t.Errorf("NonNullDefault(%+v) = %q, %v; want %q, %v", tc.col, got, ok, tc.want, tc.has)
		}
	}
}

// The round-trip hazard: a column whose default is not the type's zero cannot
// hold that zero through a bun insert, because bun writes DEFAULT instead.
func TestZeroIsNotDefault(t *testing.T) {
	for _, tc := range []struct {
		name   string
		col    Column
		hazard bool
		stored string
	}{
		{"no default", Column{Type: "int8"}, false, ""},
		{"zero default", Column{Type: "int8", Default: "0"}, false, ""},
		{"one default", Column{Type: "int8", Default: "1"}, true, "1"},
		{"true default", Column{Type: "bool", Default: "true"}, true, "true"},
		{"false default", Column{Type: "bool", Default: "false"}, false, ""},
		{"text default", Column{Type: "text", Default: "'draft'::text"}, true, "draft"},
		{"empty text default", Column{Type: "text", Default: "''::text"}, false, ""},
		{"float default", Column{Type: "float8", Default: "1.0"}, true, "1.0"},
		{"float zero written as 0.0", Column{Type: "float8", Default: "0.0"}, false, ""},
		{"serial", Column{Type: "int8", Default: "nextval('t_id_seq'::regclass)"}, false, ""},
		{"expression", Column{Type: "timestamptz", Default: "now()"}, false, ""},
		{"unknown type", Column{Type: "jsonb", Default: "'{}'::jsonb"}, false, ""},
		// An expression is never the zero: bun writes DEFAULT and the
		// database runs it.
		{"uuid generated", Column{Type: "uuid", Default: "gen_random_uuid()"}, true, "gen_random_uuid()"},
		{"int from a function", Column{Type: "int4", Default: "next_rank()"}, true, "next_rank()"},
		{"generated column", Column{Type: "int4", Default: "(a * 2)", Generated: true}, false, ""},
	} {
		hazard, stored := tc.col.ZeroIsNotDefault()
		if hazard != tc.hazard || stored != tc.stored {
			t.Errorf("%s: got %v, %q; want %v, %q", tc.name, hazard, stored, tc.hazard, tc.stored)
		}
	}
}

func TestSerial(t *testing.T) {
	if !(Column{Default: "nextval('t_id_seq'::regclass)"}).Serial() {
		t.Error("a nextval default is a sequence")
	}
	if !(Column{Identity: true}).Serial() {
		t.Error("an identity column is a sequence")
	}
	if (Column{Default: "0"}).Serial() {
		t.Error("a plain default is not a sequence")
	}
}

func TestForeignKeyOf(t *testing.T) {
	tbl := &Table{ForeignKeys: []ForeignKey{
		{Columns: []string{"plan_id"}, RefTable: "plans", RefColumns: []string{"id"}},
		{Columns: []string{"a", "b"}, RefTable: "pairs"},
	}}
	if fk := tbl.ForeignKeyOf("plan_id"); fk == nil || fk.RefTable != "plans" {
		t.Fatalf("expected the plans key, got %+v", fk)
	}
	// A composite key names no single column, so nothing is guessed from it.
	if fk := tbl.ForeignKeyOf("a"); fk != nil {
		t.Fatalf("a composite key is not a single-column reference: %+v", fk)
	}
}

func TestColumnAndQualified(t *testing.T) {
	tbl := &Table{Schema: "master", Name: "plans", Columns: []Column{
		{Name: "id", Position: 1, Type: "int8"},
		{Name: "name", Position: 2, Type: "text"},
	}}
	if c, ok := tbl.Column("name"); !ok || c.Position != 2 {
		t.Fatalf("Column(name) = %+v, %v", c, ok)
	}
	if _, ok := tbl.Column("Name"); ok {
		t.Fatal("column names are not folded: PostgreSQL's are not either")
	}
	if got := tbl.Qualified(); got != "master.plans" {
		t.Fatalf("Qualified = %q", got)
	}
}

// The table order has to be stable, because it decides the order of everything
// the scaffold writes.
func TestNamesAreSorted(t *testing.T) {
	names := Names(map[string]*Table{
		"public.plans": {}, "public.currencies": {}, "master.items": {},
	})
	if strings.Join(names, " ") != "master.items public.currencies public.plans" {
		t.Fatalf("Names = %v", names)
	}
}
