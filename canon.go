package fixturemigrate

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

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
// FindingInvalidValue, rather than a migration that fails at deploy time; so
// does a date or time a time.Time and a string field, or two servers, would
// store differently. A ~ in a json column is a FindingNullDefault, and two
// natural keys the key's type holds equal a FindingDuplicateKey. Natural keys,
// ids and references are rewritten along with the values, so the snapshot
// stays consistent.
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
		snap.noteUniques(model, table)
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
				decide, _ := decidingColumn(cfg, tables, e, col, column)
				text, ok := sourceOf(e, m, col, decide)
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
			unclear, err := unclearSpellings(ctx, db, column, m, col, entries)
			if err != nil {
				return fmt.Errorf("%s.%s: %w", model, col, err)
			}
			for _, e := range entries {
				decide, known := decidingColumn(cfg, tables, e, col, column)
				text, ok := sourceOf(e, m, col, decide)
				if !ok {
					continue
				}
				if known {
					// The column's type has decided which text the database
					// holds.
					delete(e.AsWritten, col)
				}
				if msg, bad := invalid[text]; bad {
					snap.Findings = append(snap.Findings, Finding{
						Kind: FindingInvalidValue, Model: model, Row: e.label(model),
						Detail: fmt.Sprintf("%s is %q, %s", col, text, msg),
					})
					continue
				}
				if msg, bad := unclear[e]; bad {
					snap.Findings = append(snap.Findings, Finding{
						Kind: FindingInvalidValue, Model: model, Row: e.label(model),
						Detail: col + " is " + msg,
					})
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

	// A reference names its target by the target's ref value, which has to
	// be exactly what the target's ref column holds: the reading of it that
	// column's type picks (0012 is 10 in a bigint and 0012 in a text), as
	// that column spells it. And a natural key is made of values that may
	// just have been respelled.
	for _, model := range snap.Order {
		m := cfg.Models[model]
		for _, e := range snap.Entries[model] {
			for _, col := range sortedColumns(e.Cells) {
				v := e.Cells[col]
				if v.Ref == nil {
					continue
				}
				key := v.Ref.Key
				if written, ok := e.AsWritten[col]; ok {
					decide, known := decidingColumn(cfg, tables, e, col, dbschema.Column{})
					if !known {
						// Left with both readings, for the diff to refuse a
						// change that depends on which one it is.
						continue
					}
					if decide.StringField() {
						key = written
					}
					delete(e.AsWritten, col)
				}
				if c, ok := refCanon[v.Ref.Model][key]; ok {
					key = c
				}
				e.Cells[col] = fixturechange.RefTo(v.Ref.Model, key)
			}
			if key, err := keyOf(cfg, m, model, e.Full(m)); err == nil {
				e.setKey(model, key)
			}
		}
	}
	// Two keys or two ids that were spelled apart may be one value now.
	kept := snap.Findings[:0]
	for _, f := range snap.Findings {
		if f.Kind != FindingDuplicateKey && f.Kind != FindingDuplicateID {
			kept = append(kept, f)
		}
	}
	snap.Findings = kept
	for _, model := range snap.Order {
		snap.reportDuplicates(model)
		snap.reportDuplicateIDs(cfg, model)
	}
	lintJSONNulls(cfg, snap, tables)
	return reportEqualKeys(ctx, db, cfg, snap, tables)
}

// sourceOf is the text a cast of a column of an entry starts from: the value
// as written when the deciding column (decidingColumn) is one a Go string
// field writes and the file wrote the value differently from what it resolves
// to (1.10, 017, True), because that is what dbfixture stores there; the
// resolved value otherwise.
func sourceOf(e *Entry, m *Model, col string, decide dbschema.Column) (string, bool) {
	text, ok := literalOf(e, m, col)
	if !ok {
		return "", false
	}
	if written, ok := e.AsWritten[col]; ok && decide.StringField() {
		return written, true
	}
	return text, true
}

// decidingColumn is the column whose type says which reading of an entry's
// column the database holds: own, the column itself, unless another row
// supplies the value (Entry.from), whose column it is then. known is false
// when that column is not in tables, and nothing can be decided.
func decidingColumn(cfg *Config, tables map[string]*dbschema.Table, e *Entry, col string,
	own dbschema.Column) (dbschema.Column, bool) {

	src, ok := e.from[col]
	if !ok {
		return own, own.Name != ""
	}
	m := cfg.Models[src.model]
	if m == nil {
		return dbschema.Column{}, false
	}
	table := tables[cfg.QualifiedTable(m)]
	if table == nil {
		return dbschema.Column{}, false
	}
	return table.Column(src.column)
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
// as bit(n) --, when a CHECK constraint of the column refuses it, and when
// what it stores depends on something the tool cannot see: the session that
// seeds it, or the Go type of the model's field.
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
	raw := map[string]string{}
	failed := map[string]bool{}
	for start := 0; start < len(todo); start += castBatch {
		batch := todo[start:min(start+castBatch, len(todo))]
		err := castInto(ctx, db, column, batch, inputs, canon, raw, invalid)
		if err == nil {
			continue
		}
		if !valueError(err) {
			return nil, nil, err
		}
		// One of them is not a value of the type. Find which, one by one.
		for _, v := range batch {
			if err := castInto(ctx, db, column, []string{v}, inputs, canon, raw, invalid); err != nil {
				if !valueError(err) {
					return nil, nil, err
				}
				invalid[v] = fmt.Sprintf("which the column's type, %s, cannot hold: %s", column.FullType, valueMessage(err))
				failed[v] = true
			}
		}
	}
	if dateTime(column) {
		if err := sessionDependent(ctx, db, column, todo, inputs, raw, failed, invalid); err != nil {
			return nil, nil, err
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
	canon, raw, invalid map[string]string) error {

	cast := "t.v::" + castType(column)
	selects := []string{"t.k", readExpr(column, cast), tooLong(column, "t.v")}
	if dateTime(column) {
		selects = append(selects, "("+cast+")::text")
	}
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
		canon, raw string
		long       bool
		failed     []string
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
			if dateTime(column) {
				dest = append(dest, &r.raw)
			}
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
			raw[k] = r.raw
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
// state (55), operator intervention (57), system errors (58), and data or
// index corruption (XX001, XX002). A type's input function says no in class 22
// for most types, but hstore, ltree and tsquery say 42601 from PostgreSQL 16
// on, a plain internal error (XX000) before it, and a domain's CHECK 23514.
func valueError(err error) bool {
	state := pgerr.State(err)
	if len(state) != 5 || state == "42501" || state == "XX001" || state == "XX002" {
		return false
	}
	switch state[:2] {
	case "08", "25", "40", "53", "55", "57", "58":
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

// isJSON reports a json or jsonb column.
func isJSON(c dbschema.Column) bool {
	return c.Type == "json" || c.Type == "jsonb"
}

// dateTime reports a column of dates or times, or an array of them: the
// types whose text PostgreSQL reads by the session's TimeZone and DateStyle.
func dateTime(c dbschema.Column) bool {
	typ := c.Type
	if c.Category == "A" {
		typ = c.ElemType
	}
	switch typ {
	case "date", "timestamp", "timestamptz", "time", "timetz":
		return true
	}
	return false
}

// errRollback ends a subtransaction that only looked at something, so its
// settings go with it.
var errRollback = errors.New("roll back")

// alternateSessions are the settings a value of a date or time column is
// read under besides the tool's own (UTC, ISO with year-month-day order): a
// value they read differently means different things on different servers.
// The zones are POSIX offsets, which need no time zone database: 5:45 east
// and 8 hours west of UTC.
var alternateSessions = [][2]string{
	{"<+0545>-05:45", "ISO, DMY"},
	{"<-08>+08", "ISO, MDY"},
}

// sessionDependent finds the values whose meaning depends on the session that
// writes them, and marks them invalid: dbfixture writes text into a string
// field's column under whatever TimeZone and DateStyle its connection has,
// which the tool cannot know. A timestamp with time zone written without one,
// "2026-01-01 10:00" or a date alone, is that wall time wherever the server
// is; 01/02/2026 is a day in January or in February. So is anything with now,
// today, tomorrow or yesterday in it, which is a different value every day.
//
// Each value is read again under other settings and compared with what it was
// under the tool's own, in a subtransaction that is rolled back.
func sessionDependent(ctx context.Context, db bun.IDB, column dbschema.Column, values []string,
	inputs, raw map[string]string, failed map[string]bool, invalid map[string]string) error {

	var todo, refused []string
	for _, v := range values {
		if failed[v] {
			// 01/02/2026 is out of range year first, and a day to a server
			// whose DateStyle puts the month or the day first.
			refused = append(refused, v)
			continue
		}
		if _, bad := invalid[v]; bad {
			continue
		}
		for _, word := range strings.FieldsFunc(strings.ToLower(v), func(r rune) bool { return r < 'a' || r > 'z' }) {
			switch word {
			case "now", "today", "tomorrow", "yesterday":
				invalid[v] = "which PostgreSQL evaluates when the row is written, so it is another value in every " +
					"database and on every day: write the value itself"
			}
		}
		if _, bad := invalid[v]; !bad {
			todo = append(todo, v)
		}
	}
	if len(todo) == 0 && len(refused) == 0 {
		return nil
	}
	typ := castType(column)
	differs := map[string]bool{}
	readable := map[string]bool{}
	tryCast := func(ctx context.Context, tx bun.Tx, v string) error {
		return tx.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var out string
			return tx.QueryRowContext(ctx, "SELECT (?::text::"+typ+")::text", inputs[v]).Scan(&out)
		})
	}
	compare := func(ctx context.Context, tx bun.Tx, batch []string) error {
		rowsSQL := strings.TrimSuffix(strings.Repeat("(?::text, ?::text, ?::text),", len(batch)), ",")
		args := make([]any, 0, 3*len(batch))
		for _, v := range batch {
			args = append(args, v, inputs[v], raw[v])
		}
		return tx.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			rows, err := tx.QueryContext(ctx, "SELECT t.k, t.v::"+typ+" IS NOT DISTINCT FROM t.r::"+typ+
				" FROM (VALUES "+rowsSQL+") AS t(k, v, r)", args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var k string
				var same bool
				if err := rows.Scan(&k, &same); err != nil {
					return err
				}
				if !same {
					differs[k] = true
				}
			}
			if err := rows.Err(); err != nil {
				return err
			}
			return rows.Close()
		})
	}
	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		for _, alt := range alternateSessions {
			if _, err := tx.ExecContext(ctx, "SELECT set_config('TimeZone', ?, true), set_config('DateStyle', ?, true)",
				alt[0], alt[1]); err != nil {
				return err
			}
			for start := 0; start < len(todo); start += castBatch {
				batch := todo[start:min(start+castBatch, len(todo))]
				err := compare(ctx, tx, batch)
				if err == nil {
					continue
				}
				if !valueError(err) {
					return err
				}
				for _, v := range batch {
					if err := compare(ctx, tx, []string{v}); err != nil {
						if !valueError(err) {
							return err
						}
						differs[v] = true
					}
				}
			}
			for _, v := range refused {
				if err := tryCast(ctx, tx, v); err == nil {
					readable[v] = true
				} else if !valueError(err) {
					return err
				}
			}
		}
		return errRollback
	})
	if err != nil && !errors.Is(err, errRollback) {
		return err
	}
	for v := range differs {
		invalid[v] = "which PostgreSQL reads by the TimeZone or the DateStyle of the session that writes it, so " +
			"what dbfixture stores depends on the server it seeds: spell it out in ISO 8601, with the offset " +
			"for a time zone, as " + isoExample(column, raw[v])
	}
	for v := range readable {
		invalid[v] = "which PostgreSQL reads by the DateStyle of the session that writes it, a day first or a " +
			"month first, so what dbfixture stores depends on the server it seeds: spell it out in ISO 8601, " +
			"year first, as 2026-01-02"
	}
	return nil
}

// isoExample is the unambiguous spelling of what the tool read a value as, for
// a message: PostgreSQL's own text in the tool's session, which is ISO 8601,
// with a timestamp's zone as an offset.
func isoExample(c dbschema.Column, raw string) string {
	if c.Type == "timestamptz" {
		if t, err := time.Parse("2006-01-02 15:04:05.999999999Z07", raw); err == nil {
			return t.UTC().Format(time.RFC3339Nano)
		}
	}
	return raw
}

// unclearSpellings finds the values of a date or time column that a time.Time
// field and a string field store differently, which only the model's Go type
// decides. A time.Time is written by bun in UTC, cut to microseconds; a string
// is handed to PostgreSQL as it is written. So 2026-01-01T10:00:00+02:00 is
// 08:00 in a timestamp column through one and 10:00 through the other,
// 2026-01-01T23:30:00-05:00 is the 2nd or the 1st in a date column, and
// .1234567 seconds are .123456 or .123457.
//
// A value takes the time.Time path when yaml.v3 can decode it into one: an
// unquoted YAML timestamp, or RFC 3339. The result is keyed by entry, because
// the same instant may have been written in one way that is clear and one
// that is not.
func unclearSpellings(ctx context.Context, db bun.IDB, column dbschema.Column, m *Model, col string,
	entries []*Entry) (map[*Entry]string, error) {

	out := map[*Entry]string{}
	if column.Category == "A" || !dateTime(column) {
		return out, nil
	}
	type pair struct{ asTime, asString string }
	of := map[*Entry]pair{}
	shown := map[*Entry]string{}
	var pairs []pair
	seen := map[pair]bool{}
	for _, e := range entries {
		text, ok := sourceOf(e, m, col, column)
		if !ok {
			continue
		}
		written, resolved := text, false
		if w, ok := e.AsWritten[col]; ok {
			written, resolved = w, true
		}
		t, ok := goTime(text, resolved)
		if !ok {
			continue
		}
		p := pair{t.UTC().Format("2006-01-02 15:04:05.999999-07:00"), written}
		of[e] = p
		shown[e] = written
		if !seen[p] {
			seen[p] = true
			pairs = append(pairs, p)
		}
	}
	if len(pairs) == 0 {
		return out, nil
	}
	typ := castType(column)
	unclear := map[pair]string{}
	compare := func(batch []pair) error {
		rowsSQL := strings.TrimSuffix(strings.Repeat("(?::text, ?::text),", len(batch)), ",")
		args := make([]any, 0, 2*len(batch))
		for _, p := range batch {
			args = append(args, p.asTime, p.asString)
		}
		return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			rows, err := tx.QueryContext(ctx, "SELECT t.a, t.b, t.a::"+typ+" IS NOT DISTINCT FROM t.b::"+typ+
				", (t.a::"+typ+")::text, (t.b::"+typ+")::text FROM (VALUES "+rowsSQL+") AS t(a, b)", args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var p pair
				var same bool
				var asTime, asString string
				if err := rows.Scan(&p.asTime, &p.asString, &same, &asTime, &asString); err != nil {
					return err
				}
				if !same {
					unclear[p] = fmt.Sprintf("which a time.Time field stores as %s and a string field as %s, and "+
						"only the Go model knows which this column has: write the one you mean as %s or %s, "+
						"which both read alike", asTime, asString, isoExample(column, asTime),
						isoExample(column, asString))
				}
			}
			if err := rows.Err(); err != nil {
				return err
			}
			return rows.Close()
		})
	}
	for start := 0; start < len(pairs); start += castBatch {
		batch := pairs[start:min(start+castBatch, len(pairs))]
		err := compare(batch)
		if err == nil {
			continue
		}
		if !valueError(err) {
			return nil, err
		}
		// A spelling one of the two cannot be read by PostgreSQL leaves
		// only the other path, which is no question; castValues reports a
		// value nothing can read.
		for _, p := range batch {
			if err := compare([]pair{p}); err != nil && !valueError(err) {
				return nil, err
			}
		}
	}
	for e, p := range of {
		if msg, ok := unclear[p]; ok {
			out[e] = strconv.Quote(shown[e]) + ", " + msg
		}
	}
	return out, nil
}

// goTime is the time.Time yaml.v3 decodes a value into, if it can: text is
// what the tool resolved the value to, and resolved says it was not a string
// in the file -- an unquoted timestamp, which the tool resolves to RFC 3339 in
// UTC or a date. A string decodes into a time.Time when it is RFC 3339. A date
// alone may have been either, and is midnight UTC as a time.Time.
func goTime(text string, resolved bool) (time.Time, bool) {
	if t, err := time.Parse("2006-01-02", text); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339Nano, text); err == nil {
		return t, true
	}
	if resolved {
		return yamlTime(text)
	}
	return time.Time{}, false
}

// lintJSONNulls reports a ~ written into a json or jsonb column that has no
// default, because what it stores depends on the model's Go field: a nil map,
// slice or any is marshalled to the JSON null, while a nil pointer or a
// nullzero field is written as DEFAULT, which is NULL. The tool reads ~ as
// NULL, so it is under policy.null_default, like a null bun turns into a
// column default; LintNullDefaults reports a column that has one.
func lintJSONNulls(cfg *Config, snap *Snapshot, tables map[string]*dbschema.Table) {
	for _, model := range snap.Order {
		m := cfg.Models[model]
		if m == nil {
			continue
		}
		table := tables[cfg.QualifiedTable(m)]
		if table == nil {
			continue
		}
		for _, e := range snap.Entries[model] {
			for _, col := range sortedColumns(e.Cells) {
				column, ok := table.Column(col)
				if !ok || !e.Cells[col].IsNull || !isJSON(column) {
					continue
				}
				if _, ok := column.NonNullDefault(); ok {
					continue
				}
				snap.Findings = append(snap.Findings, Finding{
					Kind: FindingNullDefault, Model: model, Row: e.label(model),
					Detail: fmt.Sprintf("%s is null, which in a %s column is the JSON null when the model's field is "+
						"a map, a slice or an any, and NULL when it is a pointer or nullzero, and only the model "+
						"knows which: set policy.null_default to warn if it writes NULL here; for the JSON null, "+
						"leave %s out of the row and give the model defaults: {%s: 'null'}",
						col, column.Type, col, col),
				})
			}
		}
	}
}

// reportEqualKeys reports the rows of a model whose natural keys differ as
// text but are one value to PostgreSQL: "Go" and "GO" in a citext column,
// "1 day" and "24 hours" in an interval. A guard matching one of them matches
// both, and with a unique index behind the key dbfixture cannot load both.
// The keys are grouped by the columns' own equality, GROUP BY over values
// cast to the columns' types; a model whose key a type without one makes up,
// or holds a value the type refuses, is passed over.
func reportEqualKeys(ctx context.Context, db bun.IDB, cfg *Config, snap *Snapshot,
	tables map[string]*dbschema.Table) error {

	for _, model := range snap.Order {
		m := cfg.Models[model]
		if m == nil {
			continue
		}
		table := tables[cfg.QualifiedTable(m)]
		entries := snap.Entries[model]
		if table == nil || len(entries) < 2 {
			continue
		}
		groups := map[string][]*Entry{}
		var order []string
		for _, e := range entries {
			cols := strings.Join(sortedColumns(e.Key), "\x00")
			if _, ok := groups[cols]; !ok {
				order = append(order, cols)
			}
			groups[cols] = append(groups[cols], e)
		}
		for _, cols := range order {
			if len(groups[cols]) < 2 || cols == "" {
				continue
			}
			same, err := equalKeys(ctx, db, table, strings.Split(cols, "\x00"), groups[cols])
			if err != nil {
				return fmt.Errorf("%s: %w", model, err)
			}
			for _, group := range same {
				keys, ids := map[string]bool{}, make([]string, 0, len(group))
				var shown []string
				for _, e := range group {
					if !keys[e.KeyStr] {
						keys[e.KeyStr] = true
						shown = append(shown, keyLabelOf(e.Key))
					}
					id := e.ID
					if id == "" {
						id = "(no id)"
					}
					ids = append(ids, id)
				}
				if len(keys) < 2 {
					continue // reportDuplicates has said so
				}
				snap.Findings = append(snap.Findings, Finding{
					Kind: FindingDuplicateKey, Model: model, Row: group[0].label(model),
					Detail: fmt.Sprintf("the natural keys %s are one value to the key's type in PostgreSQL, so "+
						"no lookup by it can tell these %s (%s) apart, and a unique index would keep dbfixture "+
						"from loading them all: make them differ as the type compares them",
						strings.Join(shown, " and "), plural(len(group), "row"), strings.Join(ids, ", ")),
				})
			}
		}
	}
	return nil
}

// equalKeys groups entries whose key values PostgreSQL holds equal, and
// returns the groups of more than one.
func equalKeys(ctx context.Context, db bun.IDB, table *dbschema.Table, cols []string,
	entries []*Entry) ([][]*Entry, error) {

	var groupBy []string
	for i, col := range cols {
		ref := false
		for _, e := range entries {
			if e.Key[col].Ref != nil {
				ref = true
			}
		}
		column, ok := table.Column(col)
		switch {
		case ref:
			// A reference is its target's key, compared as text.
			groupBy = append(groupBy, fmt.Sprintf("t.c%d", i))
		case ok && !isJSON(column):
			groupBy = append(groupBy, fmt.Sprintf("t.c%d::%s", i, castType(column)))
		default:
			return nil, nil
		}
	}
	var out [][]*Entry
	names := make([]string, 0, len(cols)+1)
	names = append(names, "i")
	for i := range cols {
		names = append(names, fmt.Sprintf("c%d", i))
	}
	row := "(?::int" + strings.Repeat(", ?::text", len(cols)) + ")"
	rowsSQL := strings.TrimSuffix(strings.Repeat(row+",", len(entries)), ",")
	args := make([]any, 0, len(entries)*(len(cols)+1))
	for i, e := range entries {
		args = append(args, i)
		for _, col := range cols {
			v := e.Key[col]
			switch {
			case v.IsNull:
				args = append(args, nil)
			case v.Ref != nil:
				// A column references one model, so its key names the row.
				args = append(args, v.Ref.Key)
			default:
				args = append(args, v.Lit)
			}
		}
	}
	query := "SELECT array_to_string(array_agg(t.i ORDER BY t.i), ',') FROM (VALUES " + rowsSQL + ") AS t(" +
		strings.Join(names, ", ") + ") GROUP BY " + strings.Join(groupBy, ", ") + " HAVING count(*) > 1"
	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var list string
			if err := rows.Scan(&list); err != nil {
				return err
			}
			var group []*Entry
			for _, part := range strings.Split(list, ",") {
				i, err := strconv.Atoi(part)
				if err != nil || i < 0 || i >= len(entries) {
					return fmt.Errorf("unexpected group %q", list)
				}
				group = append(group, entries[i])
			}
			out = append(out, group)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return rows.Close()
	})
	if err != nil {
		if valueError(err) {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}
