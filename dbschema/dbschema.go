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
	"encoding/json"
	"errors"
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
	// Type is the name of the column's type as the catalog spells it, with
	// any domain peeled off: "int8", "text", "bool", "numeric",
	// "timestamptz", "jsonb", "_text" for text[]. A column of a domain over
	// integer is "int4" here: every question asked of a column's type --
	// what its zero is, whether its values are numbers, how they are read,
	// written and exported -- is a question about the type its values are.
	Type string
	// Domain is the column's domain as SQL names it ("qty",
	// "billing.amount"), "" for a column of a plain type. Its constraints
	// apply to every value, which is why a value is cast to FullType.
	Domain string
	// FullType is the type as SQL writes it, with its modifiers and, where
	// the search path needs it, its schema: "numeric(10,2)", "character
	// varying(20)", "text[]", "timestamp with time zone", or the domain's
	// name. It is what a value is cast to so PostgreSQL can say what the
	// column would hold.
	FullType string
	// Category is the type's category (pg_type.typcategory): "A" for an
	// array, "E" for an enum, "N" numeric, "S" string, "D" date and time,
	// "U" user-defined, and so on. A domain has its base type's.
	Category string
	// ElemCategory is the category of an array's element type, "" for a
	// column that is not an array.
	ElemCategory string
	// ElemType is the name of an array's element type with any domain
	// peeled off, "bpchar" for character(3)[]; "" for a column that is not
	// an array.
	ElemType string
	// Length is the declared length of a character or bit-string column, or
	// of the elements of an array of them, a domain's included: 3 for
	// varchar(3), character(3)[] and bit(3). It is 0 when there is none.
	// An explicit cast truncates to it without a word where an INSERT
	// refuses, so a value is checked against it before it is cast.
	Length int
	// Nullable is true when the column accepts NULL: neither the column nor
	// its domain says NOT NULL.
	Nullable bool
	// Default is the column default exactly as the catalog stores it, or
	// the domain's when the column has none, "" when neither has one.
	// "nextval('t_id_seq'::regclass)" for a serial column, "0", "false",
	// "'x'::text", "now()".
	Default string
	// Identity is true for a GENERATED ... AS IDENTITY column.
	Identity bool
	// IdentityAlways is true for GENERATED ALWAYS AS IDENTITY, which refuses
	// an explicit value from anybody: dbfixture, bun or a migration.
	IdentityAlways bool
	// Generated is true for a GENERATED ALWAYS AS (...) STORED column. Its
	// Default is the generation expression, and nothing can write into it.
	Generated bool
	// Checks are the table's CHECK constraints that name this column and no
	// other, which a value can be held against on its own. A constraint
	// over several columns is not among them.
	Checks []Check
}

// Check is a CHECK constraint over one column.
type Check struct {
	// Name is the constraint's name.
	Name string
	// Expr is its expression as pg_get_expr writes it, naming the column
	// unqualified: "(price < 100)".
	Expr string
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
	// KeyIndexes lists every unique index and exclusion constraint of the
	// table, the primary key, partial and expression indexes and invalid
	// ones included, with what decides whether one makes a natural key
	// unique. Uniques is the plain, valid, whole-table part of it.
	KeyIndexes []KeyIndex
}

// KeyIndex is a unique index, a unique or primary-key constraint's index, or
// an exclusion constraint, as the natural-key lint needs to judge it.
type KeyIndex struct {
	// Name is the index's name, which a constraint's index shares.
	Name string
	// Primary is the primary key's index.
	Primary bool
	// Unique is a unique index; Exclusion an exclusion constraint, which a
	// constraint over every column WITH = makes a unique key without being
	// a unique index.
	Unique, Exclusion bool
	// Valid is false for an index nothing may trust: one a CREATE INDEX
	// CONCURRENTLY or REINDEX CONCURRENTLY that failed left behind
	// (indisvalid, indisready or indislive false). It refuses no duplicate
	// the table held when it was built, and those are still there.
	Valid bool
	// Deferrable is a constraint declared DEFERRABLE (indimmediate false),
	// whose check may wait for the end of the transaction.
	Deferrable bool
	// NullsNotDistinct is an index declared NULLS NOT DISTINCT, which holds
	// two NULLs equal. PostgreSQL 15 and later; always false before.
	NullsNotDistinct bool
	// Predicate is a partial index's predicate as pg_get_expr writes it,
	// "(deleted_at IS NULL)"; "" for an index over every row.
	Predicate string
	// Columns are the index's key columns in order, the columns an INCLUDE
	// clause adds left out.
	Columns []IndexColumn
	// Reads are the table's columns the index's expressions and predicate
	// read, sorted, as pg_depend records them. It lists the plain columns of
	// an expression or partial index too, and nothing for an index that
	// backs a constraint over plain columns only.
	Reads []string
}

// IndexColumn is one key column of a KeyIndex: a plain column or an
// expression.
type IndexColumn struct {
	// Column is the table's column, "" for an expression.
	Column string
	// Expr is the expression as pg_get_indexdef writes it, "lower(email)";
	// "" for a plain column.
	Expr string
	// Operator is an exclusion constraint's operator for the column, "=" or
	// "&&"; "" in a unique index.
	Operator string
}

// Plain reports whether every key column of the index is a plain column.
func (k KeyIndex) Plain() bool {
	for _, c := range k.Columns {
		if c.Column == "" {
			return false
		}
	}
	return true
}

// Equality reports whether the index refuses two rows whose columns are
// equal: a unique index, or an exclusion constraint whose every operator is
// =.
func (k KeyIndex) Equality() bool {
	if k.Unique {
		return true
	}
	if !k.Exclusion || len(k.Columns) == 0 {
		return false
	}
	for _, c := range k.Columns {
		if c.Operator != "=" {
			return false
		}
	}
	return true
}

// Definition is the index as a person reads it in a message: "UNIQUE
// (parent_id, code)", "UNIQUE NULLS NOT DISTINCT (parent_id, code)",
// "UNIQUE (lower(email))", "EXCLUDE (code WITH =)", followed by "WHERE"
// and the predicate of a partial index, and "DEFERRABLE" where the
// constraint is.
func (k KeyIndex) Definition() string {
	parts := make([]string, 0, len(k.Columns))
	for _, c := range k.Columns {
		item := c.Column
		if item == "" {
			item = c.Expr
			// An expression that is a call is written as it is; any other
			// is in the parentheses pg_get_indexdef puts around it, or in
			// some.
			if !callExpr(item) && trimParens(item) == item {
				item = "(" + item + ")"
			}
		}
		if k.Exclusion && !k.Unique {
			item += " WITH " + c.Operator
		}
		parts = append(parts, item)
	}
	head := "UNIQUE"
	switch {
	case k.Primary:
		head = "PRIMARY KEY"
	case k.Exclusion && !k.Unique:
		head = "EXCLUDE"
	case k.NullsNotDistinct:
		head = "UNIQUE NULLS NOT DISTINCT"
	}
	out := head + " (" + strings.Join(parts, ", ") + ")"
	if k.Predicate != "" {
		out += " WHERE " + trimParens(k.Predicate)
	}
	if k.Deferrable {
		out += " DEFERRABLE"
	}
	return out
}

// callExpr reports whether an expression is one function call,
// "lower(email)", which an index column may be without parentheses of its
// own.
func callExpr(expr string) bool {
	open := strings.IndexByte(expr, '(')
	if open <= 0 || !strings.HasSuffix(expr, ")") {
		return false
	}
	for _, r := range expr[:open] {
		if !(r == '_' || r == '.' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	depth := 0
	for i, r := range expr[open:] {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 && open+i != len(expr)-1 {
				return false
			}
		}
	}
	return depth == 0
}

// trimParens takes away the parentheses pg_get_expr puts around a whole
// predicate, "(deleted_at IS NULL)", when they enclose all of it.
func trimParens(expr string) string {
	for len(expr) >= 2 && expr[0] == '(' && expr[len(expr)-1] == ')' {
		depth, whole := 0, true
		inQuote := false
		for i := 0; i < len(expr); i++ {
			switch c := expr[i]; {
			case c == '\'':
				inQuote = !inQuote
			case inQuote:
			case c == '(':
				depth++
			case c == ')':
				depth--
				if depth == 0 && i != len(expr)-1 {
					whole = false
				}
			}
			if !whole {
				break
			}
		}
		if !whole {
			break
		}
		expr = expr[1 : len(expr)-1]
	}
	return expr
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

// StringField reports whether a model writes this column from a Go string:
// a string type, a domain over one, an enum, or an array of any of them. It
// matters for a fixture value such as 1.10 or 017, which yaml.v3 hands a
// string field as written and a number field as the number it resolves to.
func (c Column) StringField() bool {
	category := c.Category
	if category == "A" {
		category = c.ElemCategory
	}
	return category == "S" || category == "E"
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

// declaredLength reads the length out of a type modifier: varchar and bpchar
// keep n plus a four-byte header, bit and varbit keep n itself. An array's
// modifier is its elements'.
func declaredLength(c Column, typmod int) int {
	typ := c.Type
	if c.Category == "A" {
		typ = c.ElemType
	}
	switch typ {
	case "varchar", "bpchar":
		if typmod >= 4 {
			return typmod - 4
		}
	case "bit", "varbit":
		if typmod > 0 {
			return typmod
		}
	}
	return 0
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
	list := bun.List(schemas)
	tables := map[string]*Table{}

	// One row per column. Nothing is aggregated in SQL: a column name may hold
	// any character, so there is no separator a string_agg could use safely.
	//
	// A domain is followed down to the type its values are, a domain over a
	// domain included; on the way it may contribute a length, a NOT NULL and a
	// default. The default of the outermost domain that has one is what an
	// INSERT saying DEFAULT gets, unless the column has its own.
	const columnQuery = `
WITH RECURSIVE dom AS (
	SELECT oid AS dom, typbasetype AS base, typtypmod AS typmod, typnotnull AS notnull, typdefaultbin AS def
	FROM pg_type WHERE typtype = 'd'
	UNION ALL
	SELECT dom.dom, t.typbasetype, CASE WHEN dom.typmod <> -1 THEN dom.typmod ELSE t.typtypmod END,
	       dom.notnull OR t.typnotnull, COALESCE(dom.def, t.typdefaultbin)
	FROM dom JOIN pg_type t ON t.oid = dom.base WHERE t.typtype = 'd'
), domains AS (
	SELECT dom.* FROM dom JOIN pg_type b ON b.oid = dom.base WHERE b.typtype <> 'd'
)
SELECT n.nspname, c.relname, a.attname, a.attnum, bt.typname,
       CASE WHEN t.typtype = 'd' THEN format_type(t.oid, NULL) ELSE '' END,
       format_type(a.atttypid, a.atttypmod), bt.typcategory::text, COALESCE(et.typcategory::text, ''),
       COALESCE(ebt.typname, ''), COALESCE(NULLIF(a.atttypmod, -1), NULLIF(r.typmod, -1), NULLIF(er.typmod, -1), -1),
       NOT a.attnotnull AND NOT COALESCE(r.notnull, false),
       COALESCE(pg_get_expr(d.adbin, d.adrelid), pg_get_expr(r.def, 0), ''), a.attidentity <> '',
       a.attidentity = 'a', a.attgenerated <> ''
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_type t ON t.oid = a.atttypid
LEFT JOIN domains r ON r.dom = a.atttypid
JOIN pg_type bt ON bt.oid = COALESCE(r.base, a.atttypid)
LEFT JOIN pg_type et ON et.oid = bt.typelem AND bt.typcategory = 'A'
LEFT JOIN domains er ON er.dom = et.oid
LEFT JOIN pg_type ebt ON ebt.oid = COALESCE(er.base, et.oid)
LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE n.nspname IN (?) AND c.relkind IN ('r', 'p') AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY n.nspname, c.relname, a.attnum`
	if err := each(ctx, db, columnQuery, list, func(rows *sql.Rows) error {
		var schema, table string
		var col Column
		var typmod int
		if err := rows.Scan(&schema, &table, &col.Name, &col.Position, &col.Type, &col.Domain, &col.FullType,
			&col.Category, &col.ElemCategory, &col.ElemType, &typmod, &col.Nullable,
			&col.Default, &col.Identity, &col.IdentityAlways, &col.Generated); err != nil {
			return err
		}
		col.Length = declaredLength(col, typmod)
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

	// One row per CHECK constraint that names a single column. A constraint
	// over several columns cannot be held against one value; PostgreSQL
	// checks it when the row is written.
	const checkQuery = `
SELECT n.nspname, c.relname, a.attname, con.conname, pg_get_expr(con.conbin, con.conrelid)
FROM pg_constraint con
JOIN pg_class c ON c.oid = con.conrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = con.conkey[1]
WHERE n.nspname IN (?) AND con.contype = 'c' AND cardinality(con.conkey) = 1
ORDER BY n.nspname, c.relname, con.conname`
	if err := each(ctx, db, checkQuery, list, func(rows *sql.Rows) error {
		var schema, table, column string
		var check Check
		if err := rows.Scan(&schema, &table, &column, &check.Name, &check.Expr); err != nil {
			return err
		}
		if t := tables[schema+"."+table]; t != nil {
			for i := range t.Columns {
				if t.Columns[i].Name == column {
					t.Columns[i].Checks = append(t.Columns[i].Checks, check)
				}
			}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the check constraints of %s: %w", strings.Join(schemas, ", "), err)
	}

	// One row per indexed column, in index order. A partial index and an index
	// over an expression are left out: neither makes a lookup by those columns
	// unique, so neither is a natural key.
	//
	// So is an index that is not valid, which a CREATE INDEX CONCURRENTLY
	// that failed leaves behind: it refuses nothing it was built over, and
	// the duplicates it failed on are still there. An exclusion constraint
	// whose every operator is = refuses two rows equal in its columns, as a
	// unique index does, and is one; the columns an INCLUDE adds are not.
	const indexQuery = `
SELECT n.nspname, c.relname, i.indexrelid::bigint, i.indisprimary, a.attname
FROM pg_index i
JOIN pg_class c ON c.oid = i.indrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY k(attnum, ord)
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
WHERE n.nspname IN (?) AND i.indpred IS NULL AND i.indexprs IS NULL AND i.indisvalid AND i.indislive
  AND k.ord <= i.indnkeyatts AND (i.indisunique OR i.indisexclusion AND NOT EXISTS (
      SELECT 1 FROM pg_constraint x JOIN pg_operator o ON o.oid = ANY (x.conexclop)
      WHERE x.conindid = i.indexrelid AND x.contype = 'x' AND o.oprname <> '='))
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

	if err := loadKeyIndexes(ctx, db, list, tables); err != nil {
		return nil, fmt.Errorf("read the unique indexes and exclusion constraints of %s: %w",
			strings.Join(schemas, ", "), err)
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

// keyIndexQuery is one row per unique index and exclusion constraint, with
// its key columns, and the columns its expressions and predicate read, as
// JSON: a column name may hold any character, and JSON quotes every one.
// pg_index.indnullsnotdistinct exists from PostgreSQL 15 on, so it is read
// through to_jsonb, which has no such key before.
const keyIndexQuery = `
SELECT n.nspname, c.relname, ic.relname, i.indisprimary, i.indisunique, i.indisexclusion,
       i.indisvalid AND i.indisready AND i.indislive, NOT i.indimmediate,
       COALESCE((to_jsonb(i) ->> 'indnullsnotdistinct')::bool, false),
       COALESCE(pg_get_expr(i.indpred, i.indrelid), ''),
       (SELECT json_agg(json_build_object(
                  'column', CASE WHEN k.attnum > 0 THEN a.attname END,
                  'expr', CASE WHEN k.attnum = 0 THEN pg_get_indexdef(i.indexrelid, k.n::int, true) END,
                  'op', CASE WHEN i.indisexclusion THEN (SELECT o.oprname FROM pg_constraint x
                             JOIN pg_operator o ON o.oid = x.conexclop[k.n]
                             WHERE x.conindid = i.indexrelid AND x.contype = 'x') END)
                ORDER BY k.n)
        FROM unnest(i.indkey::int2[]) WITH ORDINALITY k(attnum, n)
        LEFT JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
        WHERE k.n <= i.indnkeyatts)::text,
       (SELECT COALESCE(json_agg(DISTINCT a.attname), '[]') FROM pg_depend d
        JOIN pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
        WHERE d.classid = 'pg_class'::regclass AND d.objid = i.indexrelid
          AND d.refclassid = 'pg_class'::regclass AND d.refobjid = i.indrelid AND d.refobjsubid > 0)::text
FROM pg_index i
JOIN pg_class ic ON ic.oid = i.indexrelid
JOIN pg_class c ON c.oid = i.indrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname IN (?) AND (i.indisunique OR i.indisexclusion)
ORDER BY n.nspname, c.relname, ic.relname`

// loadKeyIndexes reads every table's KeyIndexes.
func loadKeyIndexes(ctx context.Context, db bun.IDB, list bun.ListValues, tables map[string]*Table) error {
	return each(ctx, db, keyIndexQuery, list, func(rows *sql.Rows) error {
		var schema, table, columns, reads string
		var k KeyIndex
		if err := rows.Scan(&schema, &table, &k.Name, &k.Primary, &k.Unique, &k.Exclusion, &k.Valid,
			&k.Deferrable, &k.NullsNotDistinct, &k.Predicate, &columns, &reads); err != nil {
			return err
		}
		var cols []struct {
			Column, Expr, Op *string
		}
		if err := json.Unmarshal([]byte(columns), &cols); err != nil {
			return fmt.Errorf("index %s: %w", k.Name, err)
		}
		for _, c := range cols {
			var ic IndexColumn
			if c.Column != nil {
				ic.Column = *c.Column
			}
			if c.Expr != nil {
				ic.Expr = *c.Expr
			}
			if c.Op != nil {
				ic.Operator = *c.Op
			}
			k.Columns = append(k.Columns, ic)
		}
		if err := json.Unmarshal([]byte(reads), &k.Reads); err != nil {
			return fmt.Errorf("index %s: %w", k.Name, err)
		}
		sort.Strings(k.Reads)
		if t := tables[schema+"."+table]; t != nil {
			t.KeyIndexes = append(t.KeyIndexes, k)
		}
		return nil
	})
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

// NotATable says what a relation is that Load leaves out although it exists:
// "a view", "a materialized view", "a foreign table", and so on. It is "" when
// nothing of that name exists. qualified is "schema.name", the way Load keys
// its result.
//
// Only a table holds master data: a view's rows belong to the tables it reads,
// and whatever writes into it writes into those, if anything. So a model
// naming one is refused, and this is what the refusal says.
func NotATable(ctx context.Context, db bun.IDB, qualified string) (string, error) {
	schema, name, ok := strings.Cut(qualified, ".")
	if !ok {
		schema, name = "public", qualified
	}
	var kind string
	err := db.QueryRowContext(ctx, `SELECT c.relkind::text FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = ? AND c.relname = ?`, schema, name).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	switch kind {
	case "r", "p":
		return "", nil
	case "v":
		return "a view", nil
	case "m":
		return "a materialized view", nil
	case "f":
		return "a foreign table", nil
	case "S":
		return "a sequence", nil
	case "i", "I":
		return "an index", nil
	case "c":
		return "a composite type", nil
	}
	return "not a table", nil
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
