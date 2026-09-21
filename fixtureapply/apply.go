// Package fixtureapply runs a generated change set against a seeded database.
// Generated migrations call Apply and Revert; nothing else here is meant for
// hand-written code.
//
// Every statement is guarded. A row is found by its natural key, never by its
// id, because ids drift between databases. An update or a delete additionally
// requires that the row still holds the values the base state had, so a change
// somebody made by hand is not overwritten and a second run is a no-op. An
// insert requires that no row with the same natural key exists yet. Every
// reference is resolved to a real id before the statement runs, so a missing or
// ambiguous target fails the migration instead of writing NULL.
//
// A guard that matches nothing is not success. bun's migrator marks a migration
// applied the moment the function returns nil, so a statement that quietly
// matched no row is a change that will never be attempted again: fix the
// database, deploy once more, and the migration is already recorded. Every zero
// row count is therefore diagnosed and, unless the change set's policy says
// otherwise, turned into an error that rolls the transaction back.
//
// Identifiers are never taken from user input at run time: table and column
// names come from the generated file and must be plain SQL identifiers, which
// Validate checks before any statement is built. Values are always bound
// parameters.
package fixtureapply

import (
	"context"
	"database/sql"
	"errors"
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
// Pass func(string, ...any) {} to silence it; warnings go here too, so silence
// it only if something else is watching the migration.
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
// rejects anything else. Generated files only ever contain names that came from
// the configuration, but this is the line between the file and the database and
// it is cheap to hold.
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
	if err := set.Policy.Validate(); err != nil {
		return err
	}
	// A model nobody points at needs no key column, so an empty one is only an
	// error where a reference would use it.
	referenced := map[string]bool{}
	guardsID := map[string]bool{}
	for _, c := range set.Changes {
		if c.ID != "" {
			guardsID[c.Model] = true
		}
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
		if t.Serial || referenced[model] || guardsID[model] {
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
			if c.ID != "" {
				return fmt.Errorf("change %d (%s): insert with an id guard", i, c.Model)
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
			// A delete guards on the whole row it is removing. Without that it
			// would take the natural key's word for it and remove a row
			// somebody had since edited into something else.
			if len(c.Old) == 0 {
				return fmt.Errorf("change %d (%s): delete without the row it removes", i, c.Model)
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
		where := fmt.Sprintf("%s: %s %s %s", set.Name, c.Model, keyLabel(c.Key), c.Kind)
		res, err := r.exec(ctx, c)
		if err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if res.problem == "" {
			o.logf("%s: applied (%s)", where, rowCount(res.rows))
			continue
		}
		if res.problem == problemBenign {
			o.logf("%s: %s", where, res.message)
			continue
		}
		if modeFor(set.Policy, res.problem) == fixturechange.Error {
			return fmt.Errorf("%s: %s", where, res.message)
		}
		o.logf("%s: SKIPPED. %s", where, res.message)
	}
	return r.syncSequences(ctx)
}

// problem names the three materially different reasons a guarded statement
// matches nothing, plus the benign one. They are kept apart because an operator
// does something different about each: put the row back, look at who changed
// it, or find out which id the row really has.
type problem string

const (
	problemBenign  problem = "benign"
	problemMissing problem = "missing"
	problemChanged problem = "changed"
	problemIDDrift problem = "id drift"
)

// modeFor is what the change set's policy says about one problem. An unset
// policy field is the strict reading: a change that could not be made fails the
// migration rather than being recorded as done. A benign outcome, where the
// database already holds what the change wanted, is never an error.
func modeFor(p fixturechange.Policy, pr problem) fixturechange.Mode {
	switch pr {
	case problemBenign:
		return fixturechange.Warn
	case problemMissing:
		if p.MissingRow == fixturechange.Warn {
			return fixturechange.Warn
		}
	case problemChanged:
		if p.ChangedRow == fixturechange.Error {
			return fixturechange.Error
		}
		return fixturechange.Warn
	case problemIDDrift:
		if p.IDDrift == fixturechange.Warn || p.IDDrift == fixturechange.Ignore {
			return fixturechange.Warn
		}
	}
	return fixturechange.Error
}

type outcome struct {
	rows    int64
	problem problem
	message string
}

// invert turns a change into the change that undoes it.
func invert(c fixturechange.Change) fixturechange.Change {
	switch c.Kind {
	case fixturechange.Insert:
		return fixturechange.Change{Model: c.Model, Kind: fixturechange.Delete, Key: c.Key, Old: c.New}
	case fixturechange.Delete:
		return fixturechange.Change{Model: c.Model, Kind: fixturechange.Insert, Key: c.Key, New: c.Old}
	default:
		return fixturechange.Change{Model: c.Model, Kind: c.Kind, ID: c.ID, Key: c.Key, Old: c.New, New: c.Old}
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

func (r *runner) exec(ctx context.Context, c fixturechange.Change) (outcome, error) {
	t := r.set.Tables[c.Model]
	table, err := quoteIdent(t.Name)
	if err != nil {
		return outcome{}, err
	}
	switch c.Kind {
	case fixturechange.Insert:
		return r.insert(ctx, c, t, table)
	case fixturechange.Update:
		return r.update(ctx, c, t, table)
	default:
		return r.delete(ctx, c, t, table)
	}
}

func (r *runner) insert(ctx context.Context, c fixturechange.Change, t fixturechange.Table, table string) (outcome, error) {
	// An explicit id that another row already holds is checked before the
	// statement runs, so the failure names the row instead of arriving as a
	// primary-key violation from somewhere inside the driver.
	if id, ok := c.New[t.ID]; ok && id.Ref == nil && !id.IsNull && r.set.Policy.IDDrift != fixturechange.Ignore {
		taken, err := r.idTakenByAnotherRow(ctx, c, t, table, id.Lit)
		if err != nil {
			return outcome{}, err
		}
		if taken != "" {
			return outcome{problem: problemIDDrift, message: fmt.Sprintf(
				"%s %s = %s is already held by the row %s. Inserting this row would either fail on the primary key "+
					"or, without a unique index, leave two rows nothing can tell apart. Decide which row keeps the id",
				t.Name, t.ID, id.Lit, taken)}, nil
		}
	}

	var cols, exprs []string
	var args []any
	explicitID := false
	for _, col := range sortedColumns(c.New) {
		expr, a, err := r.value(ctx, c.New[col])
		if err != nil {
			return outcome{}, err
		}
		q, err := quoteIdent(col)
		if err != nil {
			return outcome{}, err
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
	// again: keying on the id as well is how a second copy appears, and then
	// every later lookup by name finds two.
	where, whereArgs, err := r.match(ctx, c.Key)
	if err != nil {
		return outcome{}, err
	}
	args = append(args, whereArgs...)
	query := fmt.Sprintf("INSERT INTO %s (%s) SELECT %s WHERE NOT EXISTS (SELECT 1 FROM %s WHERE %s)",
		table, strings.Join(cols, ", "), strings.Join(exprs, ", "), table, where)
	n, err := r.run(ctx, query, args)
	if err != nil {
		return outcome{}, err
	}
	if n > 0 {
		if explicitID && t.Serial {
			r.resync[c.Model] = true
		}
		return outcome{rows: n}, nil
	}
	return r.diagnoseInsert(ctx, c, t, table)
}

func (r *runner) update(ctx context.Context, c fixturechange.Change, t fixturechange.Table, table string) (outcome, error) {
	var sets []string
	var args []any
	for _, col := range sortedColumns(c.New) {
		expr, a, err := r.value(ctx, c.New[col])
		if err != nil {
			return outcome{}, err
		}
		q, err := quoteIdent(col)
		if err != nil {
			return outcome{}, err
		}
		sets = append(sets, q+" = "+expr)
		args = append(args, a...)
	}
	where, whereArgs, err := r.guard(ctx, c, t)
	if err != nil {
		return outcome{}, err
	}
	args = append(args, whereArgs...)
	n, err := r.run(ctx, fmt.Sprintf("UPDATE %s SET %s WHERE %s", table, strings.Join(sets, ", "), where), args)
	if err != nil {
		return outcome{}, err
	}
	if n > 0 {
		return outcome{rows: n}, nil
	}
	return r.diagnose(ctx, c, t, table, c.New)
}

func (r *runner) delete(ctx context.Context, c fixturechange.Change, t fixturechange.Table, table string) (outcome, error) {
	where, args, err := r.guard(ctx, c, t)
	if err != nil {
		return outcome{}, err
	}
	n, err := r.run(ctx, fmt.Sprintf("DELETE FROM %s WHERE %s", table, where), args)
	if err != nil {
		return outcome{}, err
	}
	if n > 0 {
		return outcome{rows: n}, nil
	}
	return r.diagnose(ctx, c, t, table, nil)
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

	byKey, err := r.count(ctx, table, c.Key)
	if err != nil {
		return outcome{}, err
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
		already, err := r.count(ctx, table, c.Key, wanted)
		if err != nil {
			return outcome{}, err
		}
		if already > 0 {
			return outcome{problem: problemBenign, message: "the row already holds these values, nothing to do"}, nil
		}
	}
	if c.ID != "" {
		withID, err := r.count(ctx, table, c.Key, fixturechange.Values{t.ID: fixturechange.Lit(c.ID)})
		if err != nil {
			return outcome{}, err
		}
		if withID == 0 {
			ids, err := r.idsFor(ctx, table, t, c.Key)
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

	same, err := r.count(ctx, table, c.Key, withoutColumn(c.New, t.ID))
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

	keyWhere, keyArgs, err := r.match(ctx, c.Key)
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

func (r *runner) idsFor(ctx context.Context, table string, t fixturechange.Table,
	key fixturechange.Values) ([]string, error) {

	where, args, err := r.match(ctx, key)
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

func (r *runner) count(ctx context.Context, table string, sets ...fixturechange.Values) (int64, error) {
	where, args, err := r.matchAll(ctx, sets...)
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

func (r *runner) run(ctx context.Context, query string, args []any) (int64, error) {
	res, err := r.tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// guard is the WHERE clause of an update or a delete: the natural key, the old
// values, and the id when the change carries one.
func (r *runner) guard(ctx context.Context, c fixturechange.Change, t fixturechange.Table) (string, []any, error) {
	sets := []fixturechange.Values{c.Key, c.Old}
	if c.ID != "" {
		sets = append(sets, fixturechange.Values{t.ID: fixturechange.Lit(c.ID)})
	}
	return r.matchAll(ctx, sets...)
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
// yields NULL, and the statement around it would happily write that NULL as a
// foreign key, or match a row whose column is NULL, and report a row affected.
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
// the highest id in it. Without this the next ordinary insert reuses an id that
// is already taken.
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

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

func rowCount(n int64) string {
	if n == 1 {
		return "1 row"
	}
	return fmt.Sprintf("%d rows", n)
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
