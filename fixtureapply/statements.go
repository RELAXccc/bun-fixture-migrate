package fixtureapply

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
)

func (r *runner) exec(ctx context.Context, c fixturechange.Change) (outcome, error) {
	t := r.set.Tables[c.Model]
	table, err := quoteIdent(t.Name)
	if err != nil {
		return outcome{}, err
	}
	switch c.Kind {
	case fixturechange.Insert:
		return r.insert(ctx, c, t, table)
	case fixturechange.Update:
		return r.update(ctx, c, t, table)
	default:
		return r.delete(ctx, c, t, table)
	}
}

func (r *runner) insert(ctx context.Context, c fixturechange.Change, t fixturechange.Table, table string) (outcome, error) {
	// An explicit id that another row already holds is checked before the
	// statement runs, so the failure names the row instead of arriving as a
	// primary-key violation from somewhere inside the driver.
	if id, ok := c.New[t.ID]; ok && id.Ref == nil && !id.IsNull && r.set.Policy.IDDrift != fixturechange.ModeIgnore {
		taken, err := r.idTakenByAnotherRow(ctx, c, t, table, id.Lit)
		if err != nil {
			return outcome{}, err
		}
		if taken != "" {
			return outcome{problem: problemIDDrift, message: fmt.Sprintf(
				"%s %s = %s is already held by the row %s. Inserting this row would either fail on the primary key "+
					"or, without a unique index, leave two rows nothing can tell apart. Decide which row keeps the id",
				t.Name, t.ID, id.Lit, taken)}, nil
		}
	}

	var cols, exprs []string
	var args []any
	explicitID := false
	for _, col := range sortedColumns(c.New) {
		expr, a, err := r.written(ctx, c, col)
		if err != nil {
			return outcome{}, err
		}
		q, err := quoteIdent(col)
		if err != nil {
			return outcome{}, err
		}
		if col == t.ID {
			explicitID = true
		}
		cols = append(cols, q)
		exprs = append(exprs, expr)
		args = append(args, a...)
	}
	// The existence check keys on the natural key alone. A row that already
	// exists under a different id is somebody else's row, not ours to insert
	// again: keying on the id as well is how a second copy appears, and then
	// every later lookup by name finds two.
	where, whereArgs, err := r.match(ctx, c.Model, c.Key)
	if err != nil {
		return outcome{}, err
	}
	where, whereArgs = r.scoped(c.Model, where, whereArgs)
	args = append(args, whereArgs...)
	query := fmt.Sprintf("INSERT INTO %s (%s) SELECT %s WHERE NOT EXISTS (SELECT 1 FROM %s WHERE %s)",
		table, strings.Join(cols, ", "), strings.Join(exprs, ", "), table, where)
	n, err := r.write(ctx, c.Model, query, args)
	if err != nil {
		return outcome{}, err
	}
	if n > 0 {
		if explicitID && t.Serial {
			r.resync[c.Model] = true
		}
		return outcome{rows: n}, nil
	}
	return r.diagnoseInsert(ctx, c, t, table)
}

func (r *runner) update(ctx context.Context, c fixturechange.Change, t fixturechange.Table, table string) (outcome, error) {
	var sets []string
	var args []any
	for _, col := range sortedColumns(c.New) {
		expr, a, err := r.written(ctx, c, col)
		if err != nil {
			return outcome{}, err
		}
		q, err := quoteIdent(col)
		if err != nil {
			return outcome{}, err
		}
		sets = append(sets, q+" = "+expr)
		args = append(args, a...)
	}
	where, whereArgs, err := r.guard(ctx, c, t)
	if err != nil {
		return outcome{}, err
	}
	args = append(args, whereArgs...)
	where, args, err = r.onlyRow(ctx, c, table, where, args)
	if err != nil {
		return outcome{}, err
	}
	n, err := r.write(ctx, c.Model, fmt.Sprintf("UPDATE %s SET %s WHERE %s", table, strings.Join(sets, ", "), where), args)
	if err != nil {
		return outcome{}, err
	}
	if n > 0 {
		return outcome{rows: n}, nil
	}
	return r.diagnose(ctx, c, t, table, c.New)
}

func (r *runner) delete(ctx context.Context, c fixturechange.Change, t fixturechange.Table, table string) (outcome, error) {
	where, args, err := r.guard(ctx, c, t)
	if err != nil {
		return outcome{}, err
	}
	if out, err := r.referenced(ctx, c, t, table, where, args); err != nil || out.problem != "" {
		return out, err
	}
	where, args, err = r.onlyRow(ctx, c, table, where, args)
	if err != nil {
		return outcome{}, err
	}
	n, err := r.run(ctx, fmt.Sprintf("DELETE FROM %s WHERE %s", table, where), args)
	if err != nil {
		return outcome{}, err
	}
	if n > 0 {
		return outcome{rows: n}, nil
	}
	return r.diagnose(ctx, c, t, table, nil)
}

// onlyRow narrows a guard to a natural key exactly one row holds. Without it
// an update or a delete matching on a key that two rows share -- an admin
// tool inserted it twice, there is no unique index -- changes both and
// reports success. With it the statement matches nothing, and diagnose says
// why. The subquery reads the table as it was before the statement, which is
// what the count has to be about.
func (r *runner) onlyRow(ctx context.Context, c fixturechange.Change, table, where string, args []any) (string, []any, error) {
	keyWhere, keyArgs, err := r.match(ctx, c.Model, c.Key)
	if err != nil {
		return "", nil, err
	}
	keyWhere, keyArgs = r.scoped(c.Model, keyWhere, keyArgs)
	return fmt.Sprintf("%s AND (SELECT count(*) FROM %s WHERE %s) = 1", where, table, keyWhere),
		append(append([]any{}, args...), keyArgs...), nil
}

func (r *runner) run(ctx context.Context, query string, args []any) (int64, error) {
	res, err := r.tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// write runs an INSERT or an UPDATE and returns its row count. For a model with
// a Where it also makes sure every row it wrote holds it: a fixture row whose
// values put it outside the predicate would otherwise be written, and then be
// invisible to export, check and every later migration, which only look
// inside it.
func (r *runner) write(ctx context.Context, model, query string, args []any) (int64, error) {
	t := r.set.Tables[model]
	if t.Where == "" {
		return r.run(ctx, query, args)
	}
	rows, err := r.tx.QueryContext(ctx, query+" RETURNING (?\n)", append(append([]any{}, args...), bun.Safe(t.Where))...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var n, outside int64
	for rows.Next() {
		var holds sql.NullBool
		if err := rows.Scan(&holds); err != nil {
			return 0, err
		}
		n++
		if !holds.Valid || !holds.Bool {
			outside++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if outside > 0 {
		return 0, fmt.Errorf("the row written into %s does not hold the model's where, %s, so it would not be master "+
			"data and nothing would find it again; nothing was changed. The fixture file and the where in the "+
			"configuration disagree about this row", t.Name, t.Where)
	}
	return n, nil
}
