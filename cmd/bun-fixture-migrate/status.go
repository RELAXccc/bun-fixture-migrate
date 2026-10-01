package main

import (
	"bytes"
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

	"github.com/uptrace/bun"
)

type statusReport struct {
	Fixture string `json:"fixture"`
	// State is nil when the configuration has no state file.
	State *stateInfo `json:"state"`
	// Base is what Uncovered was worked out against, "" when there was
	// nothing to work it out against.
	Base string `json:"base"`
	// Uncovered is what the fixture file changes that no migration makes,
	// one line per model; Refused is what generate would refuse of it.
	Uncovered []string `json:"uncovered"`
	Refused   []string `json:"refused"`
	// LeftOut are the changes generate -allow-partial refused and recorded in
	// the state file. No migration makes them until baseline -force says one
	// written by hand does.
	LeftOut []string `json:"left_out"`
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
}

type stateInfo struct {
	Path      string `json:"path"`
	Exists    bool   `json:"exists"`
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
}

// status says where the fixture file, the migrations and a database stand.
func status(o streams, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	var (
		offline  = fs.Bool("offline", false, "do not connect to the database even when one is configured")
		required = fs.Bool("require-applied", false, "fail unless the database has applied every migration in the directory")
		asJSON   = fs.Bool("json", false, "write the report as JSON")
	)
	s, err := common(o, fs, args)
	if err != nil {
		return err
	}
	useDB := !*offline && s.cfg.Database != ""
	if *required && !useDB {
		return fmt.Errorf("-require-applied needs the database")
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

	var fixtures []fixturemigrate.MigrationFile
	if s.outDir != "" {
		ms, err := fixturemigrate.ReadMigrations(s.outDir)
		if err != nil {
			return fmt.Errorf("the migrations directory: %w", err)
		}
		r.Problems = append(r.Problems, ms.Problems...)
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
	}

	var res *fixturemigrate.Result
	if useDB {
		db, err := s.connect(o.ctx)
		if err != nil {
			return err
		}
		defer db.Close()
		var applied map[string]fixturemigrate.Applied
		info := &databaseInfo{Table: s.cfg.MigrationsTable}
		err = readOnly(o.ctx, db, func(tx bun.Tx) error {
			if old != nil {
				if res, err = s.uncoveredInDB(o, tx, r, old, head); err != nil {
					return err
				}
			}
			applied, info.TableExists, err = fixturemigrate.ReadApplied(o.ctx, tx, s.cfg.MigrationsTable)
			return err
		})
		if err != nil {
			return err
		}
		r.Database = info
		inDir := map[string]bool{}
		for i, m := range r.Migrations {
			inDir[m.Name] = true
			if a, ok := applied[m.Name]; ok {
				r.Migrations[i].Applied = &appliedInfo{Group: a.GroupID, At: a.MigratedAt}
			}
		}
		for name := range applied {
			if !inDir[name] {
				info.NotInDirectory = append(info.NotInDirectory, name)
			}
		}
		sort.Strings(info.NotInDirectory)
	} else if old != nil {
		if res, err = fixturemigrate.Compute(s.cfg, old, head); err != nil {
			return err
		}
	}
	if res != nil {
		r.Uncovered = res.Summary()
		for _, ref := range res.Refusals {
			r.Refused = append(r.Refused, ref.String())
		}
	}

	if *asJSON {
		// A program reads an empty list as [], not as null.
		for _, list := range []*[]string{&r.Uncovered, &r.Refused, &r.LeftOut, &r.NotInState, &r.Problems, &r.Notes} {
			if *list == nil {
				*list = []string{}
			}
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
	if n := len(r.Uncovered) + len(r.Refused); n > 0 {
		failures = append(failures, "the fixture file has changes no migration makes")
	}
	if len(r.LeftOut) > 0 {
		failures = append(failures, plural(len(r.LeftOut), "change")+" left out of a generated migration and not migrated yet")
	}
	if len(r.Problems) > 0 {
		failures = append(failures, plural(len(r.Problems), "problem")+" in the migrations directory")
	}
	if *required {
		pending := 0
		for _, m := range r.Migrations {
			if m.Applied == nil {
				pending++
			}
		}
		if pending > 0 {
			failures = append(failures, plural(pending, "migration")+" not applied")
		}
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
			r.Problems = append(r.Problems, err.Error())
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
// file has the new spelling.
func (s *setup) uncoveredInDB(o streams, tx bun.Tx, r *statusReport, old, head *fixturemigrate.Snapshot) (*fixturemigrate.Result, error) {
	offline, err := fixturemigrate.Compute(s.cfg, old, head)
	if err != nil {
		return nil, err
	}
	tables, err := dbschema.Load(o.ctx, tx, s.cfg.Schema)
	if err != nil {
		return nil, err
	}
	if err := canonical(o, tx, s.cfg, tables, head, old); err != nil {
		return nil, err
	}
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

// fixtureMigrations is the fixture migrations of the migrations directory,
// none when there is no directory yet.
func (s *setup) fixtureMigrations() ([]fixturemigrate.MigrationFile, error) {
	if s.outDir == "" {
		return nil, nil
	}
	ms, err := fixturemigrate.ReadMigrations(s.outDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the migrations directory: %w", err)
	}
	return ms.Fixtures(), nil
}

// newestFixture is the fixture migration that sorts last, "" for none.
func newestFixture(fixtures []fixturemigrate.MigrationFile) string {
	if len(fixtures) == 0 {
		return ""
	}
	return fixtures[len(fixtures)-1].ID()
}

func printStatus(o streams, r *statusReport) {
	w := tabwriter.NewWriter(o.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "fixture file\t%s\n", r.Fixture)
	switch {
	case r.State == nil:
		fmt.Fprintf(w, "state file\tnone configured\n")
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
		fmt.Fprintf(w, "\trun: bun-fixture-migrate generate -name <what changed>\n")
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
