package main

import (
	"errors"
	"flag"
	"fmt"
	"time"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"

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
		asJSON  = fs.Bool("json", false, "write what was generated and written as JSON")
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
	opts := fixturemigrate.GenerateOptions{Name: *name, Base: *base, Old: *oldPath, FromDB: *fromDB, Out: *out,
		AllowPartial: *partial, NoLint: *noLint, DryRun: *dryRun}
	// -at is read once there is a migration to name, as it always was: a
	// run with nothing to write does not fail on it.
	var atErr error
	if *at != "" {
		opts.At, atErr = parseAt(*at)
	}
	// The database respells both sides and lints the fixture files, unless
	// -no-lint, and is the base with -from-db.
	var db bun.IDB
	if *fromDB || s.cfg.Database != "" && !*noLint {
		conn := s.database()
		defer conn.Close()
		db = conn
	}
	g, err := s.p.Generate(o.ctx, db, opts)
	if g == nil {
		return generateError(err, *fromDB)
	}
	var refusal *fixturemigrate.RefusedError
	refused := errors.As(err, &refusal)
	toWrite := g.Diff != nil && len(g.Diff.Changes) > 0
	if atErr != nil && toWrite && !refused && !errors.Is(err, fixturemigrate.ErrNameRequired) {
		return atErr
	}

	if *asJSON {
		if err == nil && !*dryRun {
			if _, err := g.Write(); err != nil {
				return err
			}
		}
		if err != nil && !refused {
			return generateError(err, *fromDB)
		}
		if werr := writeJSON(o.stdout, g); werr != nil {
			return werr
		}
		return err
	}

	// What the fixture files turn up on their own stops generate before
	// anything else is said; what the lint finds against the database comes
	// after the base, warnings too.
	if errors.Is(err, fixturemigrate.ErrFindings) && len(g.Lint) == 0 {
		printFindings(o, refusal.Findings)
	} else {
		for _, note := range g.Notes {
			fmt.Fprintln(o.stderr, note)
		}
		printFindings(o, g.Lint)
	}
	if g.Diff != nil {
		for _, line := range g.Diff.Summary() {
			fmt.Fprintln(o.stdout, line)
		}
		for _, r := range g.Diff.Refusals {
			fmt.Fprintln(o.stderr, "refused:", r.String())
		}
		// What the policy lets a migration carry on past is said, and stops
		// nothing.
		for _, w := range g.Diff.Warnings {
			fmt.Fprintln(o.stderr, "warning:", w.String())
		}
	}
	if *dryRun {
		for _, p := range g.Problems {
			fmt.Fprintln(o.stderr, "warning:", p)
		}
	} else if errors.Is(err, fixturemigrate.ErrLineage) {
		for _, p := range refusal.Problems {
			fmt.Fprintln(o.stderr, "refused:", p)
		}
	}
	for _, w := range g.Warnings {
		fmt.Fprintln(o.stderr, "warning:", w)
	}
	if err != nil {
		return generateError(err, *fromDB)
	}

	if !toWrite {
		fmt.Fprintf(o.stdout, "nothing changed in %s since %s\n", s.cfg.FixtureLabel(), g.Diff.Base)
		if g.State != nil {
			if _, err := g.Write(); err != nil {
				return err
			}
			fmt.Fprintf(o.stdout, "wrote %s: %s differs from it only in how it is written, which the state now "+
				"records too\n", g.StatePath, s.cfg.FixtureLabel())
		}
		noteLeftOut(o, g.LeftOut)
		return nil
	}
	if *dryRun {
		return writeOut(o.stdout, g.Source)
	}
	written, err := g.Write()
	for _, path := range written {
		fmt.Fprintln(o.stdout, "wrote", path)
	}
	if err != nil {
		return err
	}
	if g.State != nil {
		noteLeftOut(o, g.LeftOut)
	}
	fmt.Fprintln(o.stdout, "read it, run plan against a copy of production, then deploy")
	return nil
}

// generateError is an error of the library's generate in the command's words.
func generateError(err error, fromDB bool) error {
	var conn connectError
	switch {
	case errors.Is(err, fixturemigrate.ErrNameRequired):
		return fmt.Errorf("-name is required")
	case errors.As(err, &conn) && !fromDB:
		return fmt.Errorf("%w; generate connects to check the fixture file against the columns and to "+
			"respell its values as they hold them, and -no-lint generates without the database", err)
	}
	return err
}

// printFindings lists findings on standard error, a line each.
func printFindings(o streams, findings []fixturemigrate.Finding) {
	for _, f := range findings {
		fmt.Fprintln(o.stderr, string(f.Kind)+":", f.String())
	}
}

// noteLeftOut reminds that the state records changes no migration makes yet.
func noteLeftOut(o streams, leftOut []string) {
	if len(leftOut) == 0 {
		return
	}
	fmt.Fprintf(o.stdout, "%s left out and not migrated yet, which status fails on: write the migration by "+
		"hand, then run bun-fixture-migrate baseline -force\n", plural(len(leftOut), "change"))
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
