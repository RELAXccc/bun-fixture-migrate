package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
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

	var old *fixturemigrate.Snapshot
	if *fromDB {
		db, err := s.connect(o.ctx)
		if err != nil {
			return err
		}
		defer db.Close()
		err = readOnly(o.ctx, db, func(tx bun.Tx) error {
			tables, err := dbschema.Load(o.ctx, tx, s.cfg.Schema)
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
		files, source, err := s.baseState(o, *oldPath, *base)
		if err != nil {
			return err
		}
		if old, err = s.snapshotOf(files, source); err != nil {
			return err
		}
		if s.cfg.Database != "" && !*noLint {
			db, err := s.connect(o.ctx)
			if err != nil {
				return err
			}
			defer db.Close()
			// Both sides are respelled by the database, so a value written two
			// ways in two revisions of the file is not a change.
			err = readOnly(o.ctx, db, func(tx bun.Tx) error {
				tables, err := dbschema.Load(o.ctx, tx, s.cfg.Schema)
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
	if len(res.Changes) == 0 {
		if len(res.Refusals) > 0 {
			return refused(len(res.Refusals))
		}
		fmt.Fprintf(o.stdout, "nothing changed in %s since %s\n", s.cfg.FixtureLabel(), res.Base)
		return nil
	}
	if len(res.Refusals) > 0 && !*partial {
		return refused(len(res.Refusals))
	}
	if *name == "" {
		return fmt.Errorf("-name is required")
	}

	dir := *out
	if dir == "" {
		dir = s.outDir
	}
	var existing []string
	if dir != "" {
		if ms, err := fixturemigrate.ReadMigrations(dir); err == nil {
			for _, m := range ms.List {
				existing = append(existing, m.Name)
			}
		} else if !*dryRun {
			return fmt.Errorf("the migrations directory: %w", err)
		}
	}
	stamp := fixturemigrate.NextStamp(time.Now(), existing)
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
	if *dryRun {
		o.stdout.Write(src)
		return nil
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
		state := fixturemigrate.State{Files: headData, Migration: strings.TrimSuffix(file, ".go")}
		if err := fixturemigrate.WriteState(s.statePath, state); err != nil {
			return fmt.Errorf("the migration is written, the state file is not: %w; "+
				"run baseline before generating again", err)
		}
		fmt.Fprintln(o.stdout, "wrote", s.statePath)
	}
	fmt.Fprintln(o.stdout, "read it, run plan against a copy of production, then deploy")
	return nil
}

// baseState is the fixture file generate diffs against when it does not diff
// against the database: the file named with -old, the git revision named with
// -base, or else the state file, and HEAD only while there is no state file.
func (s *setup) baseState(o streams, oldPath, rev string) ([]fixturemigrate.FixtureFile, string, error) {
	switch {
	case oldPath != "":
		data, err := os.ReadFile(oldPath)
		return []fixturemigrate.FixtureFile{{Path: oldPath, Data: data}}, oldPath, err
	case rev != "":
		files, err := s.gitFiles(rev)
		return files, rev + ":" + s.cfg.FixtureLabel(), err
	}
	if s.statePath != "" {
		state, err := fixturemigrate.ReadState(s.statePath)
		switch {
		case err == nil:
			return state.Files, "the state after " + state.Migration, nil
		case !errors.Is(err, fixturemigrate.ErrNoState):
			return nil, "", err
		}
		fmt.Fprintf(o.stderr, "no state file at %s yet, so this diffs against git HEAD; "+
			"generate writes one with the migration, or run baseline to start it now\n", s.statePath)
	}
	files, err := s.gitFiles("HEAD")
	if err != nil {
		return nil, "", fmt.Errorf("%w; or run baseline to record what the databases hold", err)
	}
	return files, "HEAD:" + s.cfg.FixtureLabel(), nil
}
