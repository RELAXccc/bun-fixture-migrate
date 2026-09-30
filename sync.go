package fixturemigrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
)

// SyncOptions steers Sync.
type SyncOptions struct {
	// DryRun rolls everything back: the result says what would have
	// changed, and nothing did.
	DryRun bool
	// Logf receives one line per row, as a migration's would; nil is silent.
	Logf func(format string, args ...any)
}

// SyncResult is what Sync found and did.
type SyncResult struct {
	// Diff is the comparison, the database on the left and the fixture
	// files on the right.
	Diff *Result
	// Findings are what the fixture files' values turned up against the
	// database's columns; any the policy makes an error stop the sync.
	Findings []Finding
	// Outcomes are the changes as they ran.
	Outcomes []fixtureapply.Outcome
	// Applied is true when the changes were committed.
	Applied bool
}

// ErrSyncRefused is what Sync returns, wrapped, when it will not change the
// database: a finding the policy makes an error, or a difference the
// generator would refuse to write.
var ErrSyncRefused = errors.New("sync refused")

// Sync brings a database to the fixture files, without a migration file.
//
// It is for the databases that are not deployed to: a developer's, a test
// run's, a staging copy, or one to repair after drift was found. It reads the
// database, compares it with the files the way check does, and applies the
// difference the way a generated migration would -- every change guarded,
// references resolved, the policy followed, deletes that would reach other
// rows refused -- all in one REPEATABLE READ transaction, so a row changed by
// somebody else meanwhile fails the sync instead of being overwritten. An
// empty database is seeded, which dbfixture would do too.
//
// A deployed database should get its changes through migrations, which it
// records: Sync records nothing, so a migration generated for the same
// change later finds it made and does nothing.
func Sync(ctx context.Context, db *bun.DB, cfg *Config, files []FixtureFile, opts SyncOptions) (*SyncResult, error) {
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	doc, err := ParseFiles(files)
	if err != nil {
		return nil, err
	}
	head, err := FixtureSnapshot(cfg, doc, cfg.FixtureLabel())
	if err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := PrepareSession(ctx, tx); err != nil {
		return nil, err
	}
	tables, err := dbschema.Load(ctx, tx, cfg.Schema)
	if err != nil {
		return nil, err
	}
	if err := Canonicalize(ctx, tx, cfg, head, tables); err != nil {
		return nil, err
	}
	LintColumns(cfg, head, tables)
	LintZeroDefaults(cfg, head, tables)
	LintNullDefaults(cfg, head, tables)
	mode, findings := cfg.Worst(head.Findings)
	res := &SyncResult{Findings: findings}
	if mode == ModeError {
		return res, fmt.Errorf("%w: %d problems in the fixture files", ErrSyncRefused, len(findings))
	}
	database, err := DatabaseSnapshot(ctx, tx, cfg, tables, SnapshotOptions{Columns: head.Columns, Order: head.Order})
	if err != nil {
		return res, err
	}
	diff, err := Compute(cfg, database, head)
	if err != nil {
		return res, err
	}
	res.Diff = diff
	if len(diff.Refusals) > 0 {
		return res, fmt.Errorf("%w: %d differences need a hand-written change", ErrSyncRefused, len(diff.Refusals))
	}
	if len(diff.Changes) == 0 {
		return res, nil
	}
	set := fixturechange.Set{
		Name:    "sync",
		Tables:  diff.Tables,
		Changes: diff.Changes,
		Policy: fixturechange.Policy{
			MissingRow: fixturechange.Mode(cfg.Policy.MissingRow),
			ChangedRow: fixturechange.Mode(cfg.Policy.ChangedRow),
			IDDrift:    fixturechange.Mode(cfg.Policy.IDDrift),
		},
	}
	applyOpts := []fixtureapply.Option{
		fixtureapply.WithLogger(logf),
		fixtureapply.WithReport(func(o fixtureapply.Outcome) { res.Outcomes = append(res.Outcomes, o) }),
	}
	if opts.DryRun {
		applyOpts = append(applyOpts, fixtureapply.WithDryRun())
	}
	if err := fixtureapply.Apply(ctx, tx, set, applyOpts...); err != nil {
		return res, err
	}
	if opts.DryRun {
		return res, tx.Rollback()
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	res.Applied = true
	return res, nil
}
