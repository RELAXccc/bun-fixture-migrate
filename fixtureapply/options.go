package fixtureapply

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// Option configures Apply and Revert.
type Option func(*options)

type options struct {
	logf      func(format string, args ...any)
	slog      *slog.Logger
	report    func(Outcome)
	migration string
	dryRun    bool
	// nested is true when the change set runs inside a caller's
	// transaction.
	nested bool
}

// WithLogger replaces log.Printf as the destination of the per-row report.
// Pass func(string, ...any) {} to silence it; warnings go here too, so silence
// it only if something else is watching the migration.
func WithLogger(logf func(format string, args ...any)) Option {
	return func(o *options) { o.logf = logf }
}

// WithSlog sends the per-row report to a structured logger instead of the
// function WithLogger names, which it takes the place of: one record per
// change, and one for each thing that happens to the whole set, with the
// fields of the Outcome as attributes. A change that was skipped, and the
// removal of bun's record of a failed migration, are warnings; the rest is
// information. A change that fails the set is not logged, as it is not as
// text: it is the error Apply returns.
func WithSlog(logger *slog.Logger) Option {
	return func(o *options) { o.slog = logger }
}

// log writes one line of the per-row report: a record with out's fields and
// msg through WithSlog's logger when there is one, and otherwise text through
// WithLogger's function.
func (o options) log(ctx context.Context, level slog.Level, msg string, out Outcome, text string) {
	if o.slog == nil {
		o.logf("%s", text)
		return
	}
	attrs := []slog.Attr{slog.String("set", out.Set)}
	if out.Index >= 0 {
		attrs = append(attrs, slog.Int("index", out.Index))
	}
	for _, a := range []struct{ key, value string }{{"model", out.Model}, {"kind", string(out.Kind)},
		{"key", out.Key}, {"status", string(out.Status)}, {"action", string(out.Action)},
		{"problem", string(out.Problem)}, {"message", out.Message}} {
		if a.value != "" {
			attrs = append(attrs, slog.String(a.key, a.value))
		}
	}
	if out.Rows > 0 {
		attrs = append(attrs, slog.Int64("rows", out.Rows))
	}
	o.slog.LogAttrs(ctx, level, msg, attrs...)
}

// WithReport hands every change's outcome to fn as it happens, in the order
// the changes run. It is what a dry run reads; the log lines are for people.
func WithReport(fn func(Outcome)) Option {
	return func(o *options) { o.report = fn }
}

// WithMigrationName names the bun migration Apply is running as: the
// timestamp at the front of the migration's file name, which is what bun
// stores in its migrations table. A generated migration registers Up, which
// knows it from the file it is called in. Without either, Apply reads it off
// the call stack the way bun's own Register does -- the nearest caller in a
// file named like a migration -- so neither a file an earlier version wrote,
// nor a helper in an ordinary file, needs this. A helper that lives in another
// migration's file does: the stack would name that migration.
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

// Action is what an applied change did to its row.
type Action string

const (
	// ActionInserted is a row inserted: an insert, or the revert of a delete,
	// that wrote a new row.
	ActionInserted Action = "inserted"
	// ActionUpdated is a row updated.
	ActionUpdated Action = "updated"
	// ActionDeleted is a row deleted.
	ActionDeleted Action = "deleted"
	// ActionSoftDeleted is a delete of a model with a SoftDelete: the row
	// is kept, its soft delete column set to the transaction's time.
	ActionSoftDeleted Action = "soft-deleted"
	// ActionRestored is an insert of a model with a SoftDelete that found
	// a soft-deleted row holding its values and set the column back to
	// NULL: the row keeps its id, and the rows pointing at it.
	ActionRestored Action = "restored"
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
	// Action is what an applied change did to its row; empty for any other
	// status. For a model with a SoftDelete it says what Kind alone does
	// not: a delete that soft-deleted the row, an insert that restored one.
	Action Action `json:"action,omitempty"`
	// Rows is the row count of an applied change.
	Rows int64 `json:"rows,omitempty"`
	// Problem is set when the change could not be made.
	Problem Problem `json:"problem,omitempty"`
	// Message is the explanation a person reads.
	Message string `json:"message,omitempty"`
}

// ChangeError is the error Apply and Revert return when a change fails the
// set, whether its statement failed or the policy makes its problem an error.
// errors.As finds it in what bun's migrator returns, with the change's
// Outcome as WithReport received it.
//
// It is also the error of a lock timeout met while the constraints PostgreSQL
// defers are checked at the end of the set. That belongs to no change: its
// Outcome has Index -1 and ProblemLockTimeout, and WithReport does not
// receive it.
type ChangeError struct {
	Outcome Outcome
	// err is the error of the change's statement; nil when the policy made
	// a problem with the row fatal.
	err error
}

func (e *ChangeError) Error() string {
	where := fmt.Sprintf("%s: %s %s %s", e.Outcome.Set, e.Outcome.Model, e.Outcome.Key, e.Outcome.Kind)
	if e.Outcome.Index < 0 {
		where = e.Outcome.Set
	}
	if e.err != nil {
		return where + ": " + e.err.Error()
	}
	return where + ": " + e.Outcome.Message
}

// Unwrap is the error of the change's statement, such as PostgreSQL's, and
// nil for a problem with the row that the policy made fatal.
func (e *ChangeError) Unwrap() error { return e.err }

// ErrRecordRemoved is in the error of a failed Apply whose record bun's
// migrator had made before running it, and which Apply took back: the
// migration is pending again, and the next migrate runs it. errors.Is finds
// it.
var ErrRecordRemoved = errors.New("bun's record of the migration was removed, so it runs again")

// recordRemoved is a failure whose record Apply took back.
type recordRemoved struct {
	failure error
	note    string
}

func (e *recordRemoved) Error() string   { return e.failure.Error() + "\n\n" + e.note }
func (e *recordRemoved) Unwrap() []error { return []error{e.failure, ErrRecordRemoved} }
