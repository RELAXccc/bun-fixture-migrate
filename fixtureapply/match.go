package fixtureapply

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
)

// colType is what a comparison needs to know about a column's type.
type colType struct {
	// cast is the type a value is cast to before it is compared: the
	// column's own, without a length for the character types, since an
	// explicit cast to varchar(3) truncates without a word. Without a length
	// is "bpchar", not "character": the latter is char(1), and 'EUR' cast to
	// it is 'E'.
	cast string
	// base is the name of the type, or of a domain's base type.
	base string
	// array is true for an array type.
	array bool
	// equality is true when the type has an = operator of its own that says
	// two values are the same value. json, xml and point have none, and
	// neither, in pg_operator's terms, do enums, which compare through
	// anyenum. The other geometric types have one that says something else;
	// see sameIsNotEqual.
	//
	// An array has it when its element type has the default equality of a
	// btree or hash operator class, which is what an array's = compares the
	// elements with: char(3)[] holds {EUR,"US "} for the fixture's
	// ["EUR","US"], and only bpchar's = says those are the same, as it does
	// for a char(3) column. Through their text they differ, and every guard on
	// such a column failed to match.
	equality bool
	// loose is true when the type's = holds two values equal that are not
	// the same value; see looseEquality. Only a natural key compares
	// through it.
	loose bool
}

// sameIsNotEqual are the types whose = is not "the same value": box and circle
// compare areas, path the number of points, and lseg and line within a
// tolerance. A guard comparing through it would take a hand edit to another
// box of the same area for the value the change was generated against, and
// overwrite it. They compare through their text, as point does.
var sameIsNotEqual = map[string]bool{"box": true, "circle": true, "path": true, "lseg": true, "line": true}

// looseEquality are the types whose = holds values equal that a person tells
// apart: interval's says '1 mon' is '30 days' and '1 day' is '24:00:00', which
// are different values ('2026-01-31' plus one month is not plus 30 days), and
// citext's ignores case. A column under a nondeterministic collation is the
// same. A guard comparing an old value through it would take a hand edit for
// the value the change was generated against, and overwrite it even under
// ChangedRow error, so an old or a new value compares through its text.
//
// A natural key keeps the type's own =, which is what the table's unique
// index holds the key to: a row with name 'Team' in a citext column is the
// row the key 'team' names, as an insert of 'team' would find out.
var looseEquality = map[string]bool{"interval": true, "citext": true}

// colTypes reads the column types of a model's table, once per run.
func (r *runner) colTypes(ctx context.Context, model string) (map[string]colType, error) {
	if types, ok := r.types[model]; ok {
		return types, nil
	}
	table, err := quoteIdent(r.set.Tables[model].Name)
	if err != nil {
		return nil, err
	}
	rows, err := r.tx.QueryContext(ctx, `
SELECT a.attname, format_type(a.atttypid, a.atttypmod), format_type(a.atttypid, -1), bt.typname,
       bt.typcategory = 'A',
       EXISTS (SELECT 1 FROM pg_operator o WHERE o.oprname = '=' AND o.oprleft = bt.oid AND o.oprright = bt.oid),
       coalesce((SELECT NOT c.collisdeterministic FROM pg_collation c WHERE c.oid = a.attcollation), false),
       eb.typname,
       EXISTS (SELECT 1 FROM pg_opclass oc JOIN pg_am am ON am.oid = oc.opcmethod
               WHERE oc.opcdefault AND am.amname IN ('btree', 'hash') AND oc.opcintype = eb.oid)
FROM pg_attribute a
JOIN pg_type t ON t.oid = a.atttypid
JOIN pg_type bt ON bt.oid = CASE WHEN t.typbasetype <> 0 THEN t.typbasetype ELSE t.oid END
LEFT JOIN pg_type et ON et.oid = bt.typelem AND bt.typcategory = 'A'
LEFT JOIN pg_type eb ON eb.oid = CASE WHEN et.typbasetype <> 0 THEN et.typbasetype ELSE et.oid END
WHERE a.attrelid = ?::regclass AND a.attnum > 0 AND NOT a.attisdropped`, table)
	if err != nil {
		return nil, fmt.Errorf("read the column types of %s: %w", r.set.Tables[model].Name, err)
	}
	defer rows.Close()
	types := map[string]colType{}
	for rows.Next() {
		var name, full, bare string
		var elem sql.NullString
		var elemEquality bool
		var ct colType
		if err := rows.Scan(&name, &full, &bare, &ct.base, &ct.array, &ct.equality, &ct.loose,
			&elem, &elemEquality); err != nil {
			return nil, err
		}
		ct.loose = ct.loose || looseEquality[ct.base]
		if ct.array {
			ct.equality = elem.Valid && elemEquality && !sameIsNotEqual[elem.String]
			ct.loose = ct.loose || looseEquality[elem.String]
		}
		ct.cast = full
		switch ct.base {
		case "varchar", "bpchar", "_varchar", "_bpchar":
			ct.cast = bare
		}
		if sameIsNotEqual[ct.base] {
			ct.equality = false
		}
		types[name] = ct
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	r.types[model] = types
	return types, nil
}

// guard is the WHERE clause of an update or a delete: the natural key, the old
// values, and the id when the change carries one, among the model's rows.
func (r *runner) guard(ctx context.Context, c fixturechange.Change, t fixturechange.Table) (string, []any, error) {
	values := []fixturechange.Values{c.Old}
	if c.ID != "" {
		values = append(values, fixturechange.Values{t.ID: fixturechange.Lit(c.ID)})
	}
	where, args, err := r.matchAll(ctx, c.Model, c.Key, values...)
	if err != nil {
		return "", nil, err
	}
	where, args = r.scoped(c.Model, where, args)
	return where, args, nil
}

// scoped limits a condition on a model's rows to the rows its Where holds for.
//
// The predicate goes in as a bun.Safe argument rather than as text: bun reads
// every ? in a query's text as a placeholder, and jsonb's ? operator is a
// likely thing to find in one. It ends in a line break, so a -- comment at its
// end cannot swallow the rest of the statement.
func (r *runner) scoped(model, cond string, args []any) (string, []any) {
	w := r.set.Tables[model].Where
	if w == "" {
		return cond, args
	}
	return cond + " AND (?\n)", append(append([]any{}, args...), bun.Safe(w))
}

// match renders "col IS NOT DISTINCT FROM <value>" for every column, joined by
// AND. IS NOT DISTINCT FROM rather than = so a NULL compares like any other
// value. key says whether values are a natural key, which compares as the
// table's unique index does; see compare.
//
// A reference that names no row is a value no row holds, so its column
// matches nothing. The change then goes through the same diagnosis as any
// other that found no row, under the policy: a plan whose currency an admin
// renamed no longer holds what the change was generated against, which is a
// changed row and not a reason to fail the deploy whatever changed_row says.
func (r *runner) match(ctx context.Context, model string, values fixturechange.Values, key bool) (string, []any, error) {
	var parts []string
	var args []any
	for _, col := range sortedColumns(values) {
		expr, a, err := r.value(ctx, model, col, values[col])
		var missing *missingRef
		if errors.As(err, &missing) {
			parts = append(parts, "FALSE")
			continue
		}
		if err != nil {
			return "", nil, err
		}
		part, err := r.compare(ctx, model, col, expr, key)
		if err != nil {
			return "", nil, err
		}
		parts = append(parts, part)
		args = append(args, a...)
	}
	if len(parts) == 0 {
		return "TRUE", nil, nil
	}
	return strings.Join(parts, " AND "), args, nil
}

// matchAll is match of a natural key and of further values, joined by AND.
func (r *runner) matchAll(ctx context.Context, model string, key fixturechange.Values,
	values ...fixturechange.Values) (string, []any, error) {

	var parts []string
	var args []any
	for i, vs := range append([]fixturechange.Values{key}, values...) {
		if len(vs) == 0 {
			continue
		}
		part, a, err := r.match(ctx, model, vs, i == 0)
		if err != nil {
			return "", nil, err
		}
		parts = append(parts, part)
		args = append(args, a...)
	}
	if len(parts) == 0 {
		return "TRUE", nil, nil
	}
	return strings.Join(parts, " AND "), args, nil
}

// value renders one value as an expression plus its arguments. A reference is
// looked up now, not turned into a subselect: a subselect that finds nothing
// yields NULL, and the statement around it would happily write that NULL as a
// foreign key, or match a row whose column is NULL, and report a row affected.
//
// A literal is text, which PostgreSQL reads as the column's type. The one
// exception is an array column given a JSON array, which is how a YAML
// sequence arrives: it is written as a PostgreSQL array of the same shape and
// cast to the column's type. A JSON array for a bytea column is refused: read
// as text, the column would hold the characters of the list, not the bytes
// it names, which is how a YAML sequence of numbers fills a []byte field.
func (r *runner) value(ctx context.Context, model, col string, v fixturechange.Value) (string, []any, error) {
	switch {
	case v.IsNull:
		return "NULL", nil, nil
	case v.Ref == nil:
		if strings.HasPrefix(strings.TrimSpace(v.Lit), "[") {
			types, err := r.colTypes(ctx, model)
			if err != nil {
				return "", nil, err
			}
			ct, ok := types[col]
			if !ok || (!ct.array && ct.base != "bytea") {
				return "?", []any{v.Lit}, nil
			}
			literal, isArray, err := arrayLiteral(v.Lit)
			switch {
			case err != nil:
				return "", nil, fmt.Errorf("the value of %s is %s, and %w", col, v.Lit, err)
			case isArray && !ct.array:
				return "", nil, fmt.Errorf("the value of %s is the list %s, and %s is a bytea column: the list would "+
					"be stored as its characters, not as the bytes it names. Write the bytes as \\x followed by "+
					"their hex digits, as in \\x48690a", col, v.Lit, col)
			case isArray:
				return "CAST(? AS " + ct.cast + ")", []any{literal}, nil
			}
		}
		return "?", []any{v.Lit}, nil
	}
	id, err := r.resolve(ctx, *v.Ref)
	if err != nil {
		return "", nil, err
	}
	return "?", []any{id}, nil
}

// written renders a value a statement writes. A reference that names no row
// cannot be written, whatever the policy says: the only alternatives are a
// NULL and a guess.
func (r *runner) written(ctx context.Context, c fixturechange.Change, col string) (string, []any, error) {
	expr, args, err := r.value(ctx, c.Model, col, c.New[col])
	var missing *missingRef
	if errors.As(err, &missing) {
		return "", nil, fmt.Errorf("%s is to point at %s, so there is nothing to point it at. Put that row back, or "+
			"change the fixture file and generate the migration again", col, missing)
	}
	return expr, args, err
}

// missingRef is a reference that names no row: the row was renamed or removed
// in this database, or never added.
type missingRef struct {
	ref        fixturechange.Ref
	table, key string
}

func (e *missingRef) Error() string {
	return fmt.Sprintf("%s %q, and no row of %s has %s = %q: it was renamed or removed in this database, or never "+
		"added", e.ref.Model, e.ref.Key, e.table, e.key, e.ref.Key)
}

// unresolved says which references among values name no row, as a sentence
// to add to a diagnosis, or "" when they all do. An operator reading "changed
// row" alone would look for an edit to the row and find none.
func (r *runner) unresolved(ctx context.Context, values ...fixturechange.Values) (string, error) {
	var names []string
	seen := map[string]bool{}
	for _, vs := range values {
		for _, col := range sortedColumns(vs) {
			ref := vs[col].Ref
			if ref == nil || seen[ref.Model+"\x00"+ref.Key] {
				continue
			}
			seen[ref.Model+"\x00"+ref.Key] = true
			_, err := r.resolve(ctx, *ref)
			var missing *missingRef
			if errors.As(err, &missing) {
				names = append(names, fmt.Sprintf("%s %q", ref.Model, ref.Key))
				continue
			}
			if err != nil {
				return "", err
			}
		}
	}
	if len(names) == 0 {
		return "", nil
	}
	return fmt.Sprintf(" %s is not in this database under that name, so no row can point at it: it was renamed "+
		"or removed here.", strings.Join(names, " and ")), nil
}

// compare is "col IS NOT DISTINCT FROM value", typed. The value is cast to
// the column's type first, so it compares as what the column would hold:
// 1.005 in a numeric(10,2) is 1.01, an upper-case uuid is the lower-case one.
// A json column compares through jsonb, and a type without an equality of its
// own through the text of both sides; for json, xml or point the bare
// comparison is an error, and it would fail the migration at deploy time. So
// does a value, but not a natural key, of a type whose equality is loose (see
// looseEquality). The text compares byte for byte, under the "C" collation.
func (r *runner) compare(ctx context.Context, model, col, value string, key bool) (string, error) {
	q, err := quoteIdent(col)
	if err != nil {
		return "", err
	}
	types, err := r.colTypes(ctx, model)
	if err != nil {
		return "", err
	}
	ct, ok := types[col]
	switch {
	case !ok:
		return q + " IS NOT DISTINCT FROM " + value, nil
	case ct.base == "json":
		return q + "::jsonb IS NOT DISTINCT FROM (" + value + ")::jsonb", nil
	case ct.equality && (key || !ct.loose):
		return q + " IS NOT DISTINCT FROM (" + value + ")::" + ct.cast, nil
	}
	return "(" + q + `::text COLLATE "C") IS NOT DISTINCT FROM (((` + value + ")::" + ct.cast + `)::text COLLATE "C")`, nil
}

func (r *runner) resolve(ctx context.Context, ref fixturechange.Ref) (string, error) {
	cacheKey := ref.Model + "\x00" + ref.Key
	if id, ok := r.refs[cacheKey]; ok {
		return id, nil
	}
	t := r.set.Tables[ref.Model]
	table, err := quoteIdent(t.Name)
	if err != nil {
		return "", err
	}
	idCol, err := quoteIdent(t.ID)
	if err != nil {
		return "", err
	}
	keyCol, err := quoteIdent(t.Key)
	if err != nil {
		return "", err
	}
	where, args := r.scoped(ref.Model, keyCol+" = ?", []any{ref.Key})
	rows, err := r.tx.QueryContext(ctx,
		fmt.Sprintf("SELECT %s FROM %s WHERE %s LIMIT 2", idCol, table, where), args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(ids) {
	case 0:
		return "", &missingRef{ref: ref, table: t.Name, key: t.Key}
	case 1:
		r.refs[cacheKey] = ids[0]
		return ids[0], nil
	default:
		return "", fmt.Errorf("%s %q: more than one row in %s with %s = %q, cannot tell them apart",
			ref.Model, ref.Key, t.Name, t.Key, ref.Key)
	}
}
