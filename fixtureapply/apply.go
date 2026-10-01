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
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
	"github.com/RELAXccc/bun-fixture-migrate/internal/pgerr"

	"github.com/uptrace/bun"
)

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
	// Only now: waiting for another replica's change set is not the wait
	// lock_timeout is about.
	if set.LockTimeout != "" {
		if restore, err = withLockTimeout(ctx, tx, set.LockTimeout, restore); err != nil {
			return err
		}
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
			if pgerr.State(err) == pgerr.LockNotAvailable && set.LockTimeout != "" {
				err = fmt.Errorf("another session held a lock on a row of %s for longer than the lock timeout "+
					"of %s, so nothing was changed; the change set runs again on the next deploy: %w",
					set.Tables[c.Model].Name, set.LockTimeout, err)
				out.Problem, out.Message = ProblemLockTimeout, err.Error()
			}
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

// withLockTimeout sets lock_timeout for the rest of the transaction, and
// returns a restore that also puts the caller's value back.
func withLockTimeout(ctx context.Context, tx bun.IDB, timeout string,
	restore func(context.Context) error) (func(context.Context) error, error) {

	var old string
	if err := tx.QueryRowContext(ctx, "SELECT current_setting('lock_timeout')").Scan(&old); err != nil {
		return nil, fmt.Errorf("read the session's lock_timeout: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "SELECT set_config('lock_timeout', ?, true)", timeout); err != nil {
		return nil, fmt.Errorf("set lock_timeout to %s: %w", timeout, err)
	}
	return func(ctx context.Context) error {
		if err := restore(ctx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "SELECT set_config('lock_timeout', ?, true)", old); err != nil {
			return fmt.Errorf("restore the session's lock_timeout: %w", err)
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
	// problemDuplicate is a natural key more than one row holds.
	problemDuplicate problem = "duplicate"
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
	case problemDuplicate:
		return ProblemDuplicateKey
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
	case problemDuplicate:
		if p.DuplicateKey == fixturechange.ModeWarn {
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
