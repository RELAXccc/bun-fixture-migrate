// Package fixturechange holds what a generated migration carries: the rows
// that changed, how they changed, and where they live.
//
// It has no dependencies beyond the standard library. A migrations package
// that imports it and fixtureapply pulls in nothing but bun, which it already
// has.
package fixturechange

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

// Change is one row that differs between two revisions of the fixture file.
type Change struct {
	// Model is the fixture model name; Tables says which table that is.
	Model string
	Kind  Kind
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
	Tables         Tables
	Changes        []Change
}
