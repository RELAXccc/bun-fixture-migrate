package fixturemigrate

import (
	"context"
	"fmt"
	"strings"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
	"github.com/RELAXccc/bun-fixture-migrate/internal/pgerr"

	"github.com/uptrace/bun"
)

// PrepareSession fixes the session settings that decide how PostgreSQL spells
// a value as text, for the rest of the transaction tx: TimeZone UTC,
// DateStyle ISO, IntervalStyle postgres, the shortest exact float, hex bytea.
//
// Without it the same timestamp reads differently on two servers, and a
// comparison between the database and the fixture file finds drift that is
// only a setting. The commands call it on every transaction they read in; a
// program calling DatabaseSnapshot or Canonicalize itself should too.
func PrepareSession(ctx context.Context, tx bun.Tx) error {
	_, err := tx.ExecContext(ctx, "SELECT set_config('TimeZone', 'UTC', true), "+
		"set_config('DateStyle', 'ISO, YMD', true), set_config('IntervalStyle', 'postgres', true), "+
		"set_config('extra_float_digits', '1', true), set_config('bytea_output', 'hex', true)")
	if err != nil {
		return fmt.Errorf("fix the session's settings: %w", err)
	}
	return nil
}

// castBatch bounds the values cast in one statement.
const castBatch = 500

// Canonicalize has PostgreSQL say what every value of a fixture snapshot is,
// by casting it to its column's type and reading it back as the database reads
// its own rows. After it, a fixture value and a database value compare equal
// exactly when PostgreSQL holds them equal: 1.5 and 1.50 in numeric(10,2),
// "2026-01-01" and "2026-01-01 00:00:00+00" in a timestamptz, an upper-case
// and a lower-case uuid, two spellings of the same jsonb.
//
// A value the column's type cannot hold -- "abc" in an integer, 300 in a
// smallint's check-free overflow, a label an enum does not have -- becomes a
// finding, FindingInvalidValue, rather than a migration that fails at deploy
// time. Natural keys, ids and references are rewritten along with the values,
// so the snapshot stays consistent.
//
// Columns the table does not have are left alone; LintColumns reports them.
// The casts run in savepoints, so a value that fails leaves db's transaction
// usable, and nothing is written.
func Canonicalize(ctx context.Context, db bun.IDB, cfg *Config, snap *Snapshot, tables map[string]*dbschema.Table) error {
	refCanon := map[string]map[string]string{} // model -> ref value as written -> canonical
	for _, model := range snap.Order {
		// A model nobody configured has no table to cast against, and
		// passing over it would compare its values uncast: the same mistake
		// FixtureSnapshot refuses.
		m, err := cfg.model(model)
		if err != nil {
			return err
		}
		table := tables[cfg.QualifiedTable(m)]
		if table == nil {
			continue
		}
		entries := snap.Entries[model]
		cols := append([]string{m.ID}, snap.Columns[model]...)
		for _, col := range cols {
			column, ok := table.Column(col)
			if !ok || column.Generated {
				continue
			}
			var values []string
			seen := map[string]bool{}
			for _, e := range entries {
				text, ok := sourceOf(e, m, col, column)
				if ok && !seen[text] {
					seen[text] = true
					values = append(values, text)
				}
			}
			if len(values) == 0 {
				continue
			}
			canon, invalid, err := castValues(ctx, db, column, values)
			if err != nil {
				return fmt.Errorf("%s.%s: %w", model, col, err)
			}
			for _, e := range entries {
				text, ok := sourceOf(e, m, col, column)
				// The column's type has decided which text the database holds.
				delete(e.AsWritten, col)
				if !ok {
					continue
				}
				if msg, bad := invalid[text]; bad {
					snap.Findings = append(snap.Findings, Finding{
						Kind: FindingInvalidValue, Model: model, Row: e.KeyStr,
						Detail: fmt.Sprintf("%s is %q, which the column's type, %s, cannot hold: %s",
							col, text, column.FullType, msg),
					})
					continue
				}
				if col == m.ID {
					e.ID = canon[text]
				} else {
					e.Cells[col] = fixturechange.Lit(canon[text])
				}
			}
			if col == m.Ref {
				refCanon[model] = canon
			}
		}
	}

	// A reference names its target by the target's ref value, which may just
	// have been respelled; and a natural key is made of values that may have.
	for _, model := range snap.Order {
		m := cfg.Models[model]
		if m == nil {
			continue
		}
		for _, e := range snap.Entries[model] {
			for col, v := range e.Cells {
				if v.Ref == nil {
					continue
				}
				if c, ok := refCanon[v.Ref.Model][v.Ref.Key]; ok {
					e.Cells[col] = fixturechange.RefTo(v.Ref.Model, c)
				}
			}
			if key, err := keyOf(cfg, m, model, e.Cells); err == nil {
				e.Key, e.KeyStr = key, keyString(model, key)
			}
		}
	}
	// Two keys that were spelled apart may be one value now.
	kept := snap.Findings[:0]
	for _, f := range snap.Findings {
		if f.Kind != FindingDuplicateKey {
			kept = append(kept, f)
		}
	}
	snap.Findings = kept
	for _, model := range snap.Order {
		snap.reportDuplicates(model)
	}
	return nil
}

// sourceOf is the text a cast of a column of an entry starts from: the value
// as written when the column is one a Go string field writes and the file
// wrote the value differently from what it resolves to (1.10, 017, True),
// because that is what dbfixture stores there; the resolved value otherwise.
func sourceOf(e *Entry, m *Model, col string, column dbschema.Column) (string, bool) {
	text, ok := literalOf(e, m, col)
	if !ok {
		return "", false
	}
	if written, ok := e.AsWritten[col]; ok && column.StringField() {
		return written, true
	}
	return text, true
}

// literalOf is the text of a column of an entry that a cast applies to: a
// literal, not a NULL and not a reference.
func literalOf(e *Entry, m *Model, col string) (string, bool) {
	if col == m.ID {
		return e.ID, e.ID != ""
	}
	v, ok := e.Cells[col]
	if !ok || v.IsNull || v.Ref != nil {
		return "", false
	}
	return v.Lit, true
}

// castType is the type a value is cast to. Character types lose their length:
// an explicit cast to varchar(3) truncates without a word, which would make a
// value too long for the column compare equal to one that fits.
func castType(c dbschema.Column) string {
	switch c.Type {
	case "varchar", "bpchar":
		return c.Type
	case "_varchar", "_bpchar":
		return c.Type[1:] + "[]"
	}
	return c.FullType
}

// castValues casts values to the column's type and returns each one's
// canonical text, and, for each value the type refuses, PostgreSQL's reason.
func castValues(ctx context.Context, db bun.IDB, column dbschema.Column,
	values []string) (map[string]string, map[string]string, error) {

	canon, invalid := map[string]string{}, map[string]string{}
	for start := 0; start < len(values); start += castBatch {
		batch := values[start:min(start+castBatch, len(values))]
		err := castInto(ctx, db, column, batch, canon)
		if err == nil {
			continue
		}
		if !dataException(err) {
			return nil, nil, err
		}
		// One of them is not a value of the type. Find which, one by one.
		for _, v := range batch {
			if err := castInto(ctx, db, column, []string{v}, canon); err != nil {
				if !dataException(err) {
					return nil, nil, err
				}
				invalid[v] = pgerr.Message(err)
			}
		}
	}
	return canon, invalid, nil
}

func castInto(ctx context.Context, db bun.IDB, column dbschema.Column, values []string, canon map[string]string) error {
	rowsSQL := strings.TrimSuffix(strings.Repeat("(?::text),", len(values)), ",")
	expr := readExpr(column, "t.v::"+castType(column))
	if column.Category == "A" {
		// A YAML sequence arrives as a JSON array; a string in PostgreSQL's
		// own array syntax is taken as that.
		expr = "CASE WHEN left(ltrim(t.v), 1) = '[' THEN " +
			readExpr(column, "ARRAY(SELECT jsonb_array_elements_text(t.v::jsonb))::"+castType(column)) +
			" ELSE " + expr + " END"
	}
	query := "SELECT t.v, " + expr + " FROM (VALUES " + rowsSQL + ") AS t(v)"
	args := make([]any, len(values))
	for i, v := range values {
		args[i] = v
	}
	got := map[string]string{}
	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var in, out string
			if err := rows.Scan(&in, &out); err != nil {
				return err
			}
			got[in] = columnText(column.Type, out)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return rows.Close()
	})
	if err != nil {
		return err
	}
	for k, v := range got {
		canon[k] = v
	}
	return nil
}

// dataException reports an error of class 22, PostgreSQL's "the value is not
// one of this type": invalid input syntax, out of range, too long.
func dataException(err error) bool {
	return strings.HasPrefix(pgerr.State(err), "22")
}
