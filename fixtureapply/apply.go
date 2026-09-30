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
// A guard that matches nothing is not success. bun's migrator records a
// migration as applied once its function returns nil, so a statement that
// quietly matched no row is a change that will never be attempted again: fix
// the database, deploy once more, and the migration is already recorded. Every
// zero row count is therefore diagnosed and, unless the change set's policy
// says otherwise, turned into an error that rolls the transaction back.
//
// Failing is only half of it. Unless the migrator was built
// WithMarkAppliedOnSuccess(true), bun records the migration before running it
// and keeps the record when it fails, so a failed change set would be recorded
// as done all the same. Apply takes that record back; see Apply.
//
// Identifiers are never taken from user input at run time: table and column
// names come from the generated file and must be plain SQL identifiers, which
// Validate checks before any statement is built. Values never become part of
// the SQL text this package writes: they are passed as arguments, which bun
// quotes and escapes with the dialect's rules.
package fixtureapply

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
)

// Option configures Apply and Revert.
type Option func(*options)

type options struct {
	logf      func(format string, args ...any)
	report    func(Outcome)
	migration string
	dryRun    bool
}

// WithLogger replaces log.Printf as the destination of the per-row report.
// Pass func(string, ...any) {} to silence it; warnings go here too, so silence
// it only if something else is watching the migration.
func WithLogger(logf func(format string, args ...any)) Option {
	return func(o *options) { o.logf = logf }
}

// WithReport hands every change's outcome to fn as it happens, in the order
// the changes run. It is what a dry run reads; the log lines are for people.
func WithReport(fn func(Outcome)) Option {
	return func(o *options) { o.report = fn }
}

// WithMigrationName names the bun migration Apply is running as: the
// timestamp at the front of the migration's file name, which is what bun
// stores in its migrations table. Apply reads it off the call stack the way
// bun's own Register does -- the nearest caller in a file named like a
// migration -- so a generated migration never needs this, and neither does a
// helper in an ordinary file. A helper that lives in another migration's file
// does: the stack would name that migration.
func WithMigrationName(name string) Option {
	return func(o *options) { o.migration = name }
}

// WithDryRun is for a caller that rolls the transaction back afterwards, the
// way the plan command does. Everything runs as it would, except the one step
// a rollback cannot undo: moving a sequence past the explicit ids written
// (setval is not transactional). That step is reported instead, with
// StatusSequence.
func WithDryRun() Option {
	return func(o *options) { o.dryRun = true }
}

func newOptions(opts []Option) options {
	o := options{logf: log.Printf, report: func(Outcome) {}}
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// Status is what became of one change.
type Status string

const (
	// StatusApplied is a change that was made.
	StatusApplied Status = "applied"
	// StatusUnchanged is a change the database already held, which is what a
	// second run of the same migration finds.
	StatusUnchanged Status = "unchanged"
	// StatusSkipped is a change that could not be made and that the policy
	// allowed to be passed over with a warning.
	StatusSkipped Status = "skipped"
	// StatusFailed is a change that could not be made and failed the whole
	// set, which rolls it back.
	StatusFailed Status = "failed"
	// StatusUnseeded is the whole set passed over because the seed guard
	// table is empty. Index is -1.
	StatusUnseeded Status = "unseeded"
	// StatusSequence is a sequence moved past the explicit ids the set
	// wrote, or under WithDryRun one that would be. Index is -1 and Model
	// names the table's model.
	StatusSequence Status = "sequence"
)

// Problem names why a change could not be made.
type Problem string

const (
	// ProblemMissingRow is an update or a delete whose row is not there.
	ProblemMissingRow Problem = "missing row"
	// ProblemChangedRow is a row that no longer holds what the change was
	// generated against: somebody edited it in this database.
	ProblemChangedRow Problem = "changed row"
	// ProblemIDDrift is a row under a different id from the one the change
	// was generated for, or an id another row holds.
	ProblemIDDrift Problem = "id drift"
	// ProblemReferenced is a delete of a row that rows of another table, or
	// of the same one, still point at.
	ProblemReferenced Problem = "referenced"
	// ProblemError is a statement that failed outright.
	ProblemError Problem = "error"
)

// Outcome is what happened to one change of a set.
type Outcome struct {
	// Set is the change set's Name.
	Set string `json:"set"`
	// Index is the change's position in Set.Changes, -1 for an outcome about
	// the whole set.
	Index int    `json:"index"`
	Model string `json:"model,omitempty"`
	// Kind is what the change did; for Revert, what the inverted change did.
	Kind fixturechange.Kind `json:"kind,omitempty"`
	// Key is the natural key, as "col=value,col=value".
	Key    string `json:"key,omitempty"`
	Status Status `json:"status"`
	// Rows is the row count of an applied change.
	Rows int64 `json:"rows,omitempty"`
	// Problem is set when the change could not be made.
	Problem Problem `json:"problem,omitempty"`
	// Message is the explanation a person reads.
	Message string `json:"message,omitempty"`
}

// Apply runs a change set in one transaction.
//
// When it fails it returns an error, which rolls the transaction back and
// fails the migration. Under bun's migrator that is not enough on its own:
// unless the migrator was built WithMarkAppliedOnSuccess(true), bun inserts
// the migration's record before calling it and leaves the record there when
// it fails, and a recorded migration never runs again. Fix the database,
// deploy once more, and nothing happens.
//
// So a failing Apply that finds itself running under bun's migrator deletes
// that record: the newest row of the migrations table, if it carries this
// migration's name and was written within the hour. Nothing older and nothing
// else. When the migrator records on success there is normally no such row
// and nothing is deleted; the exceptions are two migrations sharing one name,
// which bun cannot run correctly anyway and the status command reports, and
// RunMigration re-running the newest migration within the hour of its first
// run, after which the next migrate runs it again and finds its changes made.
// The error says whether a record was deleted. The name is the one bun derived
// from the migration's file name; see WithMigrationName.
func Apply(ctx context.Context, db bun.IDB, set fixturechange.Set, opts ...Option) error {
	o := newOptions(opts)
	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return run(ctx, tx, set, false, o)
	})
	if err == nil {
		return nil
	}
	if o.migration == "" {
		o.migration = migrationFromStack()
	}
	return unrecord(ctx, db, set, o, err)
}

// advisoryLock is the key of the transaction-scoped advisory lock every change
// set takes: "bfm" and a version, in the high bytes, well away from the small
// numbers applications pick for their own.
const advisoryLock int64 = 0x62666d0001

// bunMigrationFile is bun's own pattern for a migration file name
// (migrate/migrations.go, fnameRE): the digits are the name bun records.
var bunMigrationFile = regexp.MustCompile(`^(\d{1,14})_([0-9a-z_\-]+)\.`)

// migrationFromStack finds the migration file Apply was called from, the way
// bun's Register finds the file it was called from.
func migrationFromStack() string {
	var pcs [64]uintptr
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	var list []runtime.Frame
	for {
		f, more := frames.Next()
		list = append(list, f)
		if !more {
			return migrationFromFrames(list)
		}
	}
}

// migrationFromFrames is the first frame outside this package whose file is
// named like a migration, innermost first. It answers only when bun's migrator
// is further out, so Apply called from a test or a tool never goes looking for
// a record to take back, whatever the calling file is named.
func migrationFromFrames(frames []runtime.Frame) string {
	name := ""
	for _, f := range frames {
		if strings.Contains(f.Function, "/bun/migrate.") {
			return name
		}
		if name == "" && !strings.Contains(f.Function, "/fixtureapply.") {
			if m := bunMigrationFile.FindStringSubmatch(filepath.Base(f.File)); m != nil {
				name = m[1]
			}
		}
	}
	return ""
}

// unrecord deletes the record bun's migrator made of this migration before
// running it, and returns the failure with a note saying what it did.
func unrecord(ctx context.Context, db bun.IDB, set fixturechange.Set, o options, failure error) error {
	// Only the migrator's own connection pool. Inside somebody else's
	// transaction a failing statement would poison it, and the migrator never
	// hands a migration anything but the *bun.DB.
	bdb, ok := db.(*bun.DB)
	if !ok || o.migration == "" {
		return failure
	}
	table := set.MigrationsTable
	if table == "" {
		table = fixturechange.DefaultMigrationsTable
	}
	// bun puts the table name into its SQL as written, unquoted, so
	// PostgreSQL folds it to lower case; this has to find the same table.
	// Validate has made sure it is a plain identifier.
	if _, err := quoteIdent(table); err != nil {
		return failure
	}
	quoted := table
	// The migration may have failed because the context ended; the record
	// still has to go, or the next deploy skips this migration.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	var exists bool
	if err := bdb.QueryRowContext(ctx, "SELECT to_regclass(?) IS NOT NULL", quoted).Scan(&exists); err != nil {
		return fmt.Errorf("%w\n\nwhether bun recorded %s as applied could not be checked (%v): if %s holds a row "+
			"named %s, delete it, or the migration will not run again", failure, o.migration, err, table, o.migration)
	}
	if !exists {
		return failure
	}
	res, err := bdb.ExecContext(ctx, fmt.Sprintf(
		"DELETE FROM %s WHERE name = ? AND id = (SELECT max(id) FROM %s) "+
			"AND migrated_at > clock_timestamp() - interval '1 hour'", quoted, quoted), o.migration)
	if err != nil {
		return fmt.Errorf("%w\n\nbun may have recorded %s as applied before running it, and the record could not "+
			"be removed (%v): delete the row named %s from %s, or the migration will not run again",
			failure, o.migration, err, o.migration, table)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return failure
	}
	o.logf("%s: bun had recorded migration %s as applied before running it; the record was removed, "+
		"so it runs again once this is fixed", set.Name, o.migration)
	return fmt.Errorf("%w\n\nbun had recorded migration %s as applied before running it (the migrator was not "+
		"built WithMarkAppliedOnSuccess(true)); that record was removed, so the migration runs again once "+
		"this is fixed", failure, o.migration)
}

// Revert undoes a change set: the list backwards, every change inverted. An
// insert becomes a delete guarded by the values it wrote, an update swaps old
// and new, a delete becomes an insert of the row it removed.
//
// It takes no record back. When it fails under a migrator that unrecords
// before running (bun's default), the migration is left looking unapplied
// while its changes are still in the database; that is harmless, because the
// next migrate runs Apply again and every change it finds already made is
// "unchanged".
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
	if set.MigrationsTable != "" {
		if _, err := quoteIdent(set.MigrationsTable); err != nil {
			return fmt.Errorf("migrations table %w", err)
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
	restore, err := session(ctx, tx)
	if err != nil {
		return err
	}
	// One change set at a time, whoever runs it: two replicas of an
	// application migrating at the same start-up would otherwise both find a
	// row missing and both insert it. The lock is the transaction's and ends
	// with it; the second one then finds every change made.
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(?)", advisoryLock); err != nil {
		return fmt.Errorf("wait for another change set to finish: %w", err)
	}
	if set.SeedGuardTable != "" {
		seeded, err := tableHasRows(ctx, tx, set.SeedGuardTable)
		if err != nil {
			return err
		}
		if !seeded {
			msg := set.SeedGuardTable + " is empty, nothing to do (the fixture loader seeds this database)"
			o.logf("%s: %s", set.Name, msg)
			o.report(Outcome{Set: set.Name, Index: -1, Status: StatusUnseeded, Message: msg})
			return restore(ctx)
		}
	}

	r := &runner{tx: tx, set: set, refs: map[string]string{}, resync: map[string]bool{},
		types: map[string]map[string]colType{}}
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
		out := Outcome{Set: set.Name, Index: i, Model: c.Model, Kind: c.Kind, Key: keyLabel(c.Key)}
		res, err := r.exec(ctx, c)
		if err != nil {
			out.Status, out.Problem, out.Message = StatusFailed, ProblemError, err.Error()
			o.report(out)
			return fmt.Errorf("%s: %w", where, err)
		}
		out.Rows, out.Message = res.rows, res.message
		switch {
		case res.problem == "":
			out.Status = StatusApplied
			o.logf("%s: applied (%s)", where, rowCount(res.rows))
		case res.problem == problemBenign:
			out.Status = StatusUnchanged
			o.logf("%s: %s", where, res.message)
		case modeFor(set.Policy, res.problem) == fixturechange.ModeError:
			out.Status, out.Problem = StatusFailed, res.problem.exported()
			o.report(out)
			return fmt.Errorf("%s: %s", where, res.message)
		default:
			out.Status, out.Problem = StatusSkipped, res.problem.exported()
			o.logf("%s: SKIPPED. %s", where, res.message)
		}
		o.report(out)
	}
	if err := r.syncSequences(ctx, o); err != nil {
		return err
	}
	return restore(ctx)
}

// session makes a literal mean in the migration what it meant to dbfixture.
// yaml.v3 reads a timestamp without a zone as UTC, and bun writes the
// time.Time it becomes with an offset, so "2026-01-01 10:00:00" in a fixture
// file is 10:00 UTC in the seeded database whatever the server's TimeZone. A
// migration binding the same text has to read it the same way, and a date such
// as 2026-01-02 has to be year-month-day. The settings are local to the
// transaction; restore puts back what a caller's own transaction had, and a
// rollback does that by itself.
func session(ctx context.Context, tx bun.IDB) (func(context.Context) error, error) {
	var tz, ds string
	if err := tx.QueryRowContext(ctx,
		"SELECT current_setting('TimeZone'), current_setting('DateStyle')").Scan(&tz, &ds); err != nil {
		return nil, fmt.Errorf("read the session's settings: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"SELECT set_config('TimeZone', 'UTC', true), set_config('DateStyle', 'ISO, YMD', true)"); err != nil {
		return nil, fmt.Errorf("fix the session's settings: %w", err)
	}
	return func(ctx context.Context) error {
		if _, err := tx.ExecContext(ctx,
			"SELECT set_config('TimeZone', ?, true), set_config('DateStyle', ?, true)", tz, ds); err != nil {
			return fmt.Errorf("restore the session's settings: %w", err)
		}
		return nil
	}, nil
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
	// problemReferenced is a delete of a row other rows point at. It is
	// always an error: the delete would cascade into, detach, or be refused
	// by rows the change set knows nothing about.
	problemReferenced problem = "referenced"
)

func (p problem) exported() Problem {
	switch p {
	case problemMissing:
		return ProblemMissingRow
	case problemChanged:
		return ProblemChangedRow
	case problemIDDrift:
		return ProblemIDDrift
	case problemReferenced:
		return ProblemReferenced
	}
	return ""
}

// modeFor is what the change set's policy says about one problem. An unset
// policy field is the strict reading: a change that could not be made fails the
// migration rather than being recorded as done. A benign outcome, where the
// database already holds what the change wanted, is never an error.
func modeFor(p fixturechange.Policy, pr problem) fixturechange.Mode {
	switch pr {
	case problemBenign:
		return fixturechange.ModeWarn
	case problemMissing:
		if p.MissingRow == fixturechange.ModeWarn {
			return fixturechange.ModeWarn
		}
	case problemChanged:
		if p.ChangedRow == fixturechange.ModeError {
			return fixturechange.ModeError
		}
		return fixturechange.ModeWarn
	case problemIDDrift:
		if p.IDDrift == fixturechange.ModeWarn || p.IDDrift == fixturechange.ModeIgnore {
			return fixturechange.ModeWarn
		}
	}
	return fixturechange.ModeError
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
	// types holds, per model, the types of its table's columns, read once.
	types map[string]map[string]colType
}

// colType is what a comparison needs to know about a column's type.
type colType struct {
	// cast is the type a value is cast to before it is compared: the
	// column's own, without a length for the character types, since an
	// explicit cast to varchar(3) truncates without a word.
	cast string
	// base is the name of the type, or of a domain's base type.
	base string
	// array is true for an array type.
	array bool
	// equality is true when the type has an = operator of its own. json,
	// xml, point and the other geometric types do not, and neither, in
	// pg_operator's terms, do arrays and enums, which compare through anyarray
	// and anyenum.
	equality bool
}

// colTypes reads the column types of a model's table, once per run.
func (r *runner) colTypes(ctx context.Context, model string) (map[string]colType, error) {
	if types, ok := r.types[model]; ok {
		return types, nil
	}
	table, err := quoteIdent(r.set.Tables[model].Name)
	if err != nil {
		return nil, err
	}
	rows, err := r.tx.QueryContext(ctx, `
SELECT a.attname, format_type(a.atttypid, a.atttypmod), format_type(a.atttypid, NULL), bt.typname,
       bt.typcategory = 'A',
       EXISTS (SELECT 1 FROM pg_operator o WHERE o.oprname = '=' AND o.oprleft = bt.oid AND o.oprright = bt.oid)
FROM pg_attribute a
JOIN pg_type t ON t.oid = a.atttypid
JOIN pg_type bt ON bt.oid = CASE WHEN t.typbasetype <> 0 THEN t.typbasetype ELSE t.oid END
WHERE a.attrelid = ?::regclass AND a.attnum > 0 AND NOT a.attisdropped`, table)
	if err != nil {
		return nil, fmt.Errorf("read the column types of %s: %w", r.set.Tables[model].Name, err)
	}
	defer rows.Close()
	types := map[string]colType{}
	for rows.Next() {
		var name, full, bare string
		var ct colType
		if err := rows.Scan(&name, &full, &bare, &ct.base, &ct.array, &ct.equality); err != nil {
			return nil, err
		}
		ct.cast = full
		switch ct.base {
		case "varchar", "bpchar", "_varchar", "_bpchar":
			ct.cast = bare
		}
		types[name] = ct
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	r.types[model] = types
	return types, nil
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
	if id, ok := c.New[t.ID]; ok && id.Ref == nil && !id.IsNull && r.set.Policy.IDDrift != fixturechange.ModeIgnore {
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
		expr, a, err := r.value(ctx, c.Model, col, c.New[col])
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
	where, whereArgs, err := r.match(ctx, c.Model, c.Key)
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
		expr, a, err := r.value(ctx, c.Model, col, c.New[col])
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
	if out, err := r.referenced(ctx, c, t, table, where, args); err != nil || out.problem != "" {
		return out, err
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

// incoming is one foreign key pointing at a table.
type incoming struct {
	name, child, action   string
	childCols, parentCols []string
}

// referenced looks, before a delete, for rows that point at the row being
// deleted, through every foreign key that points at its table.
//
// A foreign key declared ON DELETE CASCADE deletes them with it, SET NULL or
// SET DEFAULT detaches them, and RESTRICT or NO ACTION refuses the delete.
// The first two happen without a word to anybody, to rows that are not
// master data -- a subscription on the plan, an order of the product -- so
// they fail the change unless the table allows it (Table.Cascade). The last
// fails anyway; here it fails with the rows named instead of with a
// constraint's name.
func (r *runner) referenced(ctx context.Context, c fixturechange.Change, t fixturechange.Table, table, where string,
	args []any) (outcome, error) {

	fks, err := r.incoming(ctx, table)
	if err != nil {
		return outcome{}, err
	}
	for _, fk := range fks {
		cascades := fk.action == "c" || fk.action == "n" || fk.action == "d"
		if cascades && t.Cascade {
			continue
		}
		var n int64
		query := fmt.Sprintf("SELECT count(*) FROM %s WHERE (%s) IN (SELECT %s FROM %s WHERE %s)",
			fk.child, strings.Join(fk.childCols, ", "), strings.Join(fk.parentCols, ", "), table, where)
		if err := r.tx.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			return outcome{}, fmt.Errorf("count the rows pointing at %s %s: %w", t.Name, keyLabel(c.Key), err)
		}
		if n == 0 {
			continue
		}
		what := map[string]string{"c": "deletes them with it", "n": "sets their reference to NULL",
			"d": "sets their reference to its default"}[fk.action]
		msg := fmt.Sprintf("%s of %s point at %s %s through %s, which %s", rowCount(n), fk.child, t.Name,
			keyLabel(c.Key), fk.name, what)
		if cascades {
			msg += ". They are not master data and this change set does not know them: repoint or remove " +
				"them first, or set deletes: cascade on the model if deleting them with it is what you want"
		} else {
			msg = fmt.Sprintf("%s of %s point at %s %s through %s, which refuses the delete: repoint or "+
				"remove them first", rowCount(n), fk.child, t.Name, keyLabel(c.Key), fk.name)
		}
		return outcome{problem: problemReferenced, message: msg}, nil
	}
	return outcome{}, nil
}

// incoming reads the foreign keys pointing at a table, with every name
// already quoted by PostgreSQL.
func (r *runner) incoming(ctx context.Context, table string) ([]incoming, error) {
	rows, err := r.tx.QueryContext(ctx, `
SELECT con.conname::text, con.conrelid::regclass::text, con.confdeltype::text,
       array_to_json(ARRAY(SELECT quote_ident(a.attname) FROM unnest(con.conkey) WITH ORDINALITY k(n, o)
             JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.n ORDER BY k.o))::text,
       array_to_json(ARRAY(SELECT quote_ident(a.attname) FROM unnest(con.confkey) WITH ORDINALITY k(n, o)
             JOIN pg_attribute a ON a.attrelid = con.confrelid AND a.attnum = k.n ORDER BY k.o))::text
FROM pg_constraint con
WHERE con.contype = 'f' AND con.confrelid = ?::regclass
ORDER BY con.conname`, table)
	if err != nil {
		return nil, fmt.Errorf("read the foreign keys pointing at %s: %w", table, err)
	}
	defer rows.Close()
	var out []incoming
	for rows.Next() {
		var fk incoming
		var child, parent string
		if err := rows.Scan(&fk.name, &fk.child, &fk.action, &child, &parent); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(child), &fk.childCols); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(parent), &fk.parentCols); err != nil {
			return nil, err
		}
		fk.name = quoteLiteralName(fk.name)
		out = append(out, fk)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, rows.Close()
}

// quoteLiteralName puts a constraint's name in quotes for a message.
func quoteLiteralName(name string) string { return "constraint " + strconv.Quote(name) }

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
	return outcome{problem: problemChanged, message: fmt.Sprintf(
		"%s %s no longer holds the values this change was generated against, so somebody changed it in this "+
			"database. It was left alone. Compare it with the fixture file and decide which one is right",
		t.Name, keyLabel(c.Key))}, nil
}

// diagnoseInsert explains an insert whose NOT EXISTS found a row.
func (r *runner) diagnoseInsert(ctx context.Context, c fixturechange.Change, t fixturechange.Table,
	table string) (outcome, error) {

	same, err := r.count(ctx, c.Model, table, c.Key, withoutColumn(c.New, t.ID))
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

	keyWhere, keyArgs, err := r.match(ctx, c.Model, c.Key)
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

func (r *runner) idsFor(ctx context.Context, model, table string, t fixturechange.Table,
	key fixturechange.Values) ([]string, error) {

	where, args, err := r.match(ctx, model, key)
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

func (r *runner) count(ctx context.Context, model, table string, sets ...fixturechange.Values) (int64, error) {
	where, args, err := r.matchAll(ctx, model, sets...)
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
	return r.matchAll(ctx, c.Model, sets...)
}

// match renders "col IS NOT DISTINCT FROM <value>" for every column, joined by
// AND. IS NOT DISTINCT FROM rather than = so a NULL compares like any other
// value.
func (r *runner) match(ctx context.Context, model string, values fixturechange.Values) (string, []any, error) {
	var parts []string
	var args []any
	for _, col := range sortedColumns(values) {
		expr, a, err := r.value(ctx, model, col, values[col])
		if err != nil {
			return "", nil, err
		}
		part, err := r.compare(ctx, model, col, expr)
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

func (r *runner) matchAll(ctx context.Context, model string, sets ...fixturechange.Values) (string, []any, error) {
	var parts []string
	var args []any
	for _, values := range sets {
		if len(values) == 0 {
			continue
		}
		part, a, err := r.match(ctx, model, values)
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
//
// A literal is text, which PostgreSQL reads as the column's type. The one
// exception is an array column given a JSON array, which is how a YAML
// sequence arrives: its elements are unpacked and cast to the array's type.
func (r *runner) value(ctx context.Context, model, col string, v fixturechange.Value) (string, []any, error) {
	switch {
	case v.IsNull:
		return "NULL", nil, nil
	case v.Ref == nil:
		if strings.HasPrefix(strings.TrimSpace(v.Lit), "[") {
			types, err := r.colTypes(ctx, model)
			if err != nil {
				return "", nil, err
			}
			if ct, ok := types[col]; ok && ct.array {
				return "ARRAY(SELECT jsonb_array_elements_text(?::jsonb))::" + ct.cast, []any{v.Lit}, nil
			}
		}
		return "?", []any{v.Lit}, nil
	}
	id, err := r.resolve(ctx, *v.Ref)
	if err != nil {
		return "", nil, err
	}
	return "?", []any{id}, nil
}

// compare is "col IS NOT DISTINCT FROM value", typed. The value is cast to
// the column's type first, so it compares as what the column would hold:
// 1.005 in a numeric(10,2) is 1.01, an upper-case uuid is the lower-case one.
// A json column compares through jsonb, and a type without an equality of its
// own through the text of both sides; for json, xml or point the bare
// comparison is an error, and it would fail the migration at deploy time.
func (r *runner) compare(ctx context.Context, model, col, value string) (string, error) {
	q, err := quoteIdent(col)
	if err != nil {
		return "", err
	}
	types, err := r.colTypes(ctx, model)
	if err != nil {
		return "", err
	}
	ct, ok := types[col]
	switch {
	case !ok:
		return q + " IS NOT DISTINCT FROM " + value, nil
	case ct.base == "json":
		return q + "::jsonb IS NOT DISTINCT FROM (" + value + ")::jsonb", nil
	case ct.equality:
		return q + " IS NOT DISTINCT FROM (" + value + ")::" + ct.cast, nil
	}
	return q + "::text IS NOT DISTINCT FROM ((" + value + ")::" + ct.cast + ")::text", nil
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
		idCol, err := quoteIdent(t.ID)
		if err != nil {
			return err
		}
		if o.dryRun {
			msg := fmt.Sprintf("explicit ids were written into %s; the migration moves its sequence past them "+
				"if it is behind, which a dry run leaves alone", t.Name)
			o.logf("%s: %s", r.set.Name, msg)
			o.report(Outcome{Set: r.set.Name, Index: -1, Model: m, Status: StatusSequence, Message: msg})
			continue
		}
		// The quoted name goes to pg_get_serial_sequence, which parses its
		// first argument as SQL: unquoted, a mixed-case table would not be
		// found and its sequence silently left behind.
		query := fmt.Sprintf(`SELECT setval(s.seq, s.top) FROM (`+
			`SELECT pg_get_serial_sequence(?, ?)::regclass AS seq, (SELECT COALESCE(MAX(%s), 0) FROM %s) AS top`+
			`) s WHERE s.seq IS NOT NULL AND s.top > COALESCE(pg_sequence_last_value(s.seq), `+
			`(SELECT seqstart - 1 FROM pg_sequence WHERE seqrelid = s.seq))`, idCol, table)
		rows, err := r.tx.QueryContext(ctx, query, table, t.ID)
		if err != nil {
			return fmt.Errorf("move the sequence of %s past the ids written: %w", t.Name, err)
		}
		moved := false
		for rows.Next() {
			moved = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("move the sequence of %s past the ids written: %w", t.Name, err)
		}
		if err := rows.Close(); err != nil {
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
