// Package dbschema reads what PostgreSQL already knows about a database:
// tables, columns, types, column defaults, primary keys, unique indexes,
// foreign keys and sequences.
//
// That is the whole reason this tool needs no access to your Go model structs.
// A standalone command cannot reflect over types it was not compiled with, and
// it does not have to: the catalog holds everything an export or a schema check
// needs, and the configuration file supplies the rest.
//
// PostgreSQL only, 12 or later. The queries are written against pg_catalog.
package dbschema

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/uptrace/bun"
)

// Column is one column of a table.
type Column struct {
	// Name is the column name.
	Name string
	// Position is its ordinal position, which export uses as the column order
	// so an exported file looks like the table.
	Position int
	// Type is the PostgreSQL type name as the catalog spells it ("int8",
	// "text", "bool", "numeric", "timestamptz", "jsonb", "_text" for text[]).
	Type string
	// FullType is the type as SQL writes it, with its modifiers and, where
	// the search path needs it, its schema: "numeric(10,2)", "character
	// varying(20)", "text[]", "timestamp with time zone". It is what a value
	// is cast to so PostgreSQL can say what the column would hold.
	FullType string
	// Category is the type's category (pg_type.typcategory): "A" for an
	// array, "E" for an enum, "N" numeric, "S" string, "D" date and time,
	// "U" user-defined, and so on.
	Category string
	// Nullable is true when the column accepts NULL.
	Nullable bool
	// Default is the column default exactly as the catalog stores it, "" when
	// the column has none. "nextval('t_id_seq'::regclass)" for a serial
	// column, "0", "false", "'x'::text", "now()".
	Default string
	// Identity is true for a GENERATED ... AS IDENTITY column.
	Identity bool
	// IdentityAlways is true for GENERATED ALWAYS AS IDENTITY, which refuses
	// an explicit value from anybody: dbfixture, bun or a migration.
	IdentityAlways bool
	// Generated is true for a GENERATED ALWAYS AS (...) STORED column. Its
	// Default is the generation expression, and nothing can write into it.
	Generated bool
}

// Table is one table.
type Table struct {
	Schema string
	Name   string
	// Columns in ordinal order.
	Columns []Column
	// PrimaryKey lists the primary-key columns, empty when the table has none.
	PrimaryKey []string
	// Uniques lists the columns of every unique index that covers plain
	// columns only, the primary key included. A unique index over an
	// expression or a partial one is left out, because it does not make a
	// lookup by those columns unique.
	Uniques [][]string
	// ForeignKeys lists the outgoing foreign keys.
	ForeignKeys []ForeignKey
}

// ForeignKey is one foreign-key constraint.
type ForeignKey struct {
	Columns    []string
	RefSchema  string
	RefTable   string
	RefColumns []string
}

// ForeignKey is the constraint a single column takes part in, or nil.
func (t *Table) ForeignKeyOf(column string) *ForeignKey {
	for i := range t.ForeignKeys {
		fk := &t.ForeignKeys[i]
		if len(fk.Columns) == 1 && fk.Columns[0] == column {
			return fk
		}
	}
	return nil
}

// Column finds a column by name.
func (t *Table) Column(name string) (Column, bool) {
	for _, c := range t.Columns {
		if c.Name == name {
			return c, true
		}
	}
	return Column{}, false
}

// Qualified is "schema.table".
func (t *Table) Qualified() string { return t.Schema + "." + t.Name }

// Serial reports whether the column's value comes from a sequence, so an
// explicit id written into it leaves the sequence behind.
func (c Column) Serial() bool {
	return c.Identity || strings.HasPrefix(strings.ToLower(c.Default), "nextval(")
}

// ZeroText is the text of this type's Go zero value: the value a bun struct
// field holds when nobody set it, as it would be written into a fixture file.
// The second result is false for a type whose zero this package will not guess.
func (c Column) ZeroText() (string, bool) {
	switch c.Type {
	case "int2", "int4", "int8", "float4", "float8", "numeric", "money":
		return "0", true
	case "bool":
		return "false", true
	case "text", "varchar", "bpchar", "citext":
		return "", true
	case "uuid":
		// A uuid.UUID nobody set, or the zero of any [16]byte uuid type.
		return "00000000-0000-0000-0000-000000000000", true
	}
	return "", false
}

// LiteralDefault is the column default reduced to the value it produces, for a
// default that is a plain literal. The second result is false for a default
// that is an expression ("now()", "nextval(...)"), which this package does not
// evaluate, and for a column with no default.
//
// It never asks the database to evaluate anything: running a default expression
// to find out what it does is how a tool with read intentions writes something.
func (c Column) LiteralDefault() (string, bool) {
	d := strings.TrimSpace(c.Default)
	if d == "" || c.Serial() || c.Generated {
		return "", false
	}
	d = stripCasts(d)
	switch strings.ToUpper(d) {
	case "NULL":
		return "", false
	case "TRUE":
		return "true", true
	case "FALSE":
		return "false", true
	}
	if len(d) >= 2 && d[0] == '\'' && d[len(d)-1] == '\'' {
		return strings.ReplaceAll(d[1:len(d)-1], "''", "'"), true
	}
	if _, err := strconv.ParseFloat(d, 64); err == nil {
		return d, true
	}
	return "", false
}

// stripCasts removes the casts the catalog appends to a default, "'x'::text",
// "0::bigint", and the parentheses around a negative number.
func stripCasts(d string) string {
	for {
		i := strings.LastIndex(d, "::")
		if i < 0 {
			break
		}
		rest := d[i+2:]
		if strings.ContainsAny(rest, "'()") || strings.TrimSpace(rest) == "" {
			break
		}
		d = strings.TrimSpace(d[:i])
	}
	return strings.Trim(d, "()")
}

// NonNullDefault is what the database puts into this column when an INSERT
// says DEFAULT: the literal value for a plain default ("1", "x"), the
// expression as the catalog stores it otherwise ("now()"). The second result
// is false for a column without a default, one whose default is NULL, and a
// generated column, whose stored expression is not a default at all.
//
// It is the other half of bun's round-trip hazard: bun writes DEFAULT for a
// nil pointer, and for a zero in a nullzero or default-tagged field, so an
// explicit null in a fixture row becomes this and not NULL.
func (c Column) NonNullDefault() (string, bool) {
	d := strings.TrimSpace(c.Default)
	if d == "" || c.Generated {
		return "", false
	}
	if lit, ok := c.LiteralDefault(); ok {
		return lit, true
	}
	if strings.EqualFold(stripCasts(d), "NULL") {
		return "", false
	}
	return d, true
}

// ZeroIsNotDefault reports whether writing this column's zero value produces
// something else in the database. It is the whole of the round-trip hazard:
// bun's INSERT sends DEFAULT rather than the value for a zero in a field that
// carries a default, so a fixture row saying 0 here ends up holding the default,
// and an export that writes 0 here does not reproduce the database it came from.
//
// The second result is the value the database would store instead.
func (c Column) ZeroIsNotDefault() (bool, string) {
	zero, known := c.ZeroText()
	// A sequence's zero is how bun asks for the next id, which is the point.
	if !known || c.Generated || c.Serial() {
		return false, ""
	}
	def, literal := c.LiteralDefault()
	if !literal {
		// An expression -- nextval(...), gen_random_uuid() -- is never the
		// zero: bun writes DEFAULT and the database runs it.
		if expr, ok := c.NonNullDefault(); ok {
			return true, expr
		}
		return false, ""
	}
	if numbersEqual(def, zero) || def == zero {
		return false, ""
	}
	return true, def
}

func numbersEqual(a, b string) bool {
	fa, ea := strconv.ParseFloat(a, 64)
	fb, eb := strconv.ParseFloat(b, 64)
	return ea == nil && eb == nil && fa == fb
}

// Load reads every table of the given schemas. The keys of the result are
// "schema.table".
func Load(ctx context.Context, db bun.IDB, schemas ...string) (map[string]*Table, error) {
	if len(schemas) == 0 {
		schemas = []string{"public"}
	}
	list := bun.In(schemas)
	tables := map[string]*Table{}

	// One row per column. Nothing is aggregated in SQL: a column name may hold
	// any character, so there is no separator a string_agg could use safely.
	const columnQuery = `
SELECT n.nspname, c.relname, a.attname, a.attnum, t.typname,
       format_type(a.atttypid, a.atttypmod), t.typcategory::text, NOT a.attnotnull,
       COALESCE(pg_get_expr(d.adbin, d.adrelid), ''), a.attidentity <> '', a.attidentity = 'a',
       a.attgenerated <> ''
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_type t ON t.oid = a.atttypid
LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE n.nspname IN (?) AND c.relkind IN ('r', 'p') AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY n.nspname, c.relname, a.attnum`
	if err := each(ctx, db, columnQuery, list, func(rows *sql.Rows) error {
		var schema, table string
		var col Column
		if err := rows.Scan(&schema, &table, &col.Name, &col.Position, &col.Type, &col.FullType, &col.Category,
			&col.Nullable,
			&col.Default, &col.Identity, &col.IdentityAlways, &col.Generated); err != nil {
			return err
		}
		t := tables[schema+"."+table]
		if t == nil {
			t = &Table{Schema: schema, Name: table}
			tables[schema+"."+table] = t
		}
		t.Columns = append(t.Columns, col)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the columns of %s: %w", strings.Join(schemas, ", "), err)
	}

	// One row per indexed column, in index order. A partial index and an index
	// over an expression are left out: neither makes a lookup by those columns
	// unique, so neither is a natural key.
	const indexQuery = `
SELECT n.nspname, c.relname, i.indexrelid::bigint, i.indisprimary, a.attname
FROM pg_index i
JOIN pg_class c ON c.oid = i.indrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY k(attnum, ord)
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
WHERE n.nspname IN (?) AND i.indisunique AND i.indpred IS NULL AND i.indexprs IS NULL
ORDER BY n.nspname, c.relname, i.indisprimary DESC, i.indexrelid, k.ord`
	type indexKey struct {
		table string
		oid   int64
	}
	indexes := map[indexKey][]string{}
	var indexOrder []indexKey
	primary := map[indexKey]bool{}
	if err := each(ctx, db, indexQuery, list, func(rows *sql.Rows) error {
		var schema, table, column string
		var oid int64
		var isPrimary bool
		if err := rows.Scan(&schema, &table, &oid, &isPrimary, &column); err != nil {
			return err
		}
		k := indexKey{schema + "." + table, oid}
		if _, seen := indexes[k]; !seen {
			indexOrder = append(indexOrder, k)
			primary[k] = isPrimary
		}
		indexes[k] = append(indexes[k], column)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the unique indexes of %s: %w", strings.Join(schemas, ", "), err)
	}
	for _, k := range indexOrder {
		t := tables[k.table]
		if t == nil {
			continue
		}
		if primary[k] {
			t.PrimaryKey = indexes[k]
		}
		t.Uniques = append(t.Uniques, indexes[k])
	}

	// One row per foreign-key column. The two unnests share an ordinality so a
	// composite key keeps its column pairing.
	const fkQuery = `
SELECT n.nspname, c.relname, con.oid::bigint, a.attname, fn.nspname, fc.relname, fa.attname
FROM pg_constraint con
JOIN pg_class c ON c.oid = con.conrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_class fc ON fc.oid = con.confrelid
JOIN pg_namespace fn ON fn.oid = fc.relnamespace
CROSS JOIN LATERAL unnest(con.conkey, con.confkey) WITH ORDINALITY k(attnum, fattnum, ord)
JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum
JOIN pg_attribute fa ON fa.attrelid = con.confrelid AND fa.attnum = k.fattnum
WHERE n.nspname IN (?) AND con.contype = 'f'
ORDER BY n.nspname, c.relname, con.oid, k.ord`
	fks := map[indexKey]*ForeignKey{}
	var fkOrder []indexKey
	if err := each(ctx, db, fkQuery, list, func(rows *sql.Rows) error {
		var schema, table, column, refSchema, refTable, refColumn string
		var oid int64
		if err := rows.Scan(&schema, &table, &oid, &column, &refSchema, &refTable, &refColumn); err != nil {
			return err
		}
		k := indexKey{schema + "." + table, oid}
		fk := fks[k]
		if fk == nil {
			fk = &ForeignKey{RefSchema: refSchema, RefTable: refTable}
			fks[k] = fk
			fkOrder = append(fkOrder, k)
		}
		fk.Columns = append(fk.Columns, column)
		fk.RefColumns = append(fk.RefColumns, refColumn)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the foreign keys of %s: %w", strings.Join(schemas, ", "), err)
	}
	for _, k := range fkOrder {
		if t := tables[k.table]; t != nil {
			t.ForeignKeys = append(t.ForeignKeys, *fks[k])
		}
	}
	return tables, nil
}

// each runs a query and hands every row to fn. It reports the iteration error
// as well as the query error: a rows.Err left unchecked turns a connection that
// died halfway into an empty result, and an empty result here reads as "the
// table has no columns".
func each(ctx context.Context, db bun.IDB, query string, arg any, fn func(*sql.Rows) error) error {
	rows, err := db.QueryContext(ctx, query, arg)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return rows.Close()
}

// Names lists the tables of a schema map in a stable order.
func Names(tables map[string]*Table) []string {
	out := make([]string, 0, len(tables))
	for name := range tables {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
