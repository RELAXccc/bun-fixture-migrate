// Command bun-fixture-migrate works on the master data a bun application keeps
// in a dbfixture YAML file.
//
//	generate   write the migration that brings a seeded database from an older
//	           revision of the fixture file, or from the live database, to the
//	           file in the working tree
//	export     write the fixture file from a database
//	check      report what the database and the fixture file disagree about
//	scaffold   write a starter configuration from a database
//
// Exit codes: 0 when there was nothing to do or the work was done, 1 on an
// error, 2 when a difference was refused and nothing was written, 3 when a
// check found something.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/dbschema"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

const usage = `bun-fixture-migrate <command> [flags]

  generate   write the bun migration for what changed in the fixture file
  export     write the fixture file from a database
  check      report what the database and the fixture file disagree about
  scaffold   write a starter configuration from a database
  version    print the version of this binary

Run "bun-fixture-migrate <command> -h" for the flags of one command.`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// streams is where a command writes. It exists so the tests can drive a
// command the way a shell does and read what it said.
type streams struct {
	stdout, stderr io.Writer
}

// run is main with its arguments, its streams and its exit code handed to it.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 1
	}
	o := streams{stdout: stdout, stderr: stderr}
	var err error
	switch args[0] {
	case "generate":
		err = generate(o, args[1:])
	case "export":
		err = export(o, args[1:])
	case "check":
		err = check(o, args[1:])
	case "scaffold":
		err = scaffold(o, args[1:])
	case "version":
		fmt.Fprintln(stdout, version())
		return 0
	case "-h", "--help", "help":
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
	var exit exitError
	if errors.As(err, &exit) {
		fmt.Fprintln(stderr, exit.message)
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
// where the fixture file is.
type setup struct {
	cfg         *fixturemigrate.Config
	root        string
	fixturePath string
}

func common(o streams, fs *flag.FlagSet, args []string) (*setup, error) {
	fs.SetOutput(o.stderr)
	configPath := fs.String("config", "fixture-migrate.yml", "configuration file")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	cfg, err := fixturemigrate.LoadConfig(*configPath)
	if err != nil {
		return nil, err
	}
	s := &setup{cfg: cfg, root: filepath.Dir(*configPath)}
	if cfg.Fixture != "" {
		s.fixturePath = filepath.Join(s.root, cfg.Fixture)
	}
	return s, nil
}

// connect opens the configured database. A DSN written as "env:NAME" is read
// from the environment, so the password is not in the repository.
func (s *setup) connect() (*bun.DB, error) {
	dsn := s.cfg.Database
	if name, ok := strings.CutPrefix(dsn, "env:"); ok {
		dsn = os.Getenv(name)
		if dsn == "" {
			return nil, fmt.Errorf("the configuration reads the database DSN from %s, which is not set", name)
		}
	}
	if dsn == "" {
		return nil, fmt.Errorf("no database in the configuration file; this command needs one")
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to the database: %w", err)
	}
	return db, nil
}

func (s *setup) fixtureSnapshot(data []byte, source string) (*fixturemigrate.Snapshot, error) {
	doc, err := fixturemigrate.ParseDoc(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	return fixturemigrate.FixtureSnapshot(s.cfg, doc, source)
}

func (s *setup) readFixture() (*fixturemigrate.Snapshot, error) {
	if s.fixturePath == "" {
		return nil, fmt.Errorf("no fixture file in the configuration")
	}
	data, err := os.ReadFile(s.fixturePath)
	if err != nil {
		return nil, err
	}
	return s.fixtureSnapshot(data, s.cfg.Fixture)
}

// generate writes the migration.
func generate(o streams, args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	var (
		name         = fs.String("name", "", "short name for the migration, required")
		base         = fs.String("base", "HEAD", "git revision to diff the fixture file against")
		oldPath      = fs.String("old", "", "read the base state from this file instead of git")
		fromDB       = fs.Bool("from-db", false, "diff the database against the fixture file instead of diffing two revisions of the file")
		out          = fs.String("out", "", "directory for the generated file (default: the out of the configuration)")
		dryRun       = fs.Bool("dry-run", false, "print the file instead of writing it")
		allowPartial = fs.Bool("allow-partial", false, "write the changes that were accepted even when others were refused")
		noLint       = fs.Bool("no-lint", false, "do not check the fixture file against the database's column defaults")
	)
	s, err := common(o, fs, args)
	if err != nil {
		return err
	}
	head, err := s.readFixture()
	if err != nil {
		return err
	}

	var old *fixturemigrate.Snapshot
	if *fromDB {
		if *oldPath != "" {
			return fmt.Errorf("-from-db and -old ask for two different base states")
		}
		db, err := s.connect()
		if err != nil {
			return err
		}
		defer db.Close()
		if old, err = databaseSnapshot(db, s.cfg, head); err != nil {
			return err
		}
		if err := lint(o, db, s.cfg, head, *noLint); err != nil {
			return err
		}
	} else {
		var data []byte
		var source string
		if *oldPath != "" {
			source = *oldPath
			data, err = os.ReadFile(*oldPath)
		} else {
			data, err = gitShow(s.fixturePath, *base)
			source = *base + ":" + s.cfg.Fixture
		}
		if err != nil {
			return err
		}
		if old, err = s.fixtureSnapshot(data, source); err != nil {
			return err
		}
		if s.cfg.Database != "" && !*noLint {
			db, err := s.connect()
			if err != nil {
				return err
			}
			defer db.Close()
			if err := lint(o, db, s.cfg, head, false); err != nil {
				return err
			}
		}
	}

	res, err := fixturemigrate.Compute(s.cfg, old, head)
	if err != nil {
		return err
	}
	for _, line := range res.Summary() {
		fmt.Fprintln(o.stdout, line)
	}
	for _, r := range res.Refusals {
		fmt.Fprintln(o.stderr, "refused:", r.String())
	}
	if len(res.Changes) == 0 {
		if len(res.Refusals) > 0 {
			return refused(len(res.Refusals))
		}
		fmt.Fprintf(o.stdout, "nothing changed in %s since %s\n", s.cfg.Fixture, res.Base)
		return nil
	}
	if len(res.Refusals) > 0 && !*allowPartial {
		return refused(len(res.Refusals))
	}
	if *name == "" {
		return fmt.Errorf("-name is required")
	}
	stamp := time.Now().UTC().Format(fixturemigrate.Stamp)
	src, err := fixturemigrate.Render(s.cfg, *name, stamp, res)
	if err != nil {
		return err
	}
	if *dryRun {
		o.stdout.Write(src)
		return nil
	}
	dir := *out
	if dir == "" {
		dir = filepath.Join(s.root, s.cfg.Out)
	}
	target := filepath.Join(dir, fixturemigrate.FileName(stamp, *name))
	if err := os.WriteFile(target, src, 0o644); err != nil {
		return err
	}
	fmt.Fprintln(o.stdout, "wrote", target)
	fmt.Fprintln(o.stdout, "read it, then run your migrations")
	return nil
}

// lint checks the fixture file against what the database says about its own
// columns. A lint that could not run is never reported as a lint that found
// nothing: the connection error comes back as an error.
func lint(o streams, db *bun.DB, cfg *fixturemigrate.Config, snap *fixturemigrate.Snapshot, skip bool) error {
	if skip {
		return nil
	}
	tables, err := dbschema.Load(context.Background(), db, cfg.Schema)
	if err != nil {
		return err
	}
	before := len(snap.Findings)
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
func databaseSnapshot(db *bun.DB, cfg *fixturemigrate.Config, head *fixturemigrate.Snapshot) (*fixturemigrate.Snapshot, error) {
	ctx := context.Background()
	tables, err := dbschema.Load(ctx, db, cfg.Schema)
	if err != nil {
		return nil, err
	}
	columns := map[string][]string{}
	for model, cols := range head.Columns {
		columns[model] = cols
	}
	return fixturemigrate.DatabaseSnapshot(ctx, db, cfg, tables, fixturemigrate.SnapshotOptions{
		Columns: columns, Order: head.Order})
}

// export writes the fixture file from the database.
func export(o streams, args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	var (
		out    = fs.String("o", "", "write here instead of the fixture file of the configuration")
		stdout = fs.Bool("stdout", false, "write to standard output")
	)
	s, err := common(o, fs, args)
	if err != nil {
		return err
	}
	db, err := s.connect()
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	tables, err := dbschema.Load(ctx, db, s.cfg.Schema)
	if err != nil {
		return err
	}
	snap, err := fixturemigrate.DatabaseSnapshot(ctx, db, s.cfg, tables, fixturemigrate.SnapshotOptions{})
	if err != nil {
		return err
	}
	fixturemigrate.LintZeroDefaults(s.cfg, snap, tables)
	fixturemigrate.LintNullDefaults(s.cfg, snap, tables)
	mode, findings := s.cfg.Worst(snap.Findings)

	header := []string{
		"Exported by bun-fixture-migrate from a live database on " + time.Now().UTC().Format(time.RFC3339) + ".",
		"Models are in dependency order; references name the row they point at, not its id.",
	}
	for _, f := range findings {
		header = append(header, "", string(f.Kind)+": "+f.String())
	}
	data, err := fixturemigrate.Export(s.cfg, snap, tables, header)
	if err != nil {
		return err
	}
	for _, f := range findings {
		fmt.Fprintln(o.stderr, string(f.Kind)+":", f.String())
	}
	if mode == fixturemigrate.ModeError {
		return exitError{3, fmt.Sprintf(
			"%s, nothing written: this export would not reproduce the database it was taken from. "+
				"Fix them, or set the policy to warn to write it anyway", plural(len(findings), "problem"))}
	}
	if *stdout {
		o.stdout.Write(data)
		return nil
	}
	target := *out
	if target == "" {
		target = s.fixturePath
	}
	if target == "" {
		return fmt.Errorf("no fixture file in the configuration and no -o")
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		return err
	}
	fmt.Fprintln(o.stdout, "wrote", target)
	return nil
}

// check reports the drift between the database and the fixture file.
func check(o streams, args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	s, err := common(o, fs, args)
	if err != nil {
		return err
	}
	head, err := s.readFixture()
	if err != nil {
		return err
	}
	db, err := s.connect()
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	tables, err := dbschema.Load(ctx, db, s.cfg.Schema)
	if err != nil {
		return err
	}
	fixturemigrate.LintColumns(s.cfg, head, tables)
	fixturemigrate.LintZeroDefaults(s.cfg, head, tables)
	fixturemigrate.LintNullDefaults(s.cfg, head, tables)
	database, err := databaseSnapshot(db, s.cfg, head)
	if err != nil {
		return err
	}
	res, err := fixturemigrate.Check(s.cfg, database, head)
	if err != nil {
		return err
	}
	mode, findings := s.cfg.Worst(res.Findings)
	res.Findings = findings
	for _, line := range res.Lines() {
		fmt.Fprintln(o.stdout, line)
	}
	if !res.Drifted() {
		return nil
	}
	if mode == fixturemigrate.ModeError || len(res.Changes) > 0 || len(res.Refusals) > 0 {
		return exitError{3, "the database and " + s.cfg.Fixture + " do not agree"}
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
	fs.SetOutput(o.stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dsn == "" {
		*dsn = os.Getenv("DATABASE_URL")
	}
	if *dsn == "" {
		return fmt.Errorf("pass -dsn, or set DATABASE_URL")
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(*dsn))), pgdialect.New())
	defer db.Close()
	tables, err := dbschema.Load(context.Background(), db, *schema)
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
		o.stdout.Write(data)
		return nil
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		return err
	}
	fmt.Fprintln(o.stderr, "wrote", *out)
	fmt.Fprintln(o.stderr, "read it: the natural keys and the model names are guesses")
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
	rel, err := filepath.Rel(strings.TrimSpace(string(top)), abs)
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
