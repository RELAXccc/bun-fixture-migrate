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
	"log/slog"
	"sort"
	"strings"
	"time"

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
// So Apply, running under bun's migrator, first looks for that record: the
// rows of the migrations table that carry this migration's name, were written
// in the last minute, and are newer than every other migration's record, which
// is what bun's default mode has just done -- one row, or one per replica that
// started the migration at the same moment without bun's Lock. If the change
// set then fails, those rows and no others are deleted. A migrator that records
// on success has made no such row, and a record another process writes while
// this one runs is not among the rows found before it ran, so neither is
// touched. A replica's record deleted along with this one's, of a run that
// succeeds, leaves the migration pending with its changes made; the next
// migrate runs it again and finds every change made. So does RunMigration
// re-running the newest migration, whose record bun updates in place. The
// error says whether a record was deleted. The name is the one bun derived
// from the migration's file name; see WithMigrationName.
//
// Two replicas starting together are best kept apart by bun's Lock, or by
// building the migrator WithUpsert(true), which keeps one record per name;
// either way only one record of a run is ever there to take back.
//
// A set with an AuditTable records, last in the transaction, what became of
// each of its changes; see Set.AuditTable and Revert.
func Apply(ctx context.Context, db bun.IDB, set fixturechange.Set, opts ...Option) error {
	o := newOptions(opts)
	if o.migration == "" {
		o.migration = migrationFromStack()
	}
	o.nested = inCallersTx(db)
	rec := findRecord(ctx, db, set, o)
	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return run(ctx, tx, set, false, o)
	})
	if err == nil {
		return nil
	}
	return unrecord(ctx, db, set, o, rec, err)
}

// advisoryLock is the key of the transaction-scoped advisory lock every change
// set takes: "bfm" and a version, in the high bytes, well away from the small
// numbers applications pick for their own.
const advisoryLock int64 = 0x62666d0001

// Revert undoes a change set: the list backwards, every change inverted. An
// insert becomes a delete guarded by the values it wrote, an update swaps old
// and new, a delete becomes an insert of the row it removed.
//
// With an AuditTable, it reverts only the changes Apply made in this
// database: those an 'up' row of the set after its last 'down' row records as
// applied, which is the newest such row and, after a Revert that failed, the
// ones before it. A change Apply found made already (unchanged) or passed
// over (skipped) is left as it is, its outcome StatusUnchanged with a message
// saying why, and so is every change of a run that found the database
// unseeded. When the set's newest row is a 'down' row, the set is reverted
// here already -- apply -revert by hand, then bun's Rollback, or two replicas
// rolling back -- and nothing ran since: every change is left as it is,
// StatusUnchanged. Without the table, or without a row of the set, it says so
// in the log and reverts every change, as it does for a set without an
// AuditTable.
//
// That is: it assumes Apply made every change of the set on this database. A
// change Apply found already made -- the row already held the new values, or
// was already there -- is reverted all the same: the update writes the old
// value, which this database may never have held, and the insert's row is
// deleted. A change Apply skipped is inverted too: a delete it skipped because
// somebody had removed the row already puts the row back. Where a row does
// not hold what the migration writes, the change is not reverted and its
// outcome says so.
//
// It takes no record back. When it fails under a migrator that unrecords
// before running (bun's default), the migration is left looking unapplied
// while its changes are still in the database; that is harmless, because the
// next migrate runs Apply again and every change it finds already made is
// "unchanged".
func Revert(ctx context.Context, db bun.IDB, set fixturechange.Set, opts ...Option) error {
	o := newOptions(opts)
	o.nested = inCallersTx(db)
	return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return run(ctx, tx, set, true, o)
	})
}

// WaitForChangeSets takes, in tx, the advisory lock every change set runs
// under, waiting for one that runs now, in this process or another, to
// finish. It holds the lock until tx ends, so no change set runs in between.
// A program that reads the audit table and acts on what it says, as apply
// -revert -record does, takes it first, so no run comes between the two.
func WaitForChangeSets(ctx context.Context, tx bun.Tx) error {
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(?)", advisoryLock); err != nil {
		return fmt.Errorf("wait for another change set to finish: %w", err)
	}
	return nil
}

// inCallersTx says whether db is a transaction somebody else began, which a
// change set runs in a savepoint of.
func inCallersTx(db bun.IDB) bool {
	switch db.(type) {
	case bun.Tx, *bun.Tx:
		return true
	}
	return false
}

func run(ctx context.Context, tx bun.IDB, set fixturechange.Set, revert bool, o options) error {
	if err := Validate(set); err != nil {
		return err
	}
	// What became of every change, for the audit table.
	var outcomes []Outcome
	if set.AuditTable != "" {
		report := o.report
		o.report = func(out Outcome) {
			outcomes = append(outcomes, out)
			report(out)
		}
	}
	restore, lockTimeout, err := session(ctx, tx)
	if err != nil {
		return err
	}
	if err := rowSecurity(ctx, tx, set, revert); err != nil {
		return err
	}
	// Waiting for another replica's change set is not the wait lock_timeout
	// is about, whether the set's or one the session has from the DSN. Inside
	// a caller's transaction the caller's lock_timeout stays: the caller may
	// hold locks already, as plan does, and set it to bound how long it keeps
	// them while it waits.
	if !o.nested {
		if _, err := tx.ExecContext(ctx, "SELECT set_config('lock_timeout', '0', true)"); err != nil {
			return fmt.Errorf("set lock_timeout to 0: %w", err)
		}
	}
	// One change set at a time, whoever runs it: two replicas of an
	// application migrating at the same start-up would otherwise both find a
	// row missing and both insert it. The lock is the transaction's and ends
	// with it; the second one then finds every change made.
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(?)", advisoryLock); err != nil {
		return fmt.Errorf("wait for another change set to finish: %w", err)
	}
	if set.LockTimeout != "" {
		lockTimeout = set.LockTimeout
	}
	if set.LockTimeout != "" || !o.nested {
		if _, err := tx.ExecContext(ctx, "SELECT set_config('lock_timeout', ?, true)", lockTimeout); err != nil {
			return fmt.Errorf("set lock_timeout to %s: %w", lockTimeout, err)
		}
	}
	if restore, err = withDeferredConstraints(ctx, tx, set, restore); err != nil {
		return err
	}
	if set.SeedGuardTable != "" {
		seeded, err := tableHasRows(ctx, tx, set.SeedGuardTable)
		if err != nil {
			return privilege(err)
		}
		if !seeded {
			msg := set.SeedGuardTable + " is empty, nothing to do (the fixture loader seeds this database)"
			out := Outcome{Set: set.Name, Index: -1, Status: StatusUnseeded, Message: msg}
			o.log(ctx, slog.LevelInfo, "fixture change set not run, the database is not seeded", out,
				set.Name+": "+msg)
			o.report(out)
			return finish(ctx, tx, set, revert, restore, outcomes)
		}
	}
	// Revert undoes what the last Apply here did, which the audit table
	// says, read under the advisory lock so no other run comes in between.
	var base Applies
	var reverted *AuditRecord
	if revert && set.AuditTable != "" {
		if base, reverted, err = revertBase(ctx, tx, set, o); err != nil {
			return err
		}
	}

	r := &runner{tx: tx, set: set, revert: revert, dryRun: o.dryRun, refs: map[string]string{},
		resync: map[string]bool{}, advanced: map[string]bool{}, types: map[string]map[string]colType{},
		sequences: map[string]string{}, seqSelect: map[string]bool{}}
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
		if reverted != nil {
			out.Status, out.Message = StatusUnchanged, fmt.Sprintf("not reverted: already reverted here, audit "+
				"row %d, and no Apply ran since", reverted.ID)
			o.log(ctx, slog.LevelInfo, "fixture change not reverted, the change set is reverted here already", out,
				where+": "+out.Message)
			o.report(out)
			continue
		}
		if base != nil {
			if made, done, row := base.Made(i, set.Changes[i]); !made {
				out.Status, out.Message = StatusUnchanged, notMade(base, done, row)
				o.log(ctx, slog.LevelInfo, "fixture change not reverted, the migration did not make it here", out,
					where+": "+out.Message)
				o.report(out)
				continue
			}
		}
		res, err := r.exec(ctx, c)
		if err != nil {
			err = privilege(err)
			out.Status, out.Problem, out.Message = StatusFailed, ProblemError, err.Error()
			if pgerr.State(err) == pgerr.LockNotAvailable && set.LockTimeout != "" {
				err = fmt.Errorf("another session held a lock on a row of %s for longer than the lock timeout "+
					"of %s, so nothing was changed; the change set runs again on the next deploy: %w",
					set.Tables[c.Model].Name, set.LockTimeout, err)
				out.Problem, out.Message = ProblemLockTimeout, err.Error()
			}
			o.report(out)
			return &ChangeError{Outcome: out, err: err}
		}
		out.Rows, out.Message = res.rows, res.message
		switch {
		case res.problem == "" && res.message != "":
			out.Status = StatusApplied
			o.log(ctx, slog.LevelWarn, "fixture change applied", out,
				fmt.Sprintf("%s: applied (%s). %s", where, rowCount(res.rows), res.message))
		case res.problem == "":
			out.Status = StatusApplied
			o.log(ctx, slog.LevelInfo, "fixture change applied", out,
				fmt.Sprintf("%s: applied (%s)", where, rowCount(res.rows)))
		case res.problem == problemBenign:
			out.Status = StatusUnchanged
			o.log(ctx, slog.LevelInfo, "fixture change already made", out, where+": "+res.message)
		case modeFor(set.PolicyFor(c.Model), res.problem) == fixturechange.ModeError:
			out.Status, out.Problem = StatusFailed, res.problem.exported()
			o.report(out)
			return &ChangeError{Outcome: out}
		default:
			out.Status, out.Problem = StatusSkipped, res.problem.exported()
			o.log(ctx, slog.LevelWarn, "fixture change skipped", out, where+": SKIPPED. "+res.message)
		}
		o.report(out)
	}
	if err := r.syncSequences(ctx, o); err != nil {
		return err
	}
	return finish(ctx, tx, set, revert, restore, outcomes)
}

// finish ends a run that succeeded: it checks the deferred constraints and
// puts back the session's settings, and then, last of all in the transaction,
// writes the run's row into the set's audit table, if it has one. A run that
// fails before this point writes no row; one that fails here rolls back.
func finish(ctx context.Context, tx bun.IDB, set fixturechange.Set, revert bool, restore func(context.Context) error,
	outcomes []Outcome) error {

	if err := restore(ctx); err != nil {
		return err
	}
	if set.AuditTable == "" {
		return nil
	}
	return writeAudit(ctx, tx, set, revert, outcomes)
}

// revertBase is the audit rows of the runs of Apply a Revert undoes, or the
// row of the Revert that undid them already, and says in the log what Revert
// does with them, or without them.
func revertBase(ctx context.Context, tx bun.IDB, set fixturechange.Set, o options) (Applies, *AuditRecord, error) {
	base, reverted, err := ApplyRecords(ctx, tx, set)
	if pgerr.State(err) == pgerr.InsufficientPrivilege {
		return nil, nil, fmt.Errorf("%s: the role running the migration may not read the audit table %s, which says "+
			"what to revert, so nothing was changed: grant it SELECT and INSERT on the table: %w", set.Name,
			set.AuditTable, err)
	}
	if err != nil {
		return nil, nil, err
	}
	out := Outcome{Set: set.Name, Index: -1}
	switch {
	case reverted != nil:
		// Reverting again would invert the changes the Apply before that
		// Revert found made or skipped, which the first Revert left alone:
		// an admin's value overwritten by an old one this database never
		// held.
		out.Message = fmt.Sprintf("the change set is reverted here already: the newest row of %s for it, row %d, is "+
			"the Revert of %s, and no Apply ran since, so nothing is reverted", set.AuditTable, reverted.ID,
			reverted.AppliedAt.UTC().Format(time.RFC3339))
		o.log(ctx, slog.LevelWarn, "the change set is reverted here already, nothing is reverted", out,
			set.Name+": "+out.Message)
		return nil, reverted, nil
	case len(base) == 0:
		out.Message = fmt.Sprintf("%s holds no row of this change set: it never ran here with the audit table, so "+
			"every change is reverted, as if the migration had made them all in this database", set.AuditTable)
		o.log(ctx, slog.LevelWarn, "no audit row of the change set, every change is reverted", out,
			set.Name+": "+out.Message)
		return nil, nil, nil
	case base.ran() == nil:
		out.Message = fmt.Sprintf("the run here was unseeded (row %d of %s): the database was not seeded yet, so "+
			"nothing was changed, and nothing is reverted", base[0].ID, set.AuditTable)
		o.log(ctx, slog.LevelInfo, "the change set ran here unseeded, nothing is reverted", out, set.Name+": "+out.Message)
		return base, nil, nil
	}
	made := 0
	for i, c := range set.Changes {
		if ok, _, _ := base.Made(i, c); ok {
			made++
		}
	}
	out.Message = fmt.Sprintf("reverts the %s that the Apply of %s (row %d of %s) and any before it since the last "+
		"Revert made in this database; the %s they found made or skipped %s left alone", plural(made, "change"),
		base[0].AppliedAt.UTC().Format(time.RFC3339), base[0].ID, set.AuditTable,
		plural(len(set.Changes)-made, "other change"), isAre(len(set.Changes)-made))
	o.log(ctx, slog.LevelInfo, "the change set is reverted as its audit rows say", out, set.Name+": "+out.Message)
	if base[0].SetSHA256 != SetSHA256(set) {
		out.Message = fmt.Sprintf("the change set was edited after it ran here: it is not the one row %d of %s "+
			"recorded, so each change is matched to what that run did by its model, key and kind, and one the run "+
			"did not have is not reverted", base[0].ID, set.AuditTable)
		o.log(ctx, slog.LevelWarn, "the change set was edited after it ran here", out, set.Name+": "+out.Message)
	}
	return base, nil, nil
}

// notMade says why Revert leaves a change alone: no run of Apply in this
// database made it.
func notMade(base Applies, done AuditOutcome, row *AuditRecord) string {
	if row == nil {
		ran := base.ran()
		if ran == nil {
			return fmt.Sprintf("not reverted: the run here was unseeded (audit row %d): nothing was changed, so "+
				"nothing is reverted", base[0].ID)
		}
		return fmt.Sprintf("not reverted: the change set did not hold this change when it ran here, the Apply of %s "+
			"(row %d)", ran.AppliedAt.UTC().Format(time.RFC3339), ran.ID)
	}
	when := fmt.Sprintf("the Apply of %s (row %d)", row.AppliedAt.UTC().Format(time.RFC3339), row.ID)
	what := string(done.Status)
	if done.Problem != "" {
		what += " [" + string(done.Problem) + "]"
	}
	return fmt.Sprintf("not reverted: the migration did not make it in this database, where %s found it %s, so the "+
		"row is left as it is", when, what)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// session makes a literal mean in the migration what it meant to dbfixture.
// yaml.v3 reads a timestamp without a zone as UTC, and bun writes the
// time.Time it becomes with an offset, so "2026-01-01 10:00:00" in a fixture
// file is 10:00 UTC in the seeded database whatever the server's TimeZone. A
// migration binding the same text has to read it the same way, and a date such
// as 2026-01-02 has to be year-month-day. IntervalStyle is postgres, as when
// the generator read the values: it decides how an interval such as
// '-1 2:03:04' is read, and how one is spelled where intervals compare through
// their text (see looseEquality). The settings are local to the transaction;
// restore puts back what a caller's own transaction had, and a rollback does
// that by itself. restore also puts back lock_timeout, which run changes, and
// lockTimeout is the session's.
func session(ctx context.Context, tx bun.IDB) (restore func(context.Context) error, lockTimeout string, err error) {
	var tz, ds, is string
	if err := tx.QueryRowContext(ctx, "SELECT current_setting('TimeZone'), current_setting('DateStyle'), "+
		"current_setting('IntervalStyle'), current_setting('lock_timeout')").
		Scan(&tz, &ds, &is, &lockTimeout); err != nil {
		return nil, "", fmt.Errorf("read the session's settings: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "SELECT set_config('TimeZone', 'UTC', true), "+
		"set_config('DateStyle', 'ISO, YMD', true), set_config('IntervalStyle', 'postgres', true)"); err != nil {
		return nil, "", fmt.Errorf("fix the session's settings: %w", err)
	}
	return func(ctx context.Context) error {
		if _, err := tx.ExecContext(ctx, "SELECT set_config('TimeZone', ?, true), set_config('DateStyle', ?, true), "+
			"set_config('IntervalStyle', ?, true), set_config('lock_timeout', ?, true)", tz, ds, is, lockTimeout); err != nil {
			return fmt.Errorf("restore the session's settings: %w", err)
		}
		return nil
	}, lockTimeout, nil
}

// rowSecurity refuses a change set that a row-level security policy would
// reach, before any of it runs. Filtered by a policy, an update of a row the
// policy hides changes nothing, which reads as a row somebody edited and is
// skipped; a seed guard table whose rows it hides makes the whole set a no-op
// that bun records as applied; a child table whose rows it hides lets a delete
// cascade into rows nobody counted.
//
// It looks at the tables the set reads and writes, the seed guard table, and
// the tables whose foreign keys point at a table the set deletes from. A table
// that only a trigger writes into is not among them: the policy applies to the
// trigger's rows as to any other write, which is what the trigger's author
// meant. Turning row_security off would have made those writes fail instead.
func rowSecurity(ctx context.Context, tx bun.IDB, set fixturechange.Set, revert bool) error {
	deletes := map[string]bool{}
	for _, c := range set.Changes {
		// Revert turns an insert into a delete, and a delete into an insert.
		if (c.Kind == fixturechange.Delete) != revert && c.Kind != fixturechange.Update {
			deletes[c.Model] = true
		}
	}
	var rows []string
	var args []any
	add := func(name string, deleted bool) {
		q, err := quoteIdent(name)
		if err != nil {
			return // Validate has refused it already
		}
		rows = append(rows, "(?, ?)")
		args = append(args, q, deleted)
	}
	for _, model := range sortedModels(set.Tables) {
		add(set.Tables[model].Name, deletes[model])
	}
	if set.SeedGuardTable != "" {
		add(set.SeedGuardTable, false)
	}
	if len(rows) == 0 {
		return nil
	}
	// A table that does not exist is left to the statement that needs it,
	// which fails with PostgreSQL's own words.
	query := `WITH t (rel, deleted) AS (SELECT to_regclass(n), d FROM (VALUES ` + strings.Join(rows, ", ") + `) v (n, d))
SELECT DISTINCT rel::text FROM (
  SELECT rel FROM t WHERE rel IS NOT NULL
  UNION ALL
  SELECT con.conrelid FROM pg_constraint con JOIN t ON con.confrelid = t.rel WHERE t.deleted AND con.contype = 'f'
) s WHERE row_security_active(rel) ORDER BY 1`
	var active []string
	if err := tx.NewRaw(query, args...).Scan(ctx, &active); err != nil {
		return fmt.Errorf("look for row-level security on the tables of the change set: %w", err)
	}
	if len(active) == 0 {
		return nil
	}
	return fmt.Errorf("%s: row-level security is active on %s for the role running the migration: a row-level "+
		"security policy applies to it, which would hide rows from the change set or stop its changes, so nothing "+
		"was changed. Run migrations as the tables' owner while they are not FORCE ROW LEVEL SECURITY, or as a "+
		"role with BYPASSRLS", set.Name, strings.Join(active, ", "))
}

// privilege adds what to do to an error PostgreSQL raised because the role may
// not do something. A row-level security policy on a table a trigger writes
// into makes itself known the same way, when the trigger's row does not pass
// it.
func privilege(err error) error {
	var said *grantError
	if pgerr.State(err) != pgerr.InsufficientPrivilege || errors.As(err, &said) {
		return err
	}
	return fmt.Errorf("%w. The role running the migration lacks a privilege, or a row-level security policy "+
		"applies to it, which would hide rows from the change set or stop its changes: run migrations as the "+
		"tables' owner or a role with BYPASSRLS, or grant what is missing. Nothing was changed", err)
}

// withDeferredConstraints makes every DEFERRABLE constraint wait for the end of
// the change set, and returns a restore that checks them all there.
//
// A change set holds as a whole, not after each statement: the rename of a
// currency code that a DEFERRABLE foreign key points at is followed, in the
// same set, by the update of the prices pointing at it, and checked statement
// by statement the rename fails. Checked when the set is done, a constraint
// that does not hold fails the set like any other change, inside its
// transaction, so bun's record is taken back as usual. A constraint that is
// not DEFERRABLE is checked as it always is. Inside a caller's transaction the
// check also covers whatever the caller had left deferred, and afterwards
// every constraint is back in the mode it is declared with; see
// deferredByDefault.
func withDeferredConstraints(ctx context.Context, tx bun.IDB, set fixturechange.Set,
	restore func(context.Context) error) (func(context.Context) error, error) {

	if _, err := tx.ExecContext(ctx, "SET CONSTRAINTS ALL DEFERRED"); err != nil {
		return nil, fmt.Errorf("defer the constraints to the end of the change set: %w", err)
	}
	return func(ctx context.Context) error {
		if _, err := tx.ExecContext(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
			// A foreign key's check locks the row it points at, and waits
			// for a session that holds it like any statement of the set.
			if pgerr.State(err) == pgerr.LockNotAvailable {
				limit := "the session's lock_timeout"
				if set.LockTimeout != "" {
					limit = "the lock timeout of " + set.LockTimeout
				}
				out := Outcome{Set: set.Name, Index: -1, Status: StatusFailed, Problem: ProblemLockTimeout,
					Message: fmt.Sprintf("once every change was made, checking the constraints PostgreSQL defers "+
						"waited for a lock another session held for longer than %s, so nothing was changed; the "+
						"change set runs again on the next deploy", limit)}
				return &ChangeError{Outcome: out, err: fmt.Errorf("%s: %w", out.Message, err)}
			}
			return fmt.Errorf("%s: once every change was made, a constraint did not hold, so nothing was "+
				"changed: %w", set.Name, privilege(err))
		}
		if err := deferredByDefault(ctx, tx); err != nil {
			return err
		}
		return restore(ctx)
	}, nil
}

// deferredByDefault puts every constraint declared DEFERRABLE INITIALLY
// DEFERRED back to deferred, after the check at the end of a change set made
// them all immediate. Inside a caller's transaction -- plan simulating a deploy,
// a test, a program of its own -- a later statement relying on such a
// constraint, a child row inserted before its parent, would otherwise fail
// where the deploy, which commits each migration, succeeds.
//
// PostgreSQL does not say which mode a caller had set a constraint to, so it
// gets the mode it is declared with. SET CONSTRAINTS finds a constraint by its
// name in its schema, and takes every constraint of that name there: a name
// that one constraint declared INITIALLY DEFERRED shares with another that is
// not is left immediate rather than defer the other one too. A schema the role
// may not use is left out: SET CONSTRAINTS would refuse its name, and nothing
// the role does reaches its tables.
func deferredByDefault(ctx context.Context, tx bun.IDB) error {
	var names string
	if err := tx.QueryRowContext(ctx, `
SELECT coalesce(string_agg(quote_ident(n.nspname) || '.' || quote_ident(c.conname), ', '
                           ORDER BY n.nspname, c.conname), '')
FROM (SELECT DISTINCT connamespace, conname FROM pg_constraint WHERE condeferrable AND condeferred) c
JOIN pg_namespace n ON n.oid = c.connamespace
WHERE NOT pg_is_other_temp_schema(n.oid) AND has_schema_privilege(n.oid, 'USAGE')
  AND NOT EXISTS (SELECT 1 FROM pg_constraint o
                  WHERE o.connamespace = c.connamespace AND o.conname = c.conname AND NOT o.condeferred)`).
		Scan(&names); err != nil {
		return fmt.Errorf("read the constraints declared INITIALLY DEFERRED: %w", err)
	}
	if names == "" {
		return nil
	}
	// The names come from the catalog, quoted, and may hold a ? that bun
	// would read as a placeholder in the text of the statement.
	if _, err := tx.ExecContext(ctx, "SET CONSTRAINTS ? DEFERRED", bun.Safe(names)); err != nil {
		return fmt.Errorf("defer the constraints declared INITIALLY DEFERRED again: %w", err)
	}
	return nil
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

// modeFor is what a policy, the one the change's model runs under, says about
// one problem. An unset policy field is the strict reading: a change that could
// not be made fails the migration rather than being recorded as done. A benign
// outcome, where the database already holds what the change wanted, is never an
// error.
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
//
// An update that writes a key column -- a rename -- leaves its row under the
// key it gives it, so that is the key the inverted update finds the row by.
// Keeping the old key would look for a row named both the old way and the new
// way at once, and the revert of every rename would fail as a missing row.
func invert(c fixturechange.Change) fixturechange.Change {
	switch c.Kind {
	case fixturechange.Insert:
		return fixturechange.Change{Model: c.Model, Kind: fixturechange.Delete, Key: c.Key, Old: c.New}
	case fixturechange.Delete:
		return fixturechange.Change{Model: c.Model, Kind: fixturechange.Insert, Key: c.Key, New: c.Old}
	default:
		key, _ := movedKey(c.Key, c.New)
		return fixturechange.Change{Model: c.Model, Kind: c.Kind, ID: c.ID, Key: key, Old: c.New, New: c.Old}
	}
}

// movedKey is the natural key a row has once values are written into it: key,
// with every key column the values hold replaced. The second result says
// whether they hold one at all, which is what a rename is.
func movedKey(key, values fixturechange.Values) (fixturechange.Values, bool) {
	out := make(fixturechange.Values, len(key))
	moved := false
	for col, v := range key {
		if nv, ok := values[col]; ok {
			v, moved = nv, true
		}
		out[col] = v
	}
	return out, moved
}

type runner struct {
	tx  bun.IDB
	set fixturechange.Set
	// revert is true while Revert runs the inverted changes, whose rows are
	// compared with what the migration wrote rather than with what it was
	// generated against.
	revert bool
	// dryRun is WithDryRun: no sequence is moved.
	dryRun bool
	refs   map[string]string // model\x00key -> resolved id
	// resync collects the models that got an explicit id written into a
	// sequence-backed table.
	resync map[string]bool
	// advanced holds the models whose sequence moved before an explicit id
	// was written, which syncSequences reports with its own moves.
	advanced map[string]bool
	// types holds, per model, the types of its table's columns, read once.
	types map[string]map[string]colType
	// sequences holds, per model, the sequence of its id column, "" for
	// none, read once, and seqSelect, per sequence, whether the role may
	// read it as a table.
	sequences map[string]string
	seqSelect map[string]bool
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
