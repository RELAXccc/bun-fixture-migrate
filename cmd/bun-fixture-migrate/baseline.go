package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/dbschema"

	"github.com/uptrace/bun"
)

// baseline records a fixture file as the state the migrations leave a
// database in, without writing a migration.
//
// Two uses: adopting the tool on a project whose databases already hold the
// fixture file, and after writing by hand the migration for a change generate
// refused. The second is also how a change gets lost, so replacing a state
// with one that differs in content is refused unless -force says a migration
// covers it.
func baseline(o streams, args []string) error {
	fs := flag.NewFlagSet("baseline", flag.ContinueOnError)
	var (
		rev     = fs.String("from", "", "record the fixture file as of this git revision instead of the working tree")
		oldPath = fs.String("old", "", "record this file instead of the fixture file")
		force   = fs.Bool("force", false, "replace a state whose content differs: a migration you wrote covers the difference")
		offline = fs.Bool("offline", false, "do not ask the database whether a difference is only in how values are written")
	)
	s, err := common(o, fs, args)
	if err != nil {
		return err
	}
	if s.statePath == "" {
		return fmt.Errorf("no state file: set out, or state, in the configuration")
	}
	if *rev != "" && *oldPath != "" {
		return fmt.Errorf("-from and -old name two different files; pass one of them")
	}
	var files []fixturemigrate.FixtureFile
	source := s.cfg.FixtureLabel()
	switch {
	case *rev != "":
		files, err = s.gitFiles(*rev)
		source = *rev + ":" + source
	case *oldPath != "":
		var data []byte
		data, err = os.ReadFile(*oldPath)
		files = []fixturemigrate.FixtureFile{{Path: *oldPath, Data: data}}
		source = *oldPath
	default:
		files, _, err = s.readFixture()
	}
	if err != nil {
		return err
	}
	next, err := s.snapshotOf(files, source)
	if err != nil {
		return err
	}

	var prev *fixturemigrate.State
	current, err := fixturemigrate.ReadState(s.statePath)
	switch {
	case errors.Is(err, fixturemigrate.ErrNoState):
	case err != nil:
		if !*force {
			return fmt.Errorf("%w\npass -force to replace it", err)
		}
	default:
		prev = &current
	}
	if prev != nil && len(prev.LeftOut) > 0 && !*force {
		for _, line := range prev.LeftOut {
			fmt.Fprintln(o.stdout, "left out:", line)
		}
		return exitError{2, fmt.Sprintf("the state records %s generate left out, which no migration makes yet. "+
			"Write their migration by hand, then pass -force", plural(len(prev.LeftOut), "change"))}
	}

	switch {
	case prev == nil:
	case fixturemigrate.SameFiles(prev.Files, files) && len(prev.LeftOut) == 0:
		fmt.Fprintf(o.stdout, "%s already records %s\n", s.statePath, source)
		return nil
	case *force:
	default:
		before, err := s.snapshotOf(prev.Files, "the state file")
		if err != nil {
			return err
		}
		n, respelled, err := s.baselineDiff(o, before, next, *offline)
		if err != nil {
			return err
		}
		if n > 0 {
			return exitError{2, fmt.Sprintf(
				"baseline would record %s as migrated with no migration to make them. Generate one for them, "+
					"or pass -force if a migration you wrote by hand covers them", plural(n, "change"))}
		}
		if respelled {
			fmt.Fprintf(o.stdout, "%s differs from the state only in how values are written\n", source)
		}
	}
	if err := fixturemigrate.WriteState(s.statePath, fixturemigrate.State{Files: files, Migration: "baseline"}); err != nil {
		return err
	}
	fmt.Fprintf(o.stdout, "wrote %s: generate now diffs against %s\n", s.statePath, source)
	return nil
}

// baselineDiff counts the changes from the state to the files baseline is
// asked to record, and prints them. A difference the files alone cannot
// settle, such as 1.10 against 1.1, is asked of the database's column types
// when one is configured, the way generate asks: as a number it is no
// change, and refusing it would leave the state behind for good. respelled
// is true when the database settled every difference that way.
func (s *setup) baselineDiff(o streams, before, next *fixturemigrate.Snapshot, offline bool) (n int, respelled bool, err error) {
	res, err := fixturemigrate.Compute(s.cfg, before, next)
	if err != nil {
		return 0, false, err
	}
	if len(res.Changes)+len(res.Refusals) > 0 && !offline && s.cfg.Database != "" {
		db, err := s.connect(o.ctx)
		if err != nil {
			return 0, false, err
		}
		defer db.Close()
		err = readOnly(o.ctx, db, func(tx bun.Tx) error {
			tables, err := dbschema.Load(o.ctx, tx, s.cfg.Schema)
			if err != nil {
				return err
			}
			if err := canonical(o, tx, s.cfg, tables, next, before); err != nil {
				return err
			}
			res, err = fixturemigrate.Compute(s.cfg, before, next)
			return err
		})
		if err != nil {
			return 0, false, err
		}
		respelled = len(res.Changes)+len(res.Refusals) == 0
	}
	for _, line := range res.Summary() {
		fmt.Fprintln(o.stdout, line)
	}
	for _, r := range res.Refusals {
		fmt.Fprintln(o.stdout, r.String())
	}
	return len(res.Changes) + len(res.Refusals), respelled, nil
}
