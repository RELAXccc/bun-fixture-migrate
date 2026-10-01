package main

import (
	"errors"
	"flag"
	"fmt"
	"text/tabwriter"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
)

// sync brings the configured database to the fixture files directly: a
// developer's database, a test run's, a staging copy. Without -yes it only
// says what it would do.
func syncCmd(o streams, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	var (
		yes    = fs.Bool("yes", false, "make the changes; without it, sync only shows them")
		asJSON = fs.Bool("json", false, "write the report as JSON")
	)
	s, err := common(o, fs, args)
	if err != nil {
		return err
	}
	db := s.database()
	defer db.Close()
	report, err := s.p.Sync(o.ctx, db, fixturemigrate.SyncOptions{DryRun: !*yes})
	// A sync that stopped before it changed anything has no report to give
	// but its error: an empty one would say the database already holds the
	// files. A refusal is reported, and so are the changes as far as they ran.
	if report == nil || err != nil && !errors.Is(err, fixturemigrate.ErrSyncRefused) && !ranChanges(report.SyncResult) {
		return err
	}
	if *asJSON {
		if werr := writeJSON(o.stdout, report); werr != nil {
			return werr
		}
	} else {
		printSync(o, report)
	}
	switch {
	case errors.Is(err, fixturemigrate.ErrSyncRefused):
		return exitError{2, err.Error() + "; nothing was changed"}
	case err != nil:
		return err
	}
	if !*yes && len(report.Outcomes) > 0 {
		fmt.Fprintln(o.stderr, "nothing was changed; run it again with -yes to make these changes")
	}
	return nil
}

// ranChanges reports whether a sync got as far as running a change.
func ranChanges(res *fixturemigrate.SyncResult) bool {
	for _, c := range res.Outcomes {
		if c.Index >= 0 {
			return true
		}
	}
	return false
}

func printSync(o streams, r *fixturemigrate.SyncReport) {
	for _, f := range r.Findings {
		fmt.Fprintln(o.stdout, string(f.Kind)+": "+f.Model+" "+f.Row+": "+f.Detail)
	}
	if r.Diff != nil {
		for _, ref := range r.Diff.Refusals {
			fmt.Fprintln(o.stdout, "refused: "+ref.Model+" "+ref.Key+": "+ref.Reason)
		}
		for _, w := range r.Diff.Warnings {
			fmt.Fprintln(o.stdout, "warning: "+w.Model+" "+w.Key+": "+w.Reason)
		}
	}
	changes := 0
	w := tabwriter.NewWriter(o.stdout, 0, 4, 2, ' ', 0)
	for _, c := range r.Outcomes {
		if c.Index < 0 {
			continue
		}
		changes++
		status := string(c.Status)
		if r.DryRun && c.Status == fixtureapply.StatusApplied {
			status = "would apply"
		}
		line := fmt.Sprintf("  %s\t%s %s %s", status, c.Model, c.Key, c.Kind)
		if c.Problem != "" {
			line += fmt.Sprintf(" [%s]: %s", c.Problem, c.Message)
		}
		fmt.Fprintln(w, line)
	}
	w.Flush()
	refusals := 0
	if r.Diff != nil {
		refusals = len(r.Diff.Refusals)
	}
	switch {
	case changes == 0 && len(r.Findings) == 0 && refusals == 0:
		fmt.Fprintln(o.stdout, "the database already holds the fixture files")
	case r.Applied:
		fmt.Fprintf(o.stdout, "applied %s\n", plural(changes, "change"))
	}
}
