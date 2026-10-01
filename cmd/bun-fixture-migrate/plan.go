package main

import (
	"bufio"
	"bytes"
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
	"github.com/RELAXccc/bun-fixture-migrate/internal/pgerr"

	"github.com/uptrace/bun"
)

type planReport struct {
	Migrations []plannedMigration `json:"migrations"`
	// NotSimulated are the pending migrations this tool did not write and
	// cannot run: schema changes, backfills, anything hand-written.
	NotSimulated []string `json:"not_simulated"`
	// Notes say why a migration -with-sql would have run was not simulated.
	Notes []string `json:"notes"`
	// Problems are what makes the migrations directory unsafe to deploy as
	// it stands, as status reports them: two migrations bun records under
	// one name, a generated file that no longer reads as one.
	Problems []string `json:"problems"`
	// RowsLocked is how many rows the fixture migrations wrote, each locked
	// against every other writer until the plan rolled back, and
	// LockedSeconds how long the plan's transaction was open, which is how
	// long a session writing one of them, or using a table a SQL migration
	// altered, had to wait.
	RowsLocked    int64   `json:"rows_locked"`
	LockedSeconds float64 `json:"locked_seconds"`
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
	// Notes are what the result alone does not say: why a plan could not
	// tell, or where the deploy can differ from the plan.
	Notes []string `json:"notes,omitempty"`
}

type planTarget struct {
	id    string
	set   fixturechange.Set
	after []string
	// sql is the .up.sql file of a SQL migration, "" for a change set.
	// queries are its statements as bun reads them, or readErr is why bun
	// cannot read it, which fails the migration when the deploy reaches it.
	sql     string
	queries []string
	readErr error
	notes   []string
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

// sqlTarget reads a pending SQL migration for -with-sql. skip is why it is
// not to be run at all: with migrate.WithTemplateData, bun renders a SQL file
// as a Go text/template before it runs it, and plan has neither the data nor
// the functions, so a file that holds "{{" would run as SQL bun never sends.
func sqlTarget(m fixturemigrate.MigrationFile, path string) (t planTarget, skip string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return planTarget{}, "", err
	}
	if bytes.Contains(data, []byte("{{")) {
		return planTarget{}, fmt.Sprintf("%s holds \"{{\", which bun renders as a Go template before running it "+
			"when the migrator is built WithTemplateData; plan has not got the template's data, so it did not run "+
			"it, and what it changes is not in this plan", filepath.Base(path)), nil
	}
	t = planTarget{id: m.ID(), sql: path}
	t.queries, t.readErr = readSQL(data)
	for _, q := range t.queries {
		if blankLineInLiteral(q) {
			t.notes = append(t.notes, "a quoted string or dollar-quoted body in it holds a blank line, which bun "+
				"v1.2.18 keeps, as this plan did; bun after v1.2.18 drops blank lines from a SQL migration, "+
				"which would change that text")
			break
		}
	}
	// A sequence is outside every transaction: what the plan does to one,
	// the rollback leaves.
	for _, q := range t.queries {
		if lower := strings.ToLower(q); strings.Contains(lower, "setval") || strings.Contains(lower, "nextval") {
			t.notes = append(t.notes, "it calls setval or nextval, which no rollback undoes: the sequence it "+
				"moves stays moved in the database this plan ran against")
			break
		}
	}
	return t, "", nil
}

// readSQL is bun v1.2.18's reading of a SQL migration (migrate/migration.go,
// newSQLMigrationFunc), line by line through a bufio.Scanner: a "--bun:split"
// line ends a statement, any other "--bun:" line is an error, and every other
// line is kept with a newline after it, a blank one too.
//
// It is copied rather than approximated because the differences matter. The
// scanner fails on a line longer than 64 KiB, and bun then runs none of the
// file. bun's master branch drops blank lines, which v1.2.18 keeps, and a
// blank line inside a string literal is part of the value.
func readSQL(data []byte) ([]string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	var queries []string
	var query []byte
	for scanner.Scan() {
		b := scanner.Bytes()
		const prefix = "--bun:"
		if bytes.HasPrefix(b, []byte(prefix)) {
			b = b[len(prefix):]
			if bytes.Equal(b, []byte("split")) {
				queries = append(queries, string(query))
				query = query[:0]
				continue
			}
			return nil, fmt.Errorf("bun: unknown directive: %q", b)
		}
		query = append(query, b...)
		query = append(query, '\n')
	}
	if len(query) > 0 {
		queries = append(queries, string(query))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return queries, nil
}

// blankLineInLiteral reports whether a line that is blank, or only white space,
// falls inside a quoted string, a quoted identifier or a dollar-quoted body of
// a statement, where removing it changes the text. A blank line between two
// statements or inside a comment changes nothing.
func blankLineInLiteral(query string) bool {
	const (
		code = iota
		single
		escaped // an E'...' string, where a backslash escapes
		ident
		dollar
		block
	)
	state, depth, tag := code, 0, ""
	isIdent := func(c byte) bool {
		return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
	}
	// A statement as readSQL leaves it ends in a newline, which ends the last
	// line rather than starting another.
	for _, line := range strings.Split(strings.TrimSuffix(query, "\n"), "\n") {
		if strings.TrimSpace(line) == "" && state != code && state != block {
			return true
		}
		for i := 0; i < len(line); i++ {
			c := line[i]
			switch state {
			case code:
				switch {
				case c == '-' && strings.HasPrefix(line[i:], "--"):
					i = len(line)
				case c == '/' && strings.HasPrefix(line[i:], "/*"):
					state, depth = block, 1
					i++
				case c == '\'':
					state = single
					if i > 0 && (line[i-1] == 'E' || line[i-1] == 'e') && (i == 1 || !isIdent(line[i-2])) {
						state = escaped
					}
				case c == '"':
					state = ident
				case c == '$' && (i == 0 || !isIdent(line[i-1])):
					j := i + 1
					for j < len(line) && isIdent(line[j]) && line[j] != '$' {
						j++
					}
					if j < len(line) && line[j] == '$' && (j == i+1 || line[i+1] < '0' || line[i+1] > '9') {
						state, tag = dollar, line[i:j+1]
						i = j
					}
				}
			case single, escaped:
				switch {
				case c == '\\' && state == escaped:
					i++
				case c == '\'' && i+1 < len(line) && line[i+1] == '\'':
					i++
				case c == '\'':
					state = code
				}
			case ident:
				if c == '"' {
					state = code
				}
			case dollar:
				if strings.HasPrefix(line[i:], tag) {
					state = code
					i += len(tag) - 1
				}
			case block:
				switch {
				case strings.HasPrefix(line[i:], "/*"):
					depth++
					i++
				case strings.HasPrefix(line[i:], "*/"):
					if depth--; depth == 0 {
						state = code
					}
					i++
				}
			}
		}
	}
	return false
}

// runSQLMigration runs a bun SQL migration inside the plan's transaction, in a
// savepoint of its own so a failure leaves the rest of the report readable.
// bun runs a .tx.up.sql in one transaction and any other file a statement at
// a time on a connection of its own, each statement committing by itself; the
// deferred constraints are checked at the same points.
func runSQLMigration(o streams, tx bun.Tx, t planTarget) error {
	if t.readErr != nil {
		return t.readErr
	}
	inTx := strings.HasSuffix(t.sql, ".tx.up.sql")
	return tx.RunInTx(o.ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		for _, q := range t.queries {
			// bun skips a statement of nothing but white space, which a
			// "--bun:split" on the first line leaves behind.
			if strings.TrimSpace(q) == "" {
				continue
			}
			// Raw: the file's SQL may hold a "?" bun would take for a
			// placeholder, and the migrator runs it as written.
			if _, err := tx.Tx.ExecContext(ctx, q); err != nil {
				return err
			}
			if !inTx {
				if err := commitPoint(ctx, tx); err != nil {
					return err
				}
			}
		}
		if inTx {
			return commitPoint(ctx, tx)
		}
		return nil
	})
}

// commitPoint has PostgreSQL check, where the deploy would commit, the
// constraints it otherwise checks only at COMMIT: a DEFERRABLE INITIALLY
// DEFERRED foreign key, unique or exclusion constraint, or constraint
// trigger. The plan never commits, so without this a migration that breaks
// one is reported as succeeding and fails in the deploy.
//
// SET CONSTRAINTS ALL IMMEDIATE runs in a savepoint that is always rolled
// back, which puts every constraint back in the mode it had, so the next
// migration runs with the deferral the deploy gives it. Rolling back also
// leaves the checks to be made again at the next commit point; that repeats
// a check that passed, and changes no verdict.
func commitPoint(ctx context.Context, tx bun.Tx) error {
	if _, err := tx.ExecContext(ctx, "SAVEPOINT bfm_commit_point"); err != nil {
		return err
	}
	_, checkErr := tx.ExecContext(ctx, "SET CONSTRAINTS ALL IMMEDIATE")
	if _, err := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT bfm_commit_point"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT bfm_commit_point"); err != nil {
		return err
	}
	if checkErr != nil {
		return fmt.Errorf("when it commits, where PostgreSQL checks the constraints it defers: %w", checkErr)
	}
	return nil
}

// judge says what an error a migration met in the plan means for the deploy:
// "fails" when the deploy meets it too, "inconclusive" when it comes from the
// plan itself, which runs everything in one transaction and rolls it back. The
// note says why, when the error alone does not.
func judge(err error) (result, note string) {
	switch code := pgerr.State(err); {
	case errors.Is(err, bufio.ErrTooLong):
		return "fails", "bun reads a SQL migration a line at a time and stops at a line longer than 64 KiB " +
			"before running any of it, so the deploy fails here; unless the migrator is built " +
			"WithMarkAppliedOnSuccess(true), bun then keeps the migration recorded as applied and never runs " +
			"it, so its change is lost. Break the line up"
	case code == pgerr.ActiveSQLTransaction:
		return "inconclusive", "it cannot run inside a transaction, so plan cannot simulate it or what " +
			"follows it; plan without -with-sql"
	case code == pgerr.InsufficientPrivilege, code == pgerr.ReadOnlyTransaction:
		// The plan's role, not the migration: a read-only role, one without
		// the grants, or one a row-level security policy limits. The deploy
		// connects as the role the application migrates as.
		return "inconclusive", "the role plan connects as cannot write here; plan as the role the deploy uses"
	case code == pgerr.UnsafeNewEnumValue:
		return "inconclusive", "it uses an enum value a migration before it in this plan added, and " +
			"PostgreSQL lets no transaction use an enum value it added itself. The plan runs every migration " +
			"in one transaction; the deploy commits each migration, and each statement of a SQL migration " +
			"without .tx. in its name, so it can succeed where the plan cannot. It fails in the deploy too only " +
			"when one transaction adds the value and uses it. Plan again once the migration that adds it is applied"
	case inconclusive(err):
		return "inconclusive", ""
	}
	return "fails", ""
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
	code := pgerr.State(err)
	switch {
	case code == pgerr.LockNotAvailable, code == pgerr.QueryCanceled, code == pgerr.SerializationFailure,
		code == pgerr.DeadlockDetected:
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
	// The files named, read before anything connects.
	var fileTargets []planTarget
	for _, path := range files {
		if !strings.HasSuffix(path, ".go") {
			return fmt.Errorf("plan -file takes a fixture migration generate wrote, a .go file, and %s is not "+
				"one; a pending SQL migration is planned with the others under -with-sql", path)
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		set, isFixture, err := fixturemigrate.ReadChangeSet(src)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if !isFixture {
			return fmt.Errorf("%s holds no fixture change set: plan -file takes a fixture migration generate wrote", path)
		}
		fileTargets = append(fileTargets, planTarget{id: strings.TrimSuffix(filepath.Base(path), ".go"), set: set})
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
	report := &planReport{Migrations: []plannedMigration{}, NotSimulated: []string{}, Notes: []string{},
		Problems: []string{}}
	if len(files) > 0 {
		targets = fileTargets
	} else {
		if s.outDir == "" {
			return fmt.Errorf("no out directory in the configuration; name the files with -file")
		}
		ms, err := fixturemigrate.ReadMigrations(s.outDir)
		if err != nil {
			return fmt.Errorf("the migrations directory: %w", err)
		}
		report.Problems = append(report.Problems, ms.Problems...)
		// The history status checks: a fixture migration the state file does
		// not include, or the one it includes last gone from the directory.
		// Each is a deploy whose migrations were not generated one after
		// another, however well each of them plans.
		report.Problems = append(report.Problems, s.p.LineageProblems(ms)...)
		var applied map[string]fixturemigrate.Applied
		err = fixturemigrate.ReadOnly(o.ctx, db, func(tx bun.Tx) error {
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
				t, skip, err := sqlTarget(m, up)
				if err != nil {
					return err
				}
				if skip != "" {
					report.NotSimulated = append(report.NotSimulated, m.ID())
					report.Notes = append(report.Notes, skip)
					continue
				}
				t.after = append([]string{}, report.NotSimulated...)
				targets = append(targets, t)
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

	var failures []string
	skipped := 0
	for _, m := range report.Migrations {
		if m.Result == "inconclusive" {
			return exitError{1, "the plan could not finish at " + m.ID + ", which says nothing about the " +
				"migration: " + m.Error}
		}
		if m.Result == "fails" {
			failures = append(failures, m.ID+" would fail, and so would the deploy")
		}
		for _, c := range m.Changes {
			if c.Status == fixtureapply.StatusSkipped {
				skipped++
			}
		}
	}
	if *strict && skipped > 0 {
		failures = append(failures, plural(skipped, "change")+" would be skipped")
	}
	// A plan of a directory bun cannot run as it stands is no plan of the
	// deploy, however well each migration in it went.
	if n := len(report.Problems); n > 0 {
		failures = append(failures, plural(n, "problem")+" in the migrations directory")
	}
	if len(failures) > 0 {
		return exitError{3, strings.Join(failures, "; ")}
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
	// A transaction that starts read only, from default_transaction_read_only
	// on the role or in the DSN, refuses every write, which would read as
	// every migration failing, as on a standby.
	var readOnly string
	if err := tx.QueryRowContext(o.ctx, "SHOW transaction_read_only").Scan(&readOnly); err != nil {
		return err
	}
	if readOnly == "on" {
		return fmt.Errorf("the database starts every transaction of this connection read only " +
			"(default_transaction_read_only), so nothing can be planned there: plan as the role the deploy uses, " +
			"which needs the rights the migrations need")
	}
	start := time.Now()
	if lockTimeout > 0 {
		if _, err := tx.ExecContext(o.ctx, fmt.Sprintf("SET LOCAL lock_timeout = %d", lockTimeout.Milliseconds())); err != nil {
			return err
		}
	}
	failed := false
	for _, t := range targets {
		pm := plannedMigration{ID: t.id, Kind: "fixture", After: t.after, Changes: []fixtureapply.Outcome{}}
		if t.sql != "" {
			pm.Kind = "sql"
		}
		if failed {
			pm.Result = "not reached"
			report.Migrations = append(report.Migrations, pm)
			continue
		}
		pm.Result, pm.Notes = "succeeds", append([]string(nil), t.notes...)
		var err error
		if t.sql != "" {
			err = runSQLMigration(o, tx, t)
		} else {
			err = fixtureapply.Apply(o.ctx, tx, t.set,
				fixtureapply.WithDryRun(),
				fixtureapply.WithLogger(func(string, ...any) {}),
				fixtureapply.WithReport(func(out fixtureapply.Outcome) {
					if out.Status == fixtureapply.StatusUnseeded {
						pm.Result = "unseeded"
					}
					pm.Changes = append(pm.Changes, out)
				}))
			if err == nil {
				// bun's migrator commits each fixture migration on its own.
				err = commitPoint(o.ctx, tx)
			}
		}
		if err != nil {
			var note string
			pm.Result, note = judge(err)
			pm.Error, failed = err.Error(), true
			// A table or column the database lacks is what a schema migration
			// the plan did not run would have created.
			if code := pgerr.State(err); pm.Result == "fails" && len(t.after) > 0 &&
				(code == pgerr.UndefinedTable || code == pgerr.UndefinedColumn) {
				pm.Result, note = "inconclusive", "it needs a table or a column this database does not have, "+
					"and a migration that runs before it in the deploy but was not simulated can create it, so the "+
					"deploy can succeed where the plan cannot"
			}
			if note != "" {
				pm.Notes = append(pm.Notes, note)
			}
		}
		report.Migrations = append(report.Migrations, pm)
	}
	if err := o.ctx.Err(); err != nil {
		return err
	}
	err = tx.Rollback()
	report.LockedSeconds = time.Since(start).Round(time.Millisecond).Seconds()
	for _, m := range report.Migrations {
		for _, c := range m.Changes {
			if c.Status == fixtureapply.StatusApplied {
				report.RowsLocked += c.Rows
			}
		}
	}
	return err
}

// failureShown is true when the migration's error is the message of a change
// already printed as failed, which it usually is: printing it again under it
// only doubles the line.
func failureShown(m plannedMigration) bool {
	for _, c := range m.Changes {
		if c.Status == fixtureapply.StatusFailed && c.Message != "" && strings.HasSuffix(m.Error, c.Message) {
			return true
		}
	}
	return false
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
			if !failureShown(m) {
				fmt.Fprintf(o.stdout, "  %s\n", m.Error)
			}
			if len(m.After) > 0 {
				fmt.Fprintf(o.stdout, "  note: pending before it and not simulated: %s. If they change these "+
					"tables, the deploy can differ from this plan; plan -with-sql runs SQL migrations too\n",
					strings.Join(m.After, ", "))
			}
		}
		for _, n := range m.Notes {
			fmt.Fprintf(o.stdout, "  note: %s\n", n)
		}
	}
	if len(r.NotSimulated) > 0 {
		fmt.Fprintf(o.stdout, "\nnot simulated, not fixture migrations: %s\n", strings.Join(r.NotSimulated, ", "))
	}
	for _, n := range r.Notes {
		fmt.Fprintf(o.stdout, "note: %s\n", n)
	}
	if len(r.Problems) > 0 {
		fmt.Fprintln(o.stdout, "\nproblems")
		for _, p := range r.Problems {
			fmt.Fprintln(o.stdout, "  "+p)
		}
	}
	inserted, ranSQL := false, false
	for _, m := range r.Migrations {
		if m.Kind == "sql" && m.Result != "not reached" {
			ranSQL = true
		}
		for _, c := range m.Changes {
			if c.Status == fixtureapply.StatusApplied && c.Kind == fixturechange.Insert {
				inserted = true
			}
		}
	}
	switch {
	case ranSQL:
		fmt.Fprintln(o.stdout, "\nrolled back: nothing was changed, except sequences, which no rollback undoes: "+
			"an id an insert drew stays drawn, and a value a SQL migration gave a sequence stays")
	case inserted:
		fmt.Fprintln(o.stdout, "\nrolled back: nothing was changed, except that an id an insert drew from "+
			"a sequence stays drawn, which only leaves a gap")
	case len(r.Migrations) > 0:
		fmt.Fprintln(o.stdout, "\nrolled back: nothing was changed")
	}
	// Planning against a live database is not free: what the plan wrote, it
	// held locked until it rolled back.
	held := time.Duration(r.LockedSeconds * float64(time.Second)).Round(time.Millisecond)
	switch {
	case ranSQL:
		fmt.Fprintf(o.stdout, "for %s it held locked the %s it wrote and what the SQL migrations locked, "+
			"most ALTER TABLE a whole table; other sessions using them waited\n", held, plural(int(r.RowsLocked), "row"))
	case r.RowsLocked > 0:
		fmt.Fprintf(o.stdout, "for %s it held locked the %s it wrote; other sessions writing them waited\n",
			held, plural(int(r.RowsLocked), "row"))
	}
}
