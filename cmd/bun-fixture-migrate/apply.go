package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
	"github.com/RELAXccc/bun-fixture-migrate/internal/pgerr"

	"github.com/uptrace/bun"
)

// bunFile is bun v1.2.18's pattern for a migration file name
// (migrate/migrations.go, fnameRE): the digits are the name its migrator
// records.
var bunFile = regexp.MustCompile(`^(\d{1,14})_([0-9a-z_\-]+)\.`)

// applyReport is what apply -yes -json writes.
type applyReport struct {
	ID string `json:"id"`
	// Direction is "up" for Apply, "down" for -revert.
	Direction string `json:"direction"`
	// Committed is true once the changes, and the record, are in the
	// database.
	Committed bool `json:"committed"`
	// Record is what -record did to bun's migrations table: "recorded",
	// "unrecorded", or "" without -record.
	Record string `json:"record,omitempty"`
	// AlreadyReverted is -revert -record finding the change set reverted
	// here already, by the audit table: the record was deleted, and Revert
	// not run again.
	AlreadyReverted bool                   `json:"already_reverted,omitempty"`
	GroupID         int64                  `json:"group_id,omitempty"`
	Error           string                 `json:"error,omitempty"`
	Changes         []fixtureapply.Outcome `json:"changes"`
	Notes           []string               `json:"notes"`
}

// applyCmd runs one generated fixture migration against the database outside
// bun's migrator: the escape hatch for migrating by hand. Without -yes it is
// plan -file, a dry run that rolls back.
func applyCmd(o streams, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	var (
		file   = fs.String("file", "", "the generated fixture migration, a .go file, to run (required)")
		yes    = fs.Bool("yes", false, "make the changes and commit them; without it, apply rolls back and reports, as plan -file does")
		revert = fs.Bool("revert", false, "run the change set's Revert instead of its Apply")
		record = fs.Bool("record", false, "in the same transaction, record the migration in the migrations table as "+
			"bun's migrator does, or with -revert delete its record")
		lockTimeout = fs.Duration("lock-timeout", 5*time.Second, "without -yes: give up on a row another session holds a lock on after this long")
		asJSON      = fs.Bool("json", false, "write the report as JSON")
	)
	s, err := common(o, fs, args)
	if err != nil {
		return err
	}
	if *file == "" {
		return fmt.Errorf("apply needs -file, the generated migration to run")
	}
	if !strings.HasSuffix(*file, ".go") {
		return fmt.Errorf("apply -file takes a fixture migration generate wrote, a .go file, and %s is not one", *file)
	}
	src, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	set, isFixture, err := fixturemigrate.ReadChangeSet(src)
	if err != nil {
		return fmt.Errorf("%s: %w", *file, err)
	}
	if !isFixture {
		return fmt.Errorf("%s holds no fixture change set: apply -file takes a fixture migration generate wrote", *file)
	}
	id := strings.TrimSuffix(filepath.Base(*file), ".go")
	name := ""
	if m := bunFile.FindStringSubmatch(filepath.Base(*file)); m != nil {
		name = m[1]
	}
	if *record && name == "" {
		return fmt.Errorf("%s is not named as bun names a migration, digits, an underscore and a comment, so bun "+
			"would not run it and -record has no name to record it under", *file)
	}

	db, err := s.connect(o.ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	var standby bool
	if err := db.QueryRowContext(o.ctx, "SELECT pg_is_in_recovery()").Scan(&standby); err != nil {
		return err
	}
	if standby {
		return fmt.Errorf("the database is a standby, which accepts no writes: point apply at the primary")
	}

	var notes []string
	applied, tableExists, err := readAppliedRW(o.ctx, db, s.cfg.MigrationsTable)
	if err != nil {
		return err
	}
	rec, recorded := applied[name]
	if name == "" {
		recorded = false
	}
	switch {
	case *record && !tableExists:
		return fmt.Errorf("%s does not exist, so there is nothing to record the migration in: bun's migrator "+
			"creates it in Init. Run that once, or leave out -record", s.cfg.MigrationsTable)
	case *record && !*revert && recorded:
		return exitError{2, fmt.Sprintf("migration %s is recorded in %s already (group %d, %s), so bun's migrator "+
			"does not run it and apply will not record it twice; nothing was changed. Leave out -record to run the "+
			"change set again, which finds every change it made", name, s.cfg.MigrationsTable, rec.GroupID,
			rec.MigratedAt.UTC().Format("2006-01-02 15:04:05"))}
	case *record && *revert && !recorded:
		return exitError{2, fmt.Sprintf("migration %s is not recorded in %s, so there is no record to take back; "+
			"nothing was changed. Leave out -record to revert it all the same", name, s.cfg.MigrationsTable)}
	case *record && !*yes && !*revert:
		notes = append(notes, fmt.Sprintf("with -yes, migration %s is recorded in %s as bun's migrator records it, in "+
			"the transaction of its changes", name, s.cfg.MigrationsTable))

	case *record && !*yes:
		notes = append(notes, fmt.Sprintf("with -yes, the record of migration %s is deleted from %s, in the "+
			"transaction of its changes", name, s.cfg.MigrationsTable))
	case *record:
	case !*revert && recorded:
		notes = append(notes, fmt.Sprintf("migration %s is recorded in %s already: running it again changes only "+
			"what is not as it left it", name, s.cfg.MigrationsTable))
	case !*revert && name != "":
		notes = append(notes, fmt.Sprintf("without -record, bun's migrator still has migration %s to run, and runs "+
			"it on the next migrate, which finds every change made", name))
	case *revert && recorded:
		notes = append(notes, fmt.Sprintf("without -record, %s keeps recording migration %s as applied, so bun's "+
			"migrator will not run it again; to take the record out afterwards, run apply -revert -yes -record, "+
			"which finds the change set reverted and changes nothing more", s.cfg.MigrationsTable, name))
	}
	if *record && !*yes {
		if locked, err := migratorLocked(o.ctx, db, s.cfg.MigrationLocksTable, s.cfg.MigrationsTable); err != nil {
			return err
		} else if locked {
			notes = append(notes, fmt.Sprintf("%s holds the lock on %s, which bun's Lock took: a migrator is "+
				"migrating now, or one that stopped without Unlock left it. With -yes, apply -record refuses while "+
				"it is there", s.cfg.MigrationLocksTable, s.cfg.MigrationsTable))
		}
	}
	if *revert {
		note, err := revertNote(o.ctx, db, set, *record)
		if err != nil {
			return err
		}
		notes = append(notes, note)
	}

	if !*yes {
		return applyDryRun(o, db, planTarget{id: id, set: set, revert: *revert}, *lockTimeout, *asJSON, notes)
	}
	report := &applyReport{ID: id, Direction: string(fixtureapply.DirectionUp), Changes: []fixtureapply.Outcome{},
		Notes: notes}
	if *revert {
		report.Direction = string(fixtureapply.DirectionDown)
	}
	runErr := applyAndRecord(o.ctx, db, set, *revert, *record, name, s.cfg.MigrationsTable, s.cfg.MigrationLocksTable,
		report)
	if runErr != nil {
		report.Error = runErr.Error()
	}
	if *asJSON {
		if err := writeJSON(o.stdout, report); err != nil {
			return err
		}
	} else {
		printApply(o, report)
	}
	var refusal exitError
	switch {
	case errors.As(runErr, &refusal):
		return refusal
	case runErr != nil:
		// A change the database was not in the state for is the
		// migration's failure; a lock, a connection, a privilege is apply's.
		code := 3
		if result, _ := judge(runErr); result == "inconclusive" || !ranChange(report.Changes) {
			code = 1
		}
		return exitError{code, id + " failed, so nothing was changed: " + runErr.Error()}
	}
	return nil
}

// ranChange reports whether the change set got as far as its changes, which
// a failure before them, such as a connection lost, does not.
func ranChange(outcomes []fixtureapply.Outcome) bool {
	for _, c := range outcomes {
		if c.Index >= 0 || c.Status == fixtureapply.StatusFailed {
			return true
		}
	}
	return false
}

// readAppliedRW reads bun's migrations table for apply, which writes, so not
// in a read-only transaction; a standby has been refused already.
func readAppliedRW(ctx context.Context, db *bun.DB, table string) (map[string]fixturemigrate.Applied, bool, error) {
	var applied map[string]fixturemigrate.Applied
	var exists bool
	err := db.RunInTx(ctx, &sql.TxOptions{ReadOnly: true}, func(ctx context.Context, tx bun.Tx) error {
		var err error
		applied, exists, err = fixturemigrate.ReadApplied(ctx, tx, table)
		return err
	})
	return applied, exists, err
}

// revertNote says what a Revert of the set will undo here: what the audit
// table says Apply made, nothing when it says the set is reverted already,
// or, without it, every change. record is -record, which for a set reverted
// already deletes the record and runs nothing.
func revertNote(ctx context.Context, db *bun.DB, set fixturechange.Set, record bool) (string, error) {
	if set.AuditTable == "" {
		return "the change set has no audit table, so Revert inverts every change, as if the migration had made " +
			"them all in this database", nil
	}
	var base fixtureapply.Applies
	var reverted *fixtureapply.AuditRecord
	err := db.RunInTx(ctx, &sql.TxOptions{ReadOnly: true}, func(ctx context.Context, tx bun.Tx) error {
		var err error
		base, reverted, err = fixtureapply.ApplyRecords(ctx, tx, set)
		return err
	})
	if pgerr.State(err) == pgerr.InsufficientPrivilege {
		return "", fmt.Errorf("the audit table %s, which says what the revert undoes, cannot be read as this role; "+
			"grant it SELECT on the table, and USAGE on its schema: %w", set.AuditTable, err)
	}
	if err != nil {
		return "", err
	}
	unseeded := len(base) > 0
	for _, r := range base {
		unseeded = unseeded && r.Unseeded()
	}
	switch {
	case reverted != nil && record:
		return fmt.Sprintf("%s says the change set is reverted here already (row %d, %s), and no Apply ran since, "+
			"so with -yes apply does not run its Revert again, and only deletes the record", set.AuditTable,
			reverted.ID, reverted.AppliedAt.UTC().Format("2006-01-02 15:04:05")), nil
	case reverted != nil:
		return fmt.Sprintf("%s says the change set is reverted here already (row %d, %s), and no Apply ran since, "+
			"so Revert changes nothing", set.AuditTable, reverted.ID,
			reverted.AppliedAt.UTC().Format("2006-01-02 15:04:05")), nil
	case len(base) == 0:
		return fmt.Sprintf("%s holds no row of this change set, so Revert inverts every change, as if the "+
			"migration had made them all in this database", set.AuditTable), nil
	case unseeded:
		return fmt.Sprintf("%s says the change set ran here unseeded (row %d), when it changed nothing, so Revert "+
			"changes nothing", set.AuditTable, base[0].ID), nil
	}
	made := 0
	for i, c := range set.Changes {
		if ok, _, _ := base.Made(i, c); ok {
			made++
		}
	}
	return fmt.Sprintf("Revert inverts the %s %s says the migration made in this database, and leaves the other "+
		"%d alone", plural(made, "change"), set.AuditTable, len(set.Changes)-made), nil
}

// applyDryRun is plan -file for the one migration, run forward or backward:
// the same simulation, report and exit codes.
func applyDryRun(o streams, db *bun.DB, target planTarget, lockTimeout time.Duration, asJSON bool,
	notes []string) error {

	report := &planReport{Migrations: []plannedMigration{}, NotSimulated: []string{}, Notes: notes,
		Problems: []string{}}
	if report.Notes == nil {
		report.Notes = []string{}
	}
	if err := simulate(o, db, []planTarget{target}, lockTimeout, report); err != nil {
		return err
	}
	if asJSON {
		if err := writeJSON(o.stdout, report); err != nil {
			return err
		}
	} else {
		printPlan(o, report)
	}
	for _, m := range report.Migrations {
		switch m.Result {
		case "inconclusive":
			return exitError{1, "the dry run could not finish, " + unfinished(m) + ": " + m.Error}
		case "fails":
			return exitError{3, m.ID + " would fail; nothing was changed"}
		}
	}
	fmt.Fprintln(o.stderr, "nothing was changed; run it again with -yes to make these changes")
	return nil
}

// applyAndRecord runs the change set in one transaction and, with record, adds
// or deletes bun's record of the migration in the same one, so either both are
// in the database or neither is. The record is looked at again once the change
// set holds its advisory lock: a migrator that recorded the migration in the
// meantime has done so before running it, and its run waits for this one.
//
// With record, apply takes bun's lock on the migrations table first, in the
// same transaction, and gives it back before it commits; see lockMigrator.
//
// A revert with record of a set the audit table says is reverted here already
// deletes the record and does not run Revert again: the first revert, which
// left out what the migration had not made, is what this one would repeat at
// best. That is read under the advisory lock too.
func applyAndRecord(ctx context.Context, db *bun.DB, set fixturechange.Set, revert, record bool, name, table,
	locks string, report *applyReport) error {

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	locked := false
	if record {
		if locked, err = lockMigrator(ctx, tx, locks, table); err != nil {
			return err
		}
	}
	if revert && record && set.AuditTable != "" {
		if err := fixtureapply.WaitForChangeSets(ctx, tx); err != nil {
			return err
		}
		_, reverted, err := fixtureapply.ApplyRecords(ctx, tx, set)
		if pgerr.State(err) == pgerr.InsufficientPrivilege {
			return fmt.Errorf("the audit table %s, which says what the revert undoes, cannot be read as this role; "+
				"grant it SELECT on the table, and USAGE on its schema: %w", set.AuditTable, err)
		}
		if err != nil {
			return err
		}
		report.AlreadyReverted = reverted != nil
	}
	run := fixtureapply.Apply
	if revert {
		run = fixtureapply.Revert
	}
	if report.AlreadyReverted {
		run = func(context.Context, bun.IDB, fixturechange.Set, ...fixtureapply.Option) error { return nil }
	}
	if err := run(ctx, tx, set,
		fixtureapply.WithLogger(func(string, ...any) {}),
		fixtureapply.WithReport(func(out fixtureapply.Outcome) { report.Changes = append(report.Changes, out) }),
	); err != nil {
		return err
	}
	if record {
		// The name was checked to be a plain, optionally schema-qualified
		// identifier, and is used unquoted, as bun uses it.
		var n int64
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE name = ?", name).Scan(&n); err != nil {
			return err
		}
		switch {
		case !revert && n > 0:
			return exitError{2, fmt.Sprintf("migration %s was recorded in %s while apply ran, by a migrator that "+
				"is running it now; nothing was changed", name, table)}
		case revert && n == 0:
			return exitError{2, fmt.Sprintf("the record of migration %s was taken out of %s while apply ran; "+
				"nothing was changed", name, table)}
		case !revert:
			// bun's Migrate gives every migration of a run the group after
			// the newest one, and lets the column default fill in the time.
			if err := tx.QueryRowContext(ctx, "INSERT INTO "+table+" (name, group_id, migrated_at) "+
				"SELECT ?, coalesce(max(group_id), 0) + 1, current_timestamp FROM "+table+" RETURNING group_id",
				name).Scan(&report.GroupID); err != nil {
				return fmt.Errorf("record migration %s in %s: %w", name, table, err)
			}
			report.Record = "recorded"
		default:
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE name = ?", name); err != nil {
				return fmt.Errorf("delete the record of migration %s from %s: %w", name, table, err)
			}
			report.Record = "unrecorded"
		}
	}
	if locked {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+locks+" WHERE table_name = ?", table); err != nil {
			return fmt.Errorf("give back the migrator's lock in %s: %w", locks, err)
		}
	}
	if err := tx.Commit(); err != nil {
		report.Record, report.GroupID = "", 0
		return err
	}
	report.Committed = true
	return nil
}

// uniqueViolation is PostgreSQL's code for a row a unique index already holds.
const uniqueViolation = "23505"

// lockMigrator takes, in tx, the lock bun's Migrator.Lock takes on the
// migrations table: a row of the locks table naming it (bun v1.2.18,
// migrate/migrator.go, Lock), which the table's unique index lets one session
// hold. The caller deletes the row again before it commits.
//
// The advisory lock every change set takes keeps two runs of the change set
// apart, but not two records of the migration: a migrator that read which
// migrations were pending before apply committed its record records the
// migration a second time. One that took bun's Lock first holds the row, and
// apply refuses (exit 2), changing nothing. One that calls Lock while apply
// holds the row waits for apply to commit, and then reads the record apply
// wrote. A migrator that does not call Lock is kept out by nothing; that is
// what bun's Lock is for.
//
// Without the locks table, which bun's Init creates with the migrations table,
// no migrator can hold the lock, and there is none to take.
func lockMigrator(ctx context.Context, tx bun.Tx, locks, migrations string) (bool, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, "SELECT to_regclass(?) IS NOT NULL", locks).Scan(&exists); err != nil {
		return false, fmt.Errorf("look for %s: %w", locks, err)
	}
	if !exists {
		return false, nil
	}
	// The name was checked to be a plain, optionally schema-qualified
	// identifier, and is used unquoted, as bun uses it.
	if _, err := tx.ExecContext(ctx, "INSERT INTO "+locks+" (table_name) VALUES (?)", migrations); err != nil {
		if pgerr.State(err) == uniqueViolation {
			return false, exitError{2, fmt.Sprintf("%s holds the lock on %s, which bun's Lock took: a migrator is "+
				"migrating now, or one that stopped without Unlock left it. apply -record does not record beside "+
				"it, so nothing was changed; run it once the migrator is done, or, if none runs, delete the row",
				locks, migrations)}
		}
		return false, fmt.Errorf("take the migrator's lock in %s, as bun's Lock does (set migration_locks_table if "+
			"yours is another): %w", locks, err)
	}
	return true, nil
}

// migratorLocked reports whether the locks table holds bun's lock on the
// migrations table now.
func migratorLocked(ctx context.Context, db *bun.DB, locks, migrations string) (bool, error) {
	var locked bool
	err := db.RunInTx(ctx, &sql.TxOptions{ReadOnly: true}, func(ctx context.Context, tx bun.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT to_regclass(?) IS NOT NULL", locks).Scan(&exists); err != nil || !exists {
			return err
		}
		return tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM "+locks+" WHERE table_name = ?)", migrations).
			Scan(&locked)
	})
	if err != nil {
		return false, fmt.Errorf("read %s, bun's locks table: %w", locks, err)
	}
	return locked, nil
}

func printApply(o streams, r *applyReport) {
	verb := "applied"
	if r.Direction == string(fixtureapply.DirectionDown) {
		verb = "reverted"
	}
	w := tabwriter.NewWriter(o.stdout, 0, 4, 2, ' ', 0)
	for _, c := range r.Changes {
		if c.Status == fixtureapply.StatusUnseeded {
			continue
		}
		if c.Index < 0 {
			fmt.Fprintf(w, "  %s\t%s\n", c.Status, c.Message)
			continue
		}
		line := fmt.Sprintf("  %s\t%s %s %s", c.Status, c.Model, c.Key, c.Kind)
		switch {
		case c.Status == fixtureapply.StatusApplied:
			line += fmt.Sprintf(" (%s)", plural(int(c.Rows), "row"))
		case c.Problem != "":
			line += fmt.Sprintf(" [%s]: %s", c.Problem, c.Message)
		case c.Message != "":
			line += ": " + c.Message
		}
		fmt.Fprintln(w, line)
	}
	w.Flush()
	for _, n := range r.Notes {
		fmt.Fprintf(o.stdout, "note: %s\n", n)
	}
	for _, c := range r.Changes {
		if c.Status == fixtureapply.StatusUnseeded {
			fmt.Fprintf(o.stdout, "%s: nothing to do, the database is not seeded yet\n", r.ID)
		}
	}
	switch {
	case !r.Committed:
		fmt.Fprintf(o.stdout, "%s: rolled back, nothing was changed\n", r.ID)
	case r.Record == "recorded":
		fmt.Fprintf(o.stdout, "%s: %s and recorded as applied (group %d), committed\n", r.ID, verb, r.GroupID)
	case r.Record == "unrecorded" && r.AlreadyReverted:
		fmt.Fprintf(o.stdout, "%s: reverted here already, so not reverted again; its record deleted, committed\n", r.ID)
	case r.Record == "unrecorded":
		fmt.Fprintf(o.stdout, "%s: %s and its record deleted, committed\n", r.ID, verb)
	default:
		fmt.Fprintf(o.stdout, "%s: %s, committed\n", r.ID, verb)
	}
}
