package fixtureapply

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/RELAXccc/bun-fixture-migrate/internal/pgerr"

	"github.com/uptrace/bun"
)

// SyncSequences moves the sequence behind every serial and identity column of
// the named tables past the largest value the column holds. It never moves one
// back, so it is safe to call on a database in use.
//
// It is for a database dbfixture just seeded. dbfixture writes the ids the
// fixture file names, and a sequence does not see an explicit id go by: the
// first row the application inserts afterwards is given id 1 and fails on the
// primary key. Call it after fixture.Load with the tables the files seed; a
// generated migration does the same for the tables it writes ids into.
//
// Tables are plain SQL identifiers, optionally schema-qualified, as in a
// change set. It returns the columns whose sequence moved, as table.column.
func SyncSequences(ctx context.Context, db bun.IDB, tables ...string) ([]string, error) {
	var moved []string
	for _, name := range tables {
		table, err := quoteIdent(name)
		if err != nil {
			return moved, err
		}
		var cols []string
		err = db.NewRaw(`SELECT a.attname FROM pg_attribute a WHERE a.attrelid = ?::regclass `+
			`AND a.attnum > 0 AND NOT a.attisdropped AND pg_get_serial_sequence(?, a.attname) IS NOT NULL `+
			`ORDER BY a.attnum`, table, table).Scan(ctx, &cols)
		if err != nil {
			return moved, fmt.Errorf("find the sequences of %s: %w", name, err)
		}
		for _, col := range cols {
			ok, err := moveSequence(ctx, db, table, col)
			if err != nil {
				return moved, err
			}
			if ok {
				moved = append(moved, name+"."+col)
			}
		}
	}
	return moved, nil
}

// serialSequence is the sequence behind table.col as pg_get_serial_sequence
// names it, schema-qualified and quoted where it needs to be, or "" when the
// column has none. table is quoted already; col is a name as the catalog
// spells it.
//
// The quoted table name goes to pg_get_serial_sequence, which parses its
// first argument as SQL: unquoted, a mixed-case table would not be found and
// its sequence silently left behind. The column name is taken as it is
// spelled.
func serialSequence(ctx context.Context, db bun.IDB, table, col string) (string, error) {
	var seq sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT pg_get_serial_sequence(?, ?)", table, col).Scan(&seq); err != nil {
		return "", fmt.Errorf("find the sequence of %s.%s: %w", table, col, err)
	}
	return seq.String, nil
}

// behind is the condition, on a sequence q read as a table, that it would
// hand out the value v again: its next value is not past v. That is
// last_value + 1 once the sequence has been called, and last_value itself
// while it has not, as after a RESTART WITH or for a new sequence.
// pg_sequence_last_value says NULL for the latter, and its start value is not
// where a RESTART left it.
//
// Reading the sequence as a table takes SELECT on it. A role without SELECT,
// with the USAGE and UPDATE that nextval and setval take, gets
// behindByCatalog.
const behind = "(%[1]s > q.last_value OR (NOT q.is_called AND %[1]s = q.last_value))"

// behindByCatalog is behind for a role that may not read the sequence as a
// table, on q, its row of pg_sequence, from what that role may see:
// pg_sequence_last_value, which takes USAGE or SELECT, and the start value.
// pg_sequence_last_value is NULL while the sequence has not been called since
// it was created or restarted, and the start value is where it then stands --
// unless ALTER SEQUENCE ... RESTART WITH put it elsewhere, which only SELECT
// can see. Such a sequence, restarted past the ids written and not called
// since, is taken to be at its start value, and moved back to just past them.
const behindByCatalog = "(CASE WHEN pg_sequence_last_value(q.seqrelid) IS NULL THEN %[1]s >= q.seqstart " +
	"ELSE %[1]s > pg_sequence_last_value(q.seqrelid) END)"

// sequenceAccess says whether the role running the migration may read the
// sequence seq of table as a table, which takes SELECT on it, and fails with
// the GRANT to run when it may not move it, which takes UPDATE, or may not
// read where it stands at all, which takes SELECT or USAGE.
func sequenceAccess(ctx context.Context, db bun.IDB, seq, table string) (canSelect bool, err error) {
	var sel, usage, update bool
	var role string
	if err := db.QueryRowContext(ctx, "SELECT has_sequence_privilege(?::regclass, 'SELECT'), "+
		"has_sequence_privilege(?::regclass, 'USAGE'), has_sequence_privilege(?::regclass, 'UPDATE'), "+
		"quote_ident(current_user)", seq, seq, seq).Scan(&sel, &usage, &update, &role); err != nil {
		return false, fmt.Errorf("look at the privileges on the sequence of %s: %w", table, privilege(err))
	}
	var missing, why []string
	if !sel && !usage {
		missing = append(missing, "SELECT")
		why = append(why, "read where it stands, which takes SELECT (or USAGE)")
	}
	if !update {
		missing = append(missing, "UPDATE")
		why = append(why, "move it, which takes UPDATE")
	}
	if len(missing) > 0 {
		return false, &grantError{msg: fmt.Sprintf("the sequence %s of %s has to be kept past the ids written into "+
			"the table explicitly, and the role running this, %s, may not %s, so nothing was changed: "+
			"GRANT %s ON SEQUENCE %s TO %s", seq, table, role, strings.Join(why, ", nor "),
			strings.Join(missing, ", "), seq, role)}
	}
	return sel, nil
}

// grantError is a privilege the role running the migration lacks, said with
// the GRANT that gives it. It carries PostgreSQL's code for one, so plan
// judges it as it judges PostgreSQL's own, and privilege leaves its sentence
// as it is.
type grantError struct {
	msg string
	err error
}

func (e *grantError) Error() string {
	if e.err != nil {
		return e.msg + ": " + e.err.Error()
	}
	return e.msg
}

func (e *grantError) Unwrap() error    { return e.err }
func (e *grantError) SQLState() string { return pgerr.InsufficientPrivilege }

// moveSequence sets the sequence of table.col to the largest value in the
// column when the sequence would hand that value out again, and reports
// whether it did. table is quoted already; col is a name as the catalog
// spells it.
//
// setval is not transactional: a rollback does not undo it. Moving a sequence
// forward is harmless either way, which is why this only ever moves forward.
func moveSequence(ctx context.Context, db bun.IDB, table, col string) (bool, error) {
	seq, err := serialSequence(ctx, db, table, col)
	if err != nil || seq == "" {
		return false, err
	}
	canSelect, err := sequenceAccess(ctx, db, seq, table)
	if err != nil {
		return false, err
	}
	quotedCol := `"` + strings.ReplaceAll(col, `"`, `""`) + `"`
	// The sequence's name goes in verbatim: it is quoted already, and may
	// hold a ? that bun would read as a placeholder.
	query := fmt.Sprintf("SELECT setval(?::regclass, s.top) FROM ? q, (SELECT max(%s) AS top FROM %s) s WHERE %s",
		quotedCol, table, fmt.Sprintf(behind, "s.top"))
	args := []any{seq, bun.Safe(seq)}
	if !canSelect {
		query = fmt.Sprintf("SELECT setval(q.seqrelid::regclass, s.top) FROM pg_sequence q, (SELECT max(%s) AS top "+
			"FROM %s) s WHERE q.seqrelid = ?::regclass AND %s", quotedCol, table, fmt.Sprintf(behindByCatalog, "s.top"))
		args = []any{seq}
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("move the sequence of %s past the ids written: %w", table, privilege(err))
	}
	moved := false
	for rows.Next() {
		moved = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, fmt.Errorf("move the sequence of %s past the ids written: %w", table, privilege(err))
	}
	return moved, rows.Close()
}

// syncSequences moves the sequence of every table that got an explicit id past
// the highest id in it. Without this the next ordinary insert reuses an id that
// is already taken.
//
// It only ever moves a sequence forward. setval is not transactional and a
// sequence is routinely ahead of the highest id -- rows were deleted, an insert
// rolled back, another session holds values it has not committed yet -- and
// moving it back to the highest id would hand those values out a second time.
// A sequence that was never called hands out its last_value next, which
// RESTART WITH may have set anywhere.
func (r *runner) syncSequences(ctx context.Context, o options) error {
	models := make([]string, 0, len(r.resync))
	for m := range r.resync {
		models = append(models, m)
	}
	sort.Strings(models)
	for _, m := range models {
		t := r.set.Tables[m]
		table, err := quoteIdent(t.Name)
		if err != nil {
			return err
		}
		if _, err := quoteIdent(t.ID); err != nil {
			return err
		}
		if o.dryRun {
			// The role may lack what moving the sequence takes, which the
			// run fails on and the dry run has to say.
			if _, _, err := r.sequence(ctx, m, table); err != nil {
				return err
			}
			msg := fmt.Sprintf("explicit ids were written into %s; the migration moves its sequence past them "+
				"if it is behind, which a dry run leaves alone", t.Name)
			out := Outcome{Set: r.set.Name, Index: -1, Model: m, Status: StatusSequence, Message: msg}
			o.log(ctx, slog.LevelInfo, "fixture sequence left alone in a dry run", out, r.set.Name+": "+msg)
			o.report(out)
			continue
		}
		moved, err := moveSequence(ctx, r.tx, table, t.ID)
		if err != nil {
			return err
		}
		if moved || r.advanced[m] {
			msg := fmt.Sprintf("moved the sequence of %s past the explicit ids written", t.Name)
			out := Outcome{Set: r.set.Name, Index: -1, Model: m, Status: StatusSequence, Message: msg}
			o.log(ctx, slog.LevelInfo, "fixture sequence moved", out, r.set.Name+": "+msg)
			o.report(out)
		}
	}
	return nil
}

// sequence is serialSequence of a model's id column, looked up once per run,
// and sequenceAccess of it: whether the role may read it as a table. A role
// that may not move it, or not read it at all, fails here, before any id is
// written.
func (r *runner) sequence(ctx context.Context, model, table string) (seq string, canSelect bool, err error) {
	if seq, ok := r.sequences[model]; ok {
		return seq, r.seqSelect[seq], nil
	}
	if seq, err = serialSequence(ctx, r.tx, table, r.set.Tables[model].ID); err != nil {
		return "", false, err
	}
	if seq != "" {
		if canSelect, err = sequenceAccess(ctx, r.tx, seq, table); err != nil {
			return "", false, err
		}
	}
	r.sequences[model], r.seqSelect[seq] = seq, canSelect
	return seq, canSelect, nil
}

// advanceSequence moves the sequence seq of table to id when it would hand id
// out again, before a row with that id is written, and reports whether it
// did. canSelect is sequenceAccess's.
func advanceSequence(ctx context.Context, db bun.IDB, seq string, canSelect bool, table, id string) (bool, error) {
	if seq == "" {
		return false, nil
	}
	query := "SELECT setval(?::regclass, ?::bigint) FROM ? q WHERE " + fmt.Sprintf(behind, "?::bigint")
	args := []any{seq, id, bun.Safe(seq), id, id}
	if !canSelect {
		query = "SELECT setval(q.seqrelid::regclass, ?::bigint) FROM pg_sequence q WHERE q.seqrelid = ?::regclass " +
			"AND " + fmt.Sprintf(behindByCatalog, "?::bigint")
		args = []any{id, seq, id, id}
	}
	res, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("move the sequence of %s past the id %s before writing it: %w", table, id, privilege(err))
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
