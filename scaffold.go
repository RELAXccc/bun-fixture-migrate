package fixturemigrate

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"

	"github.com/uptrace/bun"
)

// ScaffoldOptions is what Scaffold knows of a database besides its tables;
// LoadScaffoldOptions reads it from the catalog.
type ScaffoldOptions struct {
	// Partitions are the tables, as "schema.table", that are partitions of
	// another. Their rows are the partitioned table's, which is the model.
	Partitions map[string]bool
	// Triggers are, per table, the BEFORE INSERT or UPDATE row triggers on
	// it, which can write columns of a row as a migration writes it.
	Triggers map[string][]string
	// MigrationsTable and MigrationLocksTable are the tables bun's migrator
	// keeps its records and its lock in, bun's defaults when empty. They are
	// no master data, and the configuration names them.
	MigrationsTable     string
	MigrationLocksTable string
}

// LoadScaffoldOptions reads the partitions and the row triggers of a schema.
func LoadScaffoldOptions(ctx context.Context, db bun.IDB, schema string) (ScaffoldOptions, error) {
	opts := ScaffoldOptions{Partitions: map[string]bool{}, Triggers: map[string][]string{}}
	rows, err := db.QueryContext(ctx, `
SELECT n.nspname || '.' || c.relname, '' FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = ? AND c.relispartition`, schema)
	if err != nil {
		return opts, fmt.Errorf("read the partitions of %s: %w", schema, err)
	}
	if err := scanPairs(rows, func(table, _ string) { opts.Partitions[table] = true }); err != nil {
		return opts, fmt.Errorf("read the partitions of %s: %w", schema, err)
	}
	// tgtype is a bit set: 1 a row trigger, 2 BEFORE, 4 INSERT, 16 UPDATE.
	rows, err = db.QueryContext(ctx, `
SELECT n.nspname || '.' || c.relname, t.tgname FROM pg_trigger t
JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = ? AND NOT t.tgisinternal AND t.tgtype::int & 3 = 3 AND t.tgtype::int & 20 <> 0
ORDER BY 1, 2`, schema)
	if err != nil {
		return opts, fmt.Errorf("read the triggers of %s: %w", schema, err)
	}
	if err := scanPairs(rows, func(table, trigger string) {
		opts.Triggers[table] = append(opts.Triggers[table], trigger)
	}); err != nil {
		return opts, fmt.Errorf("read the triggers of %s: %w", schema, err)
	}
	return opts, nil
}

// scanPairs hands every row of two text columns to fn. It reports the
// iteration's error too: a connection lost halfway is not "no more rows".
func scanPairs(rows *sql.Rows, fn func(a, b string)) error {
	defer rows.Close()
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return err
		}
		fn(a, b)
	}
	return rows.Err()
}

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
// of your intentions. Every table of the schema is proposed as a model but
// bun's own two, and only you know which of them the application writes: a
// table of users or orders kept in a fixture file is drift after every
// deploy. The natural key is guessed from the narrowest unique index, and a
// table with neither a unique index besides its primary key nor a name column
// has nothing to guess one from, so it is written commented out.
func Scaffold(tables map[string]*dbschema.Table, only []string, schema string, opts ScaffoldOptions) []byte {
	names := ScaffoldTables(tables, only, schema, opts)
	// The tables proposed as models; one without a key to guess is
	// written commented out, and a reference to it is no reference.
	models := map[string]string{} // qualified table -> model name
	for _, n := range names {
		models[n] = modelName(tables[n].Name)
	}
	// A primary key that points at another model's row is the key: a
	// plan's limits are told apart by their plan.
	commented := map[string]bool{}
	for _, n := range names {
		t := tables[n]
		if guessKey(t, scaffoldID(t)) != nil || primaryKeyReference(tables, t, models) != "" {
			continue
		}
		if _, ok := t.Column("name"); !ok {
			commented[n] = true
		}
	}
	for n := range commented {
		delete(models, n)
	}

	var b strings.Builder
	b.WriteString(configHeader(tables, names, models, schema, opts))
	b.WriteString(policyBlock)
	b.WriteString("\n# One entry per model of the fixture file. A model in the file that is not\n")
	b.WriteString("# listed here stops the tool: a model nobody configured is a mistake, and a\n")
	b.WriteString("# silently skipped model is how a change goes missing.\n")
	b.WriteString("models:\n")

	for i, name := range names {
		if i > 0 {
			b.WriteString("\n")
		}
		block := scaffoldModel(tables, tables[name], modelName(tables[name].Name), schema, models, opts.Triggers[name])
		if !commented[name] {
			b.WriteString(block)
			continue
		}
		fmt.Fprintf(&b, "  # %s has no unique index besides its primary key and no name column, so\n"+
			"  # nothing tells two of its rows apart without their ids, which differ between\n"+
			"  # databases. Give it a unique index on the columns that do, put them in key,\n"+
			"  # and take the comment marks away; until then it is no model.\n", tables[name].Qualified())
		for _, line := range strings.SplitAfter(strings.TrimSuffix(block, "\n"), "\n") {
			b.WriteString("  # " + strings.TrimPrefix(line, "  "))
		}
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// ScaffoldTables is the tables Scaffold proposes as models, as "schema.table":
// those asked for, or every table, but for partitions, whose rows are their
// table's, and bun's migrations and locks tables.
func ScaffoldTables(tables map[string]*dbschema.Table, only []string, schema string, opts ScaffoldOptions) []string {
	migrations, locks := opts.MigrationsTable, opts.MigrationLocksTable
	if migrations == "" {
		migrations = "bun_migrations"
	}
	if locks == "" {
		locks = "bun_migration_locks"
	}
	bunTables := map[string]bool{}
	for _, t := range []string{migrations, locks} {
		if !strings.Contains(t, ".") {
			t = schema + "." + t
		}
		bunTables[t] = true
	}
	var names []string
	for _, n := range dbschema.Names(tables) {
		if !opts.Partitions[n] && !bunTables[n] {
			names = append(names, n)
		}
	}
	if len(only) == 0 {
		return names
	}
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
	return kept
}

// scaffoldID is the column Scaffold takes for a table's id: its primary key
// when that is one column, "id" otherwise.
func scaffoldID(t *dbschema.Table) string {
	if len(t.PrimaryKey) == 1 {
		return t.PrimaryKey[0]
	}
	return "id"
}

// primaryKeyReference is the model a table's one-column primary key points
// at, "" when it points nowhere this configuration has or at a column other
// than that model's id.
func primaryKeyReference(tables map[string]*dbschema.Table, t *dbschema.Table, models map[string]string) string {
	if len(t.PrimaryKey) != 1 {
		return ""
	}
	fk := t.ForeignKeyOf(t.PrimaryKey[0])
	if fk == nil {
		return ""
	}
	target, ok := models[fk.RefSchema+"."+fk.RefTable]
	if !ok {
		return ""
	}
	if pk := tables[fk.RefSchema+"."+fk.RefTable].PrimaryKey; len(pk) != 1 || pk[0] != fk.RefColumns[0] {
		return ""
	}
	return target
}

// scaffoldModel is the entry of one model.
func scaffoldModel(tables map[string]*dbschema.Table, t *dbschema.Table, model, schema string,
	models map[string]string, triggers []string) string {

	var b strings.Builder
	fmt.Fprintf(&b, "  %s:\n", model)
	b.WriteString("    # GUESS: proposed because the schema has it. Delete this model unless the\n" +
		"    # fixture files own the table's rows: a table the application writes, such\n" +
		"    # as users, orders or sessions, exported into a fixture file is drift after\n" +
		"    # every deploy, and a migration generated from it overwrites real data.\n")
	table := t.Name
	if t.Schema != schema {
		table = t.Qualified()
	}
	fmt.Fprintf(&b, "    table: %s\n", table)

	id := scaffoldID(t)
	// A primary key that points at another model's row, a plan's limits
	// keyed by the plan, is a reference: the tool reads an id as the row's
	// own value, so the model has no id and the key is that column.
	pkRef := primaryKeyReference(tables, t, models)
	switch {
	case len(t.PrimaryKey) > 1:
		b.WriteString("    # This table has a composite primary key (" +
			strings.Join(t.PrimaryKey, ", ") + "). Name one column as id, or\n" +
			"    # leave id out and put every key column in key.\n")
	case pkRef != "":
		fmt.Fprintf(&b, "    # The primary key, %s, points at a row of %s, so it is a reference and\n"+
			"    # the natural key, not an id: id is left out.\n", id, pkRef)
		id = ""
	case len(t.PrimaryKey) == 1:
		fmt.Fprintf(&b, "    id: %s\n", id)
	}
	if c, ok := t.Column(id); ok && id != "" && c.Serial() {
		b.WriteString("    # The id comes from a sequence, so the migration moves the sequence\n" +
			"    # past any explicit id it writes. Without that the next ordinary insert\n" +
			"    # collides with an id the migration already used.\n")
		b.WriteString("    serial: true\n")
	}

	key := guessKey(t, id)
	if pkRef != "" {
		key = []string{t.PrimaryKey[0]}
	}
	if key == nil {
		b.WriteString("    # GUESS: this table has no unique index besides its primary key, so\n" +
			"    # there is nothing to tell two rows apart by name. Say which columns do,\n" +
			"    # and give the table a unique index on them; without one the database\n" +
			"    # cannot stop a duplicate appearing and no guard here is reliable.\n")
		key = []string{"name"}
	} else if pkRef != "" {
		b.WriteString("    # The natural key: the row of " + pkRef + " the primary key points at.\n")
	} else {
		b.WriteString("    # The natural key: what identifies a row when its id is meaningless,\n" +
			"    # taken from the narrowest unique index. Every guard the generated\n" +
			"    # migration writes matches on these columns.\n")
	}
	fmt.Fprintf(&b, "    key: [%s]\n", strings.Join(key, ", "))
	// A model keyed by a reference has no column of its own a reference to it
	// could name it by.
	if ref := guessRef(t, key, id); ref != "" && ref != "name" && pkRef == "" {
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
		// A reference holds the target's id. A column holding another
		// of its columns, a code say, holds a value that is the same in
		// every database, and is compared as it is.
		if pk := tables[fk.RefSchema+"."+fk.RefTable].PrimaryKey; len(pk) != 1 || pk[0] != fk.RefColumns[0] {
			refs = append(refs, fmt.Sprintf("      # %s points at %s.%s, which is not its id: an ordinary column",
				c.Name, fk.RefTable, fk.RefColumns[0]))
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
	if ignored := writtenByTheDatabase(t, triggers); len(ignored) > 0 {
		b.WriteString("    # GUESS: the database writes these when a row is written, from a default\n" +
			"    # such as now() or from a trigger, so they are not master data: compared,\n" +
			"    # a migrated row would be drift the moment it was written. Take a column\n" +
			"    # out of the list if the fixture files are to set it.\n")
		fmt.Fprintf(&b, "    ignore: [%s]\n", strings.Join(ignored, ", "))
	}
	if len(triggers) > 0 {
		b.WriteString("    # BEFORE row triggers (" + strings.Join(triggers, ", ") + ") can change a row\n" +
			"    # as a migration writes it. Put every column they write in ignore, or check\n" +
			"    # reports it as drift after each migration that touches the row.\n")
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
	return b.String()
}

// guessGuard is the seed guard table Scaffold proposes: of the models, the
// first in dependency order that another model points at, which a seeded
// database has rows in for the others to point at; else the first model.
// "" when there is none.
func guessGuard(tables map[string]*dbschema.Table, names []string, models map[string]string) string {
	referenced := map[string]bool{}
	targets := map[string][]string{}
	for _, n := range names {
		if _, ok := models[n]; !ok {
			continue
		}
		for _, fk := range tables[n].ForeignKeys {
			target := fk.RefSchema + "." + fk.RefTable
			if _, ok := models[target]; !ok || target == n {
				continue
			}
			referenced[target] = true
			targets[n] = append(targets[n], target)
		}
	}
	var order []string
	seen := map[string]bool{}
	var visit func(string)
	visit = func(n string) {
		if seen[n] {
			return
		}
		seen[n] = true
		deps := append([]string{}, targets[n]...)
		sort.Strings(deps)
		for _, d := range deps {
			visit(d)
		}
		order = append(order, n)
	}
	for _, n := range names {
		if _, ok := models[n]; ok {
			visit(n)
		}
	}
	for _, n := range order {
		if referenced[n] {
			return n
		}
	}
	if len(order) > 0 {
		return order[0]
	}
	return ""
}

// writtenByTheDatabase are the columns of a table the database fills in when a
// row is written: a timestamp defaulting to the time of the write, and, on a
// table with a BEFORE row trigger, a timestamp named for an update, which is
// what such a trigger most often keeps.
func writtenByTheDatabase(t *dbschema.Table, triggers []string) []string {
	var out []string
	for _, c := range t.Columns {
		if c.Type != "timestamptz" && c.Type != "timestamp" {
			continue
		}
		def := strings.ToLower(c.Default)
		now := strings.Contains(def, "now()") || strings.Contains(def, "current_timestamp") ||
			strings.Contains(def, "clock_timestamp()") || strings.Contains(def, "statement_timestamp()") ||
			strings.Contains(def, "transaction_timestamp()") || strings.Contains(def, "localtimestamp")
		name := strings.ToLower(c.Name)
		touched := len(triggers) > 0 && (strings.Contains(name, "updated") || strings.Contains(name, "modified") ||
			strings.Contains(name, "changed"))
		if now || touched {
			out = append(out, c.Name)
		}
	}
	return out
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
// for a guess you correct by hand anyway. Only the last word of a snake_case
// name is plural.
func singular(s string) string {
	if i := strings.LastIndexByte(s, '_'); i >= 0 {
		return s[:i+1] + singular(s[i+1:])
	}
	if word, ok := irregular[s]; ok {
		return word
	}
	switch {
	case strings.HasSuffix(s, "ies") && len(s) > 3:
		return s[:len(s)-3] + "y"
	case strings.HasSuffix(s, "sses"), strings.HasSuffix(s, "xes"), strings.HasSuffix(s, "zzes"),
		strings.HasSuffix(s, "ches"), strings.HasSuffix(s, "shes"):
		return s[:len(s)-2]
	case strings.HasSuffix(s, "ouses"):
		return s[:len(s)-1]
	case strings.HasSuffix(s, "uses"):
		return s[:len(s)-2]
	case strings.HasSuffix(s, "ss"), strings.HasSuffix(s, "us"), strings.HasSuffix(s, "is"):
		return s
	case strings.HasSuffix(s, "s"):
		return s[:len(s)-1]
	}
	return s
}

// irregular are the plurals the suffix rules get wrong that a table is often
// named by.
var irregular = map[string]string{
	"people": "person", "children": "child", "men": "man", "women": "woman", "criteria": "criterion",
	"indices": "index", "matrices": "matrix", "analyses": "analysis", "series": "series", "news": "news",
}

// configHeader is the top of the configuration: where the files are, bun's
// tables, the seed guard, the database.
func configHeader(tables map[string]*dbschema.Table, names []string, models map[string]string, schema string,
	opts ScaffoldOptions) string {

	migrations, locks := opts.MigrationsTable, opts.MigrationLocksTable
	if migrations == "" {
		migrations = "bun_migrations"
	}
	if locks == "" {
		locks = "bun_migration_locks"
	}
	guard := ""
	if g := guessGuard(tables, names, models); g != "" {
		guard = tables[g].Name
		if tables[g].Schema != schema {
			guard = tables[g].Qualified()
		}
	}
	var b strings.Builder
	b.WriteString(`# bun-fixture-migrate. Written by "bun-fixture-migrate scaffold" from a live
# database: everything below is either read from the catalog or guessed from it.
# Read it before you rely on it, and fix the guesses the comments point at.

# The dbfixture YAML file, relative to this file.
fixture: fixtures/fixture.yml
# Where generated migrations go, and the Go package and migrator variable they
# register with.
out: internal/migrations
package: migrations
migrator: Migrations
# The tables your migrator records applied migrations and keeps its lock in;
# set them if you build the migrator with migrate.WithTableName or
# migrate.WithLocksTableName. They are bun's, not master data, so no model
# below is one of them.
`)
	fmt.Fprintf(&b, "migrations_table: %s\nmigration_locks_table: %s\n", migrations, locks)
	b.WriteString(`# A table that is never empty in a seeded database. While it is empty, a
# generated migration does nothing at all: that database has not been seeded
# yet, and dbfixture is about to load the new state by itself. Without one, a
# new database runs every fixture migration before the seed, against tables
# with nothing in them, and the first that changes a row fails the deploy.
`)
	if guard == "" {
		b.WriteString("# GUESS: no model to take it from; name the table the seed fills first.\n")
	} else {
		fmt.Fprintf(&b, "# GUESS: %s, the first table the other models point at. It has to be the\n"+
			"# table of a model below, one the fixture files fill and the application\n"+
			"# never empties; if you delete its model, name another.\n", guard)
	}
	fmt.Fprintf(&b, "seed_guard_table: %q\n", guard)
	b.WriteString(`# How long a generated migration waits for a lock another session holds on a
# row it writes, an admin's open transaction say, before it fails, rolls back
# and leaves the migration to the next deploy. Without it the deploy waits as
# long as that transaction stays open, and the application's own writes to
# those rows queue up behind it.
lock_timeout: 10s
# Where the commands connect, unless -dsn names another database. "env:NAME"
# reads the DSN from an environment variable, which is how the password stays
# out of the repository.
database: env:DATABASE_URL
`)
	fmt.Fprintf(&b, "schema: %s\n\n", schema)
	return b.String()
}

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
  # generated against: somebody edited it in this database, or another
  # migration changed it first.
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
  #   error  refuse, and at run time fail the migration (default)
  #   warn   report it; at run time leave those rows alone and carry on
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
  #   allow    write a guarded DELETE (default). If rows of other tables still
  #            point at it, the migration fails instead of letting a foreign
  #            key's ON DELETE CASCADE or SET NULL reach them.
  #   refuse   report it and write nothing
  #   cascade  write the DELETE and let those foreign keys do what they say
  # Override it per model wherever only you can say whether the rows pointing
  # at one should go with it, be repointed or block it.
  deletes: allow

  # A null inside a sequence, [a, ~, b], in an array column.
  #   refuse  report it and refuse a change that carries it (default)
  #   keep    the array holds a NULL element there
  # yaml.v3 leaves the null out of a []string or []int64 field and keeps it in a
  # []*string or []sql.NullString one, and the fixture file does not say which
  # the model has. Set keep if your array fields keep a null; with plain slices,
  # leave nulls out of the file. Override it per model where they differ.
  array_nulls: refuse
`
