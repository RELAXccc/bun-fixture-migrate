// Command bun-fixture-migrate works on the master data a bun application keeps
// in a dbfixture YAML file.
//
//	scaffold   write a starter configuration from a database
//	export     write the fixture file from a database
//	check      report what the database and the fixture file disagree about
//	generate   write the migration that brings a seeded database from the state
//	           the existing migrations leave it in, or from the live database,
//	           to the fixture file
//	baseline   record the fixture file as migrated without writing a migration
//	status     list the migrations, which ones a database has applied, and what
//	           no migration covers yet
//	plan       run the pending fixture migrations against a database inside a
//	           transaction that is rolled back, and report what each change did
//	sync       bring a database to the fixture file directly, without a
//	           migration file: a developer's, a test run's, a staging copy
//	apply      run one generated migration outside the migrator, and with
//	           -record record it as bun's migrator would
//
// Every command that reads a configuration takes -config, which defaults to
// $BUN_FIXTURE_MIGRATE_CONFIG and then to fixture-migrate.yml, and every one
// that connects takes -dsn, which wins over the configuration's database.
//
// Exit codes: 0 when there was nothing to do or the work was done, 1 on an
// error, 2 when something was refused and nothing was written, 3 when check,
// status or plan found something, or a migration apply ran failed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/dbschema"

	"github.com/uptrace/bun"
)

const usage = `bun-fixture-migrate <command> [flags]

  scaffold   write a starter configuration from a database
  export     write the fixture file from a database
  check      report what the database and the fixture file disagree about
  generate   write the bun migration for what changed in the fixture file
  baseline   record the fixture file as migrated, without writing a migration
  status     list the migrations, what a database applied, what nothing covers
  plan       dry-run the pending fixture migrations against a database
  sync       bring a development or test database to the fixture file
  apply      run one generated migration by hand, outside the migrator
  version    print the version of this binary

Run "bun-fixture-migrate <command> -h" for the flags of one command.`

// configEnv names the default for -config, for a project whose configuration
// is not where the command runs: a monorepo, a CI job, a container.
const configEnv = "BUN_FIXTURE_MIGRATE_CONFIG"

// connects are the commands that can connect to a database, which take -dsn:
// baseline does to ask whether a difference is only in how values are
// written. scaffold has a -dsn of its own, because it runs before there is a
// configuration.
var connects = map[string]bool{
	"export": true, "check": true, "generate": true, "baseline": true, "status": true, "plan": true, "sync": true,
	"apply": true,
}

func main() {
	// Interrupted, a command stops at its next query and its transaction
	// rolls back, rather than dying with a connection half used.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// streams is where a command writes, and the context it runs under. It exists
// so the tests can drive a command the way a shell does and read what it said.
type streams struct {
	ctx            context.Context
	stdout, stderr io.Writer
}

// run is main with its context, its arguments, its streams and its exit code
// handed to it.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 1
	}
	o := streams{ctx: ctx, stdout: stdout, stderr: stderr}
	commands := map[string]func(streams, []string) error{
		"generate": generate,
		"export":   export,
		"check":    check,
		"scaffold": scaffold,
		"baseline": baseline,
		"status":   status,
		"plan":     plan,
		"sync":     syncCmd,
		"apply":    applyCmd,
	}
	var err error
	switch cmd, ok := commands[args[0]]; {
	case ok:
		err = cmd(o, args[1:])
	case args[0] == "version":
		fmt.Fprintln(stdout, version())
		return 0
	case args[0] == "-h" || args[0] == "--help" || args[0] == "help":
		fmt.Fprintln(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "bun-fixture-migrate: no command %q\n\n%s\n", args[0], usage)
		return 1
	}
	// A flag set that was asked for its help has already printed it.
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return 0
	}
	// Every error is one line, with the command's name in front, whatever
	// the exit code: in a CI log it is the line that says why the job failed.
	var exit exitError
	if errors.As(err, &exit) {
		fmt.Fprintln(stderr, "bun-fixture-migrate:", oneLine(exit.message))
		return exit.code
	}
	// What the library refuses is the command's exit code 2.
	if errors.Is(err, fixturemigrate.ErrRefused) {
		fmt.Fprintln(stderr, "bun-fixture-migrate:", oneLine(err.Error()))
		return 2
	}
	fmt.Fprintln(stderr, "bun-fixture-migrate:", oneLine(err.Error()))
	return 1
}

// oneLine is a message as the last line of a failed command prints it: an
// error that arrived in several lines, from git or a library, has them joined.
func oneLine(msg string) string {
	var parts []string
	for _, line := range strings.Split(msg, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, "; ")
}

// version is what the build carries: the module version for a binary from
// "go install", the revision for one built out of a checkout.
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "bun-fixture-migrate (built without version information)"
	}
	v := info.Main.Version
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && len(setting.Value) >= 12 {
			v += " (" + setting.Value[:12] + ")"
		}
	}
	return "bun-fixture-migrate " + v
}

type exitError struct {
	code    int
	message string
}

func (e exitError) Error() string { return e.message }

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// setup is the part every command shares: the project, which reads the
// configuration and the fixture files, and where the fixture files, the
// migrations and the state file are.
type setup struct {
	p   *fixturemigrate.Project
	cfg *fixturemigrate.Config
	// fixturePaths are the fixture files, in load order, as paths from
	// where the command runs.
	fixturePaths []string
	outDir       string
	statePath    string
}

// parseFlags reads a command's flags. The flag package prints a bad flag's
// error itself, then the usage, and run would print the error a second time;
// so here the package prints nothing, -h prints the usage, and a bad flag
// comes back as the one error run prints, saying where the flags are listed.
func parseFlags(o streams, fs *flag.FlagSet, args []string) error {
	fs.SetOutput(io.Discard)
	err := fs.Parse(args)
	fs.SetOutput(o.stderr)
	switch {
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprintf(o.stderr, "Usage of %s:\n", fs.Name())
		fs.PrintDefaults()
		return err
	case err != nil:
		return fmt.Errorf("%w; \"bun-fixture-migrate %s -h\" lists its flags", err, fs.Name())
	case fs.NArg() > 0:
		return fmt.Errorf("unexpected argument %s; every option is a flag, see -h", quoteArg(fs.Arg(0)))
	}
	return nil
}

// quoteArg is a command-line argument fit to repeat in a message. A DSN left
// behind by a mistyped flag is repeated with its password masked, and one
// that cannot be read as a URL, such as user:password@host without a scheme,
// is not repeated at all.
func quoteArg(arg string) string {
	u, err := url.Parse(arg)
	switch {
	case err == nil && u.Scheme != "" && u.Host != "":
		return strconv.Quote(redact(u))
	case strings.Contains(arg, "://") || strings.Contains(arg, "@") ||
		strings.Contains(strings.ToLower(arg), "password"):
		return "that looks like a DSN (not repeated here)"
	}
	return strconv.Quote(arg)
}

func common(o streams, fs *flag.FlagSet, args []string) (*setup, error) {
	defaultConfig := "fixture-migrate.yml"
	if env := os.Getenv(configEnv); env != "" {
		defaultConfig = env
	}
	configPath := fs.String("config", defaultConfig, "configuration file; $"+configEnv+" sets the default")
	var dsn *string
	if connects[fs.Name()] {
		dsn = fs.String("dsn", "", "the database, instead of the configuration's: a URL, or env:NAME to read one "+
			"from the environment")
	}
	if err := parseFlags(o, fs, args); err != nil {
		return nil, err
	}
	p, err := fixturemigrate.LoadProject(*configPath)
	if err != nil {
		set := false
		fs.Visit(func(f *flag.Flag) { set = set || f.Name == "config" })
		if !set && os.Getenv(configEnv) != "" {
			return nil, fmt.Errorf("%w (the configuration $%s names)", err, configEnv)
		}
		return nil, err
	}
	// Everything that asks whether a database is configured, and connect,
	// read it from the configuration, so -dsn is put there.
	if dsn != nil && *dsn != "" {
		p.Config.Database = *dsn
	}
	return &setup{p: p, cfg: p.Config, fixturePaths: p.FixturePaths(), outDir: p.OutDir(),
		statePath: p.StatePath()}, nil
}

// connect opens the configured database, and fails at once when it cannot.
func (s *setup) connect(ctx context.Context) (*bun.DB, error) {
	dsn, err := resolveDSN(s.cfg.Database)
	if err != nil {
		return nil, err
	}
	return openDB(ctx, dsn)
}

// database is the configured database, connected when the library first
// queries it; see lazyDB.
func (s *setup) database() *bun.DB {
	return lazyDB(s.cfg.Database)
}

// export writes the fixture files from the database.
func export(o streams, args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	var (
		out        = fs.String("o", "", "write here instead of the fixture file of the configuration (one fixture file only)")
		stdout     = fs.Bool("stdout", false, "write to standard output")
		allColumns = fs.Bool("all-columns", false, "write every column, not only those the fixture files already use")
		asJSON     = fs.Bool("json", false, "write what was written as JSON")
	)
	s, err := common(o, fs, args)
	if err != nil {
		return err
	}
	several := len(s.fixturePaths) > 1
	if several && *out != "" {
		return fmt.Errorf("-o writes one file; with several fixture files, export writes each of them in place")
	}
	if *asJSON && *stdout {
		return fmt.Errorf("-json and -stdout both write to standard output; pass one of them")
	}
	db := s.database()
	defer db.Close()
	exp, err := s.p.Export(o.ctx, db, fixturemigrate.ExportOptions{AllColumns: *allColumns})
	if exp == nil {
		return err
	}
	if !*asJSON {
		for _, note := range exp.Notes {
			fmt.Fprintln(o.stderr, "note:", note)
		}
	}
	if err != nil && !errors.Is(err, fixturemigrate.ErrRefused) {
		return err
	}
	if !*asJSON {
		for _, f := range exp.Findings {
			fmt.Fprintln(o.stderr, string(f.Kind)+":", f.String())
		}
		// An export is written from the database, not edited into the file
		// it replaces: what the file said in comments is gone from it.
		// Written elsewhere, with -o or to standard output, it replaces
		// nothing.
		for i, n := range exp.DroppedComments {
			if n > 0 && *out == "" && !*stdout {
				fmt.Fprintf(o.stderr, "note: the export does not keep the comments of %s: %s not in it; "+
					"put back the ones to keep before committing\n", s.fixturePaths[i], plural(n, "comment line"))
			}
		}
	}
	if err != nil {
		if *asJSON {
			if werr := writeJSON(o.stdout, exp); werr != nil {
				return werr
			}
		}
		return err
	}
	if *stdout {
		for i, f := range exp.Files {
			data := f.Data
			if several {
				data = append([]byte("# ==> "+s.cfg.Fixtures[i]+" <==\n"), data...)
			}
			if err := writeOut(o.stdout, data); err != nil {
				return err
			}
		}
		return nil
	}
	switch {
	case *out != "":
		if err := fixturemigrate.WriteFileAtomic(*out, exp.Files[0].Data, 0o644); err != nil {
			return err
		}
		exp.Written = append(exp.Written, *out)
	case len(s.fixturePaths) == 0:
		return fmt.Errorf("no fixture file in the configuration and no -o")
	default:
		if _, err := exp.Write(); err != nil {
			for _, path := range exp.Written {
				fmt.Fprintln(o.stdout, "wrote", path)
			}
			return err
		}
	}
	if *asJSON {
		return writeJSON(o.stdout, exp)
	}
	for _, path := range exp.Written {
		fmt.Fprintln(o.stdout, "wrote", path)
	}
	return nil
}

// check reports the drift between the database and the fixture file.
func check(o streams, args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "write the report as JSON")
	s, err := common(o, fs, args)
	if err != nil {
		return err
	}
	db := s.database()
	defer db.Close()
	report, err := s.p.Check(o.ctx, db)
	if err != nil {
		return err
	}
	if *asJSON {
		if err := writeJSON(o.stdout, report); err != nil {
			return err
		}
	} else {
		for _, line := range report.Lines() {
			fmt.Fprintln(o.stdout, line)
		}
		if report.Agree && report.Drifted() {
			fmt.Fprintf(o.stdout, "\nthe database and %s agree; the policy makes the findings above warnings\n",
				s.cfg.FixtureLabel())
		}
	}
	if !report.Agree {
		return exitError{3, "the database and " + s.cfg.FixtureLabel() + " do not agree"}
	}
	return nil
}

// scaffold writes a starter configuration from a database.
func scaffold(o streams, args []string) error {
	fs := flag.NewFlagSet("scaffold", flag.ContinueOnError)
	var (
		dsn        = fs.String("dsn", "", "PostgreSQL DSN (there is no configuration file yet)")
		schema     = fs.String("schema", "public", "schema to read")
		only       = fs.String("tables", "", "comma-separated tables to include, default all of them")
		out        = fs.String("o", "", "write here instead of standard output")
		migrations = fs.String("migrations-table", "bun_migrations", "the table the migrator records migrations in, "+
			"which is left out and written into the configuration")
		locks = fs.String("migration-locks-table", "bun_migration_locks", "the table the migrator keeps its lock in, "+
			"which is left out and written into the configuration")
	)
	if err := parseFlags(o, fs, args); err != nil {
		return err
	}
	if *dsn == "" {
		*dsn = os.Getenv("DATABASE_URL")
	}
	if *dsn == "" {
		return fmt.Errorf("pass -dsn, or set DATABASE_URL")
	}
	resolved, err := resolveDSN(*dsn)
	if err != nil {
		return err
	}
	db, err := openDB(o.ctx, resolved)
	if err != nil {
		return err
	}
	defer db.Close()
	var tables map[string]*dbschema.Table
	opts := fixturemigrate.ScaffoldOptions{MigrationsTable: *migrations, MigrationLocksTable: *locks}
	err = fixturemigrate.ReadOnly(o.ctx, db, func(tx bun.Tx) error {
		if tables, err = dbschema.Load(o.ctx, tx, *schema); err != nil {
			return err
		}
		read, err := fixturemigrate.LoadScaffoldOptions(o.ctx, tx, *schema)
		opts.Partitions, opts.Triggers = read.Partitions, read.Triggers
		return err
	})
	if err != nil {
		return err
	}
	var wanted []string
	if *only != "" {
		wanted = strings.Split(*only, ",")
		for i := range wanted {
			wanted[i] = strings.TrimSpace(wanted[i])
		}
	}
	// A table asked for that is not proposed is refused rather than left
	// out without a word: a typo, or a table of another schema.
	proposed := map[string]bool{}
	for _, n := range fixturemigrate.ScaffoldTables(tables, nil, *schema, opts) {
		proposed[n] = true
	}
	for _, t := range wanted {
		q := t
		if !strings.Contains(q, ".") {
			q = *schema + "." + q
		}
		switch {
		case proposed[q]:
		case tables[q] == nil:
			return fmt.Errorf("-tables names %s, which is not a table of schema %s", t, *schema)
		case opts.Partitions[q]:
			return fmt.Errorf("-tables names %s, a partition: its rows are its partitioned table's, which is the "+
				"model", t)
		default:
			return fmt.Errorf("-tables names %s, which is the migrator's own table and no master data", t)
		}
	}
	if len(fixturemigrate.ScaffoldTables(tables, wanted, *schema, opts)) == 0 {
		return fmt.Errorf("schema %s has no table to propose as a model: it does not exist, the role cannot see "+
			"its tables, or it holds only the migrator's; pass -schema", *schema)
	}
	data := fixturemigrate.Scaffold(tables, wanted, *schema, opts)
	if *out == "" {
		return writeOut(o.stdout, data)
	}
	if _, err := os.Stat(*out); err == nil {
		return fmt.Errorf("%s exists; scaffold writes a first draft and does not overwrite one", *out)
	}
	if err := fixturemigrate.WriteFileAtomic(*out, data, 0o644); err != nil {
		return err
	}
	fmt.Fprintln(o.stderr, "wrote", *out)
	fmt.Fprintln(o.stderr, "read it: which tables are master data, the natural keys, the model names and the "+
		"seed guard table are guesses")
	return nil
}

// writeOut writes what a command makes to standard output. Output that did
// not all arrive, at a full disk or a closed pipe, fails the command: an exit
// code of 0 would have the script that redirected it carry on with half a
// file.
func writeOut(w io.Writer, data []byte) error {
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write to standard output: %w", err)
	}
	return nil
}
