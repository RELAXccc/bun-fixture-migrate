// Package fixturechange holds what a generated migration carries: the rows
// that changed, how they changed, and where they live.
//
// It has no dependencies beyond the standard library. A migrations package
// that imports it and fixtureapply pulls in nothing but bun, which it already
// has.
package fixturechange

import (
	"fmt"
	"strings"
)

// Kind is what a change does to a row.
type Kind string

const (
	Insert Kind = "insert"
	Update Kind = "update"
	Delete Kind = "delete"
)

// Table says where a model's rows live and how a reference to one of them is
// resolved. The generator writes this map into the migration so the file is
// self-contained: a reviewer sees every table the migration can touch without
// looking anything up.
type Table struct {
	// Name is the SQL table, optionally schema-qualified ("public.items").
	Name string
	// ID is the primary-key column. A reference to this model selects it.
	ID string
	// Key is the column a reference matches on, usually "name".
	Key string
	// Serial is true when ID comes from a sequence. Writing an explicit ID
	// into such a table leaves the sequence behind, so fixtureapply advances
	// it afterwards.
	Serial bool
	// Cascade allows a delete to reach rows of other tables through a
	// foreign key declared ON DELETE CASCADE, SET NULL or SET DEFAULT. Without
	// it such a delete fails while any row still points at the one being
	// deleted: removing a plan must not quietly delete or detach the
	// subscriptions on it.
	Cascade bool
	// Where, when set, is an SQL predicate over the table's columns that
	// limits which of its rows are master data: the configuration's where,
	// such as tenant_id IS NULL for global rows that share a table with each
	// tenant's own. Every statement, every lookup by natural key and every
	// reference to the model sees only the rows it holds for, and a row a
	// change writes has to hold it afterwards. Without it, a change to a
	// global row would also reach a tenant's row with the same key, and a
	// reference could bind a global row to a tenant's private one.
	//
	// It is the configuration's own SQL, written into the statements as it
	// stands; fixtureapply refuses one that could reach outside the
	// parentheses it is put in.
	Where string
}

// Tables maps a model name to its table.
type Tables map[string]Table

// Ref names a row of another model by its Key column, never by id: ids drift
// between databases, names do not. fixtureapply looks the row up and binds the
// id it finds.
type Ref struct{ Model, Key string }

// Value is one column value: SQL NULL, a literal, which is passed as a bound
// parameter and coerced by the database, or a reference.
type Value struct {
	Lit    string
	Ref    *Ref
	IsNull bool
}

// Lit builds a literal value. Numbers and booleans are written as text.
func Lit(s string) Value { return Value{Lit: s} }

// Null builds a SQL NULL. A fixture row writes one with an explicit `~`; an
// omitted column is not NULL, it is whatever the model's Defaults say.
func Null() Value { return Value{IsNull: true} }

// RefTo builds a reference to a row of model by its key.
func RefTo(model, key string) Value { return Value{Ref: &Ref{Model: model, Key: key}} }

// String renders a value for log lines and comments.
func (v Value) String() string {
	switch {
	case v.IsNull:
		return "NULL"
	case v.Ref != nil:
		return v.Ref.Model + "(" + v.Ref.Key + ")"
	}
	return v.Lit
}

// Values maps a column to its value.
type Values = map[string]Value

// Change is one row that differs between the two states being compared.
type Change struct {
	// Model is the fixture model name; Tables says which table that is.
	Model string
	Kind  Kind
	// ID, when set, is an extra guard: the row must also hold this primary
	// key. A rename needs it, because the update changes the very columns the
	// natural key is made of and only the id says the right row was found.
	ID string
	// Key is the natural key: enough columns to find the row without using
	// its id.
	Key Values
	// Old is, for an Update, what every column in New held before, and for a
	// Delete every column the statement guards on. Empty for an Insert.
	Old Values
	// New is, for an Insert, every column to write, and for an Update the
	// columns that changed. Empty for a Delete.
	New Values
}

// Set is one generated migration's payload.
type Set struct {
	// Name identifies the set in log lines; the generator uses the migration
	// file name.
	Name string
	// SeedGuardTable, when set, makes the whole set a no-op while that table
	// is empty. An empty table means the database has not been seeded yet, so
	// the fixture loader will insert the new state by itself and the migration
	// must keep its hands off.
	SeedGuardTable string
	// MigrationsTable is the table bun's migrator records applied migrations
	// in: "bun_migrations" unless the migrator was built WithTableName. Empty
	// means DefaultMigrationsTable.
	//
	// It is here because of one line in migrate.Migrator.Migrate: unless the
	// migrator was built WithMarkAppliedOnSuccess(true), it records a migration
	// as applied before it runs it, and leaves the record in place when the
	// migration fails. A change set that failed and rolled back would then be
	// recorded as done and never attempted again. fixtureapply.Apply removes
	// that one record when it fails, and only that one; see Apply.
	MigrationsTable string
	Tables          Tables
	Changes         []Change
	// Policy is what the migration does when the database is not in the state
	// the change set was generated against. The generator writes the values
	// from the configuration file into it, so the migration carries its own
	// policy and a later change to the configuration does not silently change
	// what an old migration does.
	Policy Policy
	// LockTimeout, when set, is how long a statement of the change set waits
	// for a lock another session holds on a row or table it writes, in
	// PostgreSQL's spelling: "5s", "500ms", "1min". The change set then fails
	// and rolls back instead of waiting behind, say, an admin's open
	// transaction while the application's own writes queue up behind it; the
	// next deploy runs it again. Waiting for another change set to finish is
	// not affected. Empty means the session's own lock_timeout, which is
	// usually none.
	LockTimeout string
}

// DefaultMigrationsTable is the table bun's migrator uses when it was not built
// WithTableName.
const DefaultMigrationsTable = "bun_migrations"

// Mode is what a policy does when it triggers. The empty Mode is the strict
// reading of whichever policy carries it, so a Policy nobody filled in fails on
// anything unexpected.
type Mode string

const (
	// ModeError fails the migration, which rolls the transaction back and
	// leaves bun's migration table without a row for it.
	ModeError Mode = "error"
	// ModeWarn reports the row and carries on.
	ModeWarn Mode = "warn"
	// ModeIgnore does not even look.
	ModeIgnore Mode = "ignore"
)

func (m Mode) valid(allowed ...Mode) bool {
	for _, a := range allowed {
		if m == a {
			return true
		}
	}
	return false
}

// Policy is the run-time half of the configuration's policy block. The zero
// value is the strict one: anything unexpected fails and the transaction rolls
// back.
//
// This matters more than it looks. bun's migrator records a migration as
// applied as soon as the function returns nil, so a statement that matched no
// row and returned nil is a change that is now lost for good: fix the drift,
// deploy again, and the migration never runs a second time. A migration that
// cannot do what it says has to fail.
type Policy struct {
	// MissingRow is what happens when an update or a delete finds no row with
	// the natural key at all: ModeError (the default) or ModeWarn.
	MissingRow Mode
	// ChangedRow is what happens when the row is there but no longer holds the
	// values the base state had, which is somebody's hand edit: ModeWarn (keep
	// the edit and carry on) or ModeError.
	ChangedRow Mode
	// IDDrift is what happens when the id in the change set is not the id the
	// database gave the row: ModeError (the default), ModeWarn or ModeIgnore.
	IDDrift Mode
	// DuplicateKey is what happens when more than one row holds the natural
	// key a change finds its row by: ModeError (the default) or ModeWarn,
	// which leaves all of them alone and carries on. A change is never made
	// to more than one row; nothing can say which of them the fixture file
	// means.
	DuplicateKey Mode
}

// Validate reports a policy field holding something this package does not
// know. An empty field is the default and always allowed.
//
// A value outside the list is a typo, and a typo that silently means something
// else is the sort of thing this tool exists to stop: "warm" in MissingRow
// would read as the strict setting and the same typo in ChangedRow as the
// lenient one.
func (p Policy) Validate() error {
	for _, f := range []struct {
		name    string
		value   Mode
		allowed []Mode
	}{
		{"MissingRow", p.MissingRow, []Mode{ModeError, ModeWarn}},
		{"ChangedRow", p.ChangedRow, []Mode{ModeError, ModeWarn}},
		{"IDDrift", p.IDDrift, []Mode{ModeError, ModeWarn, ModeIgnore}},
		{"DuplicateKey", p.DuplicateKey, []Mode{ModeError, ModeWarn}},
	} {
		if f.value == "" || f.value.valid(f.allowed...) {
			continue
		}
		return fmt.Errorf("policy %s is %q, it has to be one of %s or empty for the default",
			f.name, f.value, modeList(f.allowed))
	}
	return nil
}

func modeList(modes []Mode) string {
	out := make([]string, 0, len(modes))
	for _, m := range modes {
		out = append(out, string(m))
	}
	return strings.Join(out, ", ")
}
