package fixturemigrate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"

	"github.com/uptrace/bun"
)

// GenerateOptions steers Generate. The fields are the generate command's
// flags.
type GenerateOptions struct {
	// Name is a short name for the migration, which its file and change set
	// are named after; required when there is a migration to write (-name).
	Name string
	// At names the migration as of this time instead of now, for output that
	// is the same on every run (-at). Zero is now.
	At time.Time
	// Base, Old and FromDB choose what the fixture files are diffed against,
	// one at most: the fixture files as of a git revision (-base), a file
	// (-old), or the database Generate is given (-from-db). Without any of
	// them, the state file, and git's HEAD while there is none.
	Base   string
	Old    string
	FromDB bool
	// Out is the directory the migration goes into instead of the
	// configuration's out, as a path from the working directory (-out).
	Out string
	// AllowPartial writes what was accepted when something else was refused,
	// and records the refused changes in the state file as left out
	// (-allow-partial).
	AllowPartial bool
	// NoLint does not check the fixture files against the database's columns
	// and defaults, nor respell their values as the columns hold them: unless
	// FromDB, the database is not used at all (-no-lint).
	NoLint bool
	// DryRun works out the migration as generate -dry-run prints it: a
	// fixture migration the state file's history does not include is a
	// warning rather than a refusal, nothing is asked of the directory the
	// migration would go into, and the result is not to be written.
	DryRun bool
}

// Generated is the migration Generate worked out, before anything is written.
// Encoded as JSON it is what generate -json prints.
type Generated struct {
	// Diff is the comparison, the base on the left and the fixture files on
	// the right: its Changes are what the migration makes, its Refusals what
	// it leaves to a migration written by hand, and its Warnings what the
	// policy lets it carry on past. Nil when Generate stopped before it
	// compared.
	Diff *Result
	// Findings are what the fixture files turned up on their own that the
	// policy does not ignore, such as two rows sharing a natural key.
	Findings []Finding
	// Lint is what checking the fixture files against the database's columns
	// and defaults turned up that the policy does not ignore. Empty without a
	// database, and with NoLint.
	Lint []Finding
	// Notes say how the base was chosen when that is worth saying: there is
	// no state file yet, so the diff is against git's HEAD.
	Notes []string
	// NotInState are the fixture migrations of the directory whose changes
	// the state file does not include, and Problems what is wrong with each,
	// then that the migration the state includes last is gone from the
	// directory. Generate refuses on them, but under DryRun.
	NotInState []string
	Problems   []string
	// Warnings are what to know about the migration that stops nothing: a
	// migration that sorts after it, a change set written in parts, a missing
	// seed guard, the package it goes into.
	Warnings []string
	// ID is the migration as bun prints it, Path where Write writes it, and
	// Source its Go code. All three are empty when there is no migration to
	// write, and Path when there is no directory to write it into.
	ID     string
	Path   string
	Source []byte
	// State is what Write writes into StatePath: the fixture files, as the
	// migration leaves a database. Nil when the state file stays as it is.
	// Without a migration it is the state taking the files' new text, when
	// they differ from it only in how values are written.
	State     *State
	StatePath string
	// LeftOut are the changes the state file records as left out, once Write
	// wrote it: no migration makes them until baseline -force says one
	// written by hand does.
	LeftOut []string
	// Written are the files Write wrote.
	Written []string

	cfg    *Config
	dryRun bool
	err    error
}

// Generate works out the migration that takes a database from the base state
// to the fixture files, and the state file that goes with it, as the generate
// command does, and writes nothing: Write does.
//
// db is the database to respell both sides with and to lint the fixture files
// against its columns, and the base under FromDB; nil works offline, as the
// command does without a database. A refusal comes back as a *RefusedError
// with the result, which says what was refused; any other error with the
// result as far as it got, or nil.
func (p *Project) Generate(ctx context.Context, db bun.IDB, opts GenerateOptions) (*Generated, error) {
	db = orNil(db)
	chosen := 0
	for _, set := range []bool{opts.FromDB, opts.Old != "", opts.Base != ""} {
		if set {
			chosen++
		}
	}
	if chosen > 1 {
		return nil, errors.New("FromDB, Old and Base ask for different base states; set one of them")
	}
	g := &Generated{cfg: p.Config, dryRun: opts.DryRun, StatePath: p.StatePath()}
	fail := func(err error) (*Generated, error) {
		g.err = err
		return g, err
	}
	headData, head, err := p.head()
	if err != nil {
		return nil, err
	}
	// What the fixture files turn up on their own, such as two rows sharing a
	// key, stops generate as it stops sync, status and baseline.
	if g.Findings, err = refuseFindings(p.Config, head); err != nil {
		return fail(err)
	}
	// The state file is read whatever the base: the changes it records as
	// left out and its history go on into the next one. A state file that
	// does not read is never passed over, whatever the base: this run would
	// overwrite it, and with it the history and the changes left out that it
	// records, or the other side of a conflict.
	var prev *State
	if g.StatePath != "" {
		state, err := ReadState(g.StatePath)
		switch {
		case err == nil:
			prev = &state
		case errors.Is(err, ErrNoState):
		default:
			return fail(err)
		}
	}

	var old *Snapshot
	fromState := false
	if opts.FromDB {
		if db == nil {
			return fail(errNoDatabase("Generate with FromDB"))
		}
		err = ReadOnly(ctx, db, func(tx bun.Tx) error {
			tables, err := dbschema.Load(ctx, tx, p.Config.Schemas()...)
			if err != nil {
				return err
			}
			before := len(head.Findings)
			if err := canonical(ctx, tx, p.Config, tables, head); err != nil {
				return err
			}
			if old, err = databaseSnapshot(ctx, tx, p.Config, tables, head); err != nil {
				return err
			}
			if opts.NoLint {
				return nil
			}
			return g.lint(ctx, tx, p.Config, head, tables, before)
		})
		if err != nil {
			return fail(err)
		}
	} else {
		files, source, note, err := p.baseState(prev, opts.Old, opts.Base)
		if note != "" {
			g.Notes = append(g.Notes, note)
		}
		if err != nil {
			return fail(err)
		}
		fromState = opts.Old == "" && opts.Base == "" && prev != nil
		if old, err = p.snapshotOf(files, source); err != nil {
			return fail(err)
		}
		if db != nil && !opts.NoLint {
			// Both sides are respelled by the database, so a value written two
			// ways in two revisions of the file is not a change.
			err = ReadOnly(ctx, db, func(tx bun.Tx) error {
				tables, err := dbschema.Load(ctx, tx, p.Config.Schemas()...)
				if err != nil {
					return err
				}
				before := len(head.Findings)
				if err := canonical(ctx, tx, p.Config, tables, head, old); err != nil {
					return err
				}
				return g.lint(ctx, tx, p.Config, head, tables, before)
			})
			if err != nil {
				return fail(err)
			}
		}
	}

	res, err := Compute(p.Config, old, head)
	if err != nil {
		return fail(err)
	}
	g.Diff = res

	dir := opts.Out
	if dir == "" {
		dir = p.OutDir()
	}
	var existing []string
	var fixtures, all []MigrationFile
	var dirErr error
	if dir != "" {
		var ms *Migrations
		if ms, dirErr = ReadMigrations(dir); dirErr == nil {
			for _, m := range ms.List {
				existing = append(existing, m.Name)
			}
			fixtures, all = ms.Fixtures(), ms.List
		}
	}
	// A fixture migration the state does not include was generated on another
	// branch, or written by hand and not recorded. A migration generated now
	// would expect rows as that one did not leave them, and the state would
	// go on without it, so the history is put right first. So is a state
	// whose newest migration is gone: what it says is made, nothing makes.
	if prev != nil {
		missing := unaccounted(prev, fixtures)
		for _, m := range missing {
			g.NotInState = append(g.NotInState, m.ID())
			g.Problems = append(g.Problems, lineageProblem(prev, m))
		}
		gone := ""
		if dir != "" && (dirErr == nil || errors.Is(dirErr, os.ErrNotExist)) {
			gone = coveredGone(prev, all, dir, g.StatePath)
		}
		if gone != "" {
			g.Problems = append(g.Problems, gone)
		}
		refused := &RefusedError{reason: ErrLineage, Problems: g.Problems}
		switch {
		case opts.DryRun:
			refused = nil
		case gone != "" && len(missing) > 0:
			refused.Message = fmt.Sprintf("%s the state file does not include, and the one it includes last is "+
				"gone, nothing written", plural(len(missing), "fixture migration"))
		case gone != "":
			// The sentence is the message, and no line of its own.
			refused.Message, refused.Problems = gone+"; nothing written", nil
		case len(missing) > 0:
			refused.Message = fmt.Sprintf("%s the state file does not include, nothing written",
				plural(len(missing), "fixture migration"))
		default:
			refused = nil
		}
		if refused != nil {
			return fail(refused)
		}
	}
	if prev != nil {
		g.LeftOut = prev.LeftOut
	}
	if len(res.Changes) == 0 {
		if len(res.Refusals) > 0 {
			return fail(refusedChanges(res.Refusals))
		}
		// The file was edited without changing a value: a comment, or a value
		// written another way, 1.1 for 1.10 in a numeric column. The state
		// takes the new text, or status -offline, which cannot tell such a
		// spelling from a change, would fail on it until the next migration.
		if fromState && !opts.DryRun && !SameFiles(prev.Files, headData) {
			if dirErr != nil && !errors.Is(dirErr, os.ErrNotExist) {
				return fail(fmt.Errorf("the migrations directory: %w", dirErr))
			}
			next := *prev
			next.Files = headData
			next.Covers, next.Base = lineageOf(prev, fixtures)
			g.State = &next
		}
		return g, nil
	}
	if len(res.Refusals) > 0 && !opts.AllowPartial {
		return fail(refusedChanges(res.Refusals))
	}
	if opts.Name == "" {
		return fail(ErrNameRequired)
	}
	now := opts.At
	if now.IsZero() {
		now = time.Now()
	}
	if dirErr != nil && !opts.DryRun {
		return fail(fmt.Errorf("the migrations directory: %w", dirErr))
	}
	stamp := NextStamp(now, existing)
	// bun orders migrations by name as strings, so a short name such as
	// "3_backfill" sorts after every timestamp and would run after this one.
	for _, name := range existing {
		if name > stamp {
			g.Warnings = append(g.Warnings, fmt.Sprintf("migration %s sorts after %s, so bun runs it after this one, "+
				"although this one was generated against the state it leaves", name, stamp))
		}
	}
	src, err := Render(p.Config, opts.Name, stamp, res)
	if err != nil {
		return fail(err)
	}
	g.Warnings = append(g.Warnings, RenderWarnings(res)...)
	if w := seedGuardWarning(p.Config); w != "" {
		g.Warnings = append(g.Warnings, w)
	}
	file := FileName(stamp, opts.Name)
	g.ID, g.Source = strings.TrimSuffix(file, ".go"), src
	if dir != "" {
		g.Path = filepath.Join(dir, file)
	}
	if opts.DryRun {
		return g, nil
	}
	if dir == "" {
		return fail(errors.New("no out directory in the configuration and no -out"))
	}
	warnings, err := CheckPackage(dir, p.Config.Package, p.Config.Migrator)
	if err != nil {
		return fail(err)
	}
	g.Warnings = append(g.Warnings, warnings...)
	if _, err := os.Stat(g.Path); err == nil {
		return fail(fmt.Errorf("%s exists already", g.Path))
	}
	if g.StatePath != "" {
		state := State{Files: headData, Migration: g.ID, Covers: g.ID, Base: newestFixture(fixtures)}
		if prev != nil {
			state.LeftOut = slices.Clone(prev.LeftOut)
		}
		// What was refused is in the files now, so the next generate does not
		// see it again; the state says it is not migrated until baseline -force
		// says a migration somebody wrote does it.
		for _, r := range res.Refusals {
			if !slices.Contains(state.LeftOut, r.String()) {
				state.LeftOut = append(state.LeftOut, r.String())
			}
		}
		g.State, g.LeftOut = &state, state.LeftOut
	}
	return g, nil
}

// lint checks the fixture files against what the database says about its own
// columns, and the natural keys against its indexes, and refuses on what the
// policy makes an error. A lint that could not run is never reported as a
// lint that found nothing: the connection error comes back as an error.
func (g *Generated) lint(ctx context.Context, tx bun.Tx, cfg *Config, head *Snapshot,
	tables map[string]*dbschema.Table, before int) error {

	lintAll(cfg, head, tables)
	if err := LintKeys(ctx, tx, cfg, head, tables); err != nil {
		return err
	}
	mode, findings := cfg.Worst(head.Findings[before:])
	g.Lint = findings
	if len(findings) == 0 || mode != ModeError {
		return nil
	}
	return &RefusedError{reason: ErrFindings, Findings: findings,
		Message: fmt.Sprintf("%s in the fixture file, nothing written", plural(len(findings), "problem"))}
}

// refusedChanges is the refusal of differences that need a hand-written
// migration.
func refusedChanges(refusals []Refusal) error {
	return &RefusedError{Refusals: refusals, Message: fmt.Sprintf(
		"%s refused, nothing written; write them yourself or re-run with -allow-partial",
		plural(len(refusals), "change"))}
}

// Write writes the migration and the state file, and returns the paths it
// wrote, which Written holds too. A result Generate refused, or worked out
// under DryRun, is not written.
func (g *Generated) Write() ([]string, error) {
	switch {
	case g.err != nil:
		return nil, g.err
	case g.dryRun:
		return nil, errors.New("a dry run is not written")
	}
	if g.Source != nil {
		// Generate looked; a file that turned up since is not overwritten.
		if _, err := os.Stat(g.Path); err == nil {
			return nil, fmt.Errorf("%s exists already", g.Path)
		}
		if err := WriteFileAtomic(g.Path, g.Source, 0o644); err != nil {
			return nil, err
		}
		g.Written = append(g.Written, g.Path)
	}
	if g.State != nil && g.StatePath != "" {
		if err := WriteState(g.StatePath, *g.State); err != nil {
			if g.Source != nil {
				return g.Written, fmt.Errorf("the migration is written, the state file is not: %w; "+
					"delete %s and generate again once that is fixed", err, g.Path)
			}
			return g.Written, err
		}
		g.Written = append(g.Written, g.StatePath)
	}
	return g.Written, nil
}

// baseState is the fixture files generate diffs against when it does not
// diff against the database: the file named with -old, the git revision named
// with -base, or else the state file, and HEAD only while there is no state
// file, which the note says.
func (p *Project) baseState(state *State, oldPath, rev string) (files []FixtureFile, source, note string, err error) {
	switch {
	case oldPath != "":
		data, err := os.ReadFile(oldPath)
		return []FixtureFile{{Path: oldPath, Data: data}}, oldPath, "", err
	case rev != "":
		files, err := p.gitFiles(rev)
		return files, rev + ":" + p.Config.FixtureLabel(), "", err
	case state != nil:
		return state.Files, "the state after " + state.Migration, "", nil
	}
	if statePath := p.StatePath(); statePath != "" {
		note = fmt.Sprintf("no state file at %s yet, so this diffs against git HEAD; "+
			"generate writes one with the migration, or run baseline to start it now", statePath)
	}
	files, err = p.gitFiles("HEAD")
	if err != nil {
		return nil, "", note, fmt.Errorf("%w; or run baseline to record what the databases hold", err)
	}
	return files, "HEAD:" + p.Config.FixtureLabel(), note, nil
}

// seedGuardWarning is what is wrong with the seed guard table for a migration
// about to be written, "" when nothing is. Without one, a database that was
// never seeded runs every fixture migration before its seed, against empty
// tables, and the first that changes a row fails the deploy. One the fixture
// files do not fill can be empty in a seeded database too, and there every
// fixture migration does nothing and is recorded as applied.
func seedGuardWarning(cfg *Config) string {
	guard := cfg.SeedGuardTable
	if guard == "" {
		return "no seed_guard_table: on a database that was never seeded this migration runs before the seed and " +
			"fails; set it to a table the fixture files fill"
	}
	if !strings.Contains(guard, ".") {
		guard = cfg.Schema + "." + guard
	}
	for _, name := range cfg.ModelNames() {
		if cfg.QualifiedTable(cfg.Models[name]) == guard {
			return ""
		}
	}
	return fmt.Sprintf("seed_guard_table %s is the table of no model, so the fixture files do not fill it: on a "+
		"seeded database where it is empty, this migration does nothing and is recorded as applied all the same",
		cfg.SeedGuardTable)
}
