package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
)

type planReport struct {
	Migrations []plannedMigration `json:"migrations"`
	// NotSimulated are the pending migrations this tool did not write and
	// cannot run: schema changes, backfills, anything hand-written.
	NotSimulated []string `json:"not_simulated"`
}

type plannedMigration struct {
	ID string `json:"id"`
	// Result is "succeeds", "fails", "unseeded" or "not reached".
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
	// After are the pending migrations that were not simulated and run
	// before this one.
	After   []string               `json:"after,omitempty"`
	Changes []fixtureapply.Outcome `json:"changes"`
}

type planTarget struct {
	id    string
	set   fixturechange.Set
	after []string
}

// fileList is a flag that can be given more than once.
type fileList []string

func (f *fileList) String() string     { return strings.Join(*f, ",") }
func (f *fileList) Set(v string) error { *f = append(*f, v); return nil }

// plan runs the fixture migrations a database has not applied yet, in the
// order bun would, inside one transaction that is always rolled back, and
// reports what every change did.
//
// It answers the question a deploy otherwise answers the hard way: will the
// migrations find this database in the state they were generated against. A
// migration that would fail here fails in the deploy, and one that would skip
// a row as somebody's edit skips it there.
func plan(o streams, args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	var files fileList
	fs.Var(&files, "file", "plan this migration file, applied or not; repeat it for several (default: every pending fixture migration)")
	var (
		strict      = fs.Bool("strict", false, "fail when a change would be skipped as well as when one would fail")
		lockTimeout = fs.Duration("lock-timeout", 5*time.Second, "give up on a row another session holds a lock on after this long")
		asJSON      = fs.Bool("json", false, "write the report as JSON")
	)
	s, err := common(o, fs, args)
	if err != nil {
		return err
	}
	db, err := s.connect(o.ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	var targets []planTarget
	report := &planReport{}
	if len(files) > 0 {
		for _, path := range files {
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			set, isFixture, err := fixturemigrate.ReadChangeSet(src)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if !isFixture {
				return fmt.Errorf("%s holds no fixture change set", path)
			}
			targets = append(targets, planTarget{id: strings.TrimSuffix(filepath.Base(path), ".go"), set: set})
		}
	} else {
		if s.outDir == "" {
			return fmt.Errorf("no out directory in the configuration; name the files with -file")
		}
		ms, err := fixturemigrate.ReadMigrations(s.outDir)
		if err != nil {
			return fmt.Errorf("the migrations directory: %w", err)
		}
		for _, p := range ms.Problems {
			fmt.Fprintln(o.stderr, "problem:", p)
		}
		var applied map[string]fixturemigrate.Applied
		err = readOnly(o.ctx, db, func(tx bun.Tx) error {
			applied, _, err = fixturemigrate.ReadApplied(o.ctx, tx, s.cfg.MigrationsTable)
			return err
		})
		if err != nil {
			return err
		}
		for _, m := range ms.List {
			if _, ok := applied[m.Name]; ok {
				continue
			}
			if m.Fixture == nil {
				report.NotSimulated = append(report.NotSimulated, m.ID())
				continue
			}
			targets = append(targets, planTarget{id: m.ID(), set: *m.Fixture,
				after: append([]string{}, report.NotSimulated...)})
		}
	}

	if len(targets) > 0 {
		if err := simulate(o, db, targets, *lockTimeout, report); err != nil {
			return err
		}
	}
	if *asJSON {
		if err := writeJSON(o.stdout, report); err != nil {
			return err
		}
	} else {
		printPlan(o, report)
	}

	skipped := 0
	for _, m := range report.Migrations {
		if m.Result == "fails" {
			return exitError{3, m.ID + " would fail, and so would the deploy"}
		}
		for _, c := range m.Changes {
			if c.Status == fixtureapply.StatusSkipped {
				skipped++
			}
		}
	}
	if *strict && skipped > 0 {
		return exitError{3, plural(skipped, "change") + " would be skipped"}
	}
	return nil
}

// simulate applies the targets in one transaction, stops at the first that
// fails as the migrator would, and rolls everything back.
func simulate(o streams, db *bun.DB, targets []planTarget, lockTimeout time.Duration, report *planReport) error {
	tx, err := db.BeginTx(o.ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if lockTimeout > 0 {
		if _, err := tx.ExecContext(o.ctx, fmt.Sprintf("SET LOCAL lock_timeout = %d", lockTimeout.Milliseconds())); err != nil {
			return err
		}
	}
	failed := false
	for _, t := range targets {
		pm := plannedMigration{ID: t.id, After: t.after, Changes: []fixtureapply.Outcome{}}
		if failed {
			pm.Result = "not reached"
			report.Migrations = append(report.Migrations, pm)
			continue
		}
		pm.Result = "succeeds"
		err := fixtureapply.Apply(o.ctx, tx, t.set,
			fixtureapply.WithLogger(func(string, ...any) {}),
			fixtureapply.WithReport(func(out fixtureapply.Outcome) {
				if out.Status == fixtureapply.StatusUnseeded {
					pm.Result = "unseeded"
				}
				pm.Changes = append(pm.Changes, out)
			}))
		if err != nil {
			pm.Result, pm.Error, failed = "fails", err.Error(), true
		}
		report.Migrations = append(report.Migrations, pm)
	}
	if err := o.ctx.Err(); err != nil {
		return err
	}
	return tx.Rollback()
}

func printPlan(o streams, r *planReport) {
	if len(r.Migrations) == 0 {
		fmt.Fprintln(o.stdout, "no pending fixture migrations")
	}
	for i, m := range r.Migrations {
		if i > 0 {
			fmt.Fprintln(o.stdout)
		}
		switch m.Result {
		case "succeeds":
			fmt.Fprintf(o.stdout, "%s: would succeed\n", m.ID)
		case "unseeded":
			fmt.Fprintf(o.stdout, "%s: would do nothing, the database is not seeded yet\n", m.ID)
		case "fails":
			fmt.Fprintf(o.stdout, "%s: would FAIL\n", m.ID)
		default:
			fmt.Fprintf(o.stdout, "%s: not reached\n", m.ID)
		}
		w := tabwriter.NewWriter(o.stdout, 0, 4, 2, ' ', 0)
		for _, c := range m.Changes {
			if c.Status == fixtureapply.StatusUnseeded {
				continue
			}
			line := fmt.Sprintf("  %s\t%s %s %s", c.Status, c.Model, c.Key, c.Kind)
			switch {
			case c.Status == fixtureapply.StatusApplied:
				line += fmt.Sprintf(" (%s)", plural(int(c.Rows), "row"))
			case c.Problem != "":
				line += fmt.Sprintf(" [%s]: %s", c.Problem, c.Message)
			case c.Message != "":
				line += ": " + c.Message
			}
			fmt.Fprintln(w, line)
		}
		w.Flush()
		if m.Result == "fails" {
			fmt.Fprintf(o.stdout, "  %s\n", m.Error)
			if len(m.After) > 0 {
				fmt.Fprintf(o.stdout, "  note: %s run before it and were not simulated; if they change "+
					"these tables, the deploy can differ from this plan\n", strings.Join(m.After, ", "))
			}
		}
	}
	if len(r.NotSimulated) > 0 {
		fmt.Fprintf(o.stdout, "\nnot simulated, not fixture migrations: %s\n", strings.Join(r.NotSimulated, ", "))
	}
	inserted := false
	for _, m := range r.Migrations {
		for _, c := range m.Changes {
			if c.Status == fixtureapply.StatusApplied && c.Kind != fixturechange.Update {
				inserted = true
			}
		}
	}
	switch {
	case inserted:
		fmt.Fprintln(o.stdout, "\nrolled back: nothing was changed, except that a sequence an insert "+
			"drew from stays where it was moved to, which only leaves a gap in the ids")
	case len(r.Migrations) > 0:
		fmt.Fprintln(o.stdout, "\nrolled back: nothing was changed")
	}
}
