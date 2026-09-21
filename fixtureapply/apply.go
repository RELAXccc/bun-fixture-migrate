// Package fixtureapply runs a generated change set against a seeded database.
// Generated migrations call Apply and Revert; nothing else here is meant for
// hand-written code.
//
// Every statement is guarded. A row is found by its natural key, never by its
// id, because ids drift between databases. An update or a delete additionally
// requires that the row still holds the values the base revision had, so a
// change somebody made by hand is kept and a second run is a no-op. An insert
// requires that no row with the same natural key exists yet. Every reference
// is resolved to a real id before the statement runs, so a missing or
// ambiguous target fails the migration instead of writing NULL.
//
// Identifiers are never taken from user input at runtime: table and column
// names come from the generated file and must be plain SQL identifiers, which
// Validate checks before any statement is built. Values are always bound
// parameters.
package fixtureapply

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
)

// Option configures Apply and Revert.
type Option func(*options)

type options struct {
	logf func(format string, args ...any)
}

// WithLogger replaces log.Printf as the destination of the per-row report.
// Pass func(string, ...any) {} to silence it.
func WithLogger(logf func(format string, args ...any)) Option {
	return func(o *options) { o.logf = logf }
}

func newOptions(opts []Option) options {
	o := options{logf: log.Printf}
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// Apply runs a change set in one transaction.
func Apply(ctx context.Context, db bun.IDB, set fixturechange.Set, opts ...Option) error {
	return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return run(ctx, tx, set, false, newOptions(opts))
	})
}

// Revert undoes a change set: the list backwards, every change inverted. An
// insert becomes a delete guarded by the values it wrote, an update swaps old
// and new, a delete becomes an insert of the row it removed.
func Revert(ctx context.Context, db bun.IDB, set fixturechange.Set, opts ...Option) error {
	return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return run(ctx, tx, set, true, newOptions(opts))
	})
}

var identPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*$`)

// quoteIdent double-quotes a plain, optionally schema-qualified identifier and
// rejects anything else. Generated files only ever contain names that came
// from the configuration, but this is the line between the file and the
// database and it is cheap to hold.
func quoteIdent(name string) (string, error) {
	parts := strings.Split(name, ".")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if !identPattern.MatchString(p) {
			return "", fmt.Errorf("%q is not a plain SQL identifier", name)
		}
		out = append(out, `"`+p+`"`)
	}
	return strings.Join(out, "."), nil
}

// Validate checks a change set without touching the database: known models,
// plain identifiers, a kind that exists, and the shape each kind needs. Apply
// and Revert call it first, and the generator's tests call it on their output.
func Validate(set fixturechange.Set) error {
	// A model nobody points at needs no key column, so an empty one is only
	// an error where a reference would use it.
	referenced := map[string]bool{}
	for _, c := range set.Changes {
		for _, values := range []fixturechange.Values{c.Key, c.Old, c.New} {
			for _, v := range values {
				if v.Ref != nil {
					referenced[v.Ref.Model] = true
				}
			}
		}
	}
	for _, model := range sortedModels(set.Tables) {
		t := set.Tables[model]
		parts := []struct{ what, name string }{{"table", t.Name}}
		if t.Serial || referenced[model] {
			parts = append(parts, struct{ what, name string }{"id column", t.ID})
		}
		if referenced[model] {
			parts = append(parts, struct{ what, name string }{"key column", t.Key})
		}
		for _, part := range parts {
			if part.name == "" {
				return fmt.Errorf("model %q: no %s", model, part.what)
			}
			if _, err := quoteIdent(part.name); err != nil {
				return fmt.Errorf("model %q: %s %w", model, part.what, err)
			}
		}
	}
	if set.SeedGuardTable != "" {
		if _, err := quoteIdent(set.SeedGuardTable); err != nil {
			return fmt.Errorf("seed guard table %w", err)
		}
	}
	for i, c := range set.Changes {
		if _, ok := set.Tables[c.Model]; !ok {
			return fmt.Errorf("change %d: unknown model %q", i, c.Model)
		}
		if len(c.Key) == 0 {
			return fmt.Errorf("change %d (%s): no key", i, c.Model)
		}
		for _, values := range []fixturechange.Values{c.Key, c.Old, c.New} {
			for _, col := range sortedColumns(values) {
				if _, err := quoteIdent(col); err != nil {
					return fmt.Errorf("change %d (%s): %w", i, c.Model, err)
				}
				if ref := values[col].Ref; ref != nil {
					if _, ok := set.Tables[ref.Model]; !ok {
						return fmt.Errorf("change %d (%s.%s): reference to unknown model %q", i, c.Model, col, ref.Model)
					}
				}
			}
		}
		switch c.Kind {
		case fixturechange.Insert:
			if len(c.New) == 0 {
				return fmt.Errorf("change %d (%s): insert without columns", i, c.Model)
			}
			if len(c.Old) != 0 {
				return fmt.Errorf("change %d (%s): insert with old values", i, c.Model)
			}
		case fixturechange.Update:
			if len(c.New) == 0 {
				return fmt.Errorf("change %d (%s): update without columns", i, c.Model)
			}
			// Every written column must carry the value it is replacing,
			// otherwise the statement would overwrite a row somebody else
			// changed.
			for _, col := range sortedColumns(c.New) {
				if _, ok := c.Old[col]; !ok {
					return fmt.Errorf("change %d (%s): update of %q without its old value", i, c.Model, col)
				}
			}
		case fixturechange.Delete:
			if len(c.New) != 0 {
				return fmt.Errorf("change %d (%s): delete with new values", i, c.Model)
			}
		default:
			return fmt.Errorf("change %d (%s): unknown kind %q", i, c.Model, c.Kind)
		}
	}
	return nil
}

func run(ctx context.Context, tx bun.IDB, set fixturechange.Set, revert bool, o options) error {
	if err := Validate(set); err != nil {
		return err
	}
	if set.SeedGuardTable != "" {
		seeded, err := tableHasRows(ctx, tx, set.SeedGuardTable)
		if err != nil {
			return err
		}
		if !seeded {
			o.logf("%s: %s is empty, nothing to do (the fixture loader seeds this database)", set.Name, set.SeedGuardTable)
			return nil
		}
	}

	r := &runner{tx: tx, set: set, refs: map[string]string{}, resync: map[string]bool{}}
	order := make([]int, len(set.Changes))
	for i := range order {
		order[i] = i
	}
	if revert {
		for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
			order[i], order[j] = order[j], order[i]
		}
	}
	for _, i := range order {
		c := set.Changes[i]
		if revert {
			c = invert(c)
		}
		n, err := r.exec(ctx, c)
		if err != nil {
			return fmt.Errorf("%s: %s %s %s: %w", set.Name, c.Model, keyLabel(c.Key), c.Kind, err)
		}
		result := "applied"
		if n == 0 {
			result = "skipped, the row is missing or no longer holds the old values"
		}
		o.logf("%s: %s %s %s: %s (%d row(s))", set.Name, c.Model, keyLabel(c.Key), c.Kind, result, n)
	}
	return r.syncSequences(ctx)
}

// invert turns a change into the change that undoes it.
func invert(c fixturechange.Change) fixturechange.Change {
	switch c.Kind {
	case fixturechange.Insert:
		return fixturechange.Change{Model: c.Model, Kind: fixturechange.Delete, Key: c.Key, Old: c.New}
	case fixturechange.Delete:
		return fixturechange.Change{Model: c.Model, Kind: fixturechange.Insert, Key: c.Key, New: c.Old}
	default:
		return fixturechange.Change{Model: c.Model, Kind: c.Kind, Key: c.Key, Old: c.New, New: c.Old}
	}
}

type runner struct {
	tx   bun.IDB
	set  fixturechange.Set
	refs map[string]string // model\x00key -> resolved id
	// resync collects the models that got an explicit id written into a
	// sequence-backed table.
	resync map[string]bool
}

func (r *runner) exec(ctx context.Context, c fixturechange.Change) (int64, error) {
	t := r.set.Tables[c.Model]
	table, err := quoteIdent(t.Name)
	if err != nil {
		return 0, err
	}
	switch c.Kind {
	case fixturechange.Insert:
		return r.insert(ctx, c, t, table)
	case fixturechange.Update:
		return r.update(ctx, c, table)
	default:
		return r.delete(ctx, c, table)
	}
}

func (r *runner) insert(ctx context.Context, c fixturechange.Change, t fixturechange.Table, table string) (int64, error) {
	var cols, exprs []string
	var args []any
	explicitID := false
	for _, col := range sortedColumns(c.New) {
		expr, a, err := r.value(ctx, c.New[col])
		if err != nil {
			return 0, err
		}
		q, err := quoteIdent(col)
		if err != nil {
			return 0, err
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
	// again.
	where, whereArgs, err := r.match(ctx, c.Key)
	if err != nil {
		return 0, err
	}
	args = append(args, whereArgs...)
	query := fmt.Sprintf("INSERT INTO %s (%s) SELECT %s WHERE NOT EXISTS (SELECT 1 FROM %s WHERE %s)",
		table, strings.Join(cols, ", "), strings.Join(exprs, ", "), table, where)
	n, err := r.run(ctx, query, args)
	if err != nil {
		return 0, err
	}
	if n > 0 && explicitID && t.Serial {
		r.resync[c.Model] = true
	}
	return n, nil
}

func (r *runner) update(ctx context.Context, c fixturechange.Change, table string) (int64, error) {
	var sets []string
	var args []any
	for _, col := range sortedColumns(c.New) {
		expr, a, err := r.value(ctx, c.New[col])
		if err != nil {
			return 0, err
		}
		q, err := quoteIdent(col)
		if err != nil {
			return 0, err
		}
		sets = append(sets, q+" = "+expr)
		args = append(args, a...)
	}
	where, whereArgs, err := r.matchAll(ctx, c.Key, c.Old)
	if err != nil {
		return 0, err
	}
	args = append(args, whereArgs...)
	return r.run(ctx, fmt.Sprintf("UPDATE %s SET %s WHERE %s", table, strings.Join(sets, ", "), where), args)
}

func (r *runner) delete(ctx context.Context, c fixturechange.Change, table string) (int64, error) {
	where, args, err := r.matchAll(ctx, c.Key, c.Old)
	if err != nil {
		return 0, err
	}
	return r.run(ctx, fmt.Sprintf("DELETE FROM %s WHERE %s", table, where), args)
}

func (r *runner) run(ctx context.Context, query string, args []any) (int64, error) {
	res, err := r.tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// match renders "col IS NOT DISTINCT FROM <value>" for every column, joined by
// AND. IS NOT DISTINCT FROM rather than = so a NULL compares like any other
// value.
func (r *runner) match(ctx context.Context, values fixturechange.Values) (string, []any, error) {
	var parts []string
	var args []any
	for _, col := range sortedColumns(values) {
		expr, a, err := r.value(ctx, values[col])
		if err != nil {
			return "", nil, err
		}
		q, err := quoteIdent(col)
		if err != nil {
			return "", nil, err
		}
		parts = append(parts, q+" IS NOT DISTINCT FROM "+expr)
		args = append(args, a...)
	}
	if len(parts) == 0 {
		return "TRUE", nil, nil
	}
	return strings.Join(parts, " AND "), args, nil
}

func (r *runner) matchAll(ctx context.Context, sets ...fixturechange.Values) (string, []any, error) {
	var parts []string
	var args []any
	for _, values := range sets {
		if len(values) == 0 {
			continue
		}
		part, a, err := r.match(ctx, values)
		if err != nil {
			return "", nil, err
		}
		parts = append(parts, part)
		args = append(args, a...)
	}
	if len(parts) == 0 {
		return "TRUE", nil, nil
	}
	return strings.Join(parts, " AND "), args, nil
}

// value renders one value as an expression plus its arguments. A reference is
// looked up now, not turned into a subselect: a subselect that finds nothing
// yields NULL, and the statement around it would happily write that NULL or
// match a row whose column is NULL.
func (r *runner) value(ctx context.Context, v fixturechange.Value) (string, []any, error) {
	switch {
	case v.IsNull:
		return "NULL", nil, nil
	case v.Ref == nil:
		return "?", []any{v.Lit}, nil
	}
	id, err := r.resolve(ctx, *v.Ref)
	if err != nil {
		return "", nil, err
	}
	return "?", []any{id}, nil
}

func (r *runner) resolve(ctx context.Context, ref fixturechange.Ref) (string, error) {
	cacheKey := ref.Model + "\x00" + ref.Key
	if id, ok := r.refs[cacheKey]; ok {
		return id, nil
	}
	t := r.set.Tables[ref.Model]
	table, err := quoteIdent(t.Name)
	if err != nil {
		return "", err
	}
	idCol, err := quoteIdent(t.ID)
	if err != nil {
		return "", err
	}
	keyCol, err := quoteIdent(t.Key)
	if err != nil {
		return "", err
	}
	rows, err := r.tx.QueryContext(ctx,
		fmt.Sprintf("SELECT %s FROM %s WHERE %s = ? LIMIT 2", idCol, table, keyCol), ref.Key)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("%s %q: no row in %s with %s = %q", ref.Model, ref.Key, t.Name, t.Key, ref.Key)
	case 1:
		r.refs[cacheKey] = ids[0]
		return ids[0], nil
	default:
		return "", fmt.Errorf("%s %q: more than one row in %s with %s = %q, cannot tell them apart",
			ref.Model, ref.Key, t.Name, t.Key, ref.Key)
	}
}

// syncSequences moves the sequence of every table that got an explicit id past
// the highest id in it. Without this the next ordinary insert reuses an id
// that is already taken.
func (r *runner) syncSequences(ctx context.Context) error {
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
		idCol, err := quoteIdent(t.ID)
		if err != nil {
			return err
		}
		query := fmt.Sprintf(
			`SELECT setval(s.seq, GREATEST(s.top, 1)) FROM (SELECT pg_get_serial_sequence(?, ?) AS seq, `+
				`(SELECT COALESCE(MAX(%s), 0) FROM %s) AS top) s WHERE s.seq IS NOT NULL`, idCol, table)
		if _, err := r.tx.ExecContext(ctx, query, t.Name, t.ID); err != nil {
			return fmt.Errorf("move the sequence of %s past the ids written: %w", t.Name, err)
		}
	}
	return nil
}

func tableHasRows(ctx context.Context, tx bun.IDB, table string) (bool, error) {
	q, err := quoteIdent(table)
	if err != nil {
		return false, err
	}
	var found bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM "+q+")").Scan(&found); err != nil {
		return false, fmt.Errorf("look for rows in %s: %w", table, err)
	}
	return found, nil
}

func keyLabel(key fixturechange.Values) string {
	parts := make([]string, 0, len(key))
	for _, col := range sortedColumns(key) {
		parts = append(parts, col+"="+key[col].String())
	}
	return strings.Join(parts, ",")
}

func sortedColumns(v fixturechange.Values) []string {
	out := make([]string, 0, len(v))
	for k := range v {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedModels(t fixturechange.Tables) []string {
	out := make([]string, 0, len(t))
	for k := range t {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
