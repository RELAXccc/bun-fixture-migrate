package fixturemigrate

import (
	"sort"
	"strings"

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
	// KeyStr is the natural key as one comparable string.
	KeyStr string
	// Cells are the compared columns. A column that is absent here is "not
	// set", which is not the same as NULL.
	Cells fixturechange.Values
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
)

func (f Finding) String() string {
	where := f.Model
	if f.Row != "" {
		where += " " + f.Row
	}
	return where + ": " + f.Detail
}

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

// keyString renders a natural key so two of them compare as strings.
func keyString(model string, key fixturechange.Values) string {
	cols := make([]string, 0, len(key))
	for c := range key {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		parts = append(parts, c+"="+key[c].String())
	}
	return model + "/" + strings.Join(parts, "/")
}

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
			Kind: FindingDuplicateKey, Model: model, Row: k,
			Detail: "this natural key is held by " + plural(len(group), "row") + " (" + strings.Join(ids, ", ") +
				"), so no lookup by it can tell them apart: give the table a unique index, or add a column to key",
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
