package main

import (
	"context"
	"database/sql/driver"
	"errors"
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
	"github.com/uptrace/bun/driver/pgdriver"
)

type planReport struct {
	Migrations []plannedMigration `json:"migrations"`
	// NotSimulated are the pending migrations this tool did not write and
	// cannot run: schema changes, backfills, anything hand-written.
	NotSimulated []string `json:"not_simulated"`
}

type plannedMigration struct {
	ID string `json:"id"`
	// Kind is "fixture" for a change set, "sql" for a SQL migration run
	// with -with-sql.
	Kind string `json:"kind"`
	// Result is "succeeds", "fails", "unseeded", "not reached", or
	// "inconclusive" when the plan itself could not finish: a lock it waited
	// for too long, a cancelled query, a lost connection. That is no verdict
	// on the migration.
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
	// sql is the .up.sql file of a SQL migration, "" for a change set.
	sql string
}

// upSQL is the .up.sql file of a SQL migration, "" for any other.
func upSQL(m fixturemigrate.MigrationFile) string {
	for _, f := range m.Files {
		if strings.HasSuffix(f, ".up.sql") {
			return f
		}
	}
	return ""
}

// runSQLMigration runs a bun SQL migration inside the plan's transaction,
// split the way bun splits it: at every "--bun:split" line, blank lines
// dropped, any other "--bun:" directive refused. It runs in a savepoint of its
// own, so a failure leaves the rest of the report readable.
func runSQLMigration(o streams, tx bun.Tx, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	queries, err := splitSQL(data)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return tx.RunInTx(o.ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		for _, q := range queries {
			// Raw: the file's SQL may hold a "?" bun would take for a
			// placeholder, and the migrator runs it as written.
			if _, err := tx.Tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
}

// splitSQL is bun's reading of a SQL migration (migrate/migration.go).
func splitSQL(data []byte) ([]string, error) {
	var queries []string
	var query strings.Builder
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if directive, ok := strings.CutPrefix(line, "--bun:"); ok {
			if directive != "split" {
				return nil, fmt.Errorf("bun: unknown directive: %q", directive)
			}
			if query.Len() > 0 {
				queries = append(queries, query.String())
				query.Reset()
			}
			continue
		}
		if strings.TrimSpace(line) != "" {
			query.WriteString(line + "\n")
		}
	}
	if query.Len() > 0 {
		queries = append(queries, query.String())
	}
	return queries, nil
}

// sqlState is the SQLSTATE of a PostgreSQL error, "" for any other error.
func sqlState(err error) string {
	var pgErr pgdriver.Error
	if errors.As(err, &pgErr) {
		return pgErr.Field('C')
	}
	return ""
}

// inconclusive reports an error that stopped the plan rather than one the
// migration would meet: waiting too long for a lock another session holds, a
// cancelled query, a serialisation failure, a lost connection. Each would
// come out differently on another try.
func inconclusive(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, driver.ErrBadConn) {
		return true
	}
	var pgErr pgdriver.Error
	if !errors.As(err, &pgErr) {
		return false
	}
	code := pgErr.Field('C')
	switch {
	case code == "55P03", code == "57014", code == "40001", code == "40P01":
		return true
	case strings.HasPrefix(code, "08"), strings.HasPrefix(code, "57P"):
		return true
	}
	return false
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
		withSQL     = fs.Bool("with-sql", false, "also run the pending SQL migrations (.up.sql) in bun's order")
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
	// A standby refuses every write, which would read as every migration
	// failing.
	var standby bool
	if err := db.QueryRowContext(o.ctx, "SELECT pg_is_in_recovery()").Scan(&standby); err != nil {
		return err
	}
	if standby {
		return fmt.Errorf("the database is a standby, which accepts no writes, so nothing can be planned " +
			"there: point plan at the primary or at a writable copy")
	}

	var targets []planTarget
	report := &planReport{Migrations: []plannedMigration{}, NotSimulated: []string{}}
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
			if up := upSQL(m); m.Fixture == nil && *withSQL && up != "" {
				targets = append(targets, planTarget{id: m.ID(), sql: up,
					after: append([]string{}, report.NotSimulated...)})
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
		if m.Result == "inconclusive" {
			return exitError{1, "the plan could not finish at " + m.ID + ", which says nothing about the " +
				"migration: " + m.Error}
		}
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
		pm.Result, pm.Kind = "succeeds", "fixture"
		if t.sql != "" {
			pm.Kind = "sql"
			if err := runSQLMigration(o, tx, t.sql); err != nil {
				pm.Result, pm.Error, failed = "fails", err.Error(), true
				if inconclusive(err) || sqlState(err) == "25001" {
					pm.Result = "inconclusive"
				}
				if sqlState(err) == "25001" {
					pm.Error += " -- it cannot run inside a transaction, so plan cannot simulate it or " +
						"what follows it; plan without -with-sql"
				}
			}
			report.Migrations = append(report.Migrations, pm)
			continue
		}
		err := fixtureapply.Apply(o.ctx, tx, t.set,
			fixtureapply.WithDryRun(),
			fixtureapply.WithLogger(func(string, ...any) {}),
			fixtureapply.WithReport(func(out fixtureapply.Outcome) {
				if out.Status == fixtureapply.StatusUnseeded {
					pm.Result = "unseeded"
				}
				pm.Changes = append(pm.Changes, out)
			}))
		if err != nil {
			pm.Result, pm.Error, failed = "fails", err.Error(), true
			if inconclusive(err) {
				pm.Result = "inconclusive"
			}
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
		kind := ""
		if m.Kind == "sql" {
			kind = " (SQL)"
		}
		switch m.Result {
		case "succeeds":
			fmt.Fprintf(o.stdout, "%s%s: would succeed\n", m.ID, kind)
		case "unseeded":
			fmt.Fprintf(o.stdout, "%s: would do nothing, the database is not seeded yet\n", m.ID)
		case "fails":
			fmt.Fprintf(o.stdout, "%s%s: would FAIL\n", m.ID, kind)
		case "inconclusive":
			fmt.Fprintf(o.stdout, "%s: could not be planned\n", m.ID)
		default:
			fmt.Fprintf(o.stdout, "%s: not reached\n", m.ID)
		}
		w := tabwriter.NewWriter(o.stdout, 0, 4, 2, ' ', 0)
		for _, c := range m.Changes {
			if c.Status == fixtureapply.StatusUnseeded {
				continue
			}
			if c.Status == fixtureapply.StatusSequence {
				fmt.Fprintf(w, "  %s\t%s\n", c.Status, c.Message)
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
		if m.Result == "fails" || m.Result == "inconclusive" {
			fmt.Fprintf(o.stdout, "  %s\n", m.Error)
			if len(m.After) > 0 {
				fmt.Fprintf(o.stdout, "  note: pending before it and not simulated: %s. If they change these "+
					"tables, the deploy can differ from this plan; plan -with-sql runs SQL migrations too\n",
					strings.Join(m.After, ", "))
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
		fmt.Fprintln(o.stdout, "\nrolled back: nothing was changed, except that an id an insert drew from "+
			"a sequence stays drawn, which only leaves a gap")
	case len(r.Migrations) > 0:
		fmt.Fprintln(o.stdout, "\nrolled back: nothing was changed")
	}
}
