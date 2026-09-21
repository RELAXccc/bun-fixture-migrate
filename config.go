// Package fixturemigrate turns two revisions of a bun dbfixture YAML file into
// a bun migration that brings an already-seeded database from the old state to
// the new one.
//
// dbfixture only loads a fixture file into an empty database. Once a database
// has been seeded, editing the YAML changes nothing there, so every edit needs
// a data migration. This package writes that migration: it diffs the two
// revisions row by row, using a natural key you configure instead of the
// row ids, and renders a Go file that hands the difference to
// fixtureapply.Apply.
//
// What it will not do is guess. Renames, deletes of rows other tables may
// reference, and rows whose natural key is not unique come back as refusals
// with a reason, and you write those migrations yourself.
package fixturemigrate

import (
	"bytes"
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"
)

// Config describes the fixture file and the models in it. It is read from a
// YAML file by LoadConfig, or built in code.
type Config struct {
	// Fixture is the path of the fixture YAML file, relative to the
	// configuration file.
	Fixture string `yaml:"fixture"`
	// Out is the directory generated migrations go into, relative to the
	// configuration file.
	Out string `yaml:"out"`
	// Package is the Go package name of that directory. Default "migrations".
	Package string `yaml:"package"`
	// Migrator is the *migrate.Migrations variable the generated file
	// registers with. Default "Migrations".
	Migrator string `yaml:"migrator"`
	// SeedGuardTable names a table that is never empty in a seeded database.
	// While it is empty the migration does nothing, because the database has
	// not been seeded yet and dbfixture will load the new state by itself.
	// Leave it out only if the migration chain never runs before the seed.
	SeedGuardTable string `yaml:"seed_guard_table"`
	// Models maps the model name used in the fixture file to its
	// configuration. Every model in the fixture file must appear here.
	Models map[string]*Model `yaml:"models"`
}

// Model is one model of the fixture file.
type Model struct {
	// Table is the SQL table, optionally schema-qualified.
	Table string `yaml:"table"`
	// ID is the primary-key column. Default "id". It is never compared and
	// never updated; it is written on an insert when the fixture row has it,
	// and it is what a reference to this model resolves to.
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
	// StableID names a column that survives a rename, usually ID or
	// dbfixture's "_id" anchor. With it the generator can tell a rename from
	// a delete plus an insert, and refuse it.
	StableID string `yaml:"stable_id"`
	// References maps a column to the model it points at. Such a column holds
	// the target's ID in the database, but the migration carries the target's
	// Ref value and looks the id up where it runs.
	References map[string]string `yaml:"references"`
	// Derived lists columns your application recalculates. They are never
	// compared and never written.
	Derived []string `yaml:"derived"`
	// Ignore lists columns that take no part at all, for instance a column
	// dbfixture fills with a template the generator cannot read.
	Ignore []string `yaml:"ignore"`
	// Defaults gives the value a column has when the fixture row leaves it
	// out. Without an entry an omitted column is treated as "not set", which
	// compares equal to another omitted column and to nothing else.
	Defaults map[string]string `yaml:"defaults"`
	// NoDelete refuses deletes of this model's rows. Set it wherever other
	// tables can point at the row and the generator cannot know what should
	// happen to them.
	NoDelete bool `yaml:"no_delete"`

	derived map[string]bool
	ignored map[string]bool
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
	if len(c.Models) == 0 {
		return fmt.Errorf("no models configured")
	}
	for _, name := range c.modelNames() {
		m := c.Models[name]
		if m == nil {
			return fmt.Errorf("model %q: empty", name)
		}
		if m.Table == "" {
			return fmt.Errorf("model %q: no table", name)
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
		m.derived = set(m.Derived)
		m.ignored = set(m.Ignore)
	}
	return nil
}

func (c *Config) modelNames() []string {
	out := make([]string, 0, len(c.Models))
	for name := range c.Models {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// model returns the configuration of a model, or an error naming the file the
// model came from. A model in the fixture file that nobody configured is a
// mistake, not "nothing to do".
func (c *Config) model(name string) (*Model, error) {
	m, ok := c.Models[name]
	if !ok {
		return nil, fmt.Errorf("model %q is in the fixture file but not in the configuration", name)
	}
	return m, nil
}

// skip reports whether a column takes no part in the comparison.
func (m *Model) skip(col string) bool {
	return col == anchorColumn || col == m.ID || m.ignored[col] || m.derived[col]
}

func set(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, s := range list {
		out[s] = true
	}
	return out
}
