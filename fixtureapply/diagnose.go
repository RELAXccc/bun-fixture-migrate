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
		note, err := r.unresolved(ctx, c.Key)
		if err != nil {
			return outcome{}, err
		}
		if c.Kind == fixturechange.Delete {
			return outcome{problem: problemBenign, message: "the row is already gone, nothing to delete." + note}, nil
		}
		if out, done, err := r.diagnoseMoved(ctx, c, t, table, wanted); err != nil || done {
			return out, err
		}
		what := "The row this change updates is not in the database, so the change cannot be made"
		if r.revert {
			what = "The row this change reverts is not in the database, so it cannot be reverted"
		}
		// Not "drop the change from the migration": every other database
		// would then never get it, and nothing would say so.
		return outcome{problem: problemMissing, message: fmt.Sprintf(
			"no row of %s has %s.%s %s. Put the row back; or, if it is meant to be gone in this database, set "+
				"MissingRow to \"warn\" in this migration's Policy, and the change is recorded as done here "+
				"without being made", t.Name, keyLabel(c.Key), note, what)}, nil
	}
	if err := r.stopped(ctx, c, t, table); err != nil {
		return outcome{}, err
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
			// A rename already made, and somebody has taken the old key since.
			if out, done, err := r.diagnoseMoved(ctx, c, t, table, wanted); err != nil ||
				(done && out.problem == problemBenign) {
				return out, err
			}
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
	note, err := r.unresolved(ctx, c.Old)
	if err != nil {
		return outcome{}, err
	}
	if r.revert {
		return outcome{problem: problemChanged, message: fmt.Sprintf(
			"%s %s does not hold what the migration writes, so this change was not reverted: the row changed after "+
				"the migration ran, or the migration never wrote it in this database.%s It was left alone. Compare "+
				"it with the fixture file as it was before the migration and decide which one is right",
			t.Name, keyLabel(c.Key), note)}, nil
	}
	return outcome{problem: problemChanged, message: fmt.Sprintf(
		"%s %s no longer holds the values this change was generated against: it was changed in this database, or "+
			"by a migration that ran before this one.%s It was left alone. Compare it with the fixture file and "+
			"decide which one is right", t.Name, keyLabel(c.Key), note)}, nil
}

// stopped fails a change whose statement changed no row although its guard
// matches one: something other than the data stopped it, a BEFORE trigger that
// returned NULL, a rule, or a row-level security policy. Read as a changed row
// it would be skipped, and the migration recorded as applied with the change
// never made.
func (r *runner) stopped(ctx context.Context, c fixturechange.Change, t fixturechange.Table, table string) error {
	where, args, err := r.guard(ctx, c, t)
	if err != nil {
		return err
	}
	var n int64
	if err := r.tx.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE %s", table, where),
		args...).Scan(&n); err != nil {
		return fmt.Errorf("count the rows the change should have matched: %w", err)
	}
	if n == 0 {
		return nil
	}
	return fmt.Errorf("%s %s holds the values this change was generated against, and the %s changed no row all "+
		"the same: a BEFORE trigger that returned NULL, a rule, or a row-level security policy stopped it. "+
		"Nothing was changed; the change set cannot be made until whatever stopped it lets it",
		t.Name, keyLabel(c.Key), c.Kind)
}

// diagnoseMoved looks for the row of an update that writes a key column -- a
// rename -- under the key the update gives it. Finding it there holding the new
// values is a rename this change already made: a second run, a replica that
// came second, or a plan of an applied migration. Without this every one of
// them would fail as a missing row. done is false when there is no such row, and
// the caller carries on with its own diagnosis.
func (r *runner) diagnoseMoved(ctx context.Context, c fixturechange.Change, t fixturechange.Table, table string,
	wanted fixturechange.Values) (out outcome, done bool, err error) {

	moved, ok := movedKey(c.Key, wanted)
	if !ok {
		return outcome{}, false, nil
	}
	n, err := r.count(ctx, c.Model, table, moved, wanted)
	if err != nil || n == 0 {
		return outcome{}, false, err
	}
	if n > 1 {
		return duplicate(t, fixturechange.Change{Key: moved}, n), true, nil
	}
	if c.ID != "" {
		withID, err := r.count(ctx, c.Model, table, moved, wanted, fixturechange.Values{t.ID: fixturechange.Lit(c.ID)})
		if err != nil {
			return outcome{}, false, err
		}
		if withID == 0 {
			ids, err := r.idsFor(ctx, c.Model, table, t, moved)
			if err != nil {
				return outcome{}, false, err
			}
			return outcome{problem: problemIDDrift, message: fmt.Sprintf(
				"%s %s is gone and %s exists, but under %s %s and not %s. This change was generated for the row "+
					"with that id; another row took the new key, and this one was not renamed",
				t.Name, keyLabel(c.Key), keyLabel(moved), t.ID, strings.Join(ids, ", "), c.ID)}, true, nil
		}
	}
	return outcome{problem: problemBenign, message: fmt.Sprintf(
		"the row already holds these values as %s, nothing to do", keyLabel(moved))}, true, nil
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
	if byKey == 0 {
		return outcome{}, fmt.Errorf("no row of %s has %s, and the insert wrote none all the same: a BEFORE trigger "+
			"that returned NULL, a rule, or a row-level security policy stopped it. Nothing was changed; the change "+
			"set cannot be made until whatever stopped it lets it", t.Name, keyLabel(c.Key))
	}
	// The row is there; under the id the fixture file gives it, or not. A row
	// under another id is not the row the file describes, even when every
	// other value agrees: whatever knows the file's id -- a reference in
	// another table, a URL, a client -- will not find it.
	if id, ok := c.New[t.ID]; ok && id.Ref == nil && !id.IsNull && r.set.Policy.IDDrift != fixturechange.ModeIgnore {
		withID, err := r.count(ctx, c.Model, table, c.Key, fixturechange.Values{t.ID: id})
		if err != nil {
			return outcome{}, err
		}
		if withID == 0 {
			ids, err := r.idsFor(ctx, c.Model, table, t, c.Key)
			if err != nil {
				return outcome{}, err
			}
			return outcome{problem: problemIDDrift, message: fmt.Sprintf(
				"%s %s exists, but under %s %s and not %s, so nothing was inserted. Whatever knows the fixture "+
					"file's id will not find this row; decide which id it should have",
				t.Name, keyLabel(c.Key), t.ID, strings.Join(ids, ", "), id.Lit)}, nil
		}
	}
	same, err := r.count(ctx, c.Model, table, c.Key, withoutColumn(c.New, t.ID))
	if err != nil {
		return outcome{}, err
	}
	if same > 0 {
		return outcome{problem: problemBenign, message: "the row is already there with these values, nothing to insert"}, nil
	}
	if r.revert {
		return outcome{problem: problemChanged, message: fmt.Sprintf(
			"%s %s is there again and holds other values than the row the migration deleted, so nothing was "+
				"inserted. Compare it with the fixture file as it was before the migration and decide which one is "+
				"right", t.Name, keyLabel(c.Key))}, nil
	}
	return outcome{problem: problemChanged, message: fmt.Sprintf(
		"%s %s already exists and holds different values, so nothing was inserted: it was added or changed in this "+
			"database, or by a migration that ran before this one. Compare it with the fixture file and decide which "+
			"one is right", t.Name, keyLabel(c.Key))}, nil
}

// idTakenByAnotherRow returns a description of the row holding that id when it
// is not the row the change is about, and "" otherwise.
func (r *runner) idTakenByAnotherRow(ctx context.Context, c fixturechange.Change, t fixturechange.Table,
	table, id string) (string, error) {

	keyWhere, keyArgs, err := r.match(ctx, c.Model, c.Key)
	if err != nil {
		return "", err
	}
	// The primary key is the table's, not the model's: an id a row outside
	// the model's Where holds is taken all the same.
	keyWhere, keyArgs = r.scoped(c.Model, keyWhere, keyArgs)
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
	// The row is named by the model's key column, or, for a model nobody
	// points at, by the columns of the change's natural key.
	cols := []string{t.Key}
	if t.Key == "" {
		cols = sortedColumns(c.Key)
	}
	var parts []string
	for _, col := range cols {
		q, err := quoteIdent(col)
		if err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("'%s=' || coalesce(%s::text, 'NULL')", col, q))
	}
	var label string
	q := fmt.Sprintf("SELECT concat_ws(',', %s) FROM %s WHERE %s = ? LIMIT 1", strings.Join(parts, ", "), table, idCol)
	if err := r.tx.QueryRowContext(ctx, q, found).Scan(&label); err != nil && !isNoRows(err) {
		return "", err
	} else if err == nil && label != "" {
		return fmt.Sprintf("%s = %s (%s)", t.ID, found, label), nil
	}
	return t.ID + " = " + found, nil
}

func (r *runner) idsFor(ctx context.Context, model, table string, t fixturechange.Table,
	key fixturechange.Values) ([]string, error) {

	where, args, err := r.match(ctx, model, key)
	if err != nil {
		return nil, err
	}
	where, args = r.scoped(model, where, args)
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
	where, args = r.scoped(model, where, args)
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
