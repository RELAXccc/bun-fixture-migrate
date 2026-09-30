package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
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

	current, err := fixturemigrate.ReadState(s.statePath)
	switch {
	case errors.Is(err, fixturemigrate.ErrNoState):
	case err != nil:
		if !*force {
			return fmt.Errorf("%w\npass -force to replace it", err)
		}
	case fixturemigrate.SameFiles(current.Files, files):
		fmt.Fprintf(o.stdout, "%s already records %s\n", s.statePath, source)
		return nil
	default:
		prev, err := s.snapshotOf(current.Files, "the state file")
		if err != nil {
			return err
		}
		res, err := fixturemigrate.Compute(s.cfg, prev, next)
		if err != nil {
			return err
		}
		if n := len(res.Changes) + len(res.Refusals); n > 0 && !*force {
			for _, line := range res.Summary() {
				fmt.Fprintln(o.stdout, line)
			}
			for _, r := range res.Refusals {
				fmt.Fprintln(o.stdout, r.String())
			}
			return exitError{2, fmt.Sprintf(
				"baseline would record %s as migrated with no migration to make them. Generate one for them, "+
					"or pass -force if a migration you wrote by hand covers them", plural(n, "change"))}
		}
	}
	if err := fixturemigrate.WriteState(s.statePath, fixturemigrate.State{Files: files, Migration: "baseline"}); err != nil {
		return err
	}
	fmt.Fprintf(o.stdout, "wrote %s: generate now diffs against %s\n", s.statePath, source)
	return nil
}
