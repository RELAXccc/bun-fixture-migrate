package fixtureapply

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strings"

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
const behind = "(%[1]s > q.last_value OR (NOT q.is_called AND %[1]s = q.last_value))"

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
	quotedCol := `"` + strings.ReplaceAll(col, `"`, `""`) + `"`
	// The sequence's name goes in verbatim: it is quoted already, and may
	// hold a ? that bun would read as a placeholder.
	query := fmt.Sprintf("SELECT setval(?::regclass, s.top) FROM ? q, (SELECT max(%s) AS top FROM %s) s WHERE %s",
		quotedCol, table, fmt.Sprintf(behind, "s.top"))
	rows, err := db.QueryContext(ctx, query, seq, bun.Safe(seq))
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

// sequence is serialSequence of a model's id column, looked up once per run.
func (r *runner) sequence(ctx context.Context, model, table string) (string, error) {
	if seq, ok := r.sequences[model]; ok {
		return seq, nil
	}
	seq, err := serialSequence(ctx, r.tx, table, r.set.Tables[model].ID)
	if err != nil {
		return "", err
	}
	r.sequences[model] = seq
	return seq, nil
}

// advanceSequence moves the sequence seq of table to id when it would hand id
// out again, before a row with that id is written, and reports whether it
// did.
func advanceSequence(ctx context.Context, db bun.IDB, seq, table, id string) (bool, error) {
	if seq == "" {
		return false, nil
	}
	res, err := db.ExecContext(ctx, "SELECT setval(?::regclass, ?::bigint) FROM ? q WHERE "+
		fmt.Sprintf(behind, "?::bigint"), seq, id, bun.Safe(seq), id, id)
	if err != nil {
		return false, fmt.Errorf("move the sequence of %s past the id %s before writing it: %w", table, id, privilege(err))
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
