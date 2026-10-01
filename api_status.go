package fixturemigrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"

	"github.com/uptrace/bun"
)

// StatusOptions steers Status.
type StatusOptions struct {
	// RequireApplied fails the status unless the database applied every
	// migration in the directory. It needs a database.
	RequireApplied bool
	// StrictOrder fails the status when a pending migration sorts before one
	// the database applied, which bun runs after it all the same. It needs a
	// database.
	StrictOrder bool
}

// StatusReport is where the fixture files, the migrations and a database
// stand, as the status command reports it. It is plain data: encoded as JSON
// it is what status -json prints.
type StatusReport struct {
	// Fixture names the fixture files.
	Fixture string `json:"fixture"`
	// State is nil when the configuration has no state file.
	State *StatusState `json:"state"`
	// Base is what Uncovered was worked out against: "the state file", or
	// "HEAD" while there is none.
	Base string `json:"base"`
	// Uncovered is what the fixture files change that no migration makes,
	// one line per model; Refused is what generate would refuse of it.
	Uncovered []string `json:"uncovered"`
	Refused   []string `json:"refused"`
	// Warnings are what generate would report and carry on past.
	Warnings []string `json:"warnings"`
	// LeftOut are the changes generate -allow-partial refused and recorded in
	// the state file. No migration makes them until baseline -force says one
	// written by hand does.
	LeftOut []string `json:"left_out"`
	// Findings are what the fixture files turned up that the policy does not
	// ignore, against the database's columns when one was asked. One the
	// policy makes an error fails the status, as it stops generate.
	Findings []ReportedFinding `json:"findings"`
	// Directory is the migrations directory, "" when none is configured.
	Directory  string            `json:"directory"`
	Migrations []StatusMigration `json:"migrations"`
	// NotInState are the fixture migrations of the directory whose changes
	// the state file does not include: generated on another branch, or
	// written by hand and not recorded with baseline -force yet.
	NotInState []string `json:"not_in_state"`
	// Database is nil when no database was asked.
	Database *StatusDatabase `json:"database"`
	// Problems are what makes the migrations directory unsafe to deploy, one
	// sentence each.
	Problems []string `json:"problems"`
	Notes    []string `json:"notes"`
	// Failures are why the status fails, one sentence each: its exit code is
	// 3 while there is one. Empty when it passes.
	Failures []string `json:"failures"`

	// GenerateRefuses is true when generate would refuse to write the
	// migration for Uncovered as things stand, so the report does not tell
	// anybody to run it.
	GenerateRefuses bool `json:"-"`
}

// StatusState is the state file as Status found it.
type StatusState struct {
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

// StatusMigration is one migration of the directory.
type StatusMigration struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Fixture bool   `json:"fixture"`
	// Changes counts the changes of a fixture migration.
	Changes int `json:"changes,omitempty"`
	// Applied is nil for a migration the database has not applied, and for
	// every migration when no database was asked.
	Applied *StatusApplied `json:"applied"`
	// OutOfOrder is a pending migration that sorts before one the database
	// applied already. bun runs it all the same, after that one.
	OutOfOrder bool `json:"out_of_order"`
}

// StatusApplied is when a database applied a migration, as bun recorded it.
type StatusApplied struct {
	Group int64     `json:"group"`
	At    time.Time `json:"at"`
}

// StatusDatabase is what Status read from the database.
type StatusDatabase struct {
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
}

// ReportedFinding is a finding as a report carries it, with the level the
// policy gives its kind.
type ReportedFinding struct {
	Kind FindingKind `json:"kind"`
	// Level is what the policy makes of the kind: ModeError or ModeWarn.
	Level  Mode   `json:"level"`
	Model  string `json:"model"`
	Row    string `json:"row,omitempty"`
	Detail string `json:"detail"`
}

// Finding is the finding without its level.
func (f ReportedFinding) Finding() Finding {
	return Finding{Kind: f.Kind, Model: f.Model, Row: f.Row, Detail: f.Detail}
}

// reportFindings is findings as a report carries them, each with the level
// the policy gives its kind; [] for none.
func reportFindings(cfg *Config, findings []Finding) []ReportedFinding {
	out := []ReportedFinding{}
	for _, f := range findings {
		level := ModeError
		if cfg != nil {
			level = cfg.FindingMode(f.Kind)
		}
		out = append(out, ReportedFinding{Kind: f.Kind, Level: level, Model: f.Model, Row: f.Row, Detail: f.Detail})
	}
	return out
}

// Status says where the fixture files, the migrations and a database stand,
// as the status command does: what the files change that no migration makes,
// against the state file or git's HEAD while there is none; the migrations of
// the directory and what the state file's history says of them; and, given a
// database, what it applied, with both sides respelled by it and the files
// linted against its columns as generate does. Given nil it works offline.
//
// Its verdict is the report's Failures. An error is only a status that could
// not be worked out: with no state file and no git to read HEAD from, nothing
// says what the fixture files change.
func (p *Project) Status(ctx context.Context, db bun.IDB, opts StatusOptions) (*StatusReport, error) {
	db = orNil(db)
	if db == nil && opts.RequireApplied {
		return nil, errors.New("RequireApplied needs the database")
	}
	if db == nil && opts.StrictOrder {
		return nil, errors.New("StrictOrder needs the database")
	}
	outDir, statePath := p.OutDir(), p.StatePath()
	r := &StatusReport{Fixture: p.Config.FixtureLabel(), Directory: outDir}
	_, head, err := p.head()
	if err != nil {
		return nil, err
	}
	old, state, err := p.statusBase(r)
	if err != nil {
		return nil, err
	}

	var fixtures, all []MigrationFile
	coveredMissing := false
	if outDir != "" {
		ms, err := ReadMigrations(outDir)
		if err != nil {
			return nil, fmt.Errorf("the migrations directory: %w", err)
		}
		r.Problems = append(r.Problems, ms.Problems...)
		all = ms.List
		for _, m := range ms.List {
			info := StatusMigration{ID: m.ID(), Name: m.Name, Fixture: m.Fixture != nil}
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
		if outDir != "" {
			if gone := coveredGone(state, all, outDir, statePath); gone != "" {
				r.Problems = append(r.Problems, gone)
				coveredMissing = true
			}
		}
	}

	var res *Result
	if db != nil {
		var applied map[string]Applied
		info := &StatusDatabase{Table: p.Config.MigrationsTable, LocksTable: p.Config.MigrationLocksTable}
		err = ReadOnly(ctx, db, func(tx bun.Tx) error {
			if old != nil {
				if res, err = p.uncoveredInDB(ctx, tx, r, old, head); err != nil {
					return err
				}
			}
			if applied, info.TableExists, err = ReadApplied(ctx, tx, p.Config.MigrationsTable); err != nil {
				return err
			}
			info.Locked, err = readLock(ctx, tx, p.Config.MigrationLocksTable, p.Config.MigrationsTable)
			return err
		})
		if err != nil {
			return nil, err
		}
		r.Database = info
		markApplied(r, applied)
	} else if old != nil {
		if res, err = Compute(p.Config, old, head); err != nil {
			return nil, err
		}
	}
	if res != nil {
		r.Uncovered = res.Summary()
		r.Refused = groupUndecided(res.Refusals)
		for _, w := range res.Warnings {
			r.Warnings = append(r.Warnings, w.String())
		}
	}
	_, findings := p.Config.Worst(head.Findings)
	r.Findings = reportFindings(p.Config, findings)
	errorFindings := 0
	for _, f := range r.Findings {
		if f.Level == ModeError {
			errorFindings++
		}
	}
	r.GenerateRefuses = errorFindings > 0 || len(r.NotInState) > 0 || coveredMissing

	// A program reads an empty list as [], not as null.
	for _, list := range []*[]string{&r.Uncovered, &r.Refused, &r.Warnings, &r.LeftOut, &r.NotInState, &r.Problems,
		&r.Notes} {
		if *list == nil {
			*list = []string{}
		}
	}
	if r.Migrations == nil {
		r.Migrations = []StatusMigration{}
	}
	if r.Database != nil && r.Database.NotInDirectory == nil {
		r.Database.NotInDirectory = []string{}
	}

	r.Failures = []string{}
	if r.State != nil && r.State.Error != "" {
		r.Failures = append(r.Failures, "the state file does not read, so nothing says what the fixture file changes")
	}
	if n := len(r.Uncovered) + len(r.Refused); n > 0 {
		r.Failures = append(r.Failures, "the fixture file has changes no migration makes")
	}
	if len(r.LeftOut) > 0 {
		r.Failures = append(r.Failures, plural(len(r.LeftOut), "change")+
			" left out of a generated migration and not migrated yet")
	}
	if errorFindings > 0 {
		r.Failures = append(r.Failures, plural(errorFindings, "finding")+
			" in the fixture file that the policy makes errors")
	}
	if len(r.Problems) > 0 {
		r.Failures = append(r.Failures, plural(len(r.Problems), "problem")+" in the migrations directory")
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
	if opts.RequireApplied && pending > 0 {
		r.Failures = append(r.Failures, plural(pending, "migration")+" not applied")
	}
	if opts.StrictOrder && outOfOrder > 0 {
		r.Failures = append(r.Failures, plural(outOfOrder, "pending migration")+" out of order")
	}
	if r.Database != nil && r.Database.Locked {
		r.Failures = append(r.Failures, r.Database.LocksTable+" holds the lock on "+r.Database.Table)
	}
	return r, nil
}

// statusBase reads what the fixture file is compared with: the state file, or
// git's HEAD while there is none, the base generate would use. With neither
// there is nothing to say what the fixture file changes, and a gate that
// passes on that would pass anything.
func (p *Project) statusBase(r *StatusReport) (*Snapshot, *State, error) {
	var files []FixtureFile
	var state *State
	statePath := p.StatePath()
	if statePath != "" {
		r.State = &StatusState{Path: statePath}
		read, err := ReadState(statePath)
		switch {
		case err == nil:
			state = &read
			r.State.Exists, r.State.Migration, r.State.Format = true, read.Migration, read.Format
			r.State.Covers, r.State.Base = read.Covers, read.Base
			r.LeftOut = read.LeftOut
			files, r.Base = read.Files, "the state file"
		case errors.Is(err, ErrNoState):
		default:
			r.State.Exists = true
			r.State.Error = strings.TrimPrefix(err.Error(), statePath+": ")
			return nil, nil, nil
		}
	}
	if state == nil {
		gitFiles, err := p.gitFiles("HEAD")
		if err != nil {
			where := "there is no state file at " + statePath
			if statePath == "" {
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
				where, p.Config.FixtureLabel(), why)
		}
		files, r.Base = gitFiles, "HEAD"
	}
	old, err := p.snapshotOf(files, r.Base)
	return old, state, err
}

// uncoveredInDB works out what the fixture file changes with both sides
// respelled by the database, as generate does, so a value written 1.10 in one
// and 1.1 in the other of a numeric column is no change. It says so when
// that is all there is, because status -offline cannot tell until the state
// file has the new spelling. It lints the fixture file against the columns as
// generate does, and what that finds is the fixture file's findings.
func (p *Project) uncoveredInDB(ctx context.Context, tx bun.Tx, r *StatusReport, old, head *Snapshot) (*Result, error) {
	offline, err := Compute(p.Config, old, head)
	if err != nil {
		return nil, err
	}
	tables, err := dbschema.Load(ctx, tx, p.Config.Schemas()...)
	if err != nil {
		return nil, err
	}
	if err := canonical(ctx, tx, p.Config, tables, head, old); err != nil {
		return nil, err
	}
	// The lint generate runs before it writes anything, so status does not
	// send anybody to a generate that refuses.
	lintAll(p.Config, head, tables)
	res, err := Compute(p.Config, old, head)
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
func markApplied(r *StatusReport, applied map[string]Applied) {
	inDir := map[string]bool{}
	for i, m := range r.Migrations {
		inDir[m.Name] = true
		if a, ok := applied[m.Name]; ok {
			r.Migrations[i].Applied = &StatusApplied{Group: a.GroupID, At: a.MigratedAt}
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

// LineageProblems is what the state file's history says is wrong with the
// fixture migrations of ms, one sentence each, as Status and plan report it:
// a fixture migration whose changes the state does not include, and the one
// it includes last gone from the directory. It is nil when they agree, and
// when there is no state file or it does not read, which Status reports.
func (p *Project) LineageProblems(ms *Migrations) []string {
	statePath := p.StatePath()
	if statePath == "" {
		return nil
	}
	state, err := ReadState(statePath)
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range unaccounted(&state, ms.Fixtures()) {
		out = append(out, lineageProblem(&state, m))
	}
	if gone := coveredGone(&state, ms.List, p.OutDir(), statePath); gone != "" {
		out = append(out, gone)
	}
	return out
}

// unaccounted is the fixture migrations of the directory the state's history
// does not include.
func unaccounted(state *State, fixtures []MigrationFile) []MigrationFile {
	out, _ := state.Unaccounted(fixtures)
	return out
}

// lineageProblem says why a fixture migration is not in the state's history,
// and what to do about it.
func lineageProblem(state *State, m MigrationFile) string {
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
	case CompareMigrations(m.ID(), covers) < 0:
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
func groupUndecided(refusals []Refusal) []string {
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
func generatedFile(m MigrationFile) bool {
	if len(m.Files) == 0 {
		return false
	}
	data, err := os.ReadFile(m.Files[0])
	return err == nil && bytes.Contains(data, []byte("\n// Generated by bun-fixture-migrate: "))
}

// lineageOf is the history a state keeps when it is rewritten without a new
// migration: what it says, or when it does not say, every fixture migration
// of the directory.
func lineageOf(state *State, fixtures []MigrationFile) (covers, base string) {
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
func (p *Project) migrations() (*Migrations, error) {
	outDir := p.OutDir()
	if outDir == "" {
		return &Migrations{}, nil
	}
	ms, err := ReadMigrations(outDir)
	if errors.Is(err, os.ErrNotExist) {
		return &Migrations{}, nil
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
func coveredGone(state *State, all []MigrationFile, dir, statePath string) string {
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
func newestFixture(fixtures []MigrationFile) string {
	if len(fixtures) == 0 {
		return ""
	}
	return fixtures[len(fixtures)-1].ID()
}
