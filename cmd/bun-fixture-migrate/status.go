package main

import (
	"flag"
	"fmt"
	"strings"
	"text/tabwriter"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"

	"github.com/uptrace/bun"
)

// status says where the fixture file, the migrations and a database stand.
func status(o streams, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	var (
		offline     = fs.Bool("offline", false, "do not connect to the database even when one is configured")
		required    = fs.Bool("require-applied", false, "fail unless the database has applied every migration in the directory")
		strictOrder = fs.Bool("strict-order", false, "fail when a pending migration sorts before one the database applied")
		asJSON      = fs.Bool("json", false, "write the report as JSON")
	)
	s, err := common(o, fs, args)
	if err != nil {
		return err
	}
	useDB := !*offline && s.cfg.Database != ""
	if *required && !useDB {
		return fmt.Errorf("-require-applied needs the database")
	}
	if *strictOrder && !useDB {
		return fmt.Errorf("-strict-order needs the database")
	}
	var db bun.IDB
	if useDB {
		conn := s.database()
		defer conn.Close()
		db = conn
	}
	r, err := s.p.Status(o.ctx, db, fixturemigrate.StatusOptions{RequireApplied: *required, StrictOrder: *strictOrder})
	if err != nil {
		return err
	}
	if *asJSON {
		if err := writeJSON(o.stdout, r); err != nil {
			return err
		}
	} else {
		printStatus(o, r)
	}
	if len(r.Failures) > 0 {
		return exitError{3, strings.Join(r.Failures, "; ")}
	}
	return nil
}

// printAudit lists what the audit table says each fixture migration's last
// run did here.
func printAudit(o streams, r *fixturemigrate.StatusReport) {
	if r.Database == nil || r.Database.Audit == nil {
		return
	}
	a := r.Database.Audit
	if !a.Exists {
		fmt.Fprintf(o.stdout, "\n%s does not exist yet: no fixture migration with an audit table ran here\n", a.Table)
		return
	}
	header := false
	for _, m := range r.Migrations {
		if m.Audit == nil {
			continue
		}
		if !header {
			fmt.Fprintf(o.stdout, "\nwhat the fixture migrations did here, according to %s\n", a.Table)
			header = true
		}
		// A Revert's applied changes are the ones it reverted.
		run := "applied"
		if m.Audit.Direction == string(fixtureapply.DirectionDown) {
			run = "reverted"
		}
		fmt.Fprintf(o.stdout, "  %s: %s %s by %s: ", m.ID, run, m.Audit.At.UTC().Format("2006-01-02 15:04:05"),
			m.Audit.By)
		if m.Audit.Unseeded {
			fmt.Fprintln(o.stdout, "nothing, the database was not seeded yet")
		} else {
			fmt.Fprintf(o.stdout, "%d %s, %d unchanged, %d skipped\n", m.Audit.Applied, run, m.Audit.Unchanged,
				m.Audit.Skipped)
		}
		for _, c := range m.Audit.SkippedChanges {
			fmt.Fprintf(o.stdout, "    skipped %s %s %s [%s]\n", c.Model, c.Key, c.Kind, c.Problem)
		}
		if m.Audit.Edited {
			fmt.Fprintln(o.stdout, "    edited after it ran here: the file's change set is not the one that ran")
		}
	}
	if !header {
		fmt.Fprintf(o.stdout, "\n%s holds no run of a fixture migration in this directory\n", a.Table)
	}
}

func printStatus(o streams, r *fixturemigrate.StatusReport) {
	w := tabwriter.NewWriter(o.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "fixture file\t%s\n", r.Fixture)
	switch {
	case r.State == nil:
		fmt.Fprintf(w, "state file\tnone configured\n")
	case r.State.Error != "":
		fmt.Fprintf(w, "state file\t%s does not read: %s\n", r.State.Path, r.State.Error)
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
		if r.GenerateRefuses {
			fmt.Fprintf(w, "\tgenerate refuses to write their migration until what is below is put right\n")
		} else {
			fmt.Fprintf(w, "\trun: bun-fixture-migrate generate -name <what changed>\n")
		}
	}
	if len(r.Warnings) > 0 {
		label := "warning"
		for _, line := range r.Warnings {
			fmt.Fprintf(w, "%s\t%s\n", label, line)
			label = ""
		}
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
	for _, f := range r.Findings {
		fmt.Fprintf(w, "finding\t%s: %s\n", f.Kind, f.Finding())
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
			if m.OutOfOrder {
				detail = "out of order: runs after " + r.Database.NewestApplied
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
		printAudit(o, r)
	}
	if r.Database != nil && r.Database.Locked {
		fmt.Fprintf(o.stdout, "\nlocked: %s holds bun's lock on %s. If no migration is running now, one died and "+
			"left it, and every migrate fails with \"migrations table is already locked\" until it is gone: "+
			"DELETE FROM %s WHERE table_name = '%s'\n", r.Database.LocksTable, r.Database.Table,
			r.Database.LocksTable, r.Database.Table)
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
