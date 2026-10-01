package main

import (
	"errors"
	"flag"
	"fmt"
	"text/tabwriter"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
)

type syncReport struct {
	Applied  bool                   `json:"applied"`
	DryRun   bool                   `json:"dry_run"`
	Findings []checkFinding         `json:"findings"`
	Refusals []checkRefusal         `json:"refusals"`
	Changes  []fixtureapply.Outcome `json:"changes"`
}

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
	files, _, err := s.readFixture()
	if err != nil {
		return err
	}
	db, err := s.connect(o.ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	res, err := fixturemigrate.Sync(o.ctx, db, s.cfg, files, fixturemigrate.SyncOptions{DryRun: !*yes})
	// A sync that stopped before it changed anything has no report to give
	// but its error: an empty one would say the database already holds the
	// files. A refusal is reported, and so are the changes as far as they ran.
	if res == nil || err != nil && !errors.Is(err, fixturemigrate.ErrSyncRefused) && !ranChanges(res) {
		return err
	}
	report := syncReport{Applied: res.Applied, DryRun: !*yes, Findings: findingsJSON(s.cfg, res.Findings),
		Refusals: []checkRefusal{}, Changes: []fixtureapply.Outcome{}}
	if res.Diff != nil {
		for _, r := range res.Diff.Refusals {
			report.Refusals = append(report.Refusals, checkRefusal{r.Model, r.Key, r.Reason})
		}
	}
	report.Changes = append(report.Changes, res.Outcomes...)
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
	if !*yes && len(report.Changes) > 0 {
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

func printSync(o streams, r syncReport) {
	for _, f := range r.Findings {
		fmt.Fprintln(o.stdout, f.Kind+": "+f.Model+" "+f.Row+": "+f.Detail)
	}
	for _, ref := range r.Refusals {
		fmt.Fprintln(o.stdout, "refused: "+ref.Model+" "+ref.Key+": "+ref.Reason)
	}
	changes := 0
	w := tabwriter.NewWriter(o.stdout, 0, 4, 2, ' ', 0)
	for _, c := range r.Changes {
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
	switch {
	case changes == 0 && len(r.Findings) == 0 && len(r.Refusals) == 0:
		fmt.Fprintln(o.stdout, "the database already holds the fixture files")
	case r.Applied:
		fmt.Fprintf(o.stdout, "applied %s\n", plural(changes, "change"))
	}
}
