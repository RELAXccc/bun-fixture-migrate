package fixturemigrate

// The commands as a library. Everything bun-fixture-migrate does from the
// command line, a Project does from Go, through the same functions: the
// command is a thin layer over them that parses flags, prints and picks an
// exit code.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
	"github.com/RELAXccc/bun-fixture-migrate/internal/pgerr"

	"github.com/uptrace/bun"
)

// Project is a configuration and the fixture files it names: what every
// command starts from, for a program that runs them from Go. A test helper
// checks a test database against the fixture files with Check, a development
// server brings its database to them with Sync, an admin tool exports
// production with Export, and a CI program runs Status, Generate and Baseline.
//
// Each method does what the command of its name does, with the same checks,
// the same refusals and the same results; Generate, Baseline and Export write
// nothing until their result's Write is called. A method that reads a
// database takes a bun.IDB: given a *bun.DB or a bun.Conn it reads in a
// REPEATABLE READ, READ ONLY transaction of its own, given a bun.Tx it reads in
// that transaction, as ReadOnly says. Where the command can work without a
// database, nil is offline.
//
// A Project is not safe for concurrent use while ReadFiles or a Write changes
// its fixture files.
type Project struct {
	// Config is the configuration, its defaults filled in. A program may
	// change it before calling a method, as the command's -dsn changes
	// Database.
	Config *Config
	// Dir is what the paths in the configuration are relative to: the
	// directory of the configuration file.
	Dir string

	// files are the fixture files as last read, in load order, and errs why
	// one could not be read; a file that does not exist has no Data.
	files []FixtureFile
	errs  []error
}

// LoadProject reads a configuration file and the fixture files it names.
// Paths in it are relative to it, as they are for the command.
//
// A fixture file that cannot be read is not an error here: Export replaces
// it, and every other method that needs it says why it could not be read.
func LoadProject(configPath string) (*Project, error) {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return nil, err
	}
	return NewProject(cfg, filepath.Dir(configPath))
}

// NewProject is LoadProject for a configuration built in code: dir is what its
// paths are relative to. It calls cfg.Prepare.
func NewProject(cfg *Config, dir string) (*Project, error) {
	if err := cfg.Prepare(); err != nil {
		return nil, err
	}
	p := &Project{Config: cfg, Dir: dir}
	// A file that does not read is the error of the methods that need it.
	_ = p.ReadFiles()
	return p, nil
}

// ReadFiles reads the fixture files again, for a program that keeps a Project
// while they change on disk. It returns what Files would.
func (p *Project) ReadFiles() error {
	p.files, p.errs = nil, nil
	for i, path := range p.FixturePaths() {
		data, err := os.ReadFile(path)
		p.files = append(p.files, FixtureFile{Path: p.Config.Fixtures[i], Data: data})
		p.errs = append(p.errs, err)
	}
	_, err := p.Files()
	return err
}

// Files are the fixture files as they were read, in load order, each under
// its path as the configuration spells it. The error is why one of them could
// not be read, or that the configuration names none.
func (p *Project) Files() ([]FixtureFile, error) {
	if len(p.Config.Fixtures) == 0 {
		return nil, errors.New("no fixture file in the configuration")
	}
	for _, err := range p.errs {
		if err != nil {
			return nil, err
		}
	}
	return slices.Clone(p.files), nil
}

// FixturePaths are the fixture files, in load order, as paths from the working
// directory: the paths of the configuration joined to Dir.
func (p *Project) FixturePaths() []string {
	out := make([]string, 0, len(p.Config.Fixtures))
	for _, f := range p.Config.Fixtures {
		out = append(out, filepath.Join(p.Dir, f))
	}
	return out
}

// OutDir is the migrations directory, "" when the configuration has none.
func (p *Project) OutDir() string {
	if p.Config.Out == "" {
		return ""
	}
	return filepath.Join(p.Dir, p.Config.Out)
}

// StatePath is the state file, "" when the configuration has none.
func (p *Project) StatePath() string {
	if p.Config.State == "" {
		return ""
	}
	return filepath.Join(p.Dir, p.Config.State)
}

// ErrRefused is what every refusal wraps: something a method will not do as
// things stand, and the command's exit code 2. The error is a *RefusedError,
// which says what to do about it, and a method that refuses still returns the
// result it worked out, for a program to show.
var ErrRefused = errors.New("refused")

// ErrFindings is wrapped by a refusal because of a finding the policy makes an
// error, such as two rows sharing a natural key, or a zero written into a
// column whose default bun writes instead.
var ErrFindings = errors.New("a finding the policy makes an error")

// ErrLineage is wrapped by a refusal because the migrations directory and the
// state file's history disagree: a fixture migration the state does not
// include, generated on another branch or written by hand and not recorded, or
// the one the state includes last gone from the directory. See the runbook,
// "the state file conflicts in a merge".
var ErrLineage = errors.New("the migrations directory and the state file's history disagree")

// ErrNameRequired is what Generate returns when there is a migration to write
// and GenerateOptions.Name is empty.
var ErrNameRequired = errors.New("the migration needs a name")

// RefusedError is a refusal. errors.Is finds ErrRefused in it, and
// ErrFindings, ErrLineage or ErrUnmigrated when that is why; it unwraps to
// what it was refused over, such as ErrSyncRefused or ErrStateConflict.
type RefusedError struct {
	// Message says what was refused, and what to do about it.
	Message string
	// Refusals are the differences that need a hand-written migration, when
	// they are why.
	Refusals []Refusal
	// Findings are the findings, when a finding the policy makes an error is
	// why: those it makes errors and those it makes warnings, as the command
	// lists them.
	Findings []Finding
	// Problems say, one sentence each, what is wrong with the migrations
	// directory and the state file's history of it, when that is why.
	Problems []string

	reason error
	err    error
}

func (e *RefusedError) Error() string { return e.Message }

// Is reports whether target is ErrRefused, or the reason of the refusal.
func (e *RefusedError) Is(target error) bool {
	return target == ErrRefused || (e.reason != nil && target == e.reason)
}

func (e *RefusedError) Unwrap() error { return e.err }

// errNoDatabase is what a method that cannot work without a database says
// when it is given none.
func errNoDatabase(method string) error {
	return fmt.Errorf("%s needs a database: pass a *bun.DB, a bun.Conn or a bun.Tx", method)
}

// orNil is db, or nil for a nil *bun.DB, *bun.Conn or *bun.Tx, which an
// interface holding it does not compare equal to: a program that declared
// var db *bun.DB and connected nothing means offline.
func orNil(db bun.IDB) bun.IDB {
	switch d := db.(type) {
	case *bun.DB:
		if d == nil {
			return nil
		}
	case *bun.Conn:
		if d == nil {
			return nil
		}
	case *bun.Tx:
		if d == nil {
			return nil
		}
	}
	return db
}

// readSavepoint is the savepoint ReadOnly reads under in a caller's
// transaction.
const readSavepoint = "bun_fixture_migrate_read"

// ReadOnly runs fn the way every command that reads a database does.
//
// Given a *bun.DB or a bun.Conn, fn runs in a REPEATABLE READ, READ ONLY
// transaction of its own, which is rolled back. READ ONLY is PostgreSQL's
// promise, not this tool's: nothing is written even when a model's where
// clause calls a function that would. REPEATABLE READ has every query of fn
// see one snapshot, so a row inserted between reading two tables cannot turn
// up as a reference to a row that is not there.
//
// Given a bun.Tx, fn runs in that transaction, in a savepoint that is rolled
// back, which leaves the transaction as it was, settings and all; whether it
// reads from one snapshot is up to the isolation level it was begun with.
//
// Either way the session is prepared as PrepareSession says, and row_security
// is off, so a role a row-level security policy limits gets an error rather
// than the rows the policy lets through. Read through such a policy, master
// data is missing rows nothing says are missing: an export writes a file
// without them, and a migration generated from it deletes them everywhere
// else.
func ReadOnly(ctx context.Context, db bun.IDB, fn func(tx bun.Tx) error) error {
	switch d := orNil(db).(type) {
	case nil:
		return errors.New("no database to read")
	case bun.Tx:
		return readInTx(ctx, d, fn)
	case *bun.Tx:
		return readInTx(ctx, *d, fn)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var readOnly string
	if err := tx.QueryRowContext(ctx, "SHOW transaction_read_only").Scan(&readOnly); err != nil {
		return err
	}
	if readOnly != "on" {
		return errors.New("the database did not start a read-only transaction; refusing to go on")
	}
	if err := prepareRead(ctx, tx); err != nil {
		return err
	}
	return rowSecurity(fn(tx))
}

// readInTx is ReadOnly in a transaction the caller began.
func readInTx(ctx context.Context, tx bun.Tx, fn func(tx bun.Tx) error) (err error) {
	if _, err := tx.ExecContext(ctx, "SAVEPOINT "+readSavepoint); err != nil {
		return err
	}
	defer func() {
		// Even when ctx is done: a savepoint not rolled back to would leave
		// the caller's transaction failed, or with this session's settings.
		bg := context.WithoutCancel(ctx)
		_, rerr := tx.ExecContext(bg, "ROLLBACK TO SAVEPOINT "+readSavepoint)
		if rerr == nil {
			_, rerr = tx.ExecContext(bg, "RELEASE SAVEPOINT "+readSavepoint)
		}
		if err == nil && rerr != nil {
			err = rerr
		}
	}()
	if err := prepareRead(ctx, tx); err != nil {
		return err
	}
	return rowSecurity(fn(tx))
}

// prepareRead fixes the session for reading master data: how values are
// spelled, and no row-level security quietly leaving rows out.
func prepareRead(ctx context.Context, tx bun.Tx) error {
	if err := PrepareSession(ctx, tx); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "SELECT set_config('row_security', 'off', true)")
	return err
}

// rlsTable is the table PostgreSQL names when row_security is off and a
// policy would filter a query.
var rlsTable = regexp.MustCompile(`row-level security policy for table "(.*)"`)

// rowSecurity turns PostgreSQL's refusal to read past a row-level security
// policy into what to do about it.
func rowSecurity(err error) error {
	if pgerr.State(err) != pgerr.InsufficientPrivilege {
		return err
	}
	m := rlsTable.FindStringSubmatch(pgerr.Message(err))
	if m == nil {
		return err
	}
	return fmt.Errorf("row-level security hides rows of %s from this role, so what it reads is not all the "+
		"master data; connect as a role that bypasses row-level security (BYPASSRLS) or owns the table "+
		"without FORCE ROW LEVEL SECURITY", m[1])
}

// snapshotOf resolves fixture files the way dbfixture loads them: in order,
// one scope of anchors.
func (p *Project) snapshotOf(files []FixtureFile, source string) (*Snapshot, error) {
	doc, err := ParseFiles(files)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	return FixtureSnapshot(p.Config, doc, source)
}

// head is the fixture files, as bytes for the state file and as one snapshot
// for everything else.
func (p *Project) head() ([]FixtureFile, *Snapshot, error) {
	files, err := p.Files()
	if err != nil {
		return nil, nil, err
	}
	snap, err := p.snapshotOf(files, p.Config.FixtureLabel())
	return files, snap, err
}

// canonical has PostgreSQL respell every value of the snapshots as the columns
// hold them, so they compare with each other and with the database the way
// the database compares values. Findings of all but the first snapshot are
// dropped: they are about base states, which are history.
func canonical(ctx context.Context, db bun.IDB, cfg *Config, tables map[string]*dbschema.Table,
	snaps ...*Snapshot) error {

	for i, snap := range snaps {
		before := len(snap.Findings)
		if err := Canonicalize(ctx, db, cfg, snap, tables); err != nil {
			return err
		}
		if i > 0 {
			snap.Findings = snap.Findings[:before]
		}
	}
	return nil
}

// lintAll checks the fixture files against what the database says about its
// own columns: columns it does not have, zeros and nulls bun replaces with a
// column default.
func lintAll(cfg *Config, snap *Snapshot, tables map[string]*dbschema.Table) {
	LintColumns(cfg, snap, tables)
	LintZeroDefaults(cfg, snap, tables)
	LintNullDefaults(cfg, snap, tables)
}

// refuseFindings refuses files with a finding the policy makes an error, such
// as two rows sharing a key: generate refuses to migrate them, and a state
// that records them only moves the refusal to the next change. The findings
// the policy does not ignore come back either way.
func refuseFindings(cfg *Config, snap *Snapshot) ([]Finding, error) {
	mode, findings := cfg.Worst(snap.Findings)
	if mode != ModeError {
		return findings, nil
	}
	n := 0
	for _, f := range findings {
		if cfg.ModeOf(f) == ModeError {
			n++
		}
	}
	return findings, &RefusedError{reason: ErrFindings, Findings: findings, Message: fmt.Sprintf(
		"%s in the fixture file that the policy makes errors, nothing written. Fix them, or set the policy to warn",
		plural(n, "finding"))}
}

// databaseSnapshot reads the database, limited to the columns the fixture file
// writes. A column no fixture row mentions is not master data, so a difference
// in it is not drift. A column the table does not have is not read: the lint
// reports it as an unknown column, which is a finding, not a query that fails.
func databaseSnapshot(ctx context.Context, db bun.IDB, cfg *Config, tables map[string]*dbschema.Table,
	head *Snapshot) (*Snapshot, error) {

	if err := checkModels(cfg, tables); err != nil {
		return nil, err
	}
	columns := map[string][]string{}
	for model, cols := range head.Columns {
		m := cfg.Models[model]
		table := tables[cfg.QualifiedTable(m)]
		columns[model] = []string{}
		for _, col := range cols {
			if _, ok := table.Column(col); ok || table == nil {
				columns[model] = append(columns[model], col)
			}
		}
	}
	return DatabaseSnapshot(ctx, db, cfg, tables, SnapshotOptions{Columns: columns, Order: head.Order})
}

// checkModels says, before a query names one, that a column the configuration
// makes a model's key or a reference is not in its table, which PostgreSQL
// would otherwise say from inside a query nobody wrote. A model whose table is
// missing is left to the snapshot, which says what it is.
func checkModels(cfg *Config, tables map[string]*dbschema.Table) error {
	for _, name := range cfg.ModelNames() {
		m := cfg.Models[name]
		table := tables[cfg.QualifiedTable(m)]
		if table == nil {
			continue
		}
		key := append([]string{}, m.Key...)
		for _, group := range m.KeyAnyOf {
			key = append(key, group...)
		}
		for _, col := range key {
			if _, ok := table.Column(col); !ok {
				return fmt.Errorf("model %q: its key is [%s], and %s has no column %s. Set key to the columns that "+
					"tell two of its rows apart, those of a unique index on the table; a table with none is no master "+
					"data this tool can migrate", name, strings.Join(key, ", "), table.Qualified(), col)
			}
		}
		refs := make([]string, 0, len(m.References))
		for col := range m.References {
			refs = append(refs, col)
		}
		sort.Strings(refs)
		for _, col := range refs {
			if slices.Contains(m.Ignore, col) || slices.Contains(m.Derived, col) {
				continue
			}
			if _, ok := table.Column(col); !ok {
				return fmt.Errorf("model %q: references names %s, and %s has no such column; fix the name, or take "+
					"it out of references", name, col, table.Qualified())
			}
		}
	}
	return nil
}

// CheckReport is what Check found: what the database and the fixture files
// disagree about. Encoded as JSON it is what check -json prints.
type CheckReport struct {
	// Agree is check's verdict, which its exit code says: no difference,
	// nothing generate would refuse, and no finding the policy makes an
	// error. A finding the policy makes a warning is reported and leaves it
	// true.
	Agree bool
	// Diff is the comparison, the database on the left and the fixture files
	// on the right: an insert is a row only the files have, a delete one only
	// the database has. Its Refusals are what generate would refuse, and its
	// Warnings what the policy lets a migration carry on past.
	Diff *Result
	// Findings are those of both sides the policy does not ignore: a natural
	// key two rows share, a zero written against a column default, a column
	// the table does not have. Config.ModeOf says the level of each.
	Findings []Finding

	cfg *Config
}

// Drifted reports whether anything at all was found, warnings included.
func (r *CheckReport) Drifted() bool {
	return (&CheckResult{Result: r.Diff, Findings: r.Findings}).Drifted()
}

// Lines is the report as check prints it: what is wrong with the files
// first, then what the database and the files disagree about.
func (r *CheckReport) Lines() []string {
	return (&CheckResult{Result: r.Diff, Findings: r.Findings}).Lines()
}

// Check compares the database with the fixture files, as the check command
// does, and changes nothing: it respells the files' values as the columns
// hold them, lints them against the columns, reads the master data the files
// describe, and compares.
func (p *Project) Check(ctx context.Context, db bun.IDB) (*CheckReport, error) {
	db = orNil(db)
	_, head, err := p.head()
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, errNoDatabase("Check")
	}
	var res *CheckResult
	err = ReadOnly(ctx, db, func(tx bun.Tx) error {
		tables, err := dbschema.Load(ctx, tx, p.Config.Schemas()...)
		if err != nil {
			return err
		}
		if err := canonical(ctx, tx, p.Config, tables, head); err != nil {
			return err
		}
		lintAll(p.Config, head, tables)
		database, err := databaseSnapshot(ctx, tx, p.Config, tables, head)
		if err != nil {
			return err
		}
		res, err = Check(p.Config, database, head)
		return err
	})
	if err != nil {
		return nil, err
	}
	return newCheckReport(p.Config, res), nil
}

// newCheckReport is the report of a comparison, with the findings the policy
// does not ignore.
func newCheckReport(cfg *Config, res *CheckResult) *CheckReport {
	_, res.Findings = cfg.Worst(res.Findings)
	return &CheckReport{Agree: res.Agree(cfg), Diff: res.Result, Findings: res.Findings, cfg: cfg}
}

// ExportOptions steers Export.
type ExportOptions struct {
	// AllColumns writes every column of every model, not only those the
	// fixture files use.
	AllColumns bool
}

// Exported is what Export wrote from the database, before it is written to
// disk. Encoded as JSON it is what export -json prints, Written included.
type Exported struct {
	// Files are the fixture files as export writes them, in load order, each
	// under its path as the configuration spells it.
	Files []FixtureFile
	// DroppedComments counts, for each of Files, the comment lines of the
	// file it replaces that it does not have: an export is written anew from
	// the database, not edited into the file.
	DroppedComments []int
	// Findings are what the database's values turned up that the policy does
	// not ignore: a zero or a null bun would replace with the column default,
	// so the files would not load back as the database. Any the policy makes
	// an error refuse the export.
	Findings []Finding
	// Notes say what the files do not: that every column and id is
	// exported, because the fixture files there now do not read.
	Notes []string
	// Written are the files Write wrote, as paths from the working directory.
	Written []string

	p       *Project
	refused error
}

// Export reads the database into fixture files, as the export command does,
// and writes nothing: Write does. For a model the fixture files hold, it
// writes the columns they use, and the ids only when their rows name them; a
// model they do not hold yet is written whole. A fixture file that does not
// exist or does not read is replaced whole, with a note.
//
// It refuses, with the result, an export that would not load back as the
// database it was taken from, such as one writing a zero bun replaces with the
// column's default, unless the policy makes that a warning.
func (p *Project) Export(ctx context.Context, db bun.IDB, opts ExportOptions) (*Exported, error) {
	db = orNil(db)
	// With several files, each model goes back into the file that holds it.
	current := slices.Clone(p.files)
	for _, err := range p.errs {
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	exp := &Exported{p: p}
	// What the files hold now decides which columns and ids the export
	// writes. Files it cannot read are being replaced, as they are.
	head, err := p.snapshotOf(current, p.Config.FixtureLabel())
	if err != nil {
		exp.Notes = append(exp.Notes, fmt.Sprintf("every column and id is exported, because the fixture files do not "+
			"read: %v", err))
		head = nil
	}
	if db == nil {
		return exp, errNoDatabase("Export")
	}
	several := len(p.Config.Fixtures) > 1
	var outputs [][]byte
	var mode Mode
	err = ReadOnly(ctx, db, func(tx bun.Tx) error {
		tables, err := dbschema.Load(ctx, tx, p.Config.Schemas()...)
		if err != nil {
			return err
		}
		if err := checkModels(p.Config, tables); err != nil {
			return err
		}
		var snapOpts SnapshotOptions
		if !opts.AllColumns {
			snapOpts.Columns = exportedColumns(p.Config, tables, head)
		}
		snap, err := DatabaseSnapshot(ctx, tx, p.Config, tables, snapOpts)
		if err != nil {
			return err
		}
		LintZeroDefaults(p.Config, snap, tables)
		LintNullDefaults(p.Config, snap, tables)
		mode, exp.Findings = p.Config.Worst(snap.Findings)
		dropIDs(p.Config, tables, head, snap)
		// No time, nor anything else that differs between two exports of
		// one database: CI diffs an export against the committed file, and
		// a header that always changes is a diff that always fails.
		header := []string{
			"Exported by bun-fixture-migrate from a live database.",
			"Models are in dependency order; references name the row they point at, not its id.",
		}
		for _, f := range exp.Findings {
			header = append(header, "", string(f.Kind)+": "+f.String())
		}
		if several {
			outputs, err = ExportFiles(p.Config, snap, tables, header, current)
			return err
		}
		data, err := Export(p.Config, snap, tables, header)
		outputs = [][]byte{data}
		return err
	})
	if err != nil {
		return exp, err
	}
	for i, data := range outputs {
		path, dropped := "", 0
		if i < len(current) {
			path = current[i].Path
			dropped = droppedComments(current[i].Data, data)
		}
		exp.Files = append(exp.Files, FixtureFile{Path: path, Data: data})
		exp.DroppedComments = append(exp.DroppedComments, dropped)
	}
	if mode == ModeError {
		exp.refused = &RefusedError{reason: ErrFindings, Findings: exp.Findings, Message: fmt.Sprintf(
			"%s, nothing written: this export would not reproduce the database it was taken from. "+
				"Fix them, or set the policy to warn to write it anyway", plural(len(exp.Findings), "problem"))}
		return exp, exp.refused
	}
	return exp, nil
}

// Write writes the exported files over the fixture files, each atomically,
// and has the Project read them as they are now. It returns the paths it
// wrote, which Written holds too.
func (e *Exported) Write() ([]string, error) {
	if e.refused != nil {
		return nil, e.refused
	}
	if len(e.p.Config.Fixtures) == 0 {
		return nil, errors.New("no fixture file in the configuration")
	}
	paths := e.p.FixturePaths()
	if len(paths) != len(e.Files) {
		return nil, fmt.Errorf("the export holds %d fixture files and the configuration names %d now",
			len(e.Files), len(paths))
	}
	// The Project reads what is on disk now, whatever got written.
	defer func() { _ = e.p.ReadFiles() }()
	for i, f := range e.Files {
		if err := WriteFileAtomic(paths[i], f.Data, 0o644); err != nil {
			return e.Written, err
		}
		e.Written = append(e.Written, paths[i])
	}
	return e.Written, nil
}

// droppedComments counts the comment lines of a fixture file that an export of
// it does not have. An export writes the file anew from the database, so a
// comment, whole-line or after a value, is not carried over.
func droppedComments(old, exported []byte) int {
	kept := map[string]bool{}
	for _, line := range strings.Split(string(exported), "\n") {
		kept[strings.TrimSpace(line)] = true
	}
	n := 0
	for _, line := range strings.Split(string(old), "\n") {
		line = strings.TrimSpace(line)
		if !kept[line] && (strings.HasPrefix(line, "#") || strings.Contains(line, " #")) {
			n++
		}
	}
	return n
}

// exportedColumns is the columns an export writes of each model the fixture
// files hold: those the files use, and the ref column, which references to the
// model name rows by; DatabaseSnapshot adds the key. A column the files never
// wrote is not master data, and exported it would be a difference generate
// refuses, a column written on one side and left out on the other. A model
// the files do not hold yet is exported whole.
func exportedColumns(cfg *Config, tables map[string]*dbschema.Table, head *Snapshot) map[string][]string {
	if head == nil {
		return nil
	}
	out := map[string][]string{}
	for model, cols := range head.Columns {
		m := cfg.Models[model]
		table := tables[cfg.QualifiedTable(m)]
		if len(cols) == 0 || table == nil {
			continue
		}
		out[model] = append([]string{}, cols...)
		if _, ok := table.Column(m.Ref); ok {
			out[model] = append(out[model], m.Ref)
		}
	}
	return out
}

// dropIDs takes the ids out of the export of every model whose ids the
// fixture files leave to the database. The ids of the database exported from
// mean nothing in another: written into a file that had none, they are an id
// change of every row, and a delete guarded on one of them misses its row
// everywhere else. A model the files do not hold yet keeps its ids when they
// come from a sequence or nothing makes them up, and loses them when a default
// such as gen_random_uuid() does.
func dropIDs(cfg *Config, tables map[string]*dbschema.Table, head *Snapshot, snap *Snapshot) {
	inFiles := map[string]bool{}
	if head != nil {
		for _, model := range head.Order {
			inFiles[model] = true
		}
	}
	for model, entries := range snap.Entries {
		m := cfg.Models[model]
		table := tables[cfg.QualifiedTable(m)]
		if table == nil {
			continue
		}
		id, ok := table.Column(m.ID)
		if !ok || id.Default == "" && !id.Identity {
			// Without an id, dbfixture could not insert the row at all.
			continue
		}
		keep := !inFiles[model] && id.Serial()
		if head != nil {
			for _, e := range head.Entries[model] {
				keep = keep || e.ID != ""
			}
		}
		if keep {
			continue
		}
		for _, e := range entries {
			e.ID = ""
		}
	}
}

// SyncReport is what Project.Sync found and did. Encoded as JSON it is what
// sync -json prints.
type SyncReport struct {
	*SyncResult
	// DryRun is true when everything was rolled back.
	DryRun bool

	cfg *Config
}

// Sync brings the database to the fixture files without a migration, as the
// sync command does: see the package function Sync, which it calls with the
// Project's configuration and files. A refusal is a *RefusedError that also
// wraps ErrSyncRefused.
func (p *Project) Sync(ctx context.Context, db *bun.DB, opts SyncOptions) (*SyncReport, error) {
	files, err := p.Files()
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, errNoDatabase("Sync")
	}
	res, err := Sync(ctx, db, p.Config, files, opts)
	if res == nil {
		return nil, err
	}
	report := &SyncReport{SyncResult: res, DryRun: opts.DryRun, cfg: p.Config}
	if errors.Is(err, ErrSyncRefused) {
		refused := &RefusedError{Message: err.Error(), Findings: res.Findings, err: err}
		if res.Diff == nil {
			// Sync stops at the findings before it compares.
			refused.reason = ErrFindings
		} else {
			refused.Refusals = res.Diff.Refusals
		}
		err = refused
	}
	return report, err
}
