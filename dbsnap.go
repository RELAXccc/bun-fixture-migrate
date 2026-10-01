package fixturemigrate

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
)

// SnapshotOptions steers what DatabaseSnapshot reads.
type SnapshotOptions struct {
	// Columns limits, per model, the columns that are read. An export leaves
	// it nil and takes every column of the table; a comparison against a
	// fixture file passes that file's columns, because a column no fixture row
	// mentions is not master data and a difference in it is not drift.
	Columns map[string][]string
	// Order overrides the model order. Nil means the dependency order worked
	// out from the configured references. A comparison passes the fixture
	// files' order, and every configured model the files hold no block of
	// is read after those, every column of it, as a model whose block holds
	// no row is: a fresh seed of the files holds no row of it, which is what
	// a comparison has to hold the database against, and what generate
	// makes of a block that left the files.
	Order []string
}

// rawRow is one database row before its references are resolved.
type rawRow struct {
	id     string
	values map[string]fixturechange.Value
}

// refValue is what a reference to this row carries: its ref column, or its id
// when the ref column is the id. A row without an id cannot be pointed at,
// and nor can one whose ref column is NULL or itself a reference.
func (r *rawRow) refValue(m *Model) (string, bool) {
	if r.id == "" {
		return "", false
	}
	if m.Ref == m.ID {
		return r.id, true
	}
	if _, isRef := m.References[m.Ref]; isRef {
		return "", false
	}
	v, ok := r.values[m.Ref]
	if !ok || v.IsNull {
		return "", false
	}
	return v.Lit, true
}

// DatabaseSnapshot reads the master data out of a database.
//
// It needs no Go model types. The configuration says which table a model lives
// in, which columns are its natural key and which columns point at another
// model; the catalog says what the columns are and how to read them. That is
// the whole of it, and it is why an export can be part of this tool at all.
func DatabaseSnapshot(ctx context.Context, db bun.IDB, cfg *Config, tables map[string]*dbschema.Table,
	opts SnapshotOptions) (*Snapshot, error) {

	order := opts.Order
	if order == nil {
		var err error
		if order, err = cfg.DependencyOrder(); err != nil {
			return nil, err
		}
	} else {
		given := set(order)
		order = append([]string(nil), order...)
		for _, model := range cfg.ModelNames() {
			if !given[model] {
				order = append(order, model)
			}
		}
	}
	snap := &Snapshot{Source: "the database", Order: order,
		Entries: map[string][]*Entry{}, Columns: map[string][]string{}}

	// First pass: read the rows as text. References still hold ids here,
	// because the row they point at may not have been read yet.
	raw := map[string][]*rawRow{}
	for _, model := range order {
		m, err := cfg.model(model)
		if err != nil {
			return nil, err
		}
		table := tables[cfg.QualifiedTable(m)]
		if table == nil {
			return nil, notATable(ctx, db, model, cfg.QualifiedTable(m))
		}
		cols, err := readColumns(m, table, opts.Columns[model])
		if err != nil {
			return nil, fmt.Errorf("model %q: %w", model, err)
		}
		rows, err := readRows(ctx, db, cfg, m, table, cols)
		if err != nil {
			return nil, fmt.Errorf("model %q: %w", model, err)
		}
		raw[model] = rows
		snap.Columns[model] = cols
		snap.noteUniques(model, table)
	}

	// Second pass: what every row is called by the rows that point at it, for
	// every model before any reference is resolved. A row can point at a row
	// of its own model that comes later in id order -- a tree whose root was
	// added after its leaves -- and, in the order a caller passes, at a model
	// that comes later.
	refValues := map[string]map[string]string{} // model -> id -> ref value
	for _, model := range order {
		m := cfg.Models[model]
		refValues[model] = map[string]string{}
		for _, r := range raw[model] {
			if v, ok := r.refValue(m); ok {
				refValues[model][r.id] = v
			}
		}
	}

	// Third pass: resolve the reference columns, build the natural key out of
	// the resolved values so it matches the fixture side, and give every row
	// an anchor. Columns are taken in name order, so the error a row with two
	// dangling references gets is the same on every run.
	for _, model := range order {
		m := cfg.Models[model]
		taken := map[string]bool{}
		for _, r := range raw[model] {
			e := &Entry{ID: r.id, Cells: fixturechange.Values{}}
			for _, col := range sortedColumns(r.values) {
				v := r.values[col]
				target, isRef := m.References[col]
				if !isRef || v.IsNull {
					e.Cells[col] = v
					continue
				}
				ref, ok := refValues[target][v.Lit]
				switch {
				case ok:
					e.Cells[col] = fixturechange.RefTo(target, ref)
				case isZero(v):
					// 0 or "" points at no row, unless a row has that id.
					e.Cells[col] = v
				default:
					return nil, fmt.Errorf(
						"%s: %s = %s points at a row of %s that this snapshot does not hold; "+
							"either that row is outside the model's where clause or the foreign key is dangling",
						model, col, v.Lit, target)
				}
			}
			key, err := keyOf(cfg, m, model, e.Full(m))
			if err != nil {
				return nil, err
			}
			e.setKey(model, key)
			e.Anchor = uniqueAnchor(anchorOf(key), r.id, taken)
			// The id this database gave a row of an ids: database model is
			// its own: the rows pointing at it are resolved above, and a
			// comparison, a guard or an export has no use for it.
			if m.idsFromDatabase() {
				e.ID = ""
			}
			snap.Entries[model] = append(snap.Entries[model], e)
		}
		snap.reportDuplicates(model)
	}
	reportDuplicateRefs(cfg, snap)
	if err := noteFolds(ctx, db, cfg, snap, tables); err != nil {
		return nil, err
	}
	if err := reportEqualKeys(ctx, db, cfg, snap, tables); err != nil {
		return nil, err
	}
	return snap, nil
}

// notATable is the error for a model whose table Load did not find: a view,
// a materialized view or a foreign table says what it is, because only a
// table holds master data.
func notATable(ctx context.Context, db bun.IDB, model, qualified string) error {
	kind, err := dbschema.NotATable(ctx, db, qualified)
	if err != nil || kind == "" {
		return fmt.Errorf("model %q: the configuration says %s, which is not a table in this database", model, qualified)
	}
	return fmt.Errorf("model %q: the configuration says %s, which is %s: only a table holds master data, so name "+
		"the table whose rows it shows", model, qualified, kind)
}

// readColumns is the column list to read for a model: the projection the caller
// asked for, or every column of the table that takes part.
func readColumns(m *Model, table *dbschema.Table, want []string) ([]string, error) {
	if want == nil {
		var out []string
		for _, c := range table.Columns {
			// A generated column is the database's to fill, like a derived
			// one is the application's: nothing can write it back.
			if m.skip(c.Name) || c.Generated {
				continue
			}
			out = append(out, c.Name)
		}
		sort.Strings(out)
		return out, nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(col string) error {
		if seen[col] || m.skip(col) {
			return nil
		}
		if _, ok := table.Column(col); !ok {
			return fmt.Errorf("column %q is not in %s", col, table.Qualified())
		}
		seen[col] = true
		out = append(out, col)
		return nil
	}
	for _, col := range want {
		if err := add(col); err != nil {
			return nil, err
		}
	}
	// A key column is always read, even when no fixture row spells it out,
	// because without it the row cannot be matched at all.
	for _, col := range m.keyColumns() {
		if _, ok := table.Column(col); !ok {
			return nil, fmt.Errorf("key column %q is not in %s", col, table.Qualified())
		}
		if seen[col] || m.skip(col) {
			continue
		}
		seen[col] = true
		out = append(out, col)
	}
	sort.Strings(out)
	return out, nil
}

// selectQuery is the one SELECT a model needs, and the second result says
// whether its first column is the primary key.
//
// Every column is cast to text in SQL, so a boolean arrives as "true" and a
// numeric in its own notation rather than as whatever the driver decided. The
// identifiers come from the configuration and the catalog and are quoted; the
// where clause is the operator's own SQL and goes in as written. The order is
// fixed so two runs against the same database read the rows in the same order,
// which is what makes an exported file stable enough to diff.
//
// It is built apart from being run so a test can read it.
func selectQuery(cfg *Config, m *Model, table *dbschema.Table, cols []string) (string, bool, error) {
	// A model without an id of its own (id: none) reads none, whatever
	// column the table calls id.
	idColumn, hasID := table.Column(m.ID)
	hasID = hasID && m.ID != ""
	var idQuoted string
	if hasID {
		var err error
		if idQuoted, err = quoteIdent(m.ID); err != nil {
			return "", false, err
		}
	}
	selects := make([]string, 0, len(cols)+1)
	if hasID {
		selects = append(selects, readExpr(idColumn, idQuoted))
	}
	for _, col := range cols {
		q, err := quoteIdent(col)
		if err != nil {
			return "", false, err
		}
		column, _ := table.Column(col)
		selects = append(selects, readExpr(column, q))
	}
	qualified, err := quoteQualified(cfg.QualifiedTable(m))
	if err != nil {
		return "", false, err
	}
	query := "SELECT " + strings.Join(selects, ", ") + " FROM " + qualified
	if m.Where != "" {
		// The line break ends a -- comment the predicate may close with.
		query += " WHERE (" + m.Where + "\n)"
	}
	// The id and the key are ordered by as the table holds them, which is
	// why they are qualified: a bare "id" would name the output column of
	// the same name, the id's text, and put 10 before 9.
	var orderBy []string
	ordered := map[string]bool{}
	if hasID {
		orderBy = append(orderBy, qualified+"."+idQuoted)
		ordered[m.ID] = true
	}
	for _, col := range m.keyColumns() {
		q, err := quoteIdent(col)
		if err != nil {
			return "", false, fmt.Errorf("key column %w", err)
		}
		if !ordered[col] {
			orderBy = append(orderBy, qualified+"."+q)
			ordered[col] = true
		}
	}
	// Without an id nothing says two rows sharing a key, or a key_any_of
	// group that is unset in both, are not equal, and PostgreSQL may return
	// such rows in either order. Every other column, as the text it is read
	// as, settles it, so two runs read the same order.
	if !hasID {
		for i, col := range cols {
			if !ordered[col] {
				orderBy = append(orderBy, strconv.Itoa(i+1))
			}
		}
	}
	if len(orderBy) > 0 {
		query += " ORDER BY " + strings.Join(orderBy, ", ")
	}
	return query, hasID, nil
}

// readExpr is how a column is read as text. PostgreSQL's own text is the
// value for nearly every type, in the session's fixed settings; json keeps
// the spelling it was written in, so it is read through jsonb, which has one;
// money's text depends on the locale, so it is read as the number it is; and
// an array is read as JSON, which is what a YAML sequence in the fixture file
// becomes, and what an export writes back as one.
//
// The elements of a char(n) array are read without their padding, the way a
// char(n) column's own value is, so "AB " and "AB" are one value on both
// sides. An array whose lower bound is not 1, '[0:1]={7,8}', has no JSON and
// no YAML spelling: it is read as PostgreSQL's own text, which differs from
// every sequence, and an export refuses it.
func readExpr(c dbschema.Column, expr string) string {
	switch {
	case c.Type == "json":
		return "(" + expr + ")::jsonb::text"
	case c.Type == "money":
		return "(" + expr + ")::numeric::text"
	case c.Category == "A":
		elems := "(" + expr + ")"
		if c.ElemType == "bpchar" {
			elems = "(" + expr + ")::text[]"
		}
		return "CASE WHEN (" + expr + ")::text LIKE '[%' THEN (" + expr + ")::text ELSE to_jsonb(" + elems +
			")::text END"
	}
	return "(" + expr + ")::text"
}

// readRows runs the one SELECT per model.
func readRows(ctx context.Context, db bun.IDB, cfg *Config, m *Model, table *dbschema.Table,
	cols []string) ([]*rawRow, error) {

	query, hasID, err := selectQuery(cfg, m, table, cols)
	if err != nil {
		return nil, err
	}
	width := len(cols)
	if hasID {
		width++
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", query, err)
	}
	defer rows.Close()
	var out []*rawRow
	for rows.Next() {
		cells := make([]sql.NullString, width)
		scan := make([]any, width)
		for i := range cells {
			scan[i] = &cells[i]
		}
		if err := rows.Scan(scan...); err != nil {
			return nil, err
		}
		r := &rawRow{values: map[string]fixturechange.Value{}}
		i := 0
		if hasID {
			if cells[0].Valid {
				idCol, _ := table.Column(m.ID)
				r.id = columnText(idCol, cells[0].String)
			}
			if zeroID(m, r.id) {
				r.id = ""
			}
			i = 1
		}
		for j, col := range cols {
			c := cells[i+j]
			if !c.Valid {
				r.values[col] = fixturechange.Null()
				continue
			}
			column, _ := table.Column(col)
			r.values[col] = fixturechange.Lit(columnText(column, c.String))
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, rows.Close()
}

// keyOf builds a natural key out of already-resolved values. They include the
// id (Entry.Full), because the natural key can be the primary key: a currency
// table keyed by its ISO code.
func keyOf(cfg *Config, m *Model, model string, values fixturechange.Values) (fixturechange.Values, error) {
	out := fixturechange.Values{}
	for _, col := range m.Key {
		v, ok := values[col]
		if !ok {
			return nil, fmt.Errorf("%s: key column %q was not read", model, col)
		}
		out[col] = v
	}
	for _, group := range m.KeyAnyOf {
		// A group none of whose columns is there is NULL, as a fixture row
		// leaving them all out is.
		chosen, value := group[0], fixturechange.Null()
		if v, ok := values[group[0]]; ok {
			value = v
		}
		for _, col := range group {
			v, ok := values[col]
			if ok && !isZero(v) {
				chosen, value = col, v
				break
			}
		}
		out[chosen] = value
	}
	return out, nil
}

// anchorOf makes dbfixture's "_id" out of a natural key. It has to be readable,
// stable and unique inside the model, in that order: it is what a reference in
// the exported file names.
func anchorOf(key fixturechange.Values) string {
	parts := make([]string, 0, len(key))
	for _, col := range sortedColumns(key) {
		v := key[col]
		s := v.Lit
		if v.Ref != nil {
			s = v.Ref.Key
		}
		if v.IsNull {
			s = "null"
		}
		if s = slug(s); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return "row"
	}
	anchor := strings.Join(parts, "_")
	// A template names the anchor as a Go identifier ("$.Plan.row.ID"), and
	// text/template reads "1_month" as a malformed number.
	if anchor[0] >= '0' && anchor[0] <= '9' {
		anchor = "r" + anchor
	}
	return anchor
}

func uniqueAnchor(base, id string, taken map[string]bool) string {
	candidate := base
	if taken[candidate] && id != "" {
		candidate = base + "_" + slug(id)
	}
	for n := 2; taken[candidate]; n++ {
		candidate = fmt.Sprintf("%s_%d", base, n)
	}
	taken[candidate] = true
	return candidate
}

// slug reduces a value to [a-z0-9_], which is what a dbfixture anchor may hold
// and still be readable inside a template.
func slug(s string) string {
	var b strings.Builder
	last := byte('_')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			c += 'a' - 'A'
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
		default:
			c = '_'
		}
		if c == '_' && last == '_' {
			continue
		}
		b.WriteByte(c)
		last = c
	}
	return strings.Trim(b.String(), "_")
}

// reportDuplicateRefs reports a model whose ref column is not unique among the
// rows something points at. Every generated migration resolves a reference with
// "WHERE <ref> = ?", so two rows sharing one ref value make that lookup pick an
// arbitrary row, or fail. The schema this tool was written for had two such
// tables: no unique index, no check in the admin UI, and nothing had ever said
// so.
func reportDuplicateRefs(cfg *Config, snap *Snapshot) {
	referenced := map[string]bool{}
	for _, model := range snap.Order {
		for col, target := range cfg.Models[model].References {
			if !cfg.Models[model].skip(col) {
				referenced[target] = true
			}
		}
	}
	for _, model := range snap.Order {
		if !referenced[model] {
			continue
		}
		m := cfg.Models[model]
		seen := map[string][]string{}
		var order []string
		for _, e := range snap.Entries[model] {
			v, ok := e.Cells[m.Ref]
			if !ok || v.Ref != nil || v.IsNull {
				continue
			}
			if _, dup := seen[v.Lit]; !dup {
				order = append(order, v.Lit)
			}
			id := e.ID
			if id == "" {
				id = "(no id)"
			}
			seen[v.Lit] = append(seen[v.Lit], id)
		}
		for _, value := range order {
			ids := seen[value]
			if len(ids) < 2 {
				continue
			}
			snap.Findings = append(snap.Findings, Finding{
				Kind: FindingDuplicateKey, Model: model, Row: m.Ref + "=" + value,
				Detail: fmt.Sprintf(
					"%s share this %s (%s), and every generated reference resolves with "+
						"WHERE %s = ?, so it cannot name one of them: add a unique index on %s",
					plural(len(ids), "row"), m.Ref, strings.Join(ids, ", "), m.Ref, m.Ref),
			})
		}
	}
}
