package fixturemigrate

import (
	"fmt"
	"sort"
	"strings"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
)

// Scaffold writes a starter configuration from a database.
//
// The adoption cost of a tool like this is that you describe your schema a
// second time. You do not have to: PostgreSQL already knows the tables, the
// primary keys, the unique indexes, the foreign keys, the column defaults and
// the sequences, which is nearly all of it. What is left is genuinely yours —
// which tables hold master data, which columns your application recalculates,
// and the policy block — and the comments say so where a guess was made.
//
// Read what comes out. It is a first draft made of guesses, not a description
// of your intentions: the natural key in particular is guessed from the
// narrowest unique index, and a table with no unique index other than its
// primary key gets one the tool cannot check.
func Scaffold(tables map[string]*dbschema.Table, only []string, schema string) []byte {
	names := dbschema.Names(tables)
	if len(only) > 0 {
		wanted := map[string]bool{}
		for _, t := range only {
			if !strings.Contains(t, ".") {
				t = schema + "." + t
			}
			wanted[t] = true
		}
		var kept []string
		for _, n := range names {
			if wanted[n] {
				kept = append(kept, n)
			}
		}
		names = kept
	}
	models := map[string]string{} // qualified table -> model name
	for _, n := range names {
		models[n] = modelName(tables[n].Name)
	}

	var b strings.Builder
	b.WriteString(configHeader)
	fmt.Fprintf(&b, "schema: %s\n\n", schema)
	b.WriteString(policyBlock)
	b.WriteString("\n# One entry per model of the fixture file. A model in the file that is not\n")
	b.WriteString("# listed here stops the tool: a model nobody configured is a mistake, and a\n")
	b.WriteString("# silently skipped model is how a change goes missing.\n")
	b.WriteString("models:\n")

	for i, name := range names {
		t := tables[name]
		model := models[name]
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "  %s:\n", model)
		table := t.Name
		if t.Schema != schema {
			table = t.Qualified()
		}
		fmt.Fprintf(&b, "    table: %s\n", table)

		id := "id"
		if len(t.PrimaryKey) == 1 {
			id = t.PrimaryKey[0]
		} else if len(t.PrimaryKey) > 1 {
			b.WriteString("    # This table has a composite primary key (" +
				strings.Join(t.PrimaryKey, ", ") + "). Name one column as id, or\n" +
				"    # leave id out and put every key column in key.\n")
		}
		if id != "id" || len(t.PrimaryKey) == 1 {
			fmt.Fprintf(&b, "    id: %s\n", id)
		}
		if c, ok := t.Column(id); ok && c.Serial() {
			b.WriteString("    # The id comes from a sequence, so the migration moves the sequence\n" +
				"    # past any explicit id it writes. Without that the next ordinary insert\n" +
				"    # collides with an id the migration already used.\n")
			b.WriteString("    serial: true\n")
		}

		key := guessKey(t, id)
		if key == nil {
			b.WriteString("    # GUESS: this table has no unique index besides its primary key, so\n" +
				"    # there is nothing to tell two rows apart by name. Say which columns do,\n" +
				"    # and give the table a unique index on them; without one the database\n" +
				"    # cannot stop a duplicate appearing and no guard here is reliable.\n")
			key = []string{"name"}
		} else {
			b.WriteString("    # The natural key: what identifies a row when its id is meaningless,\n" +
				"    # taken from the narrowest unique index. Every guard the generated\n" +
				"    # migration writes matches on these columns.\n")
		}
		fmt.Fprintf(&b, "    key: [%s]\n", strings.Join(key, ", "))
		if ref := guessRef(t, key, id); ref != "" && ref != "name" {
			b.WriteString("    # The column another model's reference to this one names it by.\n")
			fmt.Fprintf(&b, "    ref: %s\n", ref)
		}

		var refs []string
		for _, c := range t.Columns {
			fk := t.ForeignKeyOf(c.Name)
			if fk == nil {
				continue
			}
			target, ok := models[fk.RefSchema+"."+fk.RefTable]
			if !ok {
				refs = append(refs, fmt.Sprintf("      # %s points at %s.%s, which is not in this configuration",
					c.Name, fk.RefSchema, fk.RefTable))
				continue
			}
			refs = append(refs, fmt.Sprintf("      %s: %s", c.Name, target))
		}
		if len(refs) > 0 {
			b.WriteString("    # Columns holding another row's id. The migration carries the target's\n" +
				"    # name and looks the id up where it runs, because ids drift between\n" +
				"    # databases and names do not.\n")
			b.WriteString("    references:\n")
			for _, line := range refs {
				b.WriteString(line + "\n")
			}
		}

		var defaults []string
		for _, c := range t.Columns {
			if c.Name == id || c.Serial() {
				continue
			}
			def, ok := c.LiteralDefault()
			if !ok {
				continue
			}
			defaults = append(defaults, fmt.Sprintf("      %s: %q", c.Name, def))
		}
		if len(defaults) > 0 {
			b.WriteString("    # What a column means when a fixture row leaves it out, taken from the\n" +
				"    # column defaults. Without an entry an omitted column is \"not set\", and a\n" +
				"    # column written on one side and omitted on the other is refused rather\n" +
				"    # than guessed at.\n")
			b.WriteString("    defaults:\n")
			for _, line := range defaults {
				b.WriteString(line + "\n")
			}
		}
		if hazards := hazardColumns(t); len(hazards) > 0 {
			b.WriteString("    # These columns have a non-zero default. bun writes DEFAULT, not the\n" +
				"    # value, for a zero in such a column, so a fixture row saying 0 here will\n" +
				"    # not produce 0 in the database. policy.zero_default decides what happens\n" +
				"    # when one does: " + strings.Join(hazards, ", ") + "\n")
		}
		if hazards := nullHazardColumns(t, id); len(hazards) > 0 {
			b.WriteString("    # These nullable columns have a default. bun writes DEFAULT, not NULL,\n" +
				"    # for a nil pointer or a nullzero field, so a fixture row saying ~ here\n" +
				"    # will not produce NULL in the database. policy.null_default decides what\n" +
				"    # happens when one does: " + strings.Join(hazards, ", ") + "\n")
		}
	}
	return []byte(b.String())
}

// guessKey is the narrowest unique index that is not the primary key and does
// not contain the id, which is very often exactly the natural key.
func guessKey(t *dbschema.Table, id string) []string {
	var best []string
	for _, cols := range t.Uniques {
		skip := false
		for _, c := range cols {
			if c == id {
				skip = true
			}
		}
		if skip || len(cols) == 0 {
			continue
		}
		if best == nil || len(cols) < len(best) {
			best = cols
		}
	}
	return best
}

// guessRef is the column a reference to this table would name it by: a
// single-column text key, else "name" when the table has one.
func guessRef(t *dbschema.Table, key []string, id string) string {
	if len(key) == 1 {
		if c, ok := t.Column(key[0]); ok {
			if _, text := c.ZeroText(); text && c.Type != "bool" {
				return key[0]
			}
		}
	}
	if _, ok := t.Column("name"); ok {
		return "name"
	}
	if len(key) == 1 {
		return key[0]
	}
	return ""
}

func hazardColumns(t *dbschema.Table) []string {
	var out []string
	for _, c := range t.Columns {
		if hazard, stored := c.ZeroIsNotDefault(); hazard {
			zero, _ := c.ZeroText()
			out = append(out, fmt.Sprintf("%s defaults to %s, its zero is %s", c.Name, stored, zeroLabel(zero)))
		}
	}
	sort.Strings(out)
	return out
}

// nullHazardColumns are the nullable columns with a default: a ~ in a fixture
// row loads as the default there, through a pointer or a nullzero field.
func nullHazardColumns(t *dbschema.Table, id string) []string {
	var out []string
	for _, c := range t.Columns {
		if c.Name == id || !c.Nullable {
			continue
		}
		if def, ok := c.NonNullDefault(); ok {
			out = append(out, fmt.Sprintf("%s defaults to %s", c.Name, def))
		}
	}
	sort.Strings(out)
	return out
}

func zeroLabel(zero string) string {
	if zero == "" {
		return `""`
	}
	return zero
}

// modelName turns a table name into the Go model name bun would give it.
func modelName(table string) string {
	return camel(singular(table))
}

// singular is the small half of pluralisation: enough to guess a model name
// from a table name, and wrong often enough that the comment above the output
// tells you to read it. A real inflection library would be another dependency
// for a guess you correct by hand anyway.
func singular(s string) string {
	switch {
	case strings.HasSuffix(s, "ies") && len(s) > 3:
		return s[:len(s)-3] + "y"
	case strings.HasSuffix(s, "ses"), strings.HasSuffix(s, "xes"), strings.HasSuffix(s, "zes"),
		strings.HasSuffix(s, "ches"), strings.HasSuffix(s, "shes"):
		return s[:len(s)-2]
	case strings.HasSuffix(s, "s") && !strings.HasSuffix(s, "ss"):
		return s[:len(s)-1]
	}
	return s
}

const configHeader = `# bun-fixture-migrate. Written by "bun-fixture-migrate scaffold" from a live
# database: everything below is either read from the catalog or guessed from it.
# Read it before you rely on it, and fix the guesses the comments point at.

# The dbfixture YAML file, relative to this file.
fixture: fixtures/fixture.yml
# Where generated migrations go, and the Go package and migrator variable they
# register with.
out: internal/migrations
package: migrations
migrator: Migrations
# A table that is never empty in a seeded database. While it is empty, a
# generated migration does nothing at all: that database has not been seeded
# yet, and dbfixture is about to load the new state by itself. Remove this only
# if your migration chain never runs before the seed.
seed_guard_table: ""
# Where export, check and scaffold connect. "env:NAME" reads the DSN from an
# environment variable, which is how the password stays out of the repository.
database: env:DATABASE_URL
`

const policyBlock = `# The choices that depend on how you run your databases rather than on what is
# correct. Everything not here is fixed, because the alternative would let this
# tool corrupt a database.
policy:
  # The id in the fixture file is not the id the database gave the row: the
  # file's id belongs to another row, or the row lives under a different id.
  #   error  refuse, and at run time roll the migration back (default)
  #   warn   report it and carry on
  #   ignore do not look
  # Leave this at error if anything outside the database names these ids: an
  # API contract, a saved game, a report. Renumbering a row under live data
  # that points at it corrupts that data silently, and nothing will say so.
  id_drift: error

  # A generated update or delete finds no row with that natural key at all.
  #   error  fail, so the transaction rolls back and bun does not record the
  #          migration as applied (default)
  #   warn   log it and carry on
  # DATA LOSS IF SET WRONG. bun's migrator records a migration the moment the
  # function returns nil. A change skipped with a warning is never attempted
  # again: put the row back, deploy once more, and the migration has already
  # been recorded. Set warn only where a missing row is expected.
  missing_row: error

  # The row is there, but no longer holds the values the migration was
  # generated against, because somebody changed it in this database.
  #   warn   keep their change, apply nothing, say so (default)
  #   error  fail and roll back
  # warn is right when an admin UI edits this data in production: their edit is
  # newer than your file. error is right when the fixture file is the only
  # writer and a difference means something is wrong.
  changed_row: warn

  # A fixture row writes a type's zero into a column whose database default is
  # not that zero.
  #   error  refuse (default)
  #   warn   report it
  #   ignore do not look
  # bun's INSERT sends DEFAULT rather than the value for a zero in a column
  # that has a default, so such a row does not load as written: the file says 0
  # and the database holds the default. This is the check worth having. Turning
  # it off does not make the problem go away, it makes it quiet.
  zero_default: error

  # A fixture row writes an explicit null (~) into a column that has a default.
  #   error  refuse (default)
  #   warn   report it
  #   ignore do not look
  # bun sends DEFAULT rather than NULL for a nil pointer, and for a zero in a
  # nullzero field, which is how a nullable column is usually modelled. So the
  # file says null and the database holds the default. Only a field such as
  # sql.NullString without either tag writes the NULL; set warn or ignore if
  # that is how your models spell nullable columns.
  null_default: error

  # Two rows of one model share a natural key in the database.
  #   error  refuse (default)
  #   warn   report it
  # Every guard and every reference this tool writes matches on that key, so
  # two rows holding it make the lookup pick one of them at random. There is no
  # setting that makes that safe; warn exists so you can see the whole list
  # before you fix it.
  duplicate_key: error

  # A row kept its id and changed its natural key.
  #   refuse  report it and write nothing for that row (default)
  #   update  write an UPDATE of the key columns, guarded by the id and the old
  #           key
  # update is safe inside the database: the id does not change, so nothing that
  # points at the row breaks. It is not safe outside it, where client code,
  # saved data or another service may know the old name.
  renames: refuse

  # A row left the fixture file.
  #   allow   write a guarded DELETE (default)
  #   refuse  report it
  # Override it per model with "deletes: refuse" wherever other tables point at
  # the row and only you can say whether they should cascade, be repointed or
  # block the delete.
  deletes: allow
`
