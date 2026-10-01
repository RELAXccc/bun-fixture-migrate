package fixtureapply

import (
	"context"
	"fmt"
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

// moveSequence sets the sequence of table.col to the largest value in the
// column when the sequence is behind it, and reports whether it did. table is
// quoted already; col is a name as the catalog spells it.
//
// setval is not transactional: a rollback does not undo it. Moving a sequence
// forward is harmless either way, which is why this only ever moves forward.
func moveSequence(ctx context.Context, db bun.IDB, table, col string) (bool, error) {
	quotedCol := `"` + strings.ReplaceAll(col, `"`, `""`) + `"`
	// The quoted table name goes to pg_get_serial_sequence, which parses its
	// first argument as SQL: unquoted, a mixed-case table would not be found
	// and its sequence silently left behind. The column name is taken as it
	// is spelled.
	query := fmt.Sprintf(`SELECT setval(s.seq, s.top) FROM (`+
		`SELECT pg_get_serial_sequence(?, ?)::regclass AS seq, (SELECT COALESCE(MAX(%s), 0) FROM %s) AS top`+
		`) s WHERE s.seq IS NOT NULL AND s.top > COALESCE(pg_sequence_last_value(s.seq), `+
		`(SELECT seqstart - 1 FROM pg_sequence WHERE seqrelid = s.seq))`, quotedCol, table)
	rows, err := db.QueryContext(ctx, query, table, col)
	if err != nil {
		return false, fmt.Errorf("move the sequence of %s past the ids written: %w", table, err)
	}
	moved := false
	for rows.Next() {
		moved = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, fmt.Errorf("move the sequence of %s past the ids written: %w", table, err)
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
// A sequence that was never called is compared with its start value.
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
			o.logf("%s: %s", r.set.Name, msg)
			o.report(Outcome{Set: r.set.Name, Index: -1, Model: m, Status: StatusSequence, Message: msg})
			continue
		}
		moved, err := moveSequence(ctx, r.tx, table, t.ID)
		if err != nil {
			return err
		}
		if moved {
			msg := fmt.Sprintf("moved the sequence of %s past the explicit ids written", t.Name)
			o.logf("%s: %s", r.set.Name, msg)
			o.report(Outcome{Set: r.set.Name, Index: -1, Model: m, Status: StatusSequence, Message: msg})
		}
	}
	return nil
}
