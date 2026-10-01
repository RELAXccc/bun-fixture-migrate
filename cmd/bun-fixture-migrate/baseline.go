package main

import (
	"errors"
	"flag"
	"fmt"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"

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
	// The database is asked only about a difference the files alone cannot
	// settle, and connected to only then.
	var db bun.IDB
	if !*offline && s.cfg.Database != "" {
		conn := s.database()
		defer conn.Close()
		db = conn
	}
	b, err := s.p.Baseline(o.ctx, db, fixturemigrate.BaselineOptions{From: *rev, Old: *oldPath, Force: *force})
	if b == nil {
		return err
	}
	var refusal *fixturemigrate.RefusedError
	refused := errors.As(err, &refusal)
	switch {
	case !refused:
	case len(refusal.Problems) > 0:
		// Fixture migrations generated against another state.
		for _, p := range refusal.Problems {
			fmt.Fprintln(o.stderr, "refused:", p)
		}
	case errors.Is(err, fixturemigrate.ErrLineage):
		for _, id := range b.NotInState {
			fmt.Fprintln(o.stdout, "not in the state:", id)
		}
	case errors.Is(err, fixturemigrate.ErrUnmigrated) && b.Diff == nil:
		for _, line := range b.LeftOut {
			fmt.Fprintln(o.stdout, "left out:", line)
		}
	}
	if b.Diff != nil {
		for _, line := range b.Diff.Summary() {
			fmt.Fprintln(o.stdout, line)
		}
		for _, r := range b.Diff.Refusals {
			fmt.Fprintln(o.stdout, r.String())
		}
		if b.Respelled {
			fmt.Fprintf(o.stdout, "%s differs from the state only in how values are written\n", b.Recorded)
		}
	}
	if errors.Is(err, fixturemigrate.ErrFindings) {
		printFindings(o, refusal.Findings)
	}
	if err != nil {
		return err
	}
	if b.Unchanged {
		fmt.Fprintf(o.stdout, "%s already records %s\n", b.StatePath, b.Recorded)
		return nil
	}
	if _, err := b.Write(); err != nil {
		return err
	}
	fmt.Fprintf(o.stdout, "wrote %s: generate now diffs against %s\n", b.StatePath, b.Recorded)
	return nil
}
