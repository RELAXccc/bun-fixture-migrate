package main

import (
	"errors"
	"flag"
	"fmt"
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
	// Database is nil when no database was asked.
	Database *databaseInfo `json:"database"`
	Problems []string      `json:"problems"`
	Notes    []string      `json:"notes"`
}

type stateInfo struct {
	Path      string `json:"path"`
	Exists    bool   `json:"exists"`
	Migration string `json:"migration,omitempty"`
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
	old, err := s.statusBase(r)
	if err != nil {
		return err
	}

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
	} else {
		r.Notes = append(r.Notes, "no out directory in the configuration, so no migrations to list")
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
		for _, list := range []*[]string{&r.Uncovered, &r.Refused, &r.LeftOut, &r.Problems, &r.Notes} {
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
func (s *setup) statusBase(r *statusReport) (*fixturemigrate.Snapshot, error) {
	var files []fixturemigrate.FixtureFile
	found := false
	if s.statePath != "" {
		r.State = &stateInfo{Path: s.statePath}
		read, err := fixturemigrate.ReadState(s.statePath)
		switch {
		case err == nil:
			r.State.Exists, r.State.Migration = true, read.Migration
			r.LeftOut = read.LeftOut
			files, r.Base, found = read.Files, "the state file", true
		case errors.Is(err, fixturemigrate.ErrNoState):
		default:
			r.Problems = append(r.Problems, err.Error())
			return nil, nil
		}
	}
	if !found {
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
			return nil, fmt.Errorf("%s, and git cannot say what %s was at HEAD: %s. So nothing says what the "+
				"fixture file changes, and status will not pass it. Run bun-fixture-migrate baseline once the "+
				"databases hold it, or run status where git is installed and the fixture file is committed",
				where, s.cfg.FixtureLabel(), why)
		}
		files, r.Base = gitFiles, "HEAD"
	}
	return s.snapshotOf(files, r.Base)
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
