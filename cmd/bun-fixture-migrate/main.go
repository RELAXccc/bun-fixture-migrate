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
//
// Every command that reads a configuration takes -config, which defaults to
// $BUN_FIXTURE_MIGRATE_CONFIG and then to fixture-migrate.yml, and every one
// that connects takes -dsn, which wins over the configuration's database.
//
// Exit codes: 0 when there was nothing to do or the work was done, 1 on an
// error, 2 when something was refused and nothing was written, 3 when check,
// status or plan found something.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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
  version    print the version of this binary

Run "bun-fixture-migrate <command> -h" for the flags of one command.`

// configEnv names the default for -config, for a project whose configuration
// is not where the command runs: a monorepo, a CI job, a container.
const configEnv = "BUN_FIXTURE_MIGRATE_CONFIG"

// connects are the commands that can connect to a database, which take -dsn.
// baseline never does, and scaffold has a -dsn of its own because it runs
// before there is a configuration.
var connects = map[string]bool{
	"export": true, "check": true, "generate": true, "status": true, "plan": true, "sync": true,
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
		fmt.Fprintln(stderr, "bun-fixture-migrate:", exit.message)
		return exit.code
	}
	fmt.Fprintln(stderr, "bun-fixture-migrate:", err)
	return 1
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

func refused(n int) error {
	return exitError{2, fmt.Sprintf(
		"%s refused, nothing written; write them yourself or re-run with -allow-partial", plural(n, "change"))}
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// setup is the part every command shares: read the configuration, work out
// where the fixture file, the migrations and the state file are.
type setup struct {
	cfg  *fixturemigrate.Config
	root string
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
// that cannot be read as a URL is not repeated at all.
func quoteArg(arg string) string {
	u, err := url.Parse(arg)
	switch {
	case err == nil && u.Scheme != "" && u.Host != "":
		return strconv.Quote(redact(u))
	case strings.Contains(arg, "://") || strings.Contains(strings.ToLower(arg), "password"):
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
	cfg, err := fixturemigrate.LoadConfig(*configPath)
	if err != nil {
		return nil, err
	}
	// Everything that asks whether a database is configured, and connect,
	// read it from the configuration, so -dsn is put there.
	if dsn != nil && *dsn != "" {
		cfg.Database = *dsn
	}
	s := &setup{cfg: cfg, root: filepath.Dir(*configPath)}
	for _, f := range cfg.Fixtures {
		s.fixturePaths = append(s.fixturePaths, filepath.Join(s.root, f))
	}
	if cfg.Out != "" {
		s.outDir = filepath.Join(s.root, cfg.Out)
	}
	if cfg.State != "" {
		s.statePath = filepath.Join(s.root, cfg.State)
	}
	return s, nil
}

// connect opens the configured database.
func (s *setup) connect(ctx context.Context) (*bun.DB, error) {
	dsn, err := resolveDSN(s.cfg.Database)
	if err != nil {
		return nil, err
	}
	return openDB(ctx, dsn)
}

// snapshotOf resolves fixture files the way dbfixture loads them: in order,
// one scope of anchors.
func (s *setup) snapshotOf(files []fixturemigrate.FixtureFile, source string) (*fixturemigrate.Snapshot, error) {
	doc, err := fixturemigrate.ParseFiles(files)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	return fixturemigrate.FixtureSnapshot(s.cfg, doc, source)
}

// readFixture reads the fixture files, as bytes for the state file and as one
// snapshot for everything else.
func (s *setup) readFixture() ([]fixturemigrate.FixtureFile, *fixturemigrate.Snapshot, error) {
	if len(s.fixturePaths) == 0 {
		return nil, nil, fmt.Errorf("no fixture file in the configuration")
	}
	files := make([]fixturemigrate.FixtureFile, 0, len(s.fixturePaths))
	for i, path := range s.fixturePaths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, err
		}
		files = append(files, fixturemigrate.FixtureFile{Path: s.cfg.Fixtures[i], Data: data})
	}
	snap, err := s.snapshotOf(files, s.cfg.FixtureLabel())
	return files, snap, err
}

// gitFiles reads the fixture files as of a revision. A file the revision does
// not have yet is empty there: at that revision, nothing loaded it.
func (s *setup) gitFiles(rev string) ([]fixturemigrate.FixtureFile, error) {
	files := make([]fixturemigrate.FixtureFile, 0, len(s.fixturePaths))
	for i, path := range s.fixturePaths {
		data, err := gitShow(path, rev)
		if err != nil {
			if missingAt(err) {
				data = nil
			} else {
				return nil, err
			}
		}
		files = append(files, fixturemigrate.FixtureFile{Path: s.cfg.Fixtures[i], Data: data})
	}
	return files, nil
}

// missingAt reports git's answer for a path the revision does not have.
func missingAt(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "exists on disk, but not in") || strings.Contains(msg, "does not exist in")
}

// canonical has PostgreSQL respell every value of the snapshots as the columns
// hold them, so they compare with each other and with the database the way
// the database compares values. Findings of all but the first snapshot are
// dropped: they are about base states, which are history.
func canonical(o streams, db bun.IDB, cfg *fixturemigrate.Config, tables map[string]*dbschema.Table,
	snaps ...*fixturemigrate.Snapshot) error {

	for i, snap := range snaps {
		before := len(snap.Findings)
		if err := fixturemigrate.Canonicalize(o.ctx, db, cfg, snap, tables); err != nil {
			return err
		}
		if i > 0 {
			snap.Findings = snap.Findings[:before]
		}
	}
	return nil
}

// lint checks the fixture file against what the database says about its own
// columns. A lint that could not run is never reported as a lint that found
// nothing: the connection error comes back as an error.
func lint(o streams, cfg *fixturemigrate.Config, snap *fixturemigrate.Snapshot, tables map[string]*dbschema.Table,
	before int) error {

	fixturemigrate.LintColumns(cfg, snap, tables)
	fixturemigrate.LintZeroDefaults(cfg, snap, tables)
	fixturemigrate.LintNullDefaults(cfg, snap, tables)
	mode, findings := cfg.Worst(snap.Findings[before:])
	if len(findings) == 0 {
		return nil
	}
	for _, f := range findings {
		fmt.Fprintln(o.stderr, string(f.Kind)+":", f.String())
	}
	if mode == fixturemigrate.ModeError {
		return exitError{2, fmt.Sprintf("%s in the fixture file, nothing written", plural(len(findings), "problem"))}
	}
	return nil
}

// databaseSnapshot reads the database, limited to the columns the fixture file
// writes. A column no fixture row mentions is not master data, so a difference
// in it is not drift.
func databaseSnapshot(ctx context.Context, db bun.IDB, cfg *fixturemigrate.Config,
	tables map[string]*dbschema.Table, head *fixturemigrate.Snapshot) (*fixturemigrate.Snapshot, error) {

	columns := map[string][]string{}
	for model, cols := range head.Columns {
		columns[model] = cols
	}
	return fixturemigrate.DatabaseSnapshot(ctx, db, cfg, tables, fixturemigrate.SnapshotOptions{
		Columns: columns, Order: head.Order})
}

// export writes the fixture files from the database.
func export(o streams, args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	var (
		out    = fs.String("o", "", "write here instead of the fixture file of the configuration (one fixture file only)")
		stdout = fs.Bool("stdout", false, "write to standard output")
	)
	s, err := common(o, fs, args)
	if err != nil {
		return err
	}
	several := len(s.fixturePaths) > 1
	if several && *out != "" {
		return fmt.Errorf("-o writes one file; with several fixture files, export writes each of them in place")
	}
	// With several files, each model goes back into the file that holds it.
	var current []fixturemigrate.FixtureFile
	for i, path := range s.fixturePaths {
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		current = append(current, fixturemigrate.FixtureFile{Path: s.cfg.Fixtures[i], Data: data})
	}
	db, err := s.connect(o.ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	var outputs [][]byte
	var mode fixturemigrate.Mode
	var findings []fixturemigrate.Finding
	err = readOnly(o.ctx, db, func(tx bun.Tx) error {
		tables, err := dbschema.Load(o.ctx, tx, s.cfg.Schemas()...)
		if err != nil {
			return err
		}
		snap, err := fixturemigrate.DatabaseSnapshot(o.ctx, tx, s.cfg, tables, fixturemigrate.SnapshotOptions{})
		if err != nil {
			return err
		}
		fixturemigrate.LintZeroDefaults(s.cfg, snap, tables)
		fixturemigrate.LintNullDefaults(s.cfg, snap, tables)
		mode, findings = s.cfg.Worst(snap.Findings)
		// No time, nor anything else that differs between two exports of
		// one database: CI diffs an export against the committed file, and
		// a header that always changes is a diff that always fails.
		header := []string{
			"Exported by bun-fixture-migrate from a live database.",
			"Models are in dependency order; references name the row they point at, not its id.",
		}
		for _, f := range findings {
			header = append(header, "", string(f.Kind)+": "+f.String())
		}
		if several {
			outputs, err = fixturemigrate.ExportFiles(s.cfg, snap, tables, header, current)
			return err
		}
		data, err := fixturemigrate.Export(s.cfg, snap, tables, header)
		outputs = [][]byte{data}
		return err
	})
	if err != nil {
		return err
	}
	for _, f := range findings {
		fmt.Fprintln(o.stderr, string(f.Kind)+":", f.String())
	}
	if mode == fixturemigrate.ModeError {
		return exitError{2, fmt.Sprintf(
			"%s, nothing written: this export would not reproduce the database it was taken from. "+
				"Fix them, or set the policy to warn to write it anyway", plural(len(findings), "problem"))}
	}
	if *stdout {
		for i, data := range outputs {
			if several {
				data = append([]byte("# ==> "+s.cfg.Fixtures[i]+" <==\n"), data...)
			}
			if err := writeOut(o.stdout, data); err != nil {
				return err
			}
		}
		return nil
	}
	targets := s.fixturePaths
	if *out != "" {
		targets = []string{*out}
	}
	if len(targets) == 0 {
		return fmt.Errorf("no fixture file in the configuration and no -o")
	}
	for i, target := range targets {
		if err := fixturemigrate.WriteFileAtomic(target, outputs[i], 0o644); err != nil {
			return err
		}
		fmt.Fprintln(o.stdout, "wrote", target)
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
	_, head, err := s.readFixture()
	if err != nil {
		return err
	}
	db, err := s.connect(o.ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	var res *fixturemigrate.CheckResult
	err = readOnly(o.ctx, db, func(tx bun.Tx) error {
		tables, err := dbschema.Load(o.ctx, tx, s.cfg.Schemas()...)
		if err != nil {
			return err
		}
		if err := canonical(o, tx, s.cfg, tables, head); err != nil {
			return err
		}
		fixturemigrate.LintColumns(s.cfg, head, tables)
		fixturemigrate.LintZeroDefaults(s.cfg, head, tables)
		fixturemigrate.LintNullDefaults(s.cfg, head, tables)
		database, err := databaseSnapshot(o.ctx, tx, s.cfg, tables, head)
		if err != nil {
			return err
		}
		res, err = fixturemigrate.Check(s.cfg, database, head)
		return err
	})
	if err != nil {
		return err
	}
	_, res.Findings = s.cfg.Worst(res.Findings)
	agree := res.Agree(s.cfg)
	if *asJSON {
		if err := writeJSON(o.stdout, checkJSON(s.cfg, res)); err != nil {
			return err
		}
	} else {
		for _, line := range res.Lines() {
			fmt.Fprintln(o.stdout, line)
		}
		if agree && res.Drifted() {
			fmt.Fprintf(o.stdout, "\nthe database and %s agree; the policy makes the findings above warnings\n",
				s.cfg.FixtureLabel())
		}
	}
	if !agree {
		return exitError{3, "the database and " + s.cfg.FixtureLabel() + " do not agree"}
	}
	return nil
}

// scaffold writes a starter configuration from a database.
func scaffold(o streams, args []string) error {
	fs := flag.NewFlagSet("scaffold", flag.ContinueOnError)
	var (
		dsn    = fs.String("dsn", "", "PostgreSQL DSN (there is no configuration file yet)")
		schema = fs.String("schema", "public", "schema to read")
		only   = fs.String("tables", "", "comma-separated tables to include, default all of them")
		out    = fs.String("o", "", "write here instead of standard output")
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
	err = readOnly(o.ctx, db, func(tx bun.Tx) error {
		tables, err = dbschema.Load(o.ctx, tx, *schema)
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
	data := fixturemigrate.Scaffold(tables, wanted, *schema)
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
	fmt.Fprintln(o.stderr, "read it: the natural keys and the model names are guesses")
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

// gitShow reads a file as of a revision. The path is resolved inside the
// repository the file itself belongs to, so it does not matter where the
// command was started.
func gitShow(path, rev string) ([]byte, error) {
	dir := filepath.Dir(path)
	top, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%s is not in a git repository, pass -old instead: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// Resolve symlinks on both sides: a temporary directory on macOS is a
	// symlink, and git reports the resolved top level.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	topDir := strings.TrimSpace(string(top))
	if resolved, err := filepath.EvalSymlinks(topDir); err == nil {
		topDir = resolved
	}
	rel, err := filepath.Rel(topDir, abs)
	if err != nil {
		return nil, err
	}
	data, err := git(dir, "show", rev+":"+filepath.ToSlash(rel))
	if err != nil {
		return nil, fmt.Errorf("git show %s:%s: %w", rev, filepath.ToSlash(rel), err)
	}
	return data, nil
}

func git(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, errors.New(msg)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}
