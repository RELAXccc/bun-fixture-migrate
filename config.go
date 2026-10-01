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
	// Renames decides what a change of a row's natural key under an unchanged
	// id becomes: "refuse" (default) leaves it to you, "update" writes an
	// UPDATE of the key columns guarded by the id and the old key.
	Renames RenamePolicy `yaml:"renames"`
	// Deletes is the default for Model.Deletes.
	Deletes DeletePolicy `yaml:"deletes"`
	// ArrayNulls is the default for Model.ArrayNulls.
	ArrayNulls ArrayNullsPolicy `yaml:"array_nulls"`
}

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
	// Where is an SQL predicate that limits which rows of the table are master
	// data, for a table that holds other rows too. It is written into every
	// query the export and check commands run, and it is your text: keep it
	// out of reach of anything untrusted.
	Where string `yaml:"where"`

	derived map[string]bool
	ignored map[string]bool
}

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
		if m.ID == "" {
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
		for col, target := range m.References {
			if _, ok := c.Models[target]; !ok {
				return fmt.Errorf("model %q: column %q references unknown model %q", name, col, target)
			}
		}
		if m.Deletes == "" {
			m.Deletes = c.Policy.Deletes
		}
		if m.Deletes != DeleteAllow && m.Deletes != DeleteRefuse && m.Deletes != DeleteCascade {
			return fmt.Errorf("model %q: deletes is %q, it has to be %q, %q or %q", name, m.Deletes,
				DeleteAllow, DeleteRefuse, DeleteCascade)
		}
		if m.ArrayNulls != "" && !m.ArrayNulls.valid() {
			return fmt.Errorf("model %q: array_nulls is %q, it has to be %q or %q", name, m.ArrayNulls,
				ArrayNullsRefuse, ArrayNullsKeep)
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
	return nil
}

func (a ArrayNullsPolicy) valid() bool { return a == ArrayNullsRefuse || a == ArrayNullsKeep }

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
	return col == anchorColumn || col == m.ID || m.ignored[col] || m.derived[col]
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
