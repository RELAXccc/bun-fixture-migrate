package fixturemigrate

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// Entry is one row of a snapshot: its identity and its columns, with every
// value already resolved to a literal, a NULL or a reference to another model's
// row by name.
type Entry struct {
	// Anchor is dbfixture's "_id" of the row. The fixture side reads it from
	// the file; the database side makes one up from the natural key, so an
	// export can write references as templates.
	Anchor string
	// ID is the primary key as text, "" when the row has none. It is not an
	// ordinary column: it is never compared as one and never updated.
	ID string
	// Key is the natural key.
	Key fixturechange.Values
	// KeyStr is the natural key as one comparable string, equal for two
	// entries exactly when their keys are; see keyString. Messages name a
	// row by keyLabel instead.
	KeyStr string
	// Cells are the compared columns. A column that is absent here is "not
	// set", which is not the same as NULL.
	Cells fixturechange.Values
	// AsWritten holds, for a fixture row, the columns (the id included) whose
	// value a string field gets differently from what it resolves to, with
	// the text the string field gets; see Cell.StringText. Only the column's
	// type says which of the two the database holds, so Canonicalize picks
	// one and empties it, and a change carrying a value still in it is
	// refused rather than guessed at.
	//
	// A reference is in it too when the ref value of the row it names is
	// such a value: 0012 is the integer 10 in a bigint ref column and the
	// text 0012 in a text one, and the reference has to carry whichever the
	// database holds there.
	AsWritten map[string]string
	// from names, for a column in AsWritten whose value another row
	// supplies, the column of that row whose type decides between the two
	// readings: the ref column of the row a reference names, or the field a
	// template copies. Any other column decides for itself.
	from map[string]source
	// copied holds, for a fixture row, the columns a template copies from a
	// field of another row other than its id, with that field. dbfixture
	// stores what the field holds as fmt prints it, which only the field's
	// Go type decides: a string or an integer as it is, a float64 of
	// 100000000 as 1e+08, a time.Time with its zone's name. Canonicalize
	// settles a copy of a string or an integer column and reports any
	// other; a change carrying one it has not settled is refused.
	copied map[string]source
	// asJSON holds, for a fixture row, the columns whose value a json or
	// jsonb column holds as something else than Cells says, with that JSON;
	// see Cell.JSONText. Canonicalize takes it for such a column, and for a
	// timestamptz one, which holds the same instant or, for a date alone,
	// the midnight UTC a time.Time field makes of it. Without the database
	// it is not used.
	asJSON map[string]string
	// unsure holds, for a fixture row, the columns whose value means one
	// thing to one Go field type and another to another (Cell.Unsure), with
	// the reason. No column type settles them, so a change carrying one is
	// always refused.
	unsure map[string]string
}

// Full is every column an insert writes or a delete guards on: the compared
// columns plus the id when the row has one.
func (e *Entry) Full(m *Model) fixturechange.Values {
	out := make(fixturechange.Values, len(e.Cells)+1)
	for col, v := range e.Cells {
		out[col] = v
	}
	if e.ID != "" {
		out[m.ID] = fixturechange.Lit(e.ID)
	}
	return out
}

// Snapshot is one state of the master data, from a fixture file or from a
// database. Both sides of every comparison this tool makes are snapshots, which
// is why the same diff serves "the file against an older revision of itself"
// and "the file against the database".
type Snapshot struct {
	// Source names where it came from, for messages: a path, a git revision,
	// or "the database".
	Source string
	// Order is the model order. For a fixture file it is the file's own order,
	// which dbfixture already had to get right; for a database it is the
	// dependency order worked out from the configured references.
	Order []string
	// Entries holds the rows per model, in the order they were read.
	Entries map[string][]*Entry
	// Columns lists, per model, every column that appears in any of its rows.
	Columns map[string][]string
	// Findings are the problems noticing this snapshot turned up: a natural
	// key that is not unique, a zero written into a column whose default is
	// not zero. They are reported, never worked around.
	Findings []Finding

	// unique holds, per model, the columns of each unique index of its
	// table, once the catalog has been read for the snapshot (by
	// DatabaseSnapshot or Canonicalize); nil while nobody has looked.
	unique map[string][][]string
}

// noteUniques records the unique indexes of a model's table.
func (s *Snapshot) noteUniques(model string, table *dbschema.Table) {
	if s.unique == nil {
		s.unique = map[string][][]string{}
	}
	indexes := make([][]string, 0, len(table.Uniques))
	for _, index := range table.Uniques {
		indexes = append(indexes, append([]string(nil), index...))
	}
	s.unique[model] = indexes
}

// clone copies a snapshot deeply enough that rewriting an entry in it cannot
// be seen through the original. The findings and the column lists are shared:
// nothing rewrites those.
func (s *Snapshot) clone() *Snapshot {
	out := *s
	out.Entries = make(map[string][]*Entry, len(s.Entries))
	for model, entries := range s.Entries {
		copied := make([]*Entry, 0, len(entries))
		for _, e := range entries {
			c := *e
			c.Key = copyValues(e.Key)
			c.Cells = copyValues(e.Cells)
			if e.AsWritten != nil {
				c.AsWritten = make(map[string]string, len(e.AsWritten))
				for col, text := range e.AsWritten {
					c.AsWritten[col] = text
				}
			}
			if e.from != nil {
				c.from = make(map[string]source, len(e.from))
				for col, src := range e.from {
					c.from[col] = src
				}
			}
			if e.copied != nil {
				c.copied = make(map[string]source, len(e.copied))
				for col, src := range e.copied {
					c.copied[col] = src
				}
			}
			if e.asJSON != nil {
				c.asJSON = make(map[string]string, len(e.asJSON))
				for col, text := range e.asJSON {
					c.asJSON[col] = text
				}
			}
			if e.unsure != nil {
				c.unsure = make(map[string]string, len(e.unsure))
				for col, reason := range e.unsure {
					c.unsure[col] = reason
				}
			}
			copied = append(copied, &c)
		}
		out.Entries[model] = copied
	}
	return &out
}

func copyValues(v fixturechange.Values) fixturechange.Values {
	out := make(fixturechange.Values, len(v))
	for col, value := range v {
		out[col] = value
	}
	return out
}

// Finding is something wrong with a snapshot that does not stop it being read.
type Finding struct {
	// Kind groups findings for the exit code and the report.
	Kind FindingKind
	// Model and Row say where it is; Row is the natural key or the id.
	Model string
	Row   string
	// Detail is one sentence an operator can act on.
	Detail string
}

// FindingKind is what a finding is about.
type FindingKind string

const (
	// FindingDuplicateKey is two rows sharing one natural key, which makes
	// every lookup by that key ambiguous.
	FindingDuplicateKey FindingKind = "duplicate key"
	// FindingZeroDefault is a zero written into a column whose database
	// default is something else, which bun does not write.
	FindingZeroDefault FindingKind = "zero against a default"
	// FindingNullDefault is an explicit null written into a column that has a
	// default, which bun turns into DEFAULT for a pointer or nullzero field.
	FindingNullDefault FindingKind = "null against a default"
	// FindingInvalidValue is a value the column's type cannot hold, which
	// the migration would fail on at deploy time.
	FindingInvalidValue FindingKind = "invalid value"
	// FindingUnknownColumn is a column in the fixture file that the table does
	// not have.
	FindingUnknownColumn FindingKind = "unknown column"
	// FindingAmbiguousValue is a value whose meaning depends on the Go type
	// of the model's field, which this tool cannot see and no column type
	// settles: a sequence holding a null.
	FindingAmbiguousValue FindingKind = "ambiguous value"
	// FindingDuplicateID is two rows of a fixture file sharing one id, which
	// two branches each adding the next id leave behind after a merge:
	// dbfixture cannot load such a file, and no migration can insert both.
	FindingDuplicateID FindingKind = "duplicate id"
)

func (f Finding) String() string { return f.Where() + ": " + f.Detail }

// Where names the row a finding is about, as Refusal.Where does.
func (f Finding) Where() string { return rowWhere(f.Model, f.Row) }

// byKey indexes the entries of a model by their natural key, in reading order.
func byKey(entries []*Entry) (map[string][]*Entry, []string) {
	groups := map[string][]*Entry{}
	var order []string
	for _, e := range entries {
		if _, seen := groups[e.KeyStr]; !seen {
			order = append(order, e.KeyStr)
		}
		groups[e.KeyStr] = append(groups[e.KeyStr], e)
	}
	return groups, order
}

// keyString renders a natural key so two of them compare as strings, equal
// exactly when the keys are. Every name and value is quoted and every value
// carries its kind: spelled the way a person reads it, a NULL and the text
// "NULL", a reference and the text "Currency(EUR)", or {a: "x/b=y", b: "z"}
// and {a: "x", b: "y/b=z"} would be one key, and two different rows a
// duplicate. It is never shown to anybody; keyLabel is.
func keyString(model string, key fixturechange.Values) string {
	var b strings.Builder
	b.WriteString(strconv.Quote(model))
	for _, c := range sortedColumns(key) {
		b.WriteString(" " + strconv.Quote(c) + "=" + valueKey(key[c]))
	}
	return b.String()
}

// setKey gives an entry its natural key. A key column that still holds two
// readings (AsWritten) is compared under both: until the column's type says
// which one the database holds, 0012 and "10" may or may not be one key, and
// two keys are only the same when they are whichever reading it takes.
func (e *Entry) setKey(model string, key fixturechange.Values) {
	e.Key = key
	e.KeyStr = keyString(model, key)
	for _, col := range sortedColumns(key) {
		if written, ok := e.AsWritten[col]; ok {
			e.KeyStr += " " + strconv.Quote(col) + " written " + strconv.Quote(written)
		}
	}
}

// valueKey writes a value so that no two different values write the same
// text.
func valueKey(v fixturechange.Value) string {
	switch {
	case v.IsNull:
		return "null"
	case v.Ref != nil:
		return "ref(" + strconv.Quote(v.Ref.Model) + "," + strconv.Quote(v.Ref.Key) + ")"
	}
	return strconv.Quote(v.Lit)
}

// keyLabel is a natural key as refusals and findings name a row,
// "Plan/name=team", in the text reports and in the JSON ones. Two keys can
// share a label; nothing compares them by it.
func keyLabel(model string, key fixturechange.Values) string {
	cols := sortedColumns(key)
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		parts = append(parts, c+"="+key[c].String())
	}
	return model + "/" + strings.Join(parts, "/")
}

// label is the entry's natural key as keyLabel writes it.
func (e *Entry) label(model string) string { return keyLabel(model, e.Key) }

// reportDuplicates adds a finding for every natural key more than one row
// holds, naming each colliding id. A key that does not identify one row cannot
// be turned into a WHERE clause, and guessing which row was meant is how a
// migration edits the wrong one.
func (s *Snapshot) reportDuplicates(model string) {
	groups, order := byKey(s.Entries[model])
	for _, k := range order {
		group := groups[k]
		if len(group) < 2 {
			continue
		}
		ids := make([]string, 0, len(group))
		for _, e := range group {
			id := e.ID
			if id == "" {
				id = "(no id)"
			}
			ids = append(ids, id)
		}
		s.Findings = append(s.Findings, Finding{
			Kind: FindingDuplicateKey, Model: model, Row: group[0].label(model),
			Detail: "this natural key is held by " + plural(len(group), "row") + " (" + strings.Join(ids, ", ") +
				"), so no lookup by it can tell them apart: give the table a unique index, or add a column to key",
		})
	}
}

// reportDuplicateIDs adds a finding for every id more than one row of a model
// holds, naming the rows.
func (s *Snapshot) reportDuplicateIDs(cfg *Config, model string) {
	rows := map[string][]string{}
	var order []string
	for _, e := range s.Entries[model] {
		if e.ID == "" {
			continue
		}
		if _, seen := rows[e.ID]; !seen {
			order = append(order, e.ID)
		}
		rows[e.ID] = append(rows[e.ID], e.label(model))
	}
	id := cfg.Models[model].ID
	for _, value := range order {
		if len(rows[value]) < 2 {
			continue
		}
		s.Findings = append(s.Findings, Finding{
			Kind: FindingDuplicateID, Model: model, Row: id + "=" + value,
			Detail: fmt.Sprintf("%s hold this %s (%s), and dbfixture cannot load the file: the second insert "+
				"fails on the primary key. Give each row its own %s",
				plural(len(rows[value]), "row"), id, strings.Join(rows[value], ", "), id),
		})
	}
}

// sortedColumns is the column list of a value map, sorted.
func sortedColumns(v fixturechange.Values) []string {
	out := make([]string, 0, len(v))
	for k := range v {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// unionColumns is every column that appears in any of the entries.
func unionColumns(entries []*Entry) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		for col := range e.Cells {
			if seen[col] {
				continue
			}
			seen[col] = true
			out = append(out, col)
		}
	}
	sort.Strings(out)
	return out
}
