package fixtureapply

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// incoming is one foreign key pointing at a table.
type incoming struct {
	name, child, action   string
	childCols, parentCols []string
}

// referenced looks, before a delete, for rows that point at the row being
// deleted, through every foreign key that points at its table.
//
// A foreign key declared ON DELETE CASCADE deletes them with it, SET NULL or
// SET DEFAULT detaches them, and RESTRICT or NO ACTION refuses the delete.
// The first two happen without a word to anybody, to rows that are not
// master data -- a subscription on the plan, an order of the product -- so
// they fail the change unless the table allows it (Table.Cascade). The last
// fails anyway; here it fails with the rows named instead of with a
// constraint's name.
func (r *runner) referenced(ctx context.Context, c fixturechange.Change, t fixturechange.Table, table, where string,
	args []any) (outcome, error) {

	fks, err := r.incoming(ctx, table)
	if err != nil {
		return outcome{}, err
	}
	for _, fk := range fks {
		cascades := fk.action == "c" || fk.action == "n" || fk.action == "d"
		if cascades && t.Cascade {
			continue
		}
		var n int64
		query := fmt.Sprintf("SELECT count(*) FROM %s WHERE (%s) IN (SELECT %s FROM %s WHERE %s)",
			fk.child, strings.Join(fk.childCols, ", "), strings.Join(fk.parentCols, ", "), table, where)
		if err := r.tx.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			return outcome{}, fmt.Errorf("count the rows pointing at %s %s: %w", t.Name, keyLabel(c.Key), err)
		}
		if n == 0 {
			continue
		}
		what := map[string]string{"c": "deletes them with it", "n": "sets their reference to NULL",
			"d": "sets their reference to its default"}[fk.action]
		msg := fmt.Sprintf("%s of %s point at %s %s through %s, which %s", rowCount(n), fk.child, t.Name,
			keyLabel(c.Key), fk.name, what)
		if cascades {
			msg += ". They are not master data and this change set does not know them: repoint or remove " +
				"them first, or set deletes: cascade on the model if deleting them with it is what you want"
		} else {
			msg = fmt.Sprintf("%s of %s point at %s %s through %s, which refuses the delete: repoint or "+
				"remove them first", rowCount(n), fk.child, t.Name, keyLabel(c.Key), fk.name)
		}
		return outcome{problem: problemReferenced, message: msg}, nil
	}
	return outcome{}, nil
}

// incoming reads the foreign keys pointing at a table, with every name
// already quoted by PostgreSQL.
func (r *runner) incoming(ctx context.Context, table string) ([]incoming, error) {
	rows, err := r.tx.QueryContext(ctx, `
SELECT con.conname::text, con.conrelid::regclass::text, con.confdeltype::text,
       array_to_json(ARRAY(SELECT quote_ident(a.attname) FROM unnest(con.conkey) WITH ORDINALITY k(n, o)
             JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.n ORDER BY k.o))::text,
       array_to_json(ARRAY(SELECT quote_ident(a.attname) FROM unnest(con.confkey) WITH ORDINALITY k(n, o)
             JOIN pg_attribute a ON a.attrelid = con.confrelid AND a.attnum = k.n ORDER BY k.o))::text
FROM pg_constraint con
WHERE con.contype = 'f' AND con.confrelid = ?::regclass
ORDER BY con.conname`, table)
	if err != nil {
		return nil, fmt.Errorf("read the foreign keys pointing at %s: %w", table, err)
	}
	defer rows.Close()
	var out []incoming
	for rows.Next() {
		var fk incoming
		var child, parent string
		if err := rows.Scan(&fk.name, &fk.child, &fk.action, &child, &parent); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(child), &fk.childCols); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(parent), &fk.parentCols); err != nil {
			return nil, err
		}
		fk.name = quoteLiteralName(fk.name)
		out = append(out, fk)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, rows.Close()
}

// quoteLiteralName puts a constraint's name in quotes for a message.
func quoteLiteralName(name string) string { return "constraint " + strconv.Quote(name) }
