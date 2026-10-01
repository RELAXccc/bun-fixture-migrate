package fixtureapply

// A model whose bun struct has a soft_delete field keeps a row it deletes:
// bun's NewDelete sets the field's column to the time, and every query of
// the model after that reads only the rows where it is NULL. A change set of
// such a model, one whose Table has a SoftDelete, does the same: its changes
// see live rows only (scoped), a delete soft-deletes, and an insert brings a
// soft-deleted row holding its values back before it writes a new one.

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
	"github.com/RELAXccc/bun-fixture-migrate/internal/pgerr"
)

// softDeleteSavepoint is the savepoint a statement that a unique index may
// refuse runs under, so the refusal is an outcome and not a failed set.
const softDeleteSavepoint = "bun_fixture_migrate_soft_delete"

// softDelete sets the soft delete column of the row a delete names to the
// transaction's time, under the guard a delete has: the natural key, the old
// values, live, and one row holding the key.
//
// Nothing reaches the rows pointing at it, which a delete would delete,
// detach or be refused by: the row stays where they point. The outcome counts
// them all the same, because to the application they now point at nothing.
func (r *runner) softDelete(ctx context.Context, c fixturechange.Change, t fixturechange.Table,
	table string) (outcome, error) {

	where, args, err := r.guard(ctx, c, t)
	if err != nil {
		return outcome{}, err
	}
	// Counted first: once the row is soft-deleted the guard, which sees
	// live rows only, finds it no more.
	pointing, err := r.stillPointing(ctx, c, table, where, args)
	if err != nil {
		return outcome{}, err
	}
	where, args, err = r.onlyRow(ctx, c, table, where, args)
	if err != nil {
		return outcome{}, err
	}
	col, _ := quoteIdent(t.SoftDelete)
	// now() is the transaction's time, so every row one set soft-deletes
	// holds the same one; the session's TimeZone is UTC, which is what a
	// timestamp column without a zone gets.
	n, err := r.run(ctx, fmt.Sprintf("UPDATE %s SET %s = now() WHERE %s", table, col, where), args)
	if err != nil {
		return outcome{}, err
	}
	if n > 0 {
		return outcome{rows: n, action: ActionSoftDeleted, message: pointing}, nil
	}
	return r.diagnose(ctx, c, t, table, nil)
}

// stillPointing says which rows point at the rows where selects, through
// every foreign key that points at the table, or "" when none does.
func (r *runner) stillPointing(ctx context.Context, c fixturechange.Change, table, where string,
	args []any) (string, error) {

	fks, err := r.incoming(ctx, table)
	if err != nil {
		return "", err
	}
	var parts []string
	for _, fk := range fks {
		n, err := r.pointing(ctx, fk, table, where, args)
		if err != nil {
			return "", fmt.Errorf("count the rows pointing at %s %s: %w", r.set.Tables[c.Model].Name, keyLabel(c.Key), err)
		}
		if n == 0 {
			continue
		}
		verb := "point"
		if n == 1 {
			verb = "points"
		}
		parts = append(parts, fmt.Sprintf("%s of %s still %s at it through %s", pointingCount(n), fk.child, verb, fk.name))
	}
	if len(parts) == 0 {
		return "", nil
	}
	return strings.Join(parts, ", and ") + fmt.Sprintf("; bun loads a soft-deleted row through no relation, so to "+
		"the application their %s reads as nil", c.Model), nil
}

// deleted is a soft-deleted row: where it is, its id when the table has the
// id column, and when it was deleted.
type deleted struct {
	rel, ctid string
	id        string
	at        string
}

// label names the row in a message: " (id 3, deleted at T)".
func (d deleted) label() string {
	if d.id == "" {
		return " (deleted at " + d.at + ")"
	}
	return " (id " + d.id + ", deleted at " + d.at + ")"
}

// insertOrRestore is an insert of a model with a SoftDelete. It takes the first
// step that applies:
//
//  1. a live row holds the key: the insert finds it made, or that row
//     changed, as any insert would;
//  2. the newest soft-deleted row holding the key -- or, when the change
//     writes the id, the one with that id -- holds every value the change
//     writes: it is restored, and keeps its id and the rows pointing at it;
//  3. that row holds other values: a new row is inserted beside it, which a
//     unique index over live rows only lets in, and one over every row
//     refuses, which is a changed row;
//  4. no row at all holds the key: the row is inserted.
func (r *runner) insertOrRestore(ctx context.Context, c fixturechange.Change, t fixturechange.Table,
	table string) (outcome, error) {

	live, err := r.count(ctx, c.Model, table, c.Key)
	if err != nil {
		return outcome{}, err
	}
	if live > 0 {
		return r.diagnoseInsert(ctx, c, t, table)
	}
	cands, err := r.candidates(ctx, c, t, table)
	if err != nil {
		return outcome{}, err
	}
	if len(cands) == 0 {
		return r.insert(ctx, c, t, table)
	}
	cand := cands[0]
	if len(cands) > 1 && cands[1].at == cand.at {
		return outcome{problem: problemDuplicate, message: fmt.Sprintf(
			"more than one soft-deleted row of %s holds %s, deleted at the same time, %s, and nothing says which of "+
				"them to restore, so none was. Restore one by hand, or delete the others for good",
			t.Name, keyLabel(c.Key), cand.at)}, nil
	}
	holds, err := r.holdsValues(ctx, c, table, cand)
	if err != nil {
		return outcome{}, err
	}
	if holds {
		return r.restore(ctx, c, t, table, cand)
	}
	// The row the change names by its id is there, soft-deleted with other
	// values: a second row cannot have the id.
	if id, ok := c.New[t.ID]; ok && cand.id != "" && id.Ref == nil && !id.IsNull && id.Lit == cand.id {
		return outcome{problem: problemChanged, message: fmt.Sprintf(
			"%s %s is soft-deleted%s and holds other values than this change writes, so it was not restored, and "+
				"no row can be inserted beside it under the same %s. Restore it and edit it by hand, or delete it "+
				"for good, and the change is made on the next run", t.Name, keyLabel(c.Key), cand.label(), t.ID)}, nil
	}
	return r.insertBeside(ctx, c, t, table, cand)
}

// candidates are the soft-deleted rows an insert can restore, newest first,
// at most two: the one the change's id names, when it writes one and a
// soft-deleted row with that id holds the key, and otherwise those holding
// the key.
func (r *runner) candidates(ctx context.Context, c fixturechange.Change, t fixturechange.Table,
	table string) ([]deleted, error) {

	types, err := r.colTypes(ctx, c.Model)
	if err != nil {
		return nil, err
	}
	col, _ := quoteIdent(t.SoftDelete)
	idExpr, order := "NULL::text", col+" DESC"
	var idCol string
	if _, ok := types[t.ID]; ok && t.ID != "" {
		if idCol, err = quoteIdent(t.ID); err != nil {
			return nil, err
		}
		idExpr, order = idCol+"::text", order+", "+idCol+" DESC"
	}
	keyWhere, keyArgs, err := r.match(ctx, c.Model, c.Key, true)
	if err != nil {
		return nil, err
	}
	keyWhere, keyArgs = r.scopedDeleted(c.Model, keyWhere, keyArgs)
	find := func(where string, args []any) ([]deleted, error) {
		rows, err := r.tx.QueryContext(ctx, fmt.Sprintf("SELECT tableoid::text, ctid::text, %s, %s::text FROM %s "+
			"WHERE %s ORDER BY %s LIMIT 2", idExpr, col, table, where, order), args...)
		if err != nil {
			return nil, fmt.Errorf("look for a soft-deleted row of %s %s: %w", t.Name, keyLabel(c.Key), err)
		}
		defer rows.Close()
		var out []deleted
		for rows.Next() {
			var d deleted
			var id sql.NullString
			if err := rows.Scan(&d.rel, &d.ctid, &id, &d.at); err != nil {
				return nil, err
			}
			d.id = id.String
			out = append(out, d)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return out, rows.Close()
	}
	if id, ok := c.New[t.ID]; ok && idCol != "" && id.Ref == nil && !id.IsNull {
		found, err := find(keyWhere+" AND "+idCol+" = ?", append(append([]any{}, keyArgs...), id.Lit))
		if err != nil || len(found) > 0 {
			return found, err
		}
	}
	return find(keyWhere, keyArgs)
}

// holdsValues reports whether a soft-deleted row holds every value an insert
// writes: the natural key, and the id when the change writes one.
func (r *runner) holdsValues(ctx context.Context, c fixturechange.Change, table string, d deleted) (bool, error) {
	where, args, err := r.matchAll(ctx, c.Model, c.Key, c.New)
	if err != nil {
		return false, err
	}
	where, args = r.scopedDeleted(c.Model, where, args)
	var n int64
	if err := r.tx.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE tableoid = ?::oid AND "+
		"ctid = ?::tid AND %s", table, where), append([]any{d.rel, d.ctid}, args...)...).Scan(&n); err != nil {
		return false, fmt.Errorf("compare the soft-deleted row with the change: %w", err)
	}
	return n > 0, nil
}

// restore sets a soft-deleted row's soft delete column back to NULL. It goes
// through write, so a row that does not hold the model's Where is refused, as
// a row an insert writes would be.
func (r *runner) restore(ctx context.Context, c fixturechange.Change, t fixturechange.Table, table string,
	d deleted) (outcome, error) {

	col, _ := quoteIdent(t.SoftDelete)
	n, err := r.write(ctx, c.Model, fmt.Sprintf("UPDATE %s SET %s = NULL WHERE tableoid = ?::oid AND ctid = ?::tid "+
		"AND %s IS NOT NULL", table, col, col), []any{d.rel, d.ctid})
	if err != nil {
		return outcome{}, err
	}
	if n == 0 {
		return outcome{}, fmt.Errorf("%s %s is soft-deleted%s and holds the values of this change, and restoring it "+
			"changed no row all the same: a BEFORE trigger that returned NULL, a rule, or a row-level security "+
			"policy stopped it. Nothing was changed; the change set cannot be made until whatever stopped it lets it",
			t.Name, keyLabel(c.Key), d.label())
	}
	return outcome{rows: n, action: ActionRestored, info: true,
		message: "restored the row soft-deleted at " + d.at + idNote(d)}, nil
}

func idNote(d deleted) string {
	if d.id == "" {
		return ""
	}
	return " (id " + d.id + ")"
}

// insertBeside inserts a row whose key a soft-deleted row holds with other
// values. A unique index over live rows only lets the new row in; one over
// every row refuses it, which is a row that does not hold what the change was
// generated against: the savepoint takes the refusal back, and the outcome
// says which row holds the key.
func (r *runner) insertBeside(ctx context.Context, c fixturechange.Change, t fixturechange.Table, table string,
	d deleted) (outcome, error) {

	out, refused, err := r.inSavepoint(ctx, func() (outcome, error) { return r.insert(ctx, c, t, table) })
	switch {
	case err != nil:
		return outcome{}, err
	case refused != "":
		return outcome{problem: problemChanged, message: fmt.Sprintf(
			"%s %s is soft-deleted%s with other values than this change writes, and %s refuses a second row: "+
				"restore it and edit it by hand, or delete it for good, and the change is made on the next run",
			t.Name, keyLabel(c.Key), d.label(), refused)}, nil
	}
	if out.problem == "" && out.rows > 0 {
		out.message = "inserted beside the soft-deleted row" + d.label() + ", which holds other values"
	}
	return out, nil
}

// inSavepoint runs fn in a savepoint. A unique index or constraint refusing
// what it writes is rolled back to the savepoint and comes back as the
// constraint's name, "constraint \"plans_name_key\"", with the transaction
// as it was; any other error is fn's.
func (r *runner) inSavepoint(ctx context.Context, fn func() (outcome, error)) (outcome, string, error) {
	if _, err := r.tx.ExecContext(ctx, "SAVEPOINT "+softDeleteSavepoint); err != nil {
		return outcome{}, "", err
	}
	out, err := fn()
	if pgerr.State(err) == pgerr.UniqueViolation {
		if _, rerr := r.tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+softDeleteSavepoint); rerr != nil {
			return outcome{}, "", rerr
		}
		if _, rerr := r.tx.ExecContext(ctx, "RELEASE SAVEPOINT "+softDeleteSavepoint); rerr != nil {
			return outcome{}, "", rerr
		}
		return outcome{}, constraintOf(err), nil
	}
	if err != nil {
		return outcome{}, "", err
	}
	if _, err := r.tx.ExecContext(ctx, "RELEASE SAVEPOINT "+softDeleteSavepoint); err != nil {
		return outcome{}, "", err
	}
	return out, "", nil
}

// constraintName finds the constraint PostgreSQL names in a unique violation.
var constraintName = regexp.MustCompile(`constraint "([^"]+)"`)

// constraintOf names the constraint an error names, as a message reads it.
func constraintOf(err error) string {
	if m := constraintName.FindStringSubmatch(pgerr.Message(err)); m != nil {
		return quoteLiteralName(m[1])
	}
	return "a unique index"
}

// updateInSavepoint runs an update of a model with a SoftDelete. Its
// soft-deleted rows still hold their values, and a unique index over every
// row refuses a value one of them holds, which no live row does: a rename into
// the key of a row deleted long ago. The refusal is taken back, and is a row
// that cannot take what the change was generated to give it.
func (r *runner) updateInSavepoint(ctx context.Context, c fixturechange.Change, t fixturechange.Table, table string,
	fn func() (outcome, error)) (outcome, error) {

	out, refused, err := r.inSavepoint(ctx, fn)
	if err != nil || refused == "" {
		return out, err
	}
	holder := ""
	if moved, renamed := movedKey(c.Key, c.New); renamed {
		at, err := r.deletedSince(ctx, c.Model, table, moved)
		if err != nil {
			return outcome{}, err
		}
		if at != "" {
			holder = fmt.Sprintf(": a soft-deleted row holds %s, deleted at %s", keyLabel(moved), at)
		}
	}
	return outcome{problem: problemChanged, message: fmt.Sprintf(
		"%s %s cannot take the values this change writes, because %s refuses them%s. Restore that row and edit it "+
			"by hand, or delete it for good, and the change is made on the next run",
		t.Name, keyLabel(c.Key), refused, holder)}, nil
}

// deletedSince is when the newest soft-deleted row of a model holding a
// natural key was deleted, "" when none holds it or the model has no
// SoftDelete.
func (r *runner) deletedSince(ctx context.Context, model, table string, key fixturechange.Values) (string, error) {
	if r.set.Tables[model].SoftDelete == "" {
		return "", nil
	}
	where, args, err := r.match(ctx, model, key, true)
	if err != nil {
		return "", err
	}
	where, args = r.scopedDeleted(model, where, args)
	return r.newestDeleted(ctx, model, table, where, args)
}

// newestDeleted is the soft delete column's value, as text, of the newest row
// where selects, which scopedDeleted has made, "" for none.
func (r *runner) newestDeleted(ctx context.Context, model, table, where string, args []any) (string, error) {
	col, _ := quoteIdent(r.set.Tables[model].SoftDelete)
	var at string
	err := r.tx.QueryRowContext(ctx, fmt.Sprintf("SELECT %s::text FROM %s WHERE %s ORDER BY %s DESC LIMIT 1",
		col, table, where, col), args...).Scan(&at)
	if isNoRows(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("look for a soft-deleted row of %s: %w", r.set.Tables[model].Name, err)
	}
	return at, nil
}
