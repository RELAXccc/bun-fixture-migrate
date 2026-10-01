package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
	"github.com/RELAXccc/bun-fixture-migrate/internal/pgerr"

	"github.com/uptrace/bun"
)

type statusReport struct {
	Fixture string `json:"fixture"`
	// State is nil when the configuration has no state file.
	State *stateInfo `json:"state"`
	// Base is what Uncovered was worked out against.
	Base string `json:"base"`
	// Uncovered is what the fixture file changes that no migration makes,
	// one line per model; Refused is what generate would refuse of it.
	Uncovered []string `json:"uncovered"`
	Refused   []string `json:"refused"`
	// Warnings are what generate would report and carry on past.
	Warnings []string `json:"warnings"`
	// LeftOut are the changes generate -allow-partial refused and recorded in
	// the state file. No migration makes them until baseline -force says one
	// written by hand does.
	LeftOut []string `json:"left_out"`
	// Findings are what the fixture file turned up that the policy does not
	// ignore. One the policy makes an error fails status, as it stops
	// generate.
	Findings []checkFinding `json:"findings"`
	// Directory is the migrations directory, "" when none is configured.
	Directory  string          `json:"directory"`
	Migrations []migrationInfo `json:"migrations"`
	// NotInState are the fixture migrations of the directory whose changes
	// the state file does not include: generated on another branch, or
	// written by hand and not recorded with baseline -force yet.
	NotInState []string `json:"not_in_state"`
	// Database is nil when no database was asked.
	Database *databaseInfo `json:"database"`
	Problems []string      `json:"problems"`
	Notes    []string      `json:"notes"`

	// generateRefuses is true when generate would refuse to write the
	// migration for Uncovered as things stand, so the report does not tell
	// anybody to run it.
	generateRefuses bool
	// coveredMissing is true when the migration the state file names as the
	// newest it includes is not in the directory, which generate refuses.
	coveredMissing bool
}

type stateInfo struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	// Error is why a state file that exists does not read: a merge that
	// stopped in it, an edit its checksum caught. Nothing is compared then.
	Error     string `json:"error,omitempty"`
	Migration string `json:"migration,omitempty"`
	Format    int    `json:"format,omitempty"`
	// Covers is the newest fixture migration whose changes the state
	// includes, and Base what Covers was generated against.
	Covers string `json:"covers,omitempty"`
	Base   string `json:"base,omitempty"`
}

type migrationInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Fixture bool   `json:"fixture"`
	// Changes counts the changes of a fixture migration.
	Changes int `json:"changes,omitempty"`
	// Applied is nil for a migration the database has not applied, and for
	// every migration when no database was asked.
	Applied *appliedInfo `json:"applied"`
	// OutOfOrder is a pending migration that sorts before one the database
	// applied already. bun runs it all the same, after that one.
	OutOfOrder bool `json:"out_of_order"`
	// Audit is what the audit table says the migration's last run did in
	// the database; left out when it holds no row of it.
	Audit *auditInfo `json:"audit,omitempty"`
}

// auditInfo is the newest row of a fixture migration in the audit table.
type auditInfo struct {
	// Direction is "up" for an Apply, "down" for a Revert.
	Direction string    `json:"direction"`
	At        time.Time `json:"at"`
	By        string    `json:"by"`
	// Applied, Unchanged and Skipped count the run's changes by what became
	// of them; Skipped lists the skipped ones with their problems.
	Applied        int           `json:"applied"`
	Unchanged      int           `json:"unchanged"`
	Skipped        int           `json:"skipped"`
	Unseeded       bool          `json:"unseeded"`
	SkippedChanges []auditChange `json:"skipped_changes"`
	// Edited is a migration file whose change set is not the one that ran
	// here: somebody edited it after it ran.
	Edited bool `json:"edited"`
}

type auditChange struct {
	Index   int    `json:"index"`
	Model   string `json:"model"`
	Key     string `json:"key"`
	Kind    string `json:"kind"`
	Problem string `json:"problem"`
}

// auditTableInfo is the audit table status read.
type auditTableInfo struct {
	Table  string `json:"table"`
	Exists bool   `json:"exists"`
}

type appliedInfo struct {
	Group int64     `json:"group"`
	At    time.Time `json:"at"`
}

type databaseInfo struct {
	Table       string `json:"table"`
	TableExists bool   `json:"table_exists"`
	// NotInDirectory are migrations the table records that the directory
	// does not have: another package's, or a file that was deleted.
	NotInDirectory []string `json:"not_in_directory"`
	// NewestApplied is the applied migration of the directory that sorts
	// last, "" when none is applied.
	NewestApplied string `json:"newest_applied"`
	// LocksTable is bun's locks table, and Locked whether it holds the lock
	// on Table: a migrator running right now, or one that died and left it,
	// after which every migrate fails until the row is deleted.
	LocksTable string `json:"locks_table"`
	Locked     bool   `json:"locked"`
	// Audit is the audit table the configuration names; left out when it
	// names none.
	Audit *auditTableInfo `json:"audit,omitempty"`
}

// status says where the fixture file, the migrations and a database stand.
func status(o streams, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	var (
		offline     = fs.Bool("offline", false, "do not connect to the database even when one is configured")
		required    = fs.Bool("require-applied", false, "fail unless the database has applied every migration in the directory")
		strictOrder = fs.Bool("strict-order", false, "fail when a pending migration sorts before one the database applied")
		asJSON      = fs.Bool("json", false, "write the report as JSON")
	)
	s, err := common(o, fs, args)
	if err != nil {
		return err
	}
	useDB := !*offline && s.cfg.Database != ""
	if *required && !useDB {
		return fmt.Errorf("-require-applied needs the database")
	}
	if *strictOrder && !useDB {
		return fmt.Errorf("-strict-order needs the database")
	}
	r := &statusReport{Fixture: s.cfg.FixtureLabel(), Directory: s.outDir}
	_, head, err := s.readFixture()
	if err != nil {
		return err
	}
	old, state, err := s.statusBase(r)
	if err != nil {
		return err
	}

	var fixtures, all []fixturemigrate.MigrationFile
	if s.outDir != "" {
		ms, err := fixturemigrate.ReadMigrations(s.outDir)
		if err != nil {
			return fmt.Errorf("the migrations directory: %w", err)
		}
		r.Problems = append(r.Problems, ms.Problems...)
		all = ms.List
		for _, m := range ms.List {
			info := migrationInfo{ID: m.ID(), Name: m.Name, Fixture: m.Fixture != nil}
			if m.Fixture != nil {
				info.Changes = len(m.Fixture.Changes)
			}
			r.Migrations = append(r.Migrations, info)
		}
		fixtures = ms.Fixtures()
	} else {
		r.Notes = append(r.Notes, "no out directory in the configuration, so no migrations to list")
	}
	if state != nil {
		for _, m := range unaccounted(state, fixtures) {
			r.NotInState = append(r.NotInState, m.ID())
			r.Problems = append(r.Problems, lineageProblem(state, m))
		}
		if s.outDir != "" {
			if gone := coveredGone(state, all, s.outDir, s.statePath); gone != "" {
				r.Problems = append(r.Problems, gone)
				r.coveredMissing = true
			}
		}
	}

	var res *fixturemigrate.Result
	if useDB {
		db, err := s.connect(o.ctx)
		if err != nil {
			return err
		}
		defer db.Close()
		var applied map[string]fixturemigrate.Applied
		var audit map[string]fixtureapply.AuditRecord
		info := &databaseInfo{Table: s.cfg.MigrationsTable, LocksTable: s.cfg.MigrationLocksTable}
		err = readOnly(o.ctx, db, func(tx bun.Tx) error {
			if old != nil {
				if res, err = s.uncoveredInDB(o, tx, r, old, head); err != nil {
					return err
				}
			}
			if applied, info.TableExists, err = fixturemigrate.ReadApplied(o.ctx, tx, s.cfg.MigrationsTable); err != nil {
				return err
			}
			if info.Locked, err = readLock(o.ctx, tx, s.cfg.MigrationLocksTable, s.cfg.MigrationsTable); err != nil {
				return err
			}
			if s.cfg.AuditTable == "" {
				return nil
			}
			info.Audit = &auditTableInfo{Table: s.cfg.RunTimeTable(s.cfg.AuditTable)}
			audit, info.Audit.Exists, err = fixtureapply.ReadAudit(o.ctx, tx, info.Audit.Table)
			if pgerr.State(err) == pgerr.InsufficientPrivilege {
				return fmt.Errorf("the audit table %s cannot be read as this role: grant it SELECT on the table, or "+
					"run status -offline: %w", info.Audit.Table, err)
			}
			return err
		})
		if err != nil {
			return err
		}
		r.Database = info
		markApplied(r, applied)
		markAudit(r, all, audit)
	} else if old != nil {
		if res, err = fixturemigrate.Compute(s.cfg, old, head); err != nil {
			return err
		}
	}
	if res != nil {
		r.Uncovered = res.Summary()
		r.Refused = groupUndecided(res.Refusals)
		for _, w := range res.Warnings {
			r.Warnings = append(r.Warnings, w.String())
		}
	}
	_, findings := s.cfg.Worst(head.Findings)
	errorFindings := 0
	for _, f := range findings {
		r.Findings = append(r.Findings, checkFinding{Kind: string(f.Kind), Level: string(s.cfg.ModeOf(f)),
			Model: f.Model, Row: f.Row, Detail: f.Detail})
		if s.cfg.ModeOf(f) == fixturemigrate.ModeError {
			errorFindings++
		}
	}
	r.generateRefuses = errorFindings > 0 || len(r.NotInState) > 0 || r.coveredMissing

	if *asJSON {
		// A program reads an empty list as [], not as null.
		for _, list := range []*[]string{&r.Uncovered, &r.Refused, &r.Warnings, &r.LeftOut, &r.NotInState, &r.Problems, &r.Notes} {
			if *list == nil {
				*list = []string{}
			}
		}
		if r.Findings == nil {
			r.Findings = []checkFinding{}
		}
		if r.Migrations == nil {
			r.Migrations = []migrationInfo{}
		}
		if r.Database != nil && r.Database.NotInDirectory == nil {
			r.Database.NotInDirectory = []string{}
		}
		if err := writeJSON(o.stdout, r); err != nil {
			return err
		}
	} else {
		printStatus(o, r)
	}

	var failures []string
	if r.State != nil && r.State.Error != "" {
		failures = append(failures, "the state file does not read, so nothing says what the fixture file changes")
	}
	if n := len(r.Uncovered) + len(r.Refused); n > 0 {
		failures = append(failures, "the fixture file has changes no migration makes")
	}
	if len(r.LeftOut) > 0 {
		failures = append(failures, plural(len(r.LeftOut), "change")+" left out of a generated migration and not migrated yet")
	}
	if errorFindings > 0 {
		failures = append(failures, plural(errorFindings, "finding")+" in the fixture file that the policy makes errors")
	}
	if len(r.Problems) > 0 {
		failures = append(failures, plural(len(r.Problems), "problem")+" in the migrations directory")
	}
	pending, outOfOrder := 0, 0
	for _, m := range r.Migrations {
		if m.Applied == nil {
			pending++
		}
		if m.OutOfOrder {
			outOfOrder++
		}
	}
	if *required && pending > 0 {
		failures = append(failures, plural(pending, "migration")+" not applied")
	}
	if *strictOrder && outOfOrder > 0 {
		failures = append(failures, plural(outOfOrder, "pending migration")+" out of order")
	}
	if r.Database != nil && r.Database.Locked {
		failures = append(failures, r.Database.LocksTable+" holds the lock on "+r.Database.Table)
	}
	if len(failures) > 0 {
		return exitError{3, strings.Join(failures, "; ")}
	}
	return nil
}

// statusBase reads what the fixture file is compared with: the state file, or
// git's HEAD while there is none, the base generate would use. With neither
// there is nothing to say what the fixture file changes, and a gate that
// passes on that would pass anything.
func (s *setup) statusBase(r *statusReport) (*fixturemigrate.Snapshot, *fixturemigrate.State, error) {
	var files []fixturemigrate.FixtureFile
	var state *fixturemigrate.State
	if s.statePath != "" {
		r.State = &stateInfo{Path: s.statePath}
		read, err := fixturemigrate.ReadState(s.statePath)
		switch {
		case err == nil:
			state = &read
			r.State.Exists, r.State.Migration, r.State.Format = true, read.Migration, read.Format
			r.State.Covers, r.State.Base = read.Covers, read.Base
			r.LeftOut = read.LeftOut
			files, r.Base = read.Files, "the state file"
		case errors.Is(err, fixturemigrate.ErrNoState):
		default:
			r.State.Exists = true
			r.State.Error = strings.TrimPrefix(err.Error(), s.statePath+": ")
			return nil, nil, nil
		}
	}
	if state == nil {
		gitFiles, err := s.gitFiles("HEAD")
		if err != nil {
			where := "there is no state file at " + s.statePath
			if s.statePath == "" {
				where = "no state file is configured (set out or state)"
			}
			why := err.Error()
			switch {
			case errors.Is(err, exec.ErrNotFound):
				why = "git is not installed"
			case strings.Contains(why, "is not in a git repository"):
				why = "it is not in a git repository"
			}
			return nil, nil, fmt.Errorf("%s, and git cannot say what %s was at HEAD: %s. So nothing says what the "+
				"fixture file changes, and status will not pass it. Run bun-fixture-migrate baseline once the "+
				"databases hold it, or run status where git is installed and the fixture file is committed",
				where, s.cfg.FixtureLabel(), why)
		}
		files, r.Base = gitFiles, "HEAD"
	}
	old, err := s.snapshotOf(files, r.Base)
	return old, state, err
}

// uncoveredInDB works out what the fixture file changes with both sides
// respelled by the database, as generate does, so a value written 1.10 in one
// and 1.1 in the other of a numeric column is no change. It says so when
// that is all there is, because status -offline cannot tell until the state
// file has the new spelling. It lints the fixture file against the columns as
// generate does, and what that finds is the fixture file's findings.
func (s *setup) uncoveredInDB(o streams, tx bun.Tx, r *statusReport, old, head *fixturemigrate.Snapshot) (*fixturemigrate.Result, error) {
	offline, err := fixturemigrate.Compute(s.cfg, old, head)
	if err != nil {
		return nil, err
	}
	tables, err := dbschema.Load(o.ctx, tx, s.cfg.Schemas()...)
	if err != nil {
		return nil, err
	}
	if err := canonical(o, tx, s.cfg, tables, head, old); err != nil {
		return nil, err
	}
	// The lint generate runs before it writes anything, so status does not
	// send anybody to a generate that refuses.
	fixturemigrate.LintColumns(s.cfg, head, tables)
	fixturemigrate.LintZeroDefaults(s.cfg, head, tables)
	fixturemigrate.LintNullDefaults(s.cfg, head, tables)
	res, err := fixturemigrate.Compute(s.cfg, old, head)
	if err != nil {
		return nil, err
	}
	if len(res.Changes)+len(res.Refusals) == 0 && len(offline.Changes)+len(offline.Refusals) > 0 {
		r.Notes = append(r.Notes, "the fixture file differs from "+r.Base+" only in how values are written, which "+
			"status -offline cannot tell from a change; run bun-fixture-migrate generate to record the new spelling")
	}
	return res, nil
}

// markApplied fills in what the database applied, and marks every pending
// migration that sorts before the newest applied one. bun's migrator runs
// every migration it has no record of, in name order, but after all the ones
// it already ran: a branch merged late brings a migration named before one
// that is deployed, and it runs against a database it was not written for.
func markApplied(r *statusReport, applied map[string]fixturemigrate.Applied) {
	inDir := map[string]bool{}
	for i, m := range r.Migrations {
		inDir[m.Name] = true
		if a, ok := applied[m.Name]; ok {
			r.Migrations[i].Applied = &appliedInfo{Group: a.GroupID, At: a.MigratedAt}
			if m.Name > r.Database.NewestApplied {
				r.Database.NewestApplied = m.Name
			}
		}
	}
	for i, m := range r.Migrations {
		if m.Applied == nil && m.Name < r.Database.NewestApplied {
			r.Migrations[i].OutOfOrder = true
		}
	}
	for name := range applied {
		if !inDir[name] {
			r.Database.NotInDirectory = append(r.Database.NotInDirectory, name)
		}
	}
	sort.Strings(r.Database.NotInDirectory)
	for _, m := range r.Migrations {
		if m.OutOfOrder {
			r.Notes = append(r.Notes, fmt.Sprintf("%s is pending and sorts before %s, which this database applied: "+
				"bun runs it on the next migrate all the same, after migrations it was not written to follow. Run bun-fixture-migrate plan "+
				"against a copy of this database to see what it does here", m.ID, r.Database.NewestApplied))
		}
	}
}

// markAudit fills in, for every fixture migration of the directory the audit
// table holds a row of, what its newest row says the last run did here, and
// whether the file still holds the change set that ran.
func markAudit(r *statusReport, all []fixturemigrate.MigrationFile, audit map[string]fixtureapply.AuditRecord) {
	sets := map[string]*fixturechange.Set{}
	for _, m := range all {
		sets[m.ID()] = m.Fixture
	}
	for i, m := range r.Migrations {
		set := sets[m.ID]
		if set == nil {
			continue
		}
		rec, ok := audit[set.Name]
		if !ok {
			continue
		}
		info := &auditInfo{Direction: string(rec.Direction), At: rec.AppliedAt, By: rec.AppliedBy,
			Applied: rec.Count(fixtureapply.StatusApplied), Unchanged: rec.Count(fixtureapply.StatusUnchanged),
			Skipped: rec.Count(fixtureapply.StatusSkipped), SkippedChanges: []auditChange{},
			Edited: rec.SetSHA256 != fixtureapply.SetSHA256(*set)}
		for _, c := range rec.Outcomes {
			switch {
			case c.Status == fixtureapply.StatusUnseeded:
				info.Unseeded = true
			case c.Index >= 0 && c.Status == fixtureapply.StatusSkipped:
				info.SkippedChanges = append(info.SkippedChanges, auditChange{Index: c.Index, Model: c.Model, Key: c.Key,
					Kind: string(c.Kind), Problem: string(c.Problem)})
			}
		}
		r.Migrations[i].Audit = info
		if info.Edited {
			r.Notes = append(r.Notes, fmt.Sprintf("%s was edited after it ran here: its change set is not the one "+
				"%s recorded on %s. What it does on a database that has not run it differs from what it did here; "+
				"a Revert here matches its changes to that run by model, key and kind", m.ID,
				r.Database.Audit.Table, rec.AppliedAt.UTC().Format("2006-01-02 15:04:05")))
		}
	}
}

// readLock reports whether bun's locks table holds the lock Migrator.Lock
// takes on the migrations table: a row naming that table. Migrator.Unlock
// deletes it; a migrator that dies in between leaves it, and every later
// Lock fails with "migrations table is already locked".
func readLock(ctx context.Context, db bun.IDB, locksTable, migrationsTable string) (bool, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, "SELECT to_regclass(?) IS NOT NULL", locksTable).Scan(&exists); err != nil {
		return false, fmt.Errorf("look for %s: %w", locksTable, err)
	}
	if !exists {
		return false, nil
	}
	// The name was checked to be a plain, optionally schema-qualified
	// identifier, and is used unquoted, as bun uses it.
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+locksTable+" WHERE table_name = ?",
		migrationsTable).Scan(&n); err != nil {
		return false, fmt.Errorf("read %s, bun's locks table (set migration_locks_table if yours is another): %w",
			locksTable, err)
	}
	return n > 0, nil
}

// unaccounted is the fixture migrations of the directory the state's history
// does not include.
func unaccounted(state *fixturemigrate.State, fixtures []fixturemigrate.MigrationFile) []fixturemigrate.MigrationFile {
	out, _ := state.Unaccounted(fixtures)
	return out
}

// lineageProblem says why a fixture migration is not in the state's history,
// and what to do about it.
func lineageProblem(state *fixturemigrate.State, m fixturemigrate.MigrationFile) string {
	if !generatedFile(m) {
		return fmt.Sprintf("%s is a fixture migration whose changes the state file does not include. If you wrote it "+
			"by hand for a change generate left out, record that with bun-fixture-migrate baseline -force", m.ID())
	}
	covers := state.Covers
	if state.Format == 1 {
		covers = state.Migration
	}
	how := fmt.Sprintf("it sorts after %s, the newest migration the state file includes, so it was generated on "+
		"another branch and the merge kept the state file of this one", covers)
	switch {
	case covers == "":
		how = "the state file includes no fixture migration, so it was generated on another branch and the merge " +
			"kept the state file of this one"
	case fixturemigrate.CompareMigrations(m.ID(), covers) < 0:
		against := "the state file " + state.Base + " left"
		if state.Base == "" {
			against = "a state before any fixture migration"
		}
		how = fmt.Sprintf("it sorts before %s, which was generated against %s, so the two were generated "+
			"against the same state on two branches", covers, against)
	}
	return fmt.Sprintf("%s is a generated fixture migration whose changes the state file does not include: %s. "+
		"Whichever of the two runs second finds rows the other changed, and databases that run them in different "+
		"orders end up different. Keep the one a database already applied and delete the other, take the state "+
		"file as the one you kept left it, then generate again", m.ID(), how)
}

// undecidedMarker starts the part of a refusal that says a value is written
// in a way only the column's type can settle, after the columns it names.
const undecidedMarker = ", which a column written from a Go string holds as written"

// groupUndecided is the refusals as status lists them: those about a value
// only the column's type can settle, which without a database is every row of
// a numeric column written like 29.00, become one line per model and column,
// and every other refusal stays as it is.
func groupUndecided(refusals []fixturemigrate.Refusal) []string {
	type group struct {
		example string
		rows    []string
		why     string
	}
	groups := map[string]*group{}
	var order []string
	var out []string
	for _, ref := range refusals {
		cols, why, ok := strings.Cut(ref.Reason, undecidedMarker)
		var parts []string
		for _, part := range strings.Split(cols, "; ") {
			if !ok || !strings.Contains(part, " is written ") {
				ok = false
				break
			}
			parts = append(parts, part)
		}
		if !ok {
			out = append(out, ref.String())
			continue
		}
		for _, part := range parts {
			col, written, _ := strings.Cut(part, " is written ")
			key := ref.Model + "." + col
			g := groups[key]
			if g == nil {
				g = &group{example: written, why: "which" + strings.TrimPrefix(undecidedMarker, ", which") + why}
				groups[key] = g
				order = append(order, key)
			}
			g.rows = append(g.rows, ref.Key)
		}
	}
	for _, key := range order {
		g := groups[key]
		if len(g.rows) == 1 {
			model, col, _ := strings.Cut(key, ".")
			out = append(out, model+" "+g.rows[0]+": "+col+" is written "+g.example+", "+g.why)
			continue
		}
		out = append(out, fmt.Sprintf("%s is written like %s in %d rows, %s the first of them, %s",
			key, g.example, len(g.rows), g.rows[0], g.why))
	}
	return out
}

// generatedFile reports whether generate wrote a migration, by the comment it
// opens with, rather than somebody by hand. baseline -force records one
// written by hand; one generated on another branch has to be generated again.
func generatedFile(m fixturemigrate.MigrationFile) bool {
	if len(m.Files) == 0 {
		return false
	}
	data, err := os.ReadFile(m.Files[0])
	return err == nil && bytes.Contains(data, []byte("\n// Generated by bun-fixture-migrate: "))
}

// lineageOf is the history a state keeps when it is rewritten without a new
// migration: what it says, or when it does not say, every fixture migration
// of the directory.
func lineageOf(state *fixturemigrate.State, fixtures []fixturemigrate.MigrationFile) (covers, base string) {
	switch {
	case state == nil:
	case state.Format != 1:
		return state.Covers, state.Base
	case state.Migration != "baseline" && state.Migration != "":
		return state.Migration, state.Migration
	}
	newest := newestFixture(fixtures)
	return newest, newest
}

// migrations is the migrations directory, empty when there is none yet.
func (s *setup) migrations() (*fixturemigrate.Migrations, error) {
	if s.outDir == "" {
		return &fixturemigrate.Migrations{}, nil
	}
	ms, err := fixturemigrate.ReadMigrations(s.outDir)
	if errors.Is(err, os.ErrNotExist) {
		return &fixturemigrate.Migrations{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the migrations directory: %w", err)
	}
	return ms, nil
}

// coveredGone says, when the migration the state names as the newest it
// includes is not in the directory, that its changes are lost: the state,
// which generate diffs against, says they are made, and no migration makes
// them. "" when it is there, or the state names none. A file that is there
// and no longer reads as a change set is a problem of its own already.
func coveredGone(state *fixturemigrate.State, all []fixturemigrate.MigrationFile, dir, statePath string) string {
	covered := state.Covered()
	if covered == "" {
		return ""
	}
	for _, m := range all {
		if m.ID() == covered {
			return ""
		}
	}
	return fmt.Sprintf("the state file includes the changes of %s, which is not in %s: it was deleted or renamed and "+
		"no migration makes its changes. Put it back, or take the state file back from git (git checkout <rev> -- %s) "+
		"and generate again", covered, dir, statePath)
}

// newestFixture is the fixture migration that sorts last, "" for none.
func newestFixture(fixtures []fixturemigrate.MigrationFile) string {
	if len(fixtures) == 0 {
		return ""
	}
	return fixtures[len(fixtures)-1].ID()
}

// printAudit lists what the audit table says each fixture migration's last
// run did here.
func printAudit(o streams, r *statusReport) {
	if r.Database == nil || r.Database.Audit == nil {
		return
	}
	a := r.Database.Audit
	if !a.Exists {
		fmt.Fprintf(o.stdout, "\n%s does not exist yet: no fixture migration with an audit table ran here\n", a.Table)
		return
	}
	header := false
	for _, m := range r.Migrations {
		if m.Audit == nil {
			continue
		}
		if !header {
			fmt.Fprintf(o.stdout, "\nwhat the fixture migrations did here, according to %s\n", a.Table)
			header = true
		}
		run := "applied"
		if m.Audit.Direction == string(fixtureapply.DirectionDown) {
			run = "reverted"
		}
		fmt.Fprintf(o.stdout, "  %s: %s %s by %s: ", m.ID, run, m.Audit.At.UTC().Format("2006-01-02 15:04:05"),
			m.Audit.By)
		if m.Audit.Unseeded {
			fmt.Fprintln(o.stdout, "nothing, the database was not seeded yet")
		} else {
			fmt.Fprintf(o.stdout, "%d applied, %d unchanged, %d skipped\n", m.Audit.Applied, m.Audit.Unchanged,
				m.Audit.Skipped)
		}
		for _, c := range m.Audit.SkippedChanges {
			fmt.Fprintf(o.stdout, "    skipped %s %s %s [%s]\n", c.Model, c.Key, c.Kind, c.Problem)
		}
		if m.Audit.Edited {
			fmt.Fprintln(o.stdout, "    edited after it ran here: the file's change set is not the one that ran")
		}
	}
	if !header {
		fmt.Fprintf(o.stdout, "\n%s holds no run of a fixture migration in this directory\n", a.Table)
	}
}

func printStatus(o streams, r *statusReport) {
	w := tabwriter.NewWriter(o.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "fixture file\t%s\n", r.Fixture)
	switch {
	case r.State == nil:
		fmt.Fprintf(w, "state file\tnone configured\n")
	case r.State.Error != "":
		fmt.Fprintf(w, "state file\t%s does not read: %s\n", r.State.Path, r.State.Error)
	case !r.State.Exists:
		fmt.Fprintf(w, "state file\t%s does not exist yet\n", r.State.Path)
	default:
		fmt.Fprintf(w, "state file\t%s, written by %s\n", r.State.Path, r.State.Migration)
	}
	switch {
	case r.Base == "":
	case len(r.Uncovered)+len(r.Refused) == 0 && len(r.LeftOut) > 0:
	case len(r.Uncovered)+len(r.Refused) == 0:
		fmt.Fprintf(w, "not migrated\tnothing: every change since %s has a migration\n", r.Base)
	default:
		label := "not migrated"
		for _, line := range append(append([]string{}, r.Uncovered...), r.Refused...) {
			fmt.Fprintf(w, "%s\t%s\n", label, line)
			label = ""
		}
		if r.generateRefuses {
			fmt.Fprintf(w, "\tgenerate refuses to write their migration until what is below is put right\n")
		} else {
			fmt.Fprintf(w, "\trun: bun-fixture-migrate generate -name <what changed>\n")
		}
	}
	if len(r.Warnings) > 0 {
		label := "warning"
		for _, line := range r.Warnings {
			fmt.Fprintf(w, "%s\t%s\n", label, line)
			label = ""
		}
	}
	if len(r.LeftOut) > 0 {
		label := "left out"
		for _, line := range r.LeftOut {
			fmt.Fprintf(w, "%s\t%s\n", label, line)
			label = ""
		}
		fmt.Fprintf(w, "\tgenerate -allow-partial left this out; write the migration by hand, then run: "+
			"bun-fixture-migrate baseline -force\n")
	}
	for _, f := range r.Findings {
		fmt.Fprintf(w, "finding\t%s: %s\n", f.Kind, fixturemigrate.Finding{Model: f.Model, Row: f.Row, Detail: f.Detail})
	}
	w.Flush()

	if r.Directory != "" {
		where := "no database asked"
		if r.Database != nil {
			where = "applied according to " + r.Database.Table
			if !r.Database.TableExists {
				where = r.Database.Table + " does not exist: nothing was ever migrated there"
			}
		}
		fmt.Fprintf(o.stdout, "\nmigrations in %s, %s\n", r.Directory, where)
		w = tabwriter.NewWriter(o.stdout, 0, 4, 2, ' ', 0)
		if len(r.Migrations) == 0 {
			fmt.Fprintln(w, "  none")
		}
		for _, m := range r.Migrations {
			state, detail := "-", ""
			if r.Database != nil {
				state = "pending"
			}
			if m.Applied != nil {
				state = "applied"
				detail = fmt.Sprintf("group %d, %s", m.Applied.Group, m.Applied.At.UTC().Format("2006-01-02 15:04:05"))
			}
			if m.OutOfOrder {
				detail = "out of order: runs after " + r.Database.NewestApplied
			}
			kind := ""
			if m.Fixture {
				kind = "fixture, " + plural(m.Changes, "change")
			}
			line := fmt.Sprintf("  %s\t%s\t%s", state, m.ID, kind)
			if detail != "" {
				line += "\t" + detail
			}
			fmt.Fprintln(w, strings.TrimRight(line, "\t"))
		}
		w.Flush()
		if r.Database != nil && len(r.Database.NotInDirectory) > 0 {
			fmt.Fprintf(o.stdout, "recorded in %s, not in this directory: %s\n",
				r.Database.Table, strings.Join(r.Database.NotInDirectory, ", "))
		}
		printAudit(o, r)
	}
	if r.Database != nil && r.Database.Locked {
		fmt.Fprintf(o.stdout, "\nlocked: %s holds bun's lock on %s. If no migration is running now, one died and "+
			"left it, and every migrate fails with \"migrations table is already locked\" until it is gone: "+
			"DELETE FROM %s WHERE table_name = '%s'\n", r.Database.LocksTable, r.Database.Table,
			r.Database.LocksTable, r.Database.Table)
	}
	if len(r.Problems) > 0 {
		fmt.Fprintln(o.stdout, "\nproblems")
		for _, p := range r.Problems {
			fmt.Fprintln(o.stdout, "  "+p)
		}
	}
	for _, n := range r.Notes {
		fmt.Fprintln(o.stdout, "\nnote: "+n)
	}
}
