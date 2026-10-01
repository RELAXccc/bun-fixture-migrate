package fixtureapply

import (
	"context"
	"fmt"
	"strings"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// duplicate is the outcome for a natural key n rows hold.
func duplicate(t fixturechange.Table, c fixturechange.Change, n int64) outcome {
	return outcome{problem: problemDuplicate, message: fmt.Sprintf(
		"%d rows of %s hold %s. A change finds its row by the natural key, and nothing says which of them the "+
			"fixture file means, so none was touched. Remove the extra rows, and add a unique index on the key so "+
			"they cannot come back", n, t.Name, keyLabel(c.Key))}
}

// diagnose works out why a guarded update or delete matched nothing. The answer
// decides whether the migration may be recorded as applied, so it is worth
// three extra queries on a path that is not supposed to be taken.
//
// Every one of them propagates its error. A failed statement inside a
// PostgreSQL transaction poisons it: the eventual COMMIT returns the ROLLBACK
// tag and no error at all, so a diagnostic whose error is swallowed silently
// throws the whole migration away while reporting success.
func (r *runner) diagnose(ctx context.Context, c fixturechange.Change, t fixturechange.Table, table string,
	wanted fixturechange.Values) (outcome, error) {

	byKey, err := r.count(ctx, c.Model, table, c.Key)
	if err != nil {
		return outcome{}, err
	}
	if byKey > 1 {
		return duplicate(t, c, byKey), nil
	}
	if byKey == 0 {
		if c.Kind == fixturechange.Delete {
			return outcome{problem: problemBenign, message: "the row is already gone, nothing to delete"}, nil
		}
		return outcome{problem: problemMissing, message: fmt.Sprintf(
			"no row of %s has %s. The row this change updates is not in the database, so the change cannot be made. "+
				"Put the row back, or drop this change from the migration",
			t.Name, keyLabel(c.Key))}, nil
	}
	if len(wanted) > 0 {
		already, err := r.count(ctx, c.Model, table, c.Key, wanted)
		if err != nil {
			return outcome{}, err
		}
		if already > 0 {
			return outcome{problem: problemBenign, message: "the row already holds these values, nothing to do"}, nil
		}
	}
	if c.ID != "" {
		withID, err := r.count(ctx, c.Model, table, c.Key, fixturechange.Values{t.ID: fixturechange.Lit(c.ID)})
		if err != nil {
			return outcome{}, err
		}
		if withID == 0 {
			ids, err := r.idsFor(ctx, c.Model, table, t, c.Key)
			if err != nil {
				return outcome{}, err
			}
			return outcome{problem: problemIDDrift, message: fmt.Sprintf(
				"%s %s exists, but under %s %s and not %s. This change was generated for the row with that id; "+
					"applying it to a different row would move data the ids point at",
				t.Name, keyLabel(c.Key), t.ID, strings.Join(ids, ", "), c.ID)}, nil
		}
	}
	return outcome{problem: problemChanged, message: fmt.Sprintf(
		"%s %s no longer holds the values this change was generated against, so somebody changed it in this "+
			"database. It was left alone. Compare it with the fixture file and decide which one is right",
		t.Name, keyLabel(c.Key))}, nil
}

// diagnoseInsert explains an insert whose NOT EXISTS found a row.
func (r *runner) diagnoseInsert(ctx context.Context, c fixturechange.Change, t fixturechange.Table,
	table string) (outcome, error) {

	byKey, err := r.count(ctx, c.Model, table, c.Key)
	if err != nil {
		return outcome{}, err
	}
	if byKey > 1 {
		return duplicate(t, c, byKey), nil
	}
	same, err := r.count(ctx, c.Model, table, c.Key, withoutColumn(c.New, t.ID))
	if err != nil {
		return outcome{}, err
	}
	if same > 0 {
		return outcome{problem: problemBenign, message: "the row is already there with these values, nothing to insert"}, nil
	}
	return outcome{problem: problemChanged, message: fmt.Sprintf(
		"%s %s already exists and holds different values, so nothing was inserted. Somebody added or edited this "+
			"row in this database. Compare it with the fixture file and decide which one is right",
		t.Name, keyLabel(c.Key))}, nil
}

// idTakenByAnotherRow returns a description of the row holding that id when it
// is not the row the change is about, and "" otherwise.
func (r *runner) idTakenByAnotherRow(ctx context.Context, c fixturechange.Change, t fixturechange.Table,
	table, id string) (string, error) {

	keyWhere, keyArgs, err := r.match(ctx, c.Model, c.Key)
	if err != nil {
		return "", err
	}
	idCol, err := quoteIdent(t.ID)
	if err != nil {
		return "", err
	}
	args := append([]any{id}, keyArgs...)
	var found string
	query := fmt.Sprintf("SELECT %s::text FROM %s WHERE %s = ? AND NOT (%s) LIMIT 1", idCol, table, idCol, keyWhere)
	err = r.tx.QueryRowContext(ctx, query, args...).Scan(&found)
	if err != nil {
		if isNoRows(err) {
			return "", nil
		}
		return "", fmt.Errorf("look for the row holding %s = %s: %w", t.ID, id, err)
	}
	if t.Key != "" {
		keyCol, err := quoteIdent(t.Key)
		if err != nil {
			return "", err
		}
		var label string
		q := fmt.Sprintf("SELECT %s::text FROM %s WHERE %s = ? LIMIT 1", keyCol, table, idCol)
		if err := r.tx.QueryRowContext(ctx, q, found).Scan(&label); err != nil && !isNoRows(err) {
			return "", err
		} else if err == nil {
			return fmt.Sprintf("%s = %s (%s %s)", t.ID, found, t.Key, label), nil
		}
	}
	return t.ID + " = " + found, nil
}

func (r *runner) idsFor(ctx context.Context, model, table string, t fixturechange.Table,
	key fixturechange.Values) ([]string, error) {

	where, args, err := r.match(ctx, model, key)
	if err != nil {
		return nil, err
	}
	idCol, err := quoteIdent(t.ID)
	if err != nil {
		return nil, err
	}
	rows, err := r.tx.QueryContext(ctx,
		fmt.Sprintf("SELECT %s::text FROM %s WHERE %s ORDER BY 1 LIMIT 5", idCol, table, where), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, rows.Close()
}

func (r *runner) count(ctx context.Context, model, table string, sets ...fixturechange.Values) (int64, error) {
	where, args, err := r.matchAll(ctx, model, sets...)
	if err != nil {
		return 0, err
	}
	var n int64
	if err := r.tx.QueryRowContext(ctx,
		fmt.Sprintf("SELECT count(*) FROM %s WHERE %s", table, where), args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count the rows the change should have matched: %w", err)
	}
	return n, nil
}

func withoutColumn(values fixturechange.Values, col string) fixturechange.Values {
	out := make(fixturechange.Values, len(values))
	for k, v := range values {
		if k == col {
			continue
		}
		out[k] = v
	}
	return out
}
