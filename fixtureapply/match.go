package fixtureapply

import (
	"context"
	"fmt"
	"strings"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// colType is what a comparison needs to know about a column's type.
type colType struct {
	// cast is the type a value is cast to before it is compared: the
	// column's own, without a length for the character types, since an
	// explicit cast to varchar(3) truncates without a word.
	cast string
	// base is the name of the type, or of a domain's base type.
	base string
	// array is true for an array type.
	array bool
	// equality is true when the type has an = operator of its own. json,
	// xml, point and the other geometric types do not, and neither, in
	// pg_operator's terms, do arrays and enums, which compare through anyarray
	// and anyenum.
	equality bool
}

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
SELECT a.attname, format_type(a.atttypid, a.atttypmod), format_type(a.atttypid, NULL), bt.typname,
       bt.typcategory = 'A',
       EXISTS (SELECT 1 FROM pg_operator o WHERE o.oprname = '=' AND o.oprleft = bt.oid AND o.oprright = bt.oid)
FROM pg_attribute a
JOIN pg_type t ON t.oid = a.atttypid
JOIN pg_type bt ON bt.oid = CASE WHEN t.typbasetype <> 0 THEN t.typbasetype ELSE t.oid END
WHERE a.attrelid = ?::regclass AND a.attnum > 0 AND NOT a.attisdropped`, table)
	if err != nil {
		return nil, fmt.Errorf("read the column types of %s: %w", r.set.Tables[model].Name, err)
	}
	defer rows.Close()
	types := map[string]colType{}
	for rows.Next() {
		var name, full, bare string
		var ct colType
		if err := rows.Scan(&name, &full, &bare, &ct.base, &ct.array, &ct.equality); err != nil {
			return nil, err
		}
		ct.cast = full
		switch ct.base {
		case "varchar", "bpchar", "_varchar", "_bpchar":
			ct.cast = bare
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
// values, and the id when the change carries one.
func (r *runner) guard(ctx context.Context, c fixturechange.Change, t fixturechange.Table) (string, []any, error) {
	sets := []fixturechange.Values{c.Key, c.Old}
	if c.ID != "" {
		sets = append(sets, fixturechange.Values{t.ID: fixturechange.Lit(c.ID)})
	}
	return r.matchAll(ctx, c.Model, sets...)
}

// match renders "col IS NOT DISTINCT FROM <value>" for every column, joined by
// AND. IS NOT DISTINCT FROM rather than = so a NULL compares like any other
// value.
func (r *runner) match(ctx context.Context, model string, values fixturechange.Values) (string, []any, error) {
	var parts []string
	var args []any
	for _, col := range sortedColumns(values) {
		expr, a, err := r.value(ctx, model, col, values[col])
		if err != nil {
			return "", nil, err
		}
		part, err := r.compare(ctx, model, col, expr)
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

func (r *runner) matchAll(ctx context.Context, model string, sets ...fixturechange.Values) (string, []any, error) {
	var parts []string
	var args []any
	for _, values := range sets {
		if len(values) == 0 {
			continue
		}
		part, a, err := r.match(ctx, model, values)
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
// sequence arrives: its elements are unpacked and cast to the array's type.
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
			if ct, ok := types[col]; ok && ct.array {
				return "ARRAY(SELECT jsonb_array_elements_text(?::jsonb))::" + ct.cast, []any{v.Lit}, nil
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

// compare is "col IS NOT DISTINCT FROM value", typed. The value is cast to
// the column's type first, so it compares as what the column would hold:
// 1.005 in a numeric(10,2) is 1.01, an upper-case uuid is the lower-case one.
// A json column compares through jsonb, and a type without an equality of its
// own through the text of both sides; for json, xml or point the bare
// comparison is an error, and it would fail the migration at deploy time.
func (r *runner) compare(ctx context.Context, model, col, value string) (string, error) {
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
	case ct.equality:
		return q + " IS NOT DISTINCT FROM (" + value + ")::" + ct.cast, nil
	}
	return q + "::text IS NOT DISTINCT FROM ((" + value + ")::" + ct.cast + ")::text", nil
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
	rows, err := r.tx.QueryContext(ctx,
		fmt.Sprintf("SELECT %s FROM %s WHERE %s = ? LIMIT 2", idCol, table, keyCol), ref.Key)
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
		return "", fmt.Errorf("%s %q: no row in %s with %s = %q", ref.Model, ref.Key, t.Name, t.Key, ref.Key)
	case 1:
		r.refs[cacheKey] = ids[0]
		return ids[0], nil
	default:
		return "", fmt.Errorf("%s %q: more than one row in %s with %s = %q, cannot tell them apart",
			ref.Model, ref.Key, t.Name, t.Key, ref.Key)
	}
}
