package fixturemigrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"

	"github.com/uptrace/bun"
)

// ErrUnmigrated is wrapped by a refusal of Baseline to record changes no
// migration makes: fixture files that differ from the state file in content,
// or a state file that records changes generate left out. Force records them
// all the same, for when a migration written by hand makes them.
var ErrUnmigrated = errors.New("changes no migration makes")

// BaselineOptions steers Baseline. The fields are the baseline command's
// flags.
type BaselineOptions struct {
	// From records the fixture files as of this git revision instead of as
	// they are (-from).
	From string
	// Old records this file as the fixture file, under the fixture file's
	// path; with one fixture file only (-old).
	Old string
	// Force replaces a state that differs in content, records a fixture
	// migration written by hand as included, and clears what generate left
	// out: a migration you wrote covers the difference (-force).
	Force bool
}

// Baselined is the state file Baseline worked out, before it is written.
// Encoded as JSON it is what baseline -json prints.
type Baselined struct {
	// StatePath is the state file, and State what Write writes into it; nil
	// when there is nothing to write.
	StatePath string
	State     *State
	// Recorded names what is recorded: the fixture files, a git revision of
	// them, or a file.
	Recorded string
	// Unchanged is true when the state file records them already.
	Unchanged bool
	// Diff is what the recorded files change against the state file they
	// replace, when they differ and Force was not given: what baseline
	// refuses to record as migrated with no migration to make it. Respelled
	// is true when the database said every difference is only in how values
	// are written, which is no change.
	Diff      *Result
	Respelled bool
	// Findings are what the recorded files turned up that the policy does
	// not ignore; one it makes an error refuses the baseline, Force or not.
	Findings []Finding
	// Problems refuse the baseline, Force or not, one sentence each: the
	// migration the state file includes last is gone from the directory, or
	// fixture migrations were generated against another state.
	Problems []string
	// NotInState are the fixture migrations written by hand whose changes the
	// state file does not include: refused without Force, recorded as
	// included with it.
	NotInState []string
	// LeftOut are the changes the state file records as left out: refused
	// without Force, cleared with it.
	LeftOut []string
	// Written are the files Write wrote.
	Written []string

	cfg *Config
	err error
}

// Baseline records fixture files as what the migrations leave a database
// holding, without a migration, as the baseline command does, and writes
// nothing: Write does. It is how a project adopts the tool, and how a change
// that was migrated by hand is recorded.
//
// A state that differs in content is only replaced with Force, because
// recording a change nobody migrated is how a change gets lost; given a
// database, a difference only the column types can settle, such as 1.10
// against 1.1, is asked of it first, and is no change when that is all there
// is. db is nil to work offline, and is not connected to unless there is such
// a difference to settle. A refusal comes back as a *RefusedError with the
// result; any other error with the result as far as it got, or nil.
func (p *Project) Baseline(ctx context.Context, db bun.IDB, opts BaselineOptions) (*Baselined, error) {
	db = orNil(db)
	statePath := p.StatePath()
	if statePath == "" {
		return nil, errors.New("no state file: set out, or state, in the configuration")
	}
	if opts.From != "" && opts.Old != "" {
		return nil, errors.New("a baseline records the files as of From or the file Old, not both; set one of them")
	}
	b := &Baselined{StatePath: statePath, Recorded: p.Config.FixtureLabel(), cfg: p.Config}
	fail := func(err error) (*Baselined, error) {
		b.err = err
		return b, err
	}
	var files []FixtureFile
	var err error
	switch {
	case opts.From != "":
		files, err = p.gitFiles(opts.From)
		b.Recorded = opts.From + ":" + b.Recorded
	case opts.Old != "":
		// The file stands in for the fixture file, and the state records it
		// under the fixture file's path: the path of a copy in /tmp means
		// nothing to the next generate.
		if len(p.Config.Fixtures) != 1 {
			return nil, fmt.Errorf("-old records one file, and the configuration has %d fixture files: export them "+
				"in place, run baseline, then take them back with git checkout -- <the fixture files>",
				len(p.Config.Fixtures))
		}
		var data []byte
		data, err = os.ReadFile(opts.Old)
		files = []FixtureFile{{Path: p.Config.Fixtures[0], Data: data}}
		b.Recorded = opts.Old
	default:
		files, _, err = p.head()
	}
	if err != nil {
		return nil, err
	}
	// A revision before the fixture files existed holds nothing, and a state
	// of nothing says the databases hold no master data: every row would be
	// an insert to the next generate.
	if opts.From != "" {
		empty := true
		for _, f := range files {
			empty = empty && len(bytes.TrimSpace(f.Data)) == 0
		}
		if empty {
			return fail(&RefusedError{Message: fmt.Sprintf("%s is missing or empty as of %s, so there is nothing to "+
				"record; name the revision whose fixture files the migrations leave a database holding",
				p.Config.FixtureLabel(), opts.From)})
		}
	}
	next, err := p.snapshotOf(files, b.Recorded)
	if err != nil {
		return fail(err)
	}
	ms, err := p.migrations()
	if err != nil {
		return fail(err)
	}
	fixtures := ms.Fixtures()

	// A conflicted state file is two histories, and replacing it with Force
	// would drop one of them without a word, the lineage check with it: one
	// side is taken first, and then baseline sees what that side says.
	var prev *State
	current, err := ReadState(statePath)
	switch {
	case errors.Is(err, ErrNoState):
	case errors.Is(err, ErrStateConflict):
		return fail(&RefusedError{err: err, Message: fmt.Sprintf("%v; baseline does not replace a conflicted state "+
			"file, even with -force: take one side first, git checkout --ours -- %s or git checkout --theirs -- %s",
			err, statePath, statePath)})
	case err != nil:
		if !opts.Force {
			return fail(&RefusedError{err: err, Message: fmt.Sprintf("%v; pass -force to replace it", err)})
		}
	default:
		prev = &current
	}
	// The migration the state says it includes last is gone, so what the
	// state says is made, nothing makes; a baseline would make that final.
	if outDir := p.OutDir(); prev != nil && outDir != "" {
		if gone := coveredGone(prev, ms.List, outDir, statePath); gone != "" {
			b.Problems = []string{gone}
			return fail(&RefusedError{reason: ErrLineage, Message: gone + "; nothing written"})
		}
	}

	// The history. A fixture migration generated on another branch cannot be
	// recorded as included: its guards expect rows as they were before the
	// migrations of this branch, so it has to be generated again. One written
	// by hand is the reason Force exists.
	covers, base := lineageOf(prev, fixtures)
	if prev != nil {
		b.LeftOut = prev.LeftOut
		for _, m := range unaccounted(prev, fixtures) {
			if generatedFile(m) {
				b.Problems = append(b.Problems, lineageProblem(prev, m))
				continue
			}
			b.NotInState = append(b.NotInState, m.ID())
		}
		if n := len(b.Problems); n > 0 {
			return fail(&RefusedError{reason: ErrLineage, Problems: b.Problems, Message: fmt.Sprintf(
				"%s generated against another state, nothing written; baseline cannot make up for that, "+
					"generating again does", plural(n, "fixture migration"))})
		}
		if n := len(b.NotInState); n > 0 {
			if !opts.Force {
				return fail(&RefusedError{reason: ErrLineage, Message: fmt.Sprintf("%s the state file does not "+
					"include; pass -force if you wrote them by hand and the fixture file holds what they do",
					plural(n, "fixture migration"))})
			}
			covers, base = newestFixture(fixtures), newestFixture(fixtures)
		}
		if n := len(prev.LeftOut); n > 0 && !opts.Force {
			return fail(&RefusedError{reason: ErrUnmigrated, Message: fmt.Sprintf("the state records %s generate "+
				"left out, which no migration makes yet. Write their migration by hand, then pass -force",
				plural(n, "change"))})
		}
	}

	switch {
	case prev == nil:
	case SameFiles(prev.Files, files) && len(prev.LeftOut) == 0 && covers == prev.Covers:
		b.Findings, err = refuseFindings(p.Config, next)
		if err != nil {
			return fail(err)
		}
		b.Unchanged = true
		return b, nil
	case opts.Force:
	default:
		before, err := p.snapshotOf(prev.Files, "the state file")
		if err != nil {
			return fail(err)
		}
		if err := p.baselineDiff(ctx, db, b, before, next); err != nil {
			return fail(err)
		}
		if n := len(b.Diff.Changes) + len(b.Diff.Refusals); n > 0 {
			return fail(&RefusedError{reason: ErrUnmigrated, Refusals: b.Diff.Refusals, Message: fmt.Sprintf(
				"baseline would record %s as migrated with no migration to make them. Generate one for them, "+
					"or pass -force if a migration you wrote by hand covers them", plural(n, "change"))})
		}
	}
	if b.Findings, err = refuseFindings(p.Config, next); err != nil {
		return fail(err)
	}
	b.State = &State{Files: files, Migration: "baseline", Covers: covers, Base: base}
	return b, nil
}

// baselineDiff works out the changes from the state to the files baseline is
// asked to record. A difference the files alone cannot settle, such as 1.10
// against 1.1, is asked of the database's column types when there is one, the
// way generate asks: as a number it is no change, and refusing it would leave
// the state behind for good.
func (p *Project) baselineDiff(ctx context.Context, db bun.IDB, b *Baselined, before, next *Snapshot) error {
	res, err := Compute(p.Config, before, next)
	if err != nil {
		return err
	}
	b.Diff = res
	if len(res.Changes)+len(res.Refusals) == 0 || db == nil {
		return nil
	}
	err = ReadOnly(ctx, db, func(tx bun.Tx) error {
		tables, err := dbschema.Load(ctx, tx, p.Config.Schemas()...)
		if err != nil {
			return err
		}
		if err := canonical(ctx, tx, p.Config, tables, next, before); err != nil {
			return err
		}
		res, err = Compute(p.Config, before, next)
		return err
	})
	if err != nil {
		return err
	}
	b.Diff = res
	b.Respelled = len(res.Changes)+len(res.Refusals) == 0
	return nil
}

// Write writes the state file, and returns the paths it wrote, which Written
// holds too: none when the state file records the files already. A result
// Baseline refused is not written.
func (b *Baselined) Write() ([]string, error) {
	if b.err != nil {
		return nil, b.err
	}
	if b.State == nil {
		return nil, nil
	}
	if err := WriteState(b.StatePath, *b.State); err != nil {
		return nil, err
	}
	b.Written = append(b.Written, b.StatePath)
	return b.Written, nil
}
