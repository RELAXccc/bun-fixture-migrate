package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/dbschema"

	"github.com/uptrace/bun"
)

// generate writes the migration.
func generate(o streams, args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	var (
		name    = fs.String("name", "", "short name for the migration, required")
		base    = fs.String("base", "", "diff against the fixture file as of this git revision (default: the state file, or HEAD while there is none)")
		oldPath = fs.String("old", "", "diff against this file")
		fromDB  = fs.Bool("from-db", false, "diff the database against the fixture file instead")
		out     = fs.String("out", "", "directory for the generated file (default: the out of the configuration)")
		dryRun  = fs.Bool("dry-run", false, "print the file instead of writing it, and leave the state file alone")
		partial = fs.Bool("allow-partial", false, "write the changes that were accepted even when others were refused")
		noLint  = fs.Bool("no-lint", false, "do not check the fixture file against the database's columns and defaults")
		at      = fs.String("at", "", "timestamp the migration as of this time, YYYYMMDDHHMMSS or RFC 3339, instead of now: for output that is the same on every run")
	)
	s, err := common(o, fs, args)
	if err != nil {
		return err
	}
	chosen := 0
	for _, set := range []bool{*fromDB, *oldPath != "", *base != ""} {
		if set {
			chosen++
		}
	}
	if chosen > 1 {
		return fmt.Errorf("-from-db, -old and -base ask for different base states; pass one of them")
	}
	headData, head, err := s.readFixture()
	if err != nil {
		return err
	}
	// What the fixture files turn up on their own, such as two rows sharing a
	// key, stops generate as it stops sync, status and baseline.
	if err := s.refuseFindings(o, head); err != nil {
		return err
	}
	// The state file is read whatever the base: the changes it records as
	// left out and its history go on into the next one.
	// A state file that does not read is never passed over, whatever the
	// base: this run would overwrite it, and with it the history and the
	// changes left out that it records, or the other side of a conflict.
	var prev *fixturemigrate.State
	if s.statePath != "" {
		state, err := fixturemigrate.ReadState(s.statePath)
		switch {
		case err == nil:
			prev = &state
		case errors.Is(err, fixturemigrate.ErrNoState):
		default:
			return err
		}
	}

	var old *fixturemigrate.Snapshot
	fromState := false
	if *fromDB {
		db, err := s.connect(o.ctx)
		if err != nil {
			return err
		}
		defer db.Close()
		err = readOnly(o.ctx, db, func(tx bun.Tx) error {
			tables, err := dbschema.Load(o.ctx, tx, s.cfg.Schemas()...)
			if err != nil {
				return err
			}
			before := len(head.Findings)
			if err := canonical(o, tx, s.cfg, tables, head); err != nil {
				return err
			}
			if old, err = databaseSnapshot(o.ctx, tx, s.cfg, tables, head); err != nil {
				return err
			}
			if *noLint {
				return nil
			}
			return lint(o, s.cfg, head, tables, before)
		})
		if err != nil {
			return err
		}
	} else {
		files, source, err := s.baseState(o, prev, *oldPath, *base)
		if err != nil {
			return err
		}
		fromState = *oldPath == "" && *base == "" && prev != nil
		if old, err = s.snapshotOf(files, source); err != nil {
			return err
		}
		if s.cfg.Database != "" && !*noLint {
			db, err := s.connect(o.ctx)
			if err != nil {
				return fmt.Errorf("%w; generate connects to check the fixture file against the columns and to "+
					"respell its values as they hold them, and -no-lint generates without the database", err)
			}
			defer db.Close()
			// Both sides are respelled by the database, so a value written two
			// ways in two revisions of the file is not a change.
			err = readOnly(o.ctx, db, func(tx bun.Tx) error {
				tables, err := dbschema.Load(o.ctx, tx, s.cfg.Schemas()...)
				if err != nil {
					return err
				}
				before := len(head.Findings)
				if err := canonical(o, tx, s.cfg, tables, head, old); err != nil {
					return err
				}
				return lint(o, s.cfg, head, tables, before)
			})
			if err != nil {
				return err
			}
		}
	}

	res, err := fixturemigrate.Compute(s.cfg, old, head)
	if err != nil {
		return err
	}
	for _, line := range res.Summary() {
		fmt.Fprintln(o.stdout, line)
	}
	for _, r := range res.Refusals {
		fmt.Fprintln(o.stderr, "refused:", r.String())
	}
	// What the policy lets a migration carry on past is said, and stops
	// nothing.
	for _, w := range res.Warnings {
		fmt.Fprintln(o.stderr, "warning:", w.String())
	}

	dir := *out
	if dir == "" {
		dir = s.outDir
	}
	var existing []string
	var fixtures, all []fixturemigrate.MigrationFile
	var dirErr error
	if dir != "" {
		var ms *fixturemigrate.Migrations
		if ms, dirErr = fixturemigrate.ReadMigrations(dir); dirErr == nil {
			for _, m := range ms.List {
				existing = append(existing, m.Name)
			}
			fixtures, all = ms.Fixtures(), ms.List
		}
	}
	// A fixture migration the state does not include was generated on another
	// branch, or written by hand and not recorded. A migration generated now
	// would expect rows as that one did not leave them, and the state would
	// go on without it, so the history is put right first. So is a state
	// whose newest migration is gone: what it says is made, nothing makes.
	if prev != nil {
		label := "refused:"
		if *dryRun {
			label = "warning:"
		}
		missing := unaccounted(prev, fixtures)
		for _, m := range missing {
			fmt.Fprintln(o.stderr, label, lineageProblem(prev, m))
		}
		gone := ""
		if dir != "" && (dirErr == nil || errors.Is(dirErr, os.ErrNotExist)) {
			gone = coveredGone(prev, all, dir, s.statePath)
		}
		if gone != "" && *dryRun {
			fmt.Fprintln(o.stderr, label, gone)
		}
		switch {
		case *dryRun:
		case gone != "" && len(missing) > 0:
			fmt.Fprintln(o.stderr, label, gone)
			return exitError{2, fmt.Sprintf("%s the state file does not include, and the one it includes last is "+
				"gone, nothing written", plural(len(missing), "fixture migration"))}
		case gone != "":
			return exitError{2, gone + "; nothing written"}
		case len(missing) > 0:
			return exitError{2, fmt.Sprintf("%s the state file does not include, nothing written",
				plural(len(missing), "fixture migration"))}
		}
	}
	if len(res.Changes) == 0 {
		if len(res.Refusals) > 0 {
			return refused(len(res.Refusals))
		}
		fmt.Fprintf(o.stdout, "nothing changed in %s since %s\n", s.cfg.FixtureLabel(), res.Base)
		// The file was edited without changing a value: a comment, or a value
		// written another way, 1.1 for 1.10 in a numeric column. The state
		// takes the new text, or status -offline, which cannot tell such a
		// spelling from a change, would fail on it until the next migration.
		if fromState && !*dryRun && !fixturemigrate.SameFiles(prev.Files, headData) {
			if dirErr != nil && !errors.Is(dirErr, os.ErrNotExist) {
				return fmt.Errorf("the migrations directory: %w", dirErr)
			}
			next := *prev
			next.Files = headData
			next.Covers, next.Base = lineageOf(prev, fixtures)
			if err := fixturemigrate.WriteState(s.statePath, next); err != nil {
				return err
			}
			fmt.Fprintf(o.stdout, "wrote %s: %s differs from it only in how it is written, which the state now "+
				"records too\n", s.statePath, s.cfg.FixtureLabel())
		}
		noteLeftOut(o, prev)
		return nil
	}
	if len(res.Refusals) > 0 && !*partial {
		return refused(len(res.Refusals))
	}
	if *name == "" {
		return fmt.Errorf("-name is required")
	}
	now := time.Now()
	if *at != "" {
		if now, err = parseAt(*at); err != nil {
			return err
		}
	}

	if dirErr != nil && !*dryRun {
		return fmt.Errorf("the migrations directory: %w", dirErr)
	}
	stamp := fixturemigrate.NextStamp(now, existing)
	// bun orders migrations by name as strings, so a short name such as
	// "3_backfill" sorts after every timestamp and would run after this one.
	for _, name := range existing {
		if name > stamp {
			fmt.Fprintf(o.stderr, "warning: migration %s sorts after %s, so bun runs it after this one, "+
				"although this one was generated against the state it leaves\n", name, stamp)
		}
	}
	src, err := fixturemigrate.Render(s.cfg, *name, stamp, res)
	if err != nil {
		return err
	}
	for _, w := range fixturemigrate.RenderWarnings(res) {
		fmt.Fprintln(o.stderr, "warning:", w)
	}
	if w := seedGuardWarning(s.cfg); w != "" {
		fmt.Fprintln(o.stderr, "warning:", w)
	}
	if *dryRun {
		return writeOut(o.stdout, src)
	}
	if dir == "" {
		return fmt.Errorf("no out directory in the configuration and no -out")
	}
	warnings, err := fixturemigrate.CheckPackage(dir, s.cfg.Package, s.cfg.Migrator)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintln(o.stderr, "warning:", w)
	}
	file := fixturemigrate.FileName(stamp, *name)
	target := filepath.Join(dir, file)
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("%s exists already", target)
	}
	if err := fixturemigrate.WriteFileAtomic(target, src, 0o644); err != nil {
		return err
	}
	fmt.Fprintln(o.stdout, "wrote", target)
	if s.statePath != "" {
		id := strings.TrimSuffix(file, ".go")
		state := fixturemigrate.State{Files: headData, Migration: id, Covers: id, Base: newestFixture(fixtures)}
		if prev != nil {
			state.LeftOut = prev.LeftOut
		}
		// What was refused is in the files now, so the next generate does not
		// see it again; the state says it is not migrated until baseline -force
		// says a migration somebody wrote does it.
		for _, r := range res.Refusals {
			if !slices.Contains(state.LeftOut, r.String()) {
				state.LeftOut = append(state.LeftOut, r.String())
			}
		}
		if err := fixturemigrate.WriteState(s.statePath, state); err != nil {
			return fmt.Errorf("the migration is written, the state file is not: %w; "+
				"delete %s and generate again once that is fixed", err, target)
		}
		fmt.Fprintln(o.stdout, "wrote", s.statePath)
		noteLeftOut(o, &state)
	}
	fmt.Fprintln(o.stdout, "read it, run plan against a copy of production, then deploy")
	return nil
}

// noteLeftOut reminds that the state records changes no migration makes yet.
func noteLeftOut(o streams, state *fixturemigrate.State) {
	if state == nil || len(state.LeftOut) == 0 {
		return
	}
	fmt.Fprintf(o.stdout, "%s left out and not migrated yet, which status fails on: write the migration by "+
		"hand, then run bun-fixture-migrate baseline -force\n", plural(len(state.LeftOut), "change"))
}

// parseAt reads the -at flag: a migration timestamp as bun spells it, or an
// RFC 3339 time.
func parseAt(s string) (time.Time, error) {
	if t, err := time.Parse(fixturemigrate.Stamp, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("-at %q is neither YYYYMMDDHHMMSS nor an RFC 3339 time", s)
}

// baseState is the fixture file generate diffs against when it does not diff
// against the database: the file named with -old, the git revision named with
// -base, or else the state file, and HEAD only while there is no state file.
func (s *setup) baseState(o streams, state *fixturemigrate.State, oldPath, rev string) ([]fixturemigrate.FixtureFile, string, error) {
	switch {
	case oldPath != "":
		data, err := os.ReadFile(oldPath)
		return []fixturemigrate.FixtureFile{{Path: oldPath, Data: data}}, oldPath, err
	case rev != "":
		files, err := s.gitFiles(rev)
		return files, rev + ":" + s.cfg.FixtureLabel(), err
	case state != nil:
		return state.Files, "the state after " + state.Migration, nil
	}
	if s.statePath != "" {
		fmt.Fprintf(o.stderr, "no state file at %s yet, so this diffs against git HEAD; "+
			"generate writes one with the migration, or run baseline to start it now\n", s.statePath)
	}
	files, err := s.gitFiles("HEAD")
	if err != nil {
		return nil, "", fmt.Errorf("%w; or run baseline to record what the databases hold", err)
	}
	return files, "HEAD:" + s.cfg.FixtureLabel(), nil
}

// seedGuardWarning is what is wrong with the seed guard table for a migration
// about to be written, "" when nothing is. Without one, a database that was
// never seeded runs every fixture migration before its seed, against empty
// tables, and the first that changes a row fails the deploy. One the fixture
// files do not fill can be empty in a seeded database too, and there every
// fixture migration does nothing and is recorded as applied.
func seedGuardWarning(cfg *fixturemigrate.Config) string {
	guard := cfg.SeedGuardTable
	if guard == "" {
		return "no seed_guard_table: on a database that was never seeded this migration runs before the seed and " +
			"fails; set it to a table the fixture files fill"
	}
	if !strings.Contains(guard, ".") {
		guard = cfg.Schema + "." + guard
	}
	for _, name := range cfg.ModelNames() {
		if cfg.QualifiedTable(cfg.Models[name]) == guard {
			return ""
		}
	}
	return fmt.Sprintf("seed_guard_table %s is the table of no model, so the fixture files do not fill it: on a "+
		"seeded database where it is empty, this migration does nothing and is recorded as applied all the same",
		cfg.SeedGuardTable)
}
