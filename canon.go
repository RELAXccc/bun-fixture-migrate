package fixturemigrate

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
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
// A value the column cannot take as dbfixture would write it -- "abc" in an
// integer, a label an enum does not have, a value too long for varchar(3), one
// a domain's or the column's CHECK refuses -- becomes a finding,
// FindingInvalidValue, rather than a migration that fails at deploy time.
// Natural keys, ids and references are rewritten along with the values, so the
// snapshot stays consistent.
//
// Columns the table does not have are left alone; LintColumns reports them.
// The casts run in savepoints, so a value that fails leaves db's transaction
// usable, and nothing is written.
func Canonicalize(ctx context.Context, db bun.IDB, cfg *Config, snap *Snapshot, tables map[string]*dbschema.Table) error {
	refCanon := map[string]map[string]string{} // model -> ref value as written -> canonical
	for _, model := range snap.Order {
		m := cfg.Models[model]
		table := tables[cfg.QualifiedTable(m)]
		if m == nil || table == nil {
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
						Detail: fmt.Sprintf("%s is %q, %s", col, text, msg),
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

// castType is the type a value is cast to: the column's own, with its length
// and its domain, so the domain's constraints are held against the value and
// a char(3) is padded the way the column pads it. An explicit cast truncates
// to a length where an INSERT refuses, so a value is held against the length
// on its own first; see tooLong.
func castType(c dbschema.Column) string {
	return c.FullType
}

// castValues casts values to the column's type and returns each one's
// canonical text, and, for each value the column cannot take as dbfixture
// would write it, the rest of a sentence that says why.
//
// A value is invalid when PostgreSQL refuses to cast it, for whatever reason
// it gives: invalid input, a domain's CHECK, an hstore or tsquery syntax
// error. It is invalid, too, when an INSERT would refuse it although a cast
// takes it -- too long for varchar(n), char(n) or bit varying(n), not as long
// as bit(n) --, and when a CHECK constraint of the column refuses it.
func castValues(ctx context.Context, db bun.IDB, column dbschema.Column,
	values []string) (map[string]string, map[string]string, error) {

	canon, invalid := map[string]string{}, map[string]string{}
	inputs := map[string]string{}
	var todo []string
	for _, v := range values {
		in, msg := castInput(column, v)
		if msg != "" {
			invalid[v] = msg
			continue
		}
		inputs[v] = in
		todo = append(todo, v)
	}
	for start := 0; start < len(todo); start += castBatch {
		batch := todo[start:min(start+castBatch, len(todo))]
		err := castInto(ctx, db, column, batch, inputs, canon, invalid)
		if err == nil {
			continue
		}
		if !valueError(err) {
			return nil, nil, err
		}
		// One of them is not a value of the type. Find which, one by one.
		for _, v := range batch {
			if err := castInto(ctx, db, column, []string{v}, inputs, canon, invalid); err != nil {
				if !valueError(err) {
					return nil, nil, err
				}
				invalid[v] = fmt.Sprintf("which the column's type, %s, cannot hold: %s", column.FullType, valueMessage(err))
			}
		}
	}
	for v := range invalid {
		delete(canon, v)
	}
	return canon, invalid, nil
}

// castInput is the text PostgreSQL is handed for a value, or why the value is
// refused before it gets there.
//
// A YAML sequence arrives as a JSON array. In an array column it becomes the
// array literal it stands for, nested for a multidimensional array; in a
// bytea column it is the []byte yaml.v3 makes of a sequence of byte values,
// which is the only way yaml.v3 fills a []byte. In a json or jsonb column, a
// string that is not JSON is the JSON string an any field makes of it; one
// that is JSON is the document a string field hands bun.
func castInput(c dbschema.Column, v string) (string, string) {
	switch {
	case c.Category == "A" && jsonArray(v):
		lit, err := arrayLiteral(v, c.ElemType == "json" || c.ElemType == "jsonb")
		if err != nil {
			return "", "which is not an array the column's type can hold: " + err.Error()
		}
		return lit, ""
	case c.Type == "bytea" && jsonArray(v):
		return byteaOf(v)
	case c.Type == "json" || c.Type == "jsonb":
		if !json.Valid([]byte(v)) {
			return jsonString(v), ""
		}
	case c.Type == "int2" || c.Type == "int4" || c.Type == "int8":
		// yaml.v3 decodes 1.5 into an integer field as 1, without a word;
		// a string field hands PostgreSQL "1.5", which it refuses.
		if canon, ok := canonicalDecimal(v); ok {
			if i := strings.IndexByte(canon, '.'); i >= 0 {
				whole := canon[:i]
				if whole == "-0" {
					whole = "0"
				}
				return "", fmt.Sprintf("which an integer field holds as %s, because yaml.v3 drops the fraction, "+
					"and PostgreSQL refuses from a string field: write %s", whole, whole)
			}
		}
	}
	return v, ""
}

func castInto(ctx context.Context, db bun.IDB, column dbschema.Column, values []string, inputs map[string]string,
	canon, invalid map[string]string) error {

	cast := "t.v::" + castType(column)
	selects := []string{"t.k", readExpr(column, cast), tooLong(column, "t.v")}
	for _, check := range column.Checks {
		// The expression names the column; a one-row subselect gives that
		// name to the value.
		selects = append(selects, "(SELECT NOT COALESCE("+check.Expr+", true) FROM (SELECT "+cast+" AS "+
			sqlIdent(column.Name)+") AS c__)")
	}
	rowsSQL := strings.TrimSuffix(strings.Repeat("(?::text, ?::text),", len(values)), ",")
	query := "SELECT " + strings.Join(selects, ", ") + " FROM (VALUES " + rowsSQL + ") AS t(k, v)"
	args := make([]any, 0, 2*len(values))
	for _, v := range values {
		args = append(args, v, inputs[v])
	}
	type result struct {
		canon  string
		long   bool
		failed []string
	}
	got := map[string]result{}
	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r result
			var k string
			checks := make([]sql.NullBool, len(column.Checks))
			dest := []any{&k, &r.canon, &r.long}
			for i := range checks {
				dest = append(dest, &checks[i])
			}
			if err := rows.Scan(dest...); err != nil {
				return err
			}
			for i, failed := range checks {
				if failed.Valid && failed.Bool {
					r.failed = append(r.failed, column.Checks[i].Name+" "+column.Checks[i].Expr)
				}
			}
			got[k] = r
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return rows.Close()
	})
	if err != nil {
		return err
	}
	for k, r := range got {
		switch {
		case r.long:
			invalid[k] = lengthMessage(column)
		case len(r.failed) > 0:
			invalid[k] = "which the column's check constraint " + strings.Join(r.failed, " and ") + " refuses"
		default:
			canon[k] = columnText(column, r.canon)
		}
	}
	return nil
}

// tooLong is a boolean SQL expression saying whether the text expr is a value
// an INSERT into the column refuses for its length, although an explicit cast
// would take it: longer than varchar(n) or char(n) other than by trailing
// spaces, which an INSERT drops; longer than bit varying(n); not exactly as
// long as bit(n). For an array, any element.
func tooLong(c dbschema.Column, expr string) string {
	if c.Length <= 0 {
		return "false"
	}
	n := strconv.Itoa(c.Length)
	typ := c.Type
	if c.Category == "A" {
		typ = c.ElemType
	}
	var cond, elem string
	switch typ {
	case "varchar", "bpchar":
		cond = "char_length(%[1]s) > " + n + " AND rtrim(substr(%[1]s, " + n + " + 1), ' ') <> ''"
		elem = "varchar"
	case "bit":
		cond = "length(%[1]s) <> " + n
		elem = "varbit"
	case "varbit":
		cond = "length(%[1]s) > " + n
		elem = "varbit"
	default:
		return "false"
	}
	if c.Category == "A" {
		return "EXISTS (SELECT 1 FROM unnest((" + expr + ")::" + elem + "[]) AS e(v) WHERE " +
			fmt.Sprintf(cond, "e.v") + ")"
	}
	if elem == "varbit" {
		return "(" + fmt.Sprintf(cond, "("+expr+")::varbit") + ")"
	}
	return "(" + fmt.Sprintf(cond, expr) + ")"
}

func lengthMessage(c dbschema.Column) string {
	typ := c.Type
	if c.Category == "A" {
		typ = c.ElemType
	}
	switch typ {
	case "bit":
		return fmt.Sprintf("which is not %d bits long, as the column's type, %s, requires: an INSERT refuses it, "+
			"where a cast would pad or cut it without a word", c.Length, c.FullType)
	case "varbit":
		return fmt.Sprintf("which is longer than the %d bits the column's type, %s, holds: an INSERT refuses it, "+
			"where a cast would cut it without a word", c.Length, c.FullType)
	}
	return fmt.Sprintf("which is longer than the %d characters the column's type, %s, holds: an INSERT refuses it, "+
		"where a cast would cut it without a word", c.Length, c.FullType)
}

// valueError reports whether err is PostgreSQL refusing one value, as opposed
// to a connection, a transaction or the server failing: any error with a
// SQLSTATE but those of connections (08), transaction state (25),
// serialization (40), a missing privilege (42501), resources (53), object
// state (55), operator intervention (57), system errors (58), and internal
// errors (XX). A type's input function says no in class 22 for most types,
// but hstore, ltree and tsquery say 42601, and a domain's CHECK 23514.
func valueError(err error) bool {
	state := pgerr.State(err)
	if len(state) != 5 || state == "42501" {
		return false
	}
	switch state[:2] {
	case "08", "25", "40", "53", "55", "57", "58", "XX":
		return false
	}
	return true
}

// valueMessage is PostgreSQL's primary message for an error, without the
// severity and the SQLSTATE pgx puts around it.
func valueMessage(err error) string {
	msg := pgerr.Message(err)
	if state := pgerr.State(err); state != "" {
		msg = strings.TrimSuffix(msg, " (SQLSTATE "+state+")")
	}
	for _, severity := range []string{"ERROR: ", "FATAL: ", "PANIC: "} {
		msg = strings.TrimPrefix(msg, severity)
	}
	return msg
}

// sqlIdent double-quotes a name from the catalog, whatever it holds.
func sqlIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// jsonArray reports a JSON array: a YAML sequence, as the fixture reader
// carries one.
func jsonArray(v string) bool {
	return strings.HasPrefix(strings.TrimSpace(v), "[") && json.Valid([]byte(v))
}

// arrayLiteral writes a JSON array as the PostgreSQL array literal it stands
// for: a nested array is a dimension, a JSON null a NULL element, anything else
// an element in double quotes, its own JSON text for an object. For an array
// of json or jsonb, a nested array is an element like an object. A ragged
// array is left for PostgreSQL to refuse.
func arrayLiteral(v string, jsonElems bool) (string, error) {
	dec := json.NewDecoder(strings.NewReader(v))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return "", err
	}
	var b strings.Builder
	var write func(x any, top bool) error
	write = func(x any, top bool) error {
		switch x := x.(type) {
		case []any:
			if top || !jsonElems {
				b.WriteByte('{')
				for i, e := range x {
					if i > 0 {
						b.WriteByte(',')
					}
					if err := write(e, false); err != nil {
						return err
					}
				}
				b.WriteByte('}')
				return nil
			}
		case nil:
			b.WriteString("NULL")
			return nil
		case string:
			writeArrayElem(&b, x)
			return nil
		case json.Number:
			writeArrayElem(&b, x.String())
			return nil
		case bool:
			writeArrayElem(&b, strconv.FormatBool(x))
			return nil
		}
		j, err := json.Marshal(x)
		if err != nil {
			return err
		}
		writeArrayElem(&b, string(j))
		return nil
	}
	if err := write(root, true); err != nil {
		return "", err
	}
	return b.String(), nil
}

func writeArrayElem(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
}

// byteaOf is the bytea a JSON array of byte values stands for, in hex: what
// yaml.v3 makes of a sequence of whole numbers from 0 to 255 for a []byte
// field, and the only thing it makes one of.
func byteaOf(v string) (string, string) {
	dec := json.NewDecoder(strings.NewReader(v))
	dec.UseNumber()
	var elems []any
	if err := dec.Decode(&elems); err != nil {
		return "", "which is not a sequence of byte values: " + err.Error()
	}
	out := make([]byte, 0, len(elems))
	for _, e := range elems {
		n, ok := e.(json.Number)
		var b int64
		if ok {
			var err error
			b, err = strconv.ParseInt(n.String(), 10, 64)
			ok = err == nil && b >= 0 && b <= 255
		}
		if !ok {
			return "", fmt.Sprintf("which is a sequence a []byte field cannot hold: yaml.v3 fills one only from "+
				"whole numbers from 0 to 255, and %v is not one", e)
		}
		out = append(out, byte(b))
	}
	return `\x` + hex.EncodeToString(out), ""
}
