package fixtureapply

import (
	"log"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// Option configures Apply and Revert.
type Option func(*options)

type options struct {
	logf      func(format string, args ...any)
	report    func(Outcome)
	migration string
	dryRun    bool
}

// WithLogger replaces log.Printf as the destination of the per-row report.
// Pass func(string, ...any) {} to silence it; warnings go here too, so silence
// it only if something else is watching the migration.
func WithLogger(logf func(format string, args ...any)) Option {
	return func(o *options) { o.logf = logf }
}

// WithReport hands every change's outcome to fn as it happens, in the order
// the changes run. It is what a dry run reads; the log lines are for people.
func WithReport(fn func(Outcome)) Option {
	return func(o *options) { o.report = fn }
}

// WithMigrationName names the bun migration Apply is running as: the
// timestamp at the front of the migration's file name, which is what bun
// stores in its migrations table. Apply reads it off the call stack the way
// bun's own Register does -- the nearest caller in a file named like a
// migration -- so a generated migration never needs this, and neither does a
// helper in an ordinary file. A helper that lives in another migration's file
// does: the stack would name that migration.
func WithMigrationName(name string) Option {
	return func(o *options) { o.migration = name }
}

// WithDryRun is for a caller that rolls the transaction back afterwards, the
// way the plan command does. Everything runs as it would, except the one step
// a rollback cannot undo: moving a sequence past the explicit ids written
// (setval is not transactional). That step is reported instead, with
// StatusSequence.
func WithDryRun() Option {
	return func(o *options) { o.dryRun = true }
}

func newOptions(opts []Option) options {
	o := options{logf: log.Printf, report: func(Outcome) {}}
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// Status is what became of one change.
type Status string

const (
	// StatusApplied is a change that was made.
	StatusApplied Status = "applied"
	// StatusUnchanged is a change the database already held, which is what a
	// second run of the same migration finds.
	StatusUnchanged Status = "unchanged"
	// StatusSkipped is a change that could not be made and that the policy
	// allowed to be passed over with a warning.
	StatusSkipped Status = "skipped"
	// StatusFailed is a change that could not be made and failed the whole
	// set, which rolls it back.
	StatusFailed Status = "failed"
	// StatusUnseeded is the whole set passed over because the seed guard
	// table is empty. Index is -1.
	StatusUnseeded Status = "unseeded"
	// StatusSequence is a sequence moved past the explicit ids the set
	// wrote, or under WithDryRun one that would be. Index is -1 and Model
	// names the table's model.
	StatusSequence Status = "sequence"
)

// Problem names why a change could not be made.
type Problem string

const (
	// ProblemMissingRow is an update or a delete whose row is not there.
	ProblemMissingRow Problem = "missing row"
	// ProblemChangedRow is a row that no longer holds what the change was
	// generated against: somebody edited it in this database.
	ProblemChangedRow Problem = "changed row"
	// ProblemIDDrift is a row under a different id from the one the change
	// was generated for, or an id another row holds.
	ProblemIDDrift Problem = "id drift"
	// ProblemReferenced is a delete of a row that rows of another table, or
	// of the same one, still point at.
	ProblemReferenced Problem = "referenced"
	// ProblemDuplicateKey is more than one row holding the natural key the
	// change finds its row by. None of them is touched.
	ProblemDuplicateKey Problem = "duplicate key"
	// ProblemLockTimeout is a statement that waited longer than the set's
	// LockTimeout for a lock another session holds. The set fails whatever
	// the policy says, and runs again on the next deploy.
	ProblemLockTimeout Problem = "lock timeout"
	// ProblemError is a statement that failed outright.
	ProblemError Problem = "error"
)

// Outcome is what happened to one change of a set.
type Outcome struct {
	// Set is the change set's Name.
	Set string `json:"set"`
	// Index is the change's position in Set.Changes, -1 for an outcome about
	// the whole set.
	Index int    `json:"index"`
	Model string `json:"model,omitempty"`
	// Kind is what the change did; for Revert, what the inverted change did.
	Kind fixturechange.Kind `json:"kind,omitempty"`
	// Key is the natural key, as "col=value,col=value".
	Key    string `json:"key,omitempty"`
	Status Status `json:"status"`
	// Rows is the row count of an applied change.
	Rows int64 `json:"rows,omitempty"`
	// Problem is set when the change could not be made.
	Problem Problem `json:"problem,omitempty"`
	// Message is the explanation a person reads.
	Message string `json:"message,omitempty"`
}
