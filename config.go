// Package fixturemigrate works on the master data a bun application keeps in a
// dbfixture YAML file: it exports a database into such a file, compares the two,
// and writes the bun migration that closes the gap.
//
// dbfixture only loads a fixture file into an empty database. Once a database
// has been seeded, editing the YAML changes nothing there, so every edit needs a
// data migration. This package writes that migration, from the diff between the
// file and the state the earlier migrations leave (see State) or between the
// database and the file, and it can go the other way and write the file from
// the database. Sync applies the difference to a database directly.
//
// It knows nothing about your Go models. The fixture file says which models and
// columns exist, PostgreSQL's catalog says which tables, types, defaults, keys
// and foreign keys exist, and the configuration file joins the two.
//
// What it will not do is guess. Anything ambiguous comes back as a refusal with
// the model, the row and a reason, and you write that one migration yourself.
//
// LoadProject is where a program starts: a Project runs every command of
// bun-fixture-migrate from Go, with the command's checks and refusals.
package fixturemigrate

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"gopkg.in/yaml.v3"
)

// Config describes the fixture file, the database and the models in it. It is
// read from a YAML file by LoadConfig, or built in code.
type Config struct {
	// Fixture is the path of the fixture YAML file, relative to the
	// configuration file.
	Fixture string `yaml:"fixture"`
	// Fixtures are the fixture files, for an application that loads several
	// with one fixture.Load(ctx, fsys, "a.yml", "b.yml"): in that order, one
	// scope of anchors across them, a row of a later file able to name a row
	// of an earlier one. Set this or Fixture; Prepare puts Fixture here.
	Fixtures []string `yaml:"fixtures"`
	// Out is the directory generated migrations go into, relative to the
	// configuration file.
	Out string `yaml:"out"`
	// Package is the Go package name of that directory. Default "migrations".
	Package string `yaml:"package"`
	// Migrator is the *migrate.Migrations variable the generated file
	// registers with. Default "Migrations".
	Migrator string `yaml:"migrator"`
	// MigrationsTable is the table your migrator records applied migrations
	// in. Default "bun_migrations", which is bun's own default; set it if the
	// migrator is built with migrate.WithTableName. A failing generated
	// migration deletes the record bun made of it before running it, and the
	// status and plan commands read which migrations were applied from here.
	MigrationsTable string `yaml:"migrations_table"`
	// MigrationLocksTable is the table bun's Migrator.Lock inserts a row
	// into while it migrates. Default "bun_migration_locks", bun's own; set
	// it if the migrator is built with migrate.WithLocksTableName. status
	// reports a row a crashed migrator left there, after which every migrate
	// fails until somebody deletes it.
	MigrationLocksTable string `yaml:"migration_locks_table"`
	// State is the state file: the fixture file as the generated migrations
	// leave a database. generate diffs against it and rewrites it, relative to
	// the configuration file. Default "<out>/fixture_state.yml".
	State string `yaml:"state"`
	// SeedGuardTable names a table that is never empty in a seeded database.
	// While it is empty the migration does nothing, because the database has
	// not been seeded yet and dbfixture will load the new state by itself.
	// Leave it out only if the migration chain never runs before the seed.
	SeedGuardTable string `yaml:"seed_guard_table"`
	// LockTimeout is how long a generated migration waits for a lock another
	// session holds on a row it writes before it fails and rolls back, in
	// PostgreSQL's spelling: "5s", "500ms", "1min". Without it a migration
	// can wait behind an open admin transaction for as long as that stays
	// open, with the application's writes to the same rows queued behind the
	// migration. The failed migration runs again on the next deploy. Empty
	// means no limit of the tool's own.
	LockTimeout string `yaml:"lock_timeout"`
	// AuditTable, when set, is the table every generated migration records
	// each of its runs in, in the transaction that made its changes: which
	// changes it applied, found made already or skipped, and why. fixtureapply
	// creates it the first time, which takes CREATE on its schema. A Revert
	// then undoes only the changes the migration made in that database, and
	// status shows per database what a deploy skipped. Optionally
	// schema-qualified; written without one, it is in Schema, as a model's
	// table is. Empty records nothing.
	AuditTable string `yaml:"audit_table"`
	// Database is the PostgreSQL DSN the export, check and scaffold commands
	// read. "env:NAME" reads it from an environment variable, which is how you
	// keep a password out of the repository. The generate command needs it
	// only with -from-db.
	Database string `yaml:"database"`
	// Schema is the default PostgreSQL schema for tables named without one.
	// Default "public".
	Schema string `yaml:"schema"`
	// Policy holds the decisions that depend on how you run your databases
	// rather than on what is correct. See Policy.
	Policy Policy `yaml:"policy"`
	// Models maps the model name used in the fixture file to its
	// configuration. Every model in the fixture file must appear here, and
	// every model here must have a table.
	Models map[string]*Model `yaml:"models"`
}

// Mode is what a policy does when it triggers.
type Mode string

const (
	// ModeError fails and, at run time, rolls the migration back.
	ModeError Mode = "error"
	// ModeWarn reports and carries on.
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

// Policy holds the choices that are genuinely yours. Everything not in here is
// hard-coded, because the alternative would let the tool corrupt a database and
// a knob that does that is not a feature. Each field is documented in the
// example configuration with the consequence of changing it.
type Policy struct {
	// IDDrift decides what happens when the id in the fixture file is not the
	// id the database gave the row: the file's id belongs to another row, or
	// the row lives under a different id. Default "error", because live data
	// pointing at those ids breaks silently otherwise. Set "ignore" only if
	// your ids are internal and nothing outside the database names them.
	IDDrift Mode `yaml:"id_drift"`
	// MissingRow decides what a generated update or delete does when the row
	// it should touch is not in the database at all. Default "error".
	MissingRow Mode `yaml:"missing_row"`
	// ChangedRow decides what a generated update or delete does when the row is
	// there but no longer holds the values the base revision had, which is
	// somebody's hand edit. Default "warn": the row is left alone and their
	// edit is kept.
	ChangedRow Mode `yaml:"changed_row"`
	// ZeroDefault decides what happens when a fixture row writes a type's zero
	// into a column whose database default is not that zero. bun sends DEFAULT
	// for such a value, so the database will not hold the zero the file says.
	// Default "error".
	ZeroDefault Mode `yaml:"zero_default"`
	// NullDefault decides what happens when a fixture row writes an explicit
	// null into a column that has a default. bun sends DEFAULT for a nil
	// pointer and for a zero in a nullzero field, which is how a nullable
	// column is usually modelled, so the database will hold the default and
	// not NULL. Default "error".
	NullDefault Mode `yaml:"null_default"`
	// DuplicateKey decides what happens when two rows of a model share one
	// natural key in the database, which makes every lookup by that key
	// ambiguous. Default "error".
	DuplicateKey Mode `yaml:"duplicate_key"`
	// KeyIndex decides what happens when no unique index or constraint of
	// the table makes a model's natural key unique among its rows: the
	// database then lets the application add a second row with the key, and
	// every change to it fails as a duplicate key from then on. It is
	// checked against a database by check, generate, status and sync, and
	// never written into a migration. Default "warn".
	KeyIndex Mode `yaml:"key_index"`
	// Renames decides what a change of a row's natural key under an unchanged
	// id becomes: "refuse" (default) leaves it to you, "update" writes an
	// UPDATE of the key columns guarded by the id and the old key.
	Renames RenamePolicy `yaml:"renames"`
	// Deletes is the default for Model.Deletes.
	Deletes DeletePolicy `yaml:"deletes"`
	// ArrayNulls is the default for Model.ArrayNulls.
	ArrayNulls ArrayNullsPolicy `yaml:"array_nulls"`

	// Mode is the default for Model.Mode: which rows of a model, and which of
	// their values, the fixture files own. Default "sync".
	Mode Ownership `yaml:"mode"`
}

// Ownership says how much of a model's rows the fixture files own, and so
// what a difference between them and a database is: drift to report and
// migrate, or the database's own business.
type Ownership string

const (
	// OwnSync is the default: the fixture files own every row of the model.
	// A row they add is inserted, a value they change is updated, and a row
	// they do not hold is deleted.
	OwnSync Ownership = "sync"
	// OwnUpsert adds and updates the rows the fixture files hold, and never
	// deletes one: a row a database holds and the files do not, a tenant's
	// or one the application added, is not drift and nothing deletes it.
	OwnUpsert Ownership = "upsert"
	// OwnInsert only seeds: a row the fixture files hold and a database does
	// not, by natural key, is inserted, and nothing else is written. A row
	// the database holds is its own from then on, values and all.
	OwnInsert Ownership = "insert"
)

// IDSource says who gives a model's rows their ids.
type IDSource string

const (
	// IDsFile is the default: the fixture files' ids are the rows' ids. An
	// insert writes them, and a row under another id is id drift.
	IDsFile IDSource = "file"
	// IDsDatabase says the database gives every row its id, from a sequence,
	// an identity or a default. The fixture files may carry ids for their
	// references to resolve against, but a migration never writes one,
	// nothing compares them with a database's, and an export writes none.
	IDsDatabase IDSource = "database"
)

// ArrayNullsPolicy says what a null inside a YAML sequence means for the
// models' array fields. yaml.v3 leaves it out of a []string or []int64 field
// and keeps it in a []*string one, and the tool cannot see which a model has.
type ArrayNullsPolicy string

const (
	// ArrayNullsRefuse makes such a value an "ambiguous value" finding, and
	// refuses a change that carries it. Leaving the null out reads the same
	// for every field.
	ArrayNullsRefuse ArrayNullsPolicy = "refuse"
	// ArrayNullsKeep says the array fields keep a null element ([]*string,
	// []sql.NullString and the like), so the array holds a NULL there.
	ArrayNullsKeep ArrayNullsPolicy = "keep"
)

// RenamePolicy is what to do with a row whose natural key changed.
type RenamePolicy string

const (
	// RenameRefuse reports the rename and writes nothing for that row.
	RenameRefuse RenamePolicy = "refuse"
	// RenameUpdate writes an UPDATE of the key columns.
	RenameUpdate RenamePolicy = "update"
)

// DeletePolicy is what to do with a row that left the fixture file.
type DeletePolicy string

const (
	// DeleteAllow writes a guarded DELETE.
	DeleteAllow DeletePolicy = "allow"
	// DeleteRefuse reports it and writes nothing.
	DeleteRefuse DeletePolicy = "refuse"
	// DeleteCascade writes a guarded DELETE and lets it reach the rows other
	// tables point at it with, through a foreign key declared ON DELETE
	// CASCADE, SET NULL or SET DEFAULT. Under DeleteAllow the migration fails
	// while any such row exists.
	DeleteCascade DeletePolicy = "cascade"
)

// Model is one model of the fixture file.
type Model struct {
	// Table is the SQL table, optionally schema-qualified.
	Table string `yaml:"table"`
	// ID is the primary-key column. Default "id". It is never compared as an
	// ordinary column; it is written on an insert when the fixture row has it,
	// it is what a reference to this model resolves to, and Policy.IDDrift
	// decides what happens when it disagrees with the database.
	//
	// NoID, "none", says the model has no id of its own: a table whose
	// primary key is a reference to another model's row, a plan's details
	// keyed by the plan's id, which the model keys and references by that
	// column instead: id: none, key: [id], references: {id: Plan}. Such a
	// model's rows are found by their key alone, and nothing can point at
	// them. Prepare makes it "", and keeps it so.
	ID string `yaml:"id"`
	// Ref is the column a reference to this model matches on. Default "name".
	Ref string `yaml:"ref"`
	// Serial is true when ID comes from a sequence. The migration then moves
	// the sequence past any explicit id it wrote.
	Serial bool `yaml:"serial"`
	// Key lists the columns that identify a row without using its id.
	// Default: the Ref column.
	Key []string `yaml:"key"`
	// KeyAnyOf adds, per group, the first column of that group that holds a
	// value other than "", 0 or NULL. It covers tables with a handful of
	// mutually exclusive foreign-key columns, where which column is set is
	// part of the row's identity.
	KeyAnyOf [][]string `yaml:"key_any_of"`
	// References maps a column to the model it points at. Such a column holds
	// the target's ID in the database, but the migration carries the target's
	// Ref value and looks the id up where it runs.
	References map[string]string `yaml:"references"`
	// Derived lists columns your application recalculates. They are never
	// compared, never written and never exported.
	Derived []string `yaml:"derived"`
	// Ignore lists columns that take no part at all, for instance a column
	// dbfixture fills with a template the generator cannot read.
	Ignore []string `yaml:"ignore"`
	// Defaults gives the value a column has when the fixture row leaves it
	// out. Without an entry an omitted column is treated as "not set", which
	// compares equal to another omitted column and to nothing else. The
	// scaffold command fills this in from the database's column defaults. A
	// YAML null (~) means NULL, which is what a column added to the table
	// later holds in the rows written before it.
	Defaults Defaults `yaml:"defaults"`
	// Deletes overrides Policy.Deletes for this model. Set "refuse" wherever
	// other tables can point at the row and the tool cannot know what should
	// happen to them.
	Deletes DeletePolicy `yaml:"deletes"`
	// ArrayNulls overrides Policy.ArrayNulls for this model.
	ArrayNulls ArrayNullsPolicy `yaml:"array_nulls"`
	// IDDrift, MissingRow, ChangedRow and DuplicateKey override the policy
	// block's for this model, when set: what generate, check and the
	// generated migration do when one of this model's rows is not as
	// expected. A model whose rows an admin UI edits can keep their edits
	// under changed_row: warn while another fails on one under error. They
	// are written into the generated migration, as the policy block is.
	IDDrift      Mode `yaml:"id_drift"`
	MissingRow   Mode `yaml:"missing_row"`
	ChangedRow   Mode `yaml:"changed_row"`
	DuplicateKey Mode `yaml:"duplicate_key"`
	// KeyIndex overrides Policy.KeyIndex for this model. It is checked
	// against a database and is not written into the migration.
	KeyIndex Mode `yaml:"key_index"`
	// Where is an SQL predicate that limits which rows of the table are master
	// data, for a table that holds other rows too. It is written into every
	// query the export and check commands run, and it is your text: keep it
	// out of reach of anything untrusted.
	Where string `yaml:"where"`

	// Mode overrides Policy.Mode for this model: sync, upsert or insert; see
	// Ownership. Deletes only means something under sync, and is refused
	// with the others.
	Mode Ownership `yaml:"mode"`
	// InsertOnly lists columns an insert writes and the database owns
	// afterwards: a feature flag's enabled, set when the flag is created and
	// toggled in production from then on. An insert writes them, and
	// nothing else touches or compares them: no update writes them, no
	// delete is guarded by them, and check reports no difference in them.
	// Unlike ignore, which is never written, and derived, which the
	// application recalculates, they are the file's until the row exists.
	// A column of the natural key, the ref column or the id cannot be one.
	InsertOnly []string `yaml:"insert_only"`
	// IDs says who gives the rows their ids: "file" (default), or "database"
	// for a table the application inserts into too, whose sequence the
	// fixture files' ids would collide with; see IDSource. Under database
	// the natural key and the ref column cannot be the id.
	IDs IDSource `yaml:"ids"`
	// SoftDelete is the column of the model's bun soft_delete field, for a
	// model the application deletes rows of with bun's soft delete: a
	// nullable timestamptz or timestamp column, NULL for a live row. Only
	// live rows are master data. check, sync and export read no other, a
	// generated migration's delete sets the column to now() rather than
	// delete the row, and its insert restores a soft-deleted row that holds
	// its values before it writes a new one. The column itself is never
	// compared, written or exported. The tool never guesses it from a
	// deleted_at column: without the Go tag, bun reads every row.
	SoftDelete string `yaml:"soft_delete"`

	derived    map[string]bool
	ignored    map[string]bool
	insertOnly map[string]bool
	// noID is set by Prepare for a model whose ID is NoID, so a second
	// Prepare keeps the ID it made "".
	noID bool
	// deletesInherited is set when Deletes was filled in from the policy,
	// so a second Prepare can tell it from one the model sets.
	deletesInherited bool
}

// NoID is the id of a model that has none of its own: see Model.ID.
const NoID = "none"

// Defaults maps a column to the value a fixture row that leaves it out stands
// for. NullDefault is NULL.
type Defaults map[string]string

// NullDefault is how Defaults holds NULL, which a YAML null (~) in the
// configuration file decodes to.
const NullDefault = "\x00NULL"

// UnmarshalYAML reads a mapping of scalars, a null as NullDefault.
func (d *Defaults) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: defaults has to be a mapping of columns to values", n.Line)
	}
	out := Defaults{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		switch {
		case value.ShortTag() == "!!null":
			out[key.Value] = NullDefault
		case value.Kind == yaml.ScalarNode:
			out[key.Value] = value.Value
		default:
			return fmt.Errorf("line %d: the default of %s is not a single value", value.Line, key.Value)
		}
	}
	*d = out
	return nil
}

// LoadConfig reads a configuration file and fills in the defaults.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		// yaml.v3 puts each of several errors on a line of its own; an
		// error here is one line.
		var typeErr *yaml.TypeError
		if errors.As(err, &typeErr) {
			return nil, fmt.Errorf("%s: yaml: %s", path, strings.Join(typeErr.Errors, "; "))
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.Prepare(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

// Prepare fills in the defaults and checks the configuration. LoadConfig calls
// it; a Config built in code has to.
func (c *Config) Prepare() error {
	if c.Package == "" {
		c.Package = "migrations"
	}
	if c.Migrator == "" {
		c.Migrator = "Migrations"
	}
	if c.Schema == "" {
		c.Schema = "public"
	}
	// Prepare fills one from the other, and has to stay repeatable.
	switch {
	case c.Fixture != "" && len(c.Fixtures) > 0 && c.Fixtures[0] != c.Fixture:
		return fmt.Errorf("fixture and fixtures both set; name the files in fixtures")
	case len(c.Fixtures) > 0:
		c.Fixture = c.Fixtures[0]
	case c.Fixture != "":
		c.Fixtures = []string{c.Fixture}
	}
	seenFixture := map[string]bool{}
	for _, f := range c.Fixtures {
		if f == "" || seenFixture[f] {
			return fmt.Errorf("fixtures: %q is empty or listed twice", f)
		}
		seenFixture[f] = true
	}
	if c.MigrationsTable == "" {
		c.MigrationsTable = "bun_migrations"
	}
	if err := fixtureapply.Validate(fixturechange.Set{LockTimeout: c.LockTimeout}); err != nil {
		return err
	}
	if _, err := quoteQualified(c.MigrationsTable); err != nil {
		return fmt.Errorf("migrations_table: %w", err)
	}
	if c.MigrationLocksTable == "" {
		c.MigrationLocksTable = "bun_migration_locks"
	}
	if _, err := quoteQualified(c.MigrationLocksTable); err != nil {
		return fmt.Errorf("migration_locks_table: %w", err)
	}
	if c.AuditTable != "" {
		if _, err := quoteQualified(c.AuditTable); err != nil {
			return fmt.Errorf("audit_table: %w", err)
		}
	}
	if c.State == "" && c.Out != "" {
		c.State = filepath.Join(c.Out, "fixture_state.yml")
	}
	if err := c.Policy.prepare(); err != nil {
		return err
	}
	if len(c.Models) == 0 {
		return fmt.Errorf("no models configured")
	}
	for _, name := range c.ModelNames() {
		m := c.Models[name]
		if m == nil {
			return fmt.Errorf("model %q: empty", name)
		}
		if m.Table == "" {
			return fmt.Errorf("model %q: no table", name)
		}
		// The run time refuses a where that could reach past its own
		// parentheses; better here than in the first migration that uses it.
		if m.Where != "" {
			set := fixturechange.Set{Tables: fixturechange.Tables{name: {Name: m.Table, Where: m.Where}}}
			if err := fixtureapply.Validate(set); err != nil {
				return err
			}
		}
		switch {
		case m.ID == NoID:
			m.noID, m.ID = true, ""
		case m.ID == "" && !m.noID:
			m.ID = "id"
		}
		if m.Ref == "" {
			m.Ref = "name"
		}
		if len(m.Key) == 0 && len(m.KeyAnyOf) == 0 {
			m.Key = []string{m.Ref}
		}
		for _, col := range m.Key {
			if col == "" {
				return fmt.Errorf("model %q: empty key column", name)
			}
		}
		for i, group := range m.KeyAnyOf {
			if len(group) == 0 {
				return fmt.Errorf("model %q: key_any_of group %d is empty", name, i)
			}
		}
		for _, col := range sortedKeysOf(m.References) {
			target := m.References[col]
			tm, ok := c.Models[target]
			if !ok {
				return fmt.Errorf("model %q: column %q references unknown model %q", name, col, target)
			}
			// A reference holds the id of the row it names, which a model
			// without one does not have.
			if tm != nil && tm.hasNoID() {
				hint := ""
				if len(tm.Key) == 1 && tm.References[tm.Key[0]] != "" {
					hint = fmt.Sprintf(", %s, whose id %s's %s holds", tm.References[tm.Key[0]], target, tm.Key[0])
				}
				return fmt.Errorf("model %q: column %q references %s, which has no id of its own (id: none), and a "+
					"reference holds the id of the row it names: point it at the model %s's key points at%s",
					name, col, target, target, hint)
			}
		}
		// The id is the row's own value to the tool, compared and written as
		// it stands; a reference is looked up by the name of the row it
		// names. A primary key that is also a reference, a plan's details
		// keyed by the plan, would be read as the template text naming the
		// plan.
		if target, ok := m.References[m.ID]; ok && m.ID != "" {
			return fmt.Errorf("model %q: its id, %s, is also a reference to %s, and the tool reads an id as the "+
				"row's own value, never as a reference to look up: set id: none, so the model has no id of its "+
				"own, and keep %s in key and references", name, m.ID, target, m.ID)
		}
		if m.noID {
			switch {
			case m.Serial:
				return fmt.Errorf("model %q: serial is true, and id is none: a model without an id has no "+
					"sequence to move; take serial out", name)
			case m.IDs == IDsDatabase:
				return fmt.Errorf("model %q: ids is database, and id is none: a model without an id has none "+
					"for the database to give; take ids out", name)
			}
		}
		if err := m.prepareOwnership(name, c.Policy); err != nil {
			return err
		}
		if m.Deletes == "" {
			m.Deletes = c.Policy.Deletes
		}
		if m.Deletes != DeleteAllow && m.Deletes != DeleteRefuse && m.Deletes != DeleteCascade {
			return fmt.Errorf("model %q: deletes is %q, it has to be %q, %q or %q", name, m.Deletes,
				DeleteAllow, DeleteRefuse, DeleteCascade)
		}
		if err := m.prepareSoftDelete(name); err != nil {
			return err
		}
		if m.ArrayNulls != "" && !m.ArrayNulls.valid() {
			return fmt.Errorf("model %q: array_nulls is %q, it has to be %q or %q", name, m.ArrayNulls,
				ArrayNullsRefuse, ArrayNullsKeep)
		}
		if err := m.prepareKeyIndex(name); err != nil {
			return err
		}
		for _, f := range m.runTimePolicy() {
			if *f.value != "" && !f.value.valid(f.allowed...) {
				return fmt.Errorf("model %q: %s is %q, it has to be one of %s, or left out for the policy block's",
					name, f.name, *f.value, modeList(f.allowed))
			}
		}
		m.derived = set(m.Derived)
		m.ignored = set(m.Ignore)
	}
	return nil
}

func (p *Policy) prepare() error {
	type field struct {
		name    string
		value   *Mode
		def     Mode
		allowed []Mode
	}
	for _, f := range []field{
		{"id_drift", &p.IDDrift, ModeError, []Mode{ModeError, ModeWarn, ModeIgnore}},
		{"missing_row", &p.MissingRow, ModeError, []Mode{ModeError, ModeWarn}},
		{"changed_row", &p.ChangedRow, ModeWarn, []Mode{ModeError, ModeWarn}},
		{"zero_default", &p.ZeroDefault, ModeError, []Mode{ModeError, ModeWarn, ModeIgnore}},
		{"null_default", &p.NullDefault, ModeError, []Mode{ModeError, ModeWarn, ModeIgnore}},
		{"duplicate_key", &p.DuplicateKey, ModeError, []Mode{ModeError, ModeWarn}},
		{"key_index", &p.KeyIndex, ModeWarn, keyIndexModes},
	} {
		if *f.value == "" {
			*f.value = f.def
		}
		if !f.value.valid(f.allowed...) {
			return fmt.Errorf("policy %s is %q, it has to be one of %s", f.name, *f.value, modeList(f.allowed))
		}
	}
	if p.Renames == "" {
		p.Renames = RenameRefuse
	}
	if p.Renames != RenameRefuse && p.Renames != RenameUpdate {
		return fmt.Errorf("policy renames is %q, it has to be %q or %q", p.Renames, RenameRefuse, RenameUpdate)
	}
	if p.Deletes == "" {
		p.Deletes = DeleteAllow
	}
	if p.Deletes != DeleteAllow && p.Deletes != DeleteRefuse && p.Deletes != DeleteCascade {
		return fmt.Errorf("policy deletes is %q, it has to be %q, %q or %q", p.Deletes,
			DeleteAllow, DeleteRefuse, DeleteCascade)
	}
	if p.ArrayNulls == "" {
		p.ArrayNulls = ArrayNullsRefuse
	}
	if !p.ArrayNulls.valid() {
		return fmt.Errorf("policy array_nulls is %q, it has to be %q or %q", p.ArrayNulls,
			ArrayNullsRefuse, ArrayNullsKeep)
	}
	if p.Mode == "" {
		p.Mode = OwnSync
	}
	if !p.Mode.valid() {
		return fmt.Errorf("policy mode is %q, it has to be %q, %q or %q", p.Mode, OwnSync, OwnUpsert, OwnInsert)
	}
	return nil
}

func (o Ownership) valid() bool { return o == OwnSync || o == OwnUpsert || o == OwnInsert }

// prepareOwnership fills in and checks what a model says about who owns its
// rows, their values and their ids: mode, insert_only and ids. It runs before
// Deletes is filled in from the policy, and stays repeatable.
func (m *Model) prepareOwnership(name string, p Policy) error {
	if m.Mode == "" {
		m.Mode = p.Mode
	}
	if !m.Mode.valid() {
		return fmt.Errorf("model %q: mode is %q, it has to be %q, %q or %q", name, m.Mode, OwnSync, OwnUpsert, OwnInsert)
	}
	if m.Deletes == "" {
		m.deletesInherited = true
	}
	// Under upsert and insert nothing is deleted, so a deletes the model
	// sets would say something that never happens.
	if m.Mode != OwnSync && !m.deletesInherited {
		return fmt.Errorf("model %q: deletes is %q, but mode is %s, under which no row is ever deleted: deletes "+
			"only applies under mode sync, so take one of them out", name, m.Deletes, m.Mode)
	}
	if m.IDs == "" {
		m.IDs = IDsFile
	}
	if m.IDs != IDsFile && m.IDs != IDsDatabase {
		return fmt.Errorf("model %q: ids is %q, it has to be %q or %q", name, m.IDs, IDsFile, IDsDatabase)
	}
	if m.IDs == IDsDatabase {
		// The id differs from one database to the next, so nothing that has
		// to name the same row everywhere can be made of it.
		if m.Ref == m.ID {
			return fmt.Errorf("model %q: ids is database, so its %s differs from one database to the next, and "+
				"references cannot name a row by it: set ref to a column of its own", name, m.ID)
		}
		for _, col := range m.keyColumns() {
			if col == m.ID {
				return fmt.Errorf("model %q: ids is database, so its %s differs from one database to the next, and "+
					"cannot be part of the natural key: key on the columns that name a row everywhere", name, m.ID)
			}
		}
	}
	key := set(m.keyColumns())
	seen := map[string]bool{}
	for _, col := range m.InsertOnly {
		var why string
		switch {
		case col == "":
			why = "is empty"
		case seen[col]:
			why = "is listed twice"
		case col == m.ID:
			why = "is the id, which is written on an insert and never updated anyway"
		case key[col]:
			why = "is part of the natural key, which every row is found by and has to be the files'"
		case col == m.Ref:
			why = "is the ref column, which every reference to the model names its row by and has to be the files'"
		case slices.Contains(m.Ignore, col):
			why = "is in ignore, which is never written at all"
		case slices.Contains(m.Derived, col):
			why = "is in derived, which the application writes, not an insert"
		}
		if why != "" {
			return fmt.Errorf("model %q: insert_only column %q %s", name, col, why)
		}
		seen[col] = true
	}
	m.insertOnly = seen
	return nil
}

// prepareSoftDelete checks a model's soft_delete. The column says whether a
// row is live and nothing else, so it can be no other column the model names:
// a change never writes or compares it. It runs once Deletes is filled in.
func (m *Model) prepareSoftDelete(name string) error {
	col := m.SoftDelete
	if col == "" {
		return nil
	}
	if !identPattern.MatchString(col) {
		return fmt.Errorf("model %q: soft_delete is %q, which is not a plain column name", name, col)
	}
	_, isDefault := m.Defaults[col]
	_, isReference := m.References[col]
	var why string
	switch {
	case col == m.ID:
		why = "is the id"
	case slices.Contains(m.keyColumns(), col):
		why = "is part of the natural key, which a soft-deleted row keeps"
	case col == m.Ref:
		why = "is the ref column, which every reference to the model names its row by"
	case slices.Contains(m.Ignore, col):
		why = "is in ignore; take it out of there, soft_delete leaves it out of every comparison already"
	case slices.Contains(m.Derived, col):
		why = "is in derived; take it out of there, soft_delete leaves it out of every comparison already"
	case slices.Contains(m.InsertOnly, col):
		why = "is in insert_only, which an insert writes"
	case isDefault:
		why = "has a default in defaults, and a fixture row leaving it out is live"
	case isReference:
		why = "is a reference"
	case m.Deletes == DeleteCascade && !m.deletesInherited:
		why = "is set with deletes: cascade, and a soft delete reaches no row through a foreign key: take deletes out"
	case m.Mode == OwnInsert:
		// A soft-deleted row is a row the database holds and owns under
		// mode insert, and must not come back; a migration cannot tell,
		// since mode is not in what it carries.
		why = "is set with mode insert, which this version does not support: a soft-deleted row would be " +
			"restored by an insert of its key, and under mode insert the database owns it"
	case mentions(m.Where, col):
		why = "is named in where: drop it from where, soft_delete already limits the model to live rows, and " +
			"a migration that restores a row has to find the soft-deleted ones"
	}
	if why != "" {
		return fmt.Errorf("model %q: soft_delete column %q %s", name, col, why)
	}
	return nil
}

// mentions reports whether an SQL predicate names a column: as a word outside
// quotes, string constants and comments, in any case, or as a quoted name
// exactly.
func mentions(where, col string) bool {
	identChar := func(c byte) bool {
		return c == '_' || c == '$' || (c >= '0' && c <= '9') || (c|0x20 >= 'a' && c|0x20 <= 'z') || c >= 0x80
	}
	for i := 0; i < len(where); i++ {
		c := where[i]
		switch {
		case c == '\'':
			escapes := i > 0 && (where[i-1]|0x20) == 'e' && (i == 1 || !identChar(where[i-2]))
			j := i + 1
			for ; j < len(where) && where[j] != '\''; j++ {
				if escapes && where[j] == '\\' {
					j++
				}
			}
			i = j
		case c == '"':
			j := strings.IndexByte(where[i+1:], '"')
			if j < 0 {
				return false
			}
			if where[i+1:i+1+j] == col {
				return true
			}
			i += j + 1
		case c == '$' && (i == 0 || !identChar(where[i-1])):
			tag := dollarTag.FindString(where[i:])
			if tag == "" {
				continue
			}
			j := strings.Index(where[i+len(tag):], tag)
			if j < 0 {
				return false
			}
			i += len(tag) + j + len(tag) - 1
		case c == '-' && strings.HasPrefix(where[i:], "--"):
			j := strings.IndexByte(where[i:], '\n')
			if j < 0 {
				return false
			}
			i += j
		case c == '/' && strings.HasPrefix(where[i:], "/*"):
			nest, j := 1, i+2
			for ; j < len(where) && nest > 0; j++ {
				switch {
				case strings.HasPrefix(where[j:], "/*"):
					nest, j = nest+1, j+1
				case strings.HasPrefix(where[j:], "*/"):
					nest, j = nest-1, j+1
				}
			}
			i = j - 1
		case identChar(c) && (i == 0 || !identChar(where[i-1])):
			j := i
			for j < len(where) && identChar(where[j]) {
				j++
			}
			if strings.EqualFold(where[i:j], col) {
				return true
			}
			i = j - 1
		}
	}
	return false
}

// dollarTag is the opening of a dollar-quoted string, $$ or $tag$.
var dollarTag = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*)?\$`)

// ownsValue reports whether the fixture files own a column's value in a row
// the database already holds: compare it, update it and guard a delete with
// it. They own none under mode insert, and no insert_only column.
func (m *Model) ownsValue(col string) bool {
	return m.Mode != OwnInsert && !m.insertOnly[col]
}

// hasNoID reports a model without an id of its own (NoID), before Prepare
// or after it.
func (m *Model) hasNoID() bool { return m.noID || m.ID == NoID }

// idsFromDatabase reports a model whose ids the database gives: see
// IDsDatabase.
func (m *Model) idsFromDatabase() bool { return m.IDs == IDsDatabase }

func (a ArrayNullsPolicy) valid() bool { return a == ArrayNullsRefuse || a == ArrayNullsKeep }

// policyField is a policy a model can override: its key in the
// configuration, the model's value and the values it may take.
type policyField struct {
	name    string
	value   *Mode
	allowed []Mode
}

// runTimePolicy is the model's overrides of the policies a generated
// migration carries, in the order Policy declares them.
func (m *Model) runTimePolicy() []policyField {
	return []policyField{
		{"id_drift", &m.IDDrift, []Mode{ModeError, ModeWarn, ModeIgnore}},
		{"missing_row", &m.MissingRow, []Mode{ModeError, ModeWarn}},
		{"changed_row", &m.ChangedRow, []Mode{ModeError, ModeWarn}},
		{"duplicate_key", &m.DuplicateKey, []Mode{ModeError, ModeWarn}},
	}
}

// ModelPolicy is the policy that governs a model: the policy block, with
// every policy the model sets for itself in its place.
func (c *Config) ModelPolicy(model string) Policy {
	p := c.Policy
	m := c.Models[model]
	if m == nil {
		return p
	}
	for _, o := range []struct{ to, from *Mode }{
		{&p.IDDrift, &m.IDDrift}, {&p.MissingRow, &m.MissingRow},
		{&p.ChangedRow, &m.ChangedRow}, {&p.DuplicateKey, &m.DuplicateKey},
		{&p.KeyIndex, &m.KeyIndex},
	} {
		if *o.from != "" {
			*o.to = *o.from
		}
	}
	return p
}

// policyName names, for a message, where the value of a model's policy key
// comes from, as ModelPolicy resolves it: "policy.id_drift" for the policy
// block's, and "the model's id_drift" where the model sets it itself, which
// is where it has to be changed. key is id_drift, missing_row, changed_row,
// duplicate_key, deletes or array_nulls.
func (c *Config) policyName(model, key string) string {
	own := false
	if m := c.Models[model]; m != nil {
		switch key {
		case "deletes":
			own = m.Deletes != "" && !m.deletesInherited
		case "array_nulls":
			own = m.ArrayNulls != ""
		default:
			for _, f := range m.runTimePolicy() {
				own = own || (f.name == key && *f.value != "")
			}
		}
	}
	if own {
		return "the model's " + key
	}
	return "policy." + key
}

// TablePolicy is what a change set carries for a model in its table: the
// policies the model sets for itself, the rest left to the set's own, and nil
// when it sets none.
func (c *Config) TablePolicy(model string) *fixturechange.Policy {
	m := c.Models[model]
	if m == nil || m.IDDrift == "" && m.MissingRow == "" && m.ChangedRow == "" && m.DuplicateKey == "" {
		return nil
	}
	return &fixturechange.Policy{
		MissingRow:   fixturechange.Mode(m.MissingRow),
		ChangedRow:   fixturechange.Mode(m.ChangedRow),
		IDDrift:      fixturechange.Mode(m.IDDrift),
		DuplicateKey: fixturechange.Mode(m.DuplicateKey),
	}
}

// ModeOf is what the policy makes of a finding: FindingMode of its kind, and
// for a duplicate key the model's own duplicate_key when it sets one.
func (c *Config) ModeOf(f Finding) Mode {
	if f.Kind == FindingDuplicateKey {
		return c.ModelPolicy(f.Model).DuplicateKey
	}
	if f.Kind == FindingUnbackedKey {
		return keyIndexMode(c.ModelPolicy(f.Model).KeyIndex, f)
	}
	return c.FindingMode(f.Kind)
}

// keyIndexModes are the values key_index takes, in the policy block and in a
// model.
var keyIndexModes = []Mode{ModeError, ModeWarn, ModeIgnore}

// prepareKeyIndex checks the key_index a model sets for itself. It is no
// run-time policy, so it is not among runTimePolicy's.
func (m *Model) prepareKeyIndex(name string) error {
	if m.KeyIndex != "" && !m.KeyIndex.valid(keyIndexModes...) {
		return fmt.Errorf("model %q: key_index is %q, it has to be one of %s, or left out for the policy block's",
			name, m.KeyIndex, modeList(keyIndexModes))
	}
	return nil
}

// keyIndexMode is what key_index makes of an unbacked-key finding: the
// policy, but never more than a warning for one the lint could not decide,
// which says so.
func keyIndexMode(mode Mode, f Finding) Mode {
	if f.unsure && mode == ModeError {
		return ModeWarn
	}
	return mode
}

// arrayNulls is what a null inside a sequence means for a model.
func (c *Config) arrayNulls(m *Model) ArrayNullsPolicy {
	if m.ArrayNulls != "" {
		return m.ArrayNulls
	}
	return c.Policy.ArrayNulls
}

func modeList(modes []Mode) string {
	out := make([]string, 0, len(modes))
	for _, m := range modes {
		out = append(out, string(m))
	}
	return strings.Join(out, ", ")
}

// FixtureLabel names the fixture files in messages.
func (c *Config) FixtureLabel() string {
	if len(c.Fixtures) == 0 {
		return c.Fixture
	}
	return strings.Join(c.Fixtures, ", ")
}

// ModelNames lists the configured models in alphabetical order.
func (c *Config) ModelNames() []string {
	out := make([]string, 0, len(c.Models))
	for name := range c.Models {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// model returns the configuration of a model, or an error naming the model. A
// model in the fixture file that nobody configured is a mistake, not "nothing
// to do": a silently skipped model is how a change goes missing.
func (c *Config) model(name string) (*Model, error) {
	m, ok := c.Models[name]
	if !ok {
		return nil, fmt.Errorf("model %q is in the fixture file but not in the configuration", name)
	}
	return m, nil
}

// QualifiedTable is a model's table with the configured schema put in front of
// it when the table was written without one.
func (c *Config) QualifiedTable(m *Model) string {
	if strings.Contains(m.Table, ".") {
		return m.Table
	}
	return c.Schema + "." + m.Table
}

// RunTimeTable is how a change set names a table: as the configuration
// writes it when the default schema is public, and qualified with the
// default schema otherwise. A migration runs on the application's own
// connection, whose search_path nothing here can vouch for: a table of schema
// app named "roles" there is not found, or a public.roles is found instead.
// Keeping public tables unqualified keeps the migrations generated before
// this the same.
func (c *Config) RunTimeTable(table string) string {
	if table == "" || strings.Contains(table, ".") || c.Schema == "" || c.Schema == "public" {
		return table
	}
	return c.Schema + "." + table
}

// Schemas is every schema a model's table is in: Schema, for the tables
// named without one, and the schema of every table named with one. The
// catalog has to be read from all of them; a model in a schema it was not
// read from looks like a table that does not exist.
func (c *Config) Schemas() []string {
	out := []string{c.Schema}
	seen := map[string]bool{c.Schema: true}
	for _, name := range c.ModelNames() {
		if schema, _, ok := strings.Cut(c.Models[name].Table, "."); ok && !seen[schema] {
			seen[schema] = true
			out = append(out, schema)
		}
	}
	return out
}

// DependencyOrder sorts the models so a model comes after everything it points
// at, which is the order a fixture file has to be written in for dbfixture to
// resolve its references. A cycle is reported rather than broken: only you can
// say which of the two rows is written first.
func (c *Config) DependencyOrder() ([]string, error) {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	state := map[string]int{}
	var order []string
	var stack []string
	var visit func(string) error
	visit = func(name string) error {
		switch state[name] {
		case black:
			return nil
		case grey:
			return fmt.Errorf("the models %s reference each other in a circle; "+
				"put them in the fixture file by hand, or break the circle with ignore",
				strings.Join(append(stack, name), " -> "))
		}
		state[name] = grey
		stack = append(stack, name)
		m := c.Models[name]
		targets := make([]string, 0, len(m.References))
		for col, target := range m.References {
			if m.skip(col) || target == name {
				continue
			}
			targets = append(targets, target)
		}
		sort.Strings(targets)
		for _, target := range targets {
			if err := visit(target); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		state[name] = black
		order = append(order, name)
		return nil
	}
	for _, name := range c.ModelNames() {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// skip reports whether a column takes no part in the comparison.
func (m *Model) skip(col string) bool {
	return col == anchorColumn || col == m.ID || m.ignored[col] || m.derived[col] ||
		(m.SoftDelete != "" && col == m.SoftDelete)
}

// inKeyAnyOf reports a column of a key_any_of group.
func (m *Model) inKeyAnyOf(col string) bool {
	for _, group := range m.KeyAnyOf {
		for _, c := range group {
			if c == col {
				return true
			}
		}
	}
	return false
}

// keyColumns is every column that can end up in a natural key.
func (m *Model) keyColumns() []string {
	out := append([]string{}, m.Key...)
	for _, group := range m.KeyAnyOf {
		out = append(out, group...)
	}
	return out
}

func set(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, s := range list {
		out[s] = true
	}
	return out
}
