package fixturemigrate

// The natural-key lint: whether a unique index or constraint of the table
// makes each model's natural key unique among the model's rows.
//
// Every guard and every reference a migration writes looks a row up by its
// natural key, and the run time refuses a key two rows hold rather than pick
// one. What it cannot do is keep the second row out: only an index can, and
// without one the application, an admin UI or a race with a migration's own
// INSERT ... WHERE NOT EXISTS adds it, and every change to that key fails
// from then on. An index that looks right is often not: one over more
// columns than the key, one that holds NULLs distinct over a nullable key
// column, a partial one whose predicate the model's rows are not all within,
// and one a failed CREATE INDEX CONCURRENTLY left invalid.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
	"github.com/RELAXccc/bun-fixture-migrate/internal/pgerr"

	"github.com/uptrace/bun"
)

// RowFilter is the predicate that keeps a model to its rows, the rows its
// natural key has to be unique among: its where, and, for a model whose rows
// are soft-deleted, the live ones, where softDelete is NULL. "" keeps every
// row. A where may end in a comment, so its closing parenthesis goes on a
// line of its own, as the run time puts it.
func RowFilter(where, softDelete string) string {
	var parts []string
	if strings.TrimSpace(where) != "" {
		parts = append(parts, "("+where+closeParen(where))
	}
	if softDelete != "" {
		parts = append(parts, sqlIdent(softDelete)+" IS NULL")
	}
	return strings.Join(parts, " AND ")
}

// closeParen closes the parenthesis around a where on a line of its own when
// the where holds a comment that would swallow it.
func closeParen(where string) string {
	if strings.Contains(where, "--") {
		return "\n)"
	}
	return ")"
}

// modelFilter is the row filter LintKeys tests a model's keys under. A model
// that soft-deletes its rows reads its live ones, so a partial index over the
// live rows backs its key.
func modelFilter(m *Model) string { return RowFilter(m.Where, m.SoftDelete) }

// KeyVerdict is what the lint makes of one natural key of a table.
type KeyVerdict struct {
	// Backed is true when a unique index or constraint of the table makes
	// the key unique among the rows the filter keeps.
	Backed bool
	// Index is the index that backs the key or, for one that is not backed,
	// the index Detail is about; "" when there is none.
	Index string
	// Detail says, for a key that is not backed, why, and what to do: the
	// sentence of the finding. "" for a backed key.
	Detail string
	// Unsure is a key the lint could not decide about: an index might back
	// it, and neither PostgreSQL nor the predicates as written settle
	// whether it does. Detail says so.
	Unsure bool

	// stricter are the indexes that back the key and are stricter than it:
	// over a part of its columns, or over expressions of them, so two keys
	// the tool tells apart can be one value to the index.
	stricter []dbschema.KeyIndex
}

// KeyBacked judges whether the unique indexes and exclusion constraints of
// table make key unique among the rows filter keeps (see RowFilter): plan
// asks it of every model a pending migration changes. It reads, in db, what
// the catalog cannot say: what an index expression makes of the key's
// columns, and whether the filter implies a partial index's predicate. It
// writes nothing, and leaves a transaction as it found it.
func KeyBacked(ctx context.Context, db bun.IDB, table *dbschema.Table, key []string, filter string) (KeyVerdict, error) {
	var v KeyVerdict
	err := ReadOnly(ctx, db, func(tx bun.Tx) error {
		version, err := serverVersion(ctx, tx)
		if err != nil {
			return err
		}
		k := newLintKey(table, key, nil, "key ["+strings.Join(key, ", ")+"]")
		v, err = judgeKey(table, k, filter, version, &sqlProbe{ctx: ctx, tx: tx, table: table})
		return err
	})
	return v, err
}

// LintKeys checks that a unique index or constraint backs each natural key of
// every model of snap that has a table, under its where, and the ref column
// of every model something references, which every reference is resolved
// by. A key that is not backed is a FindingUnbackedKey, under
// policy.key_index. A key an index backs that is stricter than it, unique
// over lower(email) or over a part of the key, has the fixture rows grouped
// by the index: two of them it holds equal are a FindingDuplicateKey, as
// dbfixture cannot load both and a migration fails on the second.
//
// It reads, and writes nothing: the catalog through tables, and, in
// savepoints that are rolled back, what an index expression makes of the
// key's columns and whether the planner may use a partial index for a
// lookup under the model's where.
func LintKeys(ctx context.Context, db bun.IDB, cfg *Config, snap *Snapshot, tables map[string]*dbschema.Table) error {
	return ReadOnly(ctx, db, func(tx bun.Tx) error {
		version, err := serverVersion(ctx, tx)
		if err != nil {
			return err
		}
		referenced := referencedModels(cfg, snap.Order)
		for _, model := range snap.Order {
			m := cfg.Models[model]
			if m == nil {
				continue
			}
			table := tables[cfg.QualifiedTable(m)]
			if table == nil {
				continue
			}
			probe := &sqlProbe{ctx: ctx, tx: tx, table: table}
			filter := modelFilter(m)
			keys := effectiveKeys(m, table)
			if referenced[model] && m.Ref != "" && !coveredBy(keys, m.Ref) {
				if _, ok := table.Column(m.Ref); ok {
					// A ref value is never NULL: a row without one is no
					// row a reference can name.
					keys = append(keys, newLintKey(table, []string{m.Ref}, map[string]bool{m.Ref: true},
						"ref "+m.Ref))
				}
			}
			for _, key := range keys {
				v, err := judgeKey(table, key, filter, version, probe)
				if err != nil {
					return fmt.Errorf("%s %s: %w", model, key.label, err)
				}
				if !v.Backed {
					snap.Findings = append(snap.Findings, Finding{Kind: FindingUnbackedKey, Model: model,
						Row: key.label, Detail: v.Detail, unsure: v.Unsure})
					continue
				}
				if key.ref {
					continue
				}
				for _, index := range v.stricter {
					if err := reportStricter(ctx, tx, probe, model, table, key, index, snap); err != nil {
						return fmt.Errorf("%s %s: %w", model, key.label, err)
					}
				}
			}
		}
		return nil
	})
}

// referencedModels are the models another model's reference points at.
func referencedModels(cfg *Config, order []string) map[string]bool {
	out := map[string]bool{}
	for _, model := range order {
		m := cfg.Models[model]
		if m == nil {
			continue
		}
		for col, target := range m.References {
			if !m.skip(col) {
				out[target] = true
			}
		}
	}
	return out
}

// coveredBy reports whether one of keys is exactly the one column col.
func coveredBy(keys []lintKey, col string) bool {
	for _, k := range keys {
		if len(k.cols) == 1 && k.cols[0] == col {
			return true
		}
	}
	return false
}

// lintKey is one effective natural key of a model: its key columns, plus one
// column of each key_any_of group.
type lintKey struct {
	cols []string
	// nullable are the columns of cols a row the model holds may have NULL
	// in, which a unique index holds distinct.
	nullable map[string]bool
	// label names the key in a finding: "key [plan_id, code]", "ref code".
	label string
	// ref is the ref column of a referenced model, tested as a key.
	ref bool
	// anyOf are the key_any_of columns of the model: those in cols hold a
	// value in every row the key names, and the others are no part of it.
	anyOf map[string]bool
}

// newLintKey is a key over cols, nullable where the table's column is. The
// columns in notNull hold a value in every row the key names.
func newLintKey(t *dbschema.Table, cols []string, notNull map[string]bool, label string) lintKey {
	k := lintKey{cols: cols, nullable: map[string]bool{}, label: label, ref: strings.HasPrefix(label, "ref ")}
	for _, col := range cols {
		if c, ok := t.Column(col); ok && c.Nullable && !notNull[col] {
			k.nullable[col] = true
		}
	}
	return k
}

// effectiveKeys are the keys a model's rows are looked up by: its key, plus
// one column of each key_any_of group, in every combination. The column a
// group adds holds a value other than "", 0 or NULL, which is how keyOf
// chose it. A key naming a column the table does not have is left to the
// check that says so.
func effectiveKeys(m *Model, t *dbschema.Table) []lintKey {
	combos := [][]string{append([]string{}, m.Key...)}
	for _, group := range m.KeyAnyOf {
		var next [][]string
		for _, c := range combos {
			for _, col := range group {
				next = append(next, append(append([]string{}, c...), col))
			}
		}
		combos = next
	}
	chosen := map[string]bool{}
	for _, group := range m.KeyAnyOf {
		for _, col := range group {
			chosen[col] = true
		}
	}
	var out []lintKey
	seen := map[string]bool{}
	for _, cols := range combos {
		missing := len(cols) == 0
		for _, col := range cols {
			if _, ok := t.Column(col); !ok {
				missing = true
			}
		}
		sorted := append([]string{}, cols...)
		sort.Strings(sorted)
		id := strings.Join(sorted, "\x00")
		if missing || seen[id] {
			continue
		}
		seen[id] = true
		k := newLintKey(t, cols, chosen, "key ["+strings.Join(cols, ", ")+"]")
		k.anyOf = chosen
		out = append(out, k)
	}
	return out
}

// errCannotTell is a question PostgreSQL could not answer about an index,
// an expression it could not evaluate, say, with its message.
type errCannotTell struct{ msg, state string }

func (e errCannotTell) Error() string { return e.msg }

// keyProbe asks the database what the catalog does not say about an index.
// The verdict is worked out from its answers; tests answer for it.
type keyProbe interface {
	// readsOnly reports whether an index expression reads no column of the
	// table but cols: whether it can be parsed over a row of only those.
	readsOnly(expr string, cols []string) (bool, error)
	// nullOver reports whether an index expression is NULL over a row of
	// the columns cols, every one of them NULL.
	nullOver(expr string, cols []string) (bool, error)
	// planned lists the indexes PostgreSQL's planner would use, with
	// sequential scans off, to look a key up under the filter by the
	// index's columns. A partial index is among those it may use exactly
	// when the lookup's WHERE implies its predicate.
	planned(index dbschema.KeyIndex, key lintKey, filter string) ([]string, error)
}

// keyReason is why an index does not back a key, the most specific first.
type keyReason int

const (
	reasonNone keyReason = iota
	// reasonUnsure: it may back the key, and nothing settles whether.
	reasonUnsure
	// reasonNullable: a nullable key column it holds NULLs distinct in.
	reasonNullable
	// reasonPartial: a predicate the model's rows are not all within.
	reasonPartial
	// reasonSuperset: columns outside the key.
	reasonSuperset
	// reasonInvalid: an index a failed CREATE INDEX CONCURRENTLY left.
	reasonInvalid
	// reasonNoIndex: nothing over the key's columns at all.
	reasonNoIndex
)

// candidate is what the lint found out about one index for one key.
type candidate struct {
	index  dbschema.KeyIndex
	reason keyReason
	// nulls are the key columns, or index expressions, that are NULL in a
	// row the index then holds distinct (reasonNullable).
	nulls []string
	// exprNull is set when nulls are expressions.
	exprNull bool
	// outside are the index's columns outside the key (reasonSuperset).
	outside []string
	// why says what could not be decided (reasonUnsure).
	why      string
	stricter bool
	// partialToo is set on a candidate that fails on its NULLs whose
	// predicate the model's rows are not all within either.
	partialToo bool
}

// judgeKey is the verdict on one key: whether a valid unique index or
// exclusion constraint of every operator = has its columns within the key,
// a predicate the filter implies, and NULLs held equal where the key may
// hold one; and if none has, the most specific reason why not.
func judgeKey(t *dbschema.Table, key lintKey, filter string, version int, p keyProbe) (KeyVerdict, error) {
	inKey := map[string]bool{}
	for _, col := range key.cols {
		inKey[col] = true
	}
	var cands []candidate
	var invalid *dbschema.KeyIndex
	var v KeyVerdict
	for _, index := range t.KeyIndexes {
		if !index.Equality() {
			continue
		}
		if !index.Valid {
			if invalid == nil && plainWithin(index, inKey) {
				index := index
				invalid = &index
			}
			continue
		}
		c, err := judgeIndex(t, index, key, inKey, filter, p)
		if err != nil {
			return v, err
		}
		if c.reason == reasonNone {
			if !v.Backed {
				v.Backed, v.Index = true, index.Name
			}
			if c.stricter {
				v.stricter = append(v.stricter, index)
			}
		}
		cands = append(cands, c)
	}
	if v.Backed {
		return v, nil
	}
	// Nothing backs the key: the finding is about the index that came
	// nearest, an undecided one first, since it may back the key after all.
	best := candidate{reason: reasonNoIndex}
	if invalid != nil {
		best = candidate{index: *invalid, reason: reasonInvalid}
	}
	for _, c := range cands {
		if c.reason < best.reason || c.reason == best.reason && nearer(c.index, best.index, inKey) {
			best = c
		}
	}
	v.Index = best.index.Name
	v.Unsure = best.reason == reasonUnsure
	v.Detail = keyDetail(t, key, filter, version, best)
	return v, nil
}

// nearer reports whether of two indexes that fail a key for the same reason
// a is the one to name: over more of the key's columns, or else with a
// predicate that says less.
func nearer(a, b dbschema.KeyIndex, inKey map[string]bool) bool {
	na, nb := keyColumnsOf(a, inKey), keyColumnsOf(b, inKey)
	if na != nb {
		return na > nb
	}
	return len(conjuncts(a.Predicate)) < len(conjuncts(b.Predicate))
}

// keyColumnsOf counts the plain columns of an index that are in the key.
func keyColumnsOf(index dbschema.KeyIndex, inKey map[string]bool) int {
	n := 0
	for _, c := range index.Columns {
		if c.Column != "" && inKey[c.Column] {
			n++
		}
	}
	return n
}

// overlaps reports whether an index has a column of the key, plain or read
// by an expression.
func overlaps(index dbschema.KeyIndex, inKey map[string]bool) bool {
	for _, c := range index.Columns {
		if c.Column != "" && inKey[c.Column] {
			return true
		}
	}
	if !index.Plain() {
		for _, col := range index.Reads {
			if inKey[col] {
				return true
			}
		}
	}
	return false
}

// provable reports whether the planner could prove a partial index's
// predicate from a lookup of the key under the filter at all: whether every
// column of the table the predicate names is named by the filter, or is a
// column of the index the lookup holds to a value (col = $n proves col IS
// NOT NULL; a NULL looked up as NULL proves nothing). PostgreSQL proves a
// predicate about a column only from a condition on that column, so a
// predicate on deleted_at is never implied by a where on status alone, and
// no planner needs asking.
func provable(t *dbschema.Table, index dbschema.KeyIndex, key lintKey, filter string) bool {
	named := columnsNamed(t, filter)
	for _, c := range index.Columns {
		if c.Column != "" && !key.nullable[c.Column] {
			named[c.Column] = true
		}
	}
	for col := range columnsNamed(t, index.Predicate) {
		if !named[col] {
			return false
		}
	}
	return true
}

var identToken = regexp.MustCompile(`"((?:[^"]|"")+)"|[A-Za-z_][A-Za-z0-9_$]*`)

// columnsNamed are the columns of t an SQL condition names, outside its
// string literals: a word that is a column's name, unquoted and lower-cased
// or double-quoted as it is.
func columnsNamed(t *dbschema.Table, expr string) map[string]bool {
	out := map[string]bool{}
	parts := strings.Split(expr, "'")
	for i := 0; i < len(parts); i += 2 {
		for _, m := range identToken.FindAllStringSubmatch(parts[i], -1) {
			name := strings.ToLower(m[0])
			if m[1] != "" {
				name = strings.ReplaceAll(m[1], `""`, `"`)
			}
			if _, ok := t.Column(name); ok {
				out[name] = true
			}
		}
	}
	return out
}

// plainWithin reports whether every plain column of an index is in the key,
// and the columns its expressions and predicate read are too.
func plainWithin(index dbschema.KeyIndex, inKey map[string]bool) bool {
	for _, c := range index.Columns {
		if c.Column != "" && !inKey[c.Column] {
			return false
		}
	}
	if !index.Plain() {
		for _, col := range index.Reads {
			if !inKey[col] {
				return false
			}
		}
	}
	return true
}

// judgeIndex judges one valid index: its columns, then its predicate, then
// its NULLs.
func judgeIndex(t *dbschema.Table, index dbschema.KeyIndex, key lintKey, inKey map[string]bool, filter string,
	p keyProbe) (candidate, error) {

	c := candidate{index: index}
	// Columns: every plain one in the key, every expression over the key's
	// columns alone. Over fewer, or over expressions, it is stricter.
	plain := map[string]bool{}
	for _, col := range index.Columns {
		if col.Column != "" {
			plain[col.Column] = true
			if !inKey[col.Column] {
				c.outside = append(c.outside, col.Column)
			}
			continue
		}
		c.stricter = true
		ok, err := p.readsOnly(col.Expr, key.cols)
		if cannot := (errCannotTell{}); errors.As(err, &cannot) {
			c.reason, c.why = reasonUnsure, fmt.Sprintf("PostgreSQL could not evaluate %s over the key's "+
				"columns: %s", col.Expr, cannot.msg)
			return c, nil
		}
		if err != nil {
			return c, err
		}
		if !ok {
			c.outside = append(c.outside, col.Expr)
		}
	}
	if len(c.outside) > 0 {
		// An index that shares no column with the key, the primary key
		// over the id say, says nothing about it.
		c.reason = reasonSuperset
		if !overlaps(index, inKey) {
			c.reason = reasonNoIndex
		}
		return c, nil
	}
	if len(plain) < len(key.cols) {
		c.stricter = true
	}

	// The predicate: the filter has to imply it, which the predicates as
	// written may show, and the planner otherwise.
	undecided := ""
	switch {
	case index.Predicate == "" || impliedAsWritten(filter, index.Predicate):
	case !provable(t, index, key, filter):
		c.reason = reasonPartial
	default:
		used, err := p.planned(index, key, filter)
		var cannot errCannotTell
		switch {
		case errors.As(err, &cannot):
			undecided = "PostgreSQL's planner could not be asked: " + cannot.msg
		case err != nil:
			return c, err
		case contains(used, index.Name):
		case len(used) == 0:
			c.reason = reasonPartial
		default:
			// Another index the lookup may use instead says nothing of
			// this one; if it backs the key, the key is backed anyway.
			undecided = "the planner chose " + strings.Join(used, " and ") + " for the lookup instead"
		}
	}

	// NULLs: a key column that may be NULL, plain in the index or read by
	// an expression that is then NULL, lets any number of rows hold the key
	// unless the index holds NULLs equal.
	if !index.NullsNotDistinct && len(key.nullable) > 0 {
		for _, col := range index.Columns {
			if col.Column != "" {
				if key.nullable[col.Column] {
					c.nulls = append(c.nulls, col.Column)
				}
				continue
			}
			null, err := p.nullOver(col.Expr, key.cols)
			if cannot := (errCannotTell{}); errors.As(err, &cannot) {
				undecided = fmt.Sprintf("PostgreSQL could not evaluate %s with the key's columns NULL: %s",
					col.Expr, cannot.msg)
				continue
			}
			if err != nil {
				return c, err
			}
			if !null {
				continue
			}
			// NULL over a row of NULLs, but maybe only for a NULL in a
			// column that holds a value in every row.
			var notNull []string
			for _, k := range key.cols {
				if !key.nullable[k] {
					notNull = append(notNull, k)
				}
			}
			readsNotNullOnly, err := p.readsOnly(col.Expr, notNull)
			if err != nil && !errors.As(err, &errCannotTell{}) {
				return c, err
			}
			if err != nil || !readsNotNullOnly {
				c.nulls, c.exprNull = append(c.nulls, col.Expr), true
			}
		}
	}
	switch {
	case len(c.nulls) > 0:
		c.partialToo = c.reason == reasonPartial
		c.reason = reasonNullable
	case c.reason == reasonPartial:
		// Whatever else is undecided, the index does not back the key.
	case undecided != "":
		c.reason, c.why = reasonUnsure, undecided
	}
	return c, nil
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

// impliedAsWritten reports whether every conjunct of a predicate is a
// conjunct of the filter, compared as written once case, spacing,
// parentheses, quotes around plain identifiers and casts are taken away. It
// settles the common case without the planner, a soft-delete filter under a
// live-rows index, and is the planner's fallback. It only ever says yes for
// a predicate the filter implies; no is no answer.
func impliedAsWritten(filter, predicate string) bool {
	have := map[string]bool{}
	for _, c := range conjuncts(filter) {
		have[c] = true
	}
	want := conjuncts(predicate)
	if len(want) == 0 {
		return false
	}
	for _, c := range want {
		if !have[c] {
			return false
		}
	}
	return true
}

var (
	castPattern   = regexp.MustCompile(`::\s*("[^"]+"|[a-z_][a-z0-9_]*(\s+(varying|precision|without time zone|with time zone))?)(\[\])?(\(\d+(,\d+)?\))?`)
	quotedIdent   = regexp.MustCompile(`"([a-z_][a-z0-9_]*)"`)
	spacesPattern = regexp.MustCompile(`\s+`)
)

// conjuncts splits an SQL condition at its top-level ANDs, each normalised;
// a conjunct that is an AND in parentheses, as pg_get_expr writes each one,
// is split too.
func conjuncts(expr string) []string {
	expr = normalisePredicate(expr)
	if expr == "" {
		return nil
	}
	var parts []string
	depth, start, quote := 0, 0, false
	for i := 0; i < len(expr); i++ {
		switch ch := expr[i]; {
		case ch == '\'':
			quote = !quote
		case quote:
		case ch == '(':
			depth++
		case ch == ')':
			depth--
		case depth == 0 && strings.HasPrefix(expr[i:], " and "):
			parts = append(parts, expr[start:i])
			start = i + len(" and ")
		}
	}
	if start == 0 {
		return []string{expr}
	}
	parts = append(parts, expr[start:])
	var out []string
	for _, part := range parts {
		out = append(out, conjuncts(part)...)
	}
	return out
}

// normalisePredicate lower-cases a condition outside its string literals,
// collapses its spacing, and drops casts and the quotes around plain
// identifiers.
func normalisePredicate(expr string) string {
	var b strings.Builder
	parts := strings.Split(expr, "'")
	for i, part := range parts {
		if i > 0 {
			b.WriteByte('\'')
		}
		if i%2 == 1 {
			b.WriteString(part)
			continue
		}
		part = strings.ToLower(part)
		part = spacesPattern.ReplaceAllString(part, " ")
		part = castPattern.ReplaceAllString(part, "")
		part = quotedIdent.ReplaceAllString(part, "$1")
		part = strings.ReplaceAll(part, "( ", "(")
		part = strings.ReplaceAll(part, " )", ")")
		b.WriteString(part)
	}
	return strings.TrimSpace(stripParens(strings.TrimSpace(b.String())))
}

// stripParens takes away parentheses that enclose the whole of expr.
func stripParens(expr string) string {
	for len(expr) >= 2 && expr[0] == '(' && expr[len(expr)-1] == ')' {
		depth, quote := 0, false
		whole := true
		for i := 0; i < len(expr) && whole; i++ {
			switch ch := expr[i]; {
			case ch == '\'':
				quote = !quote
			case quote:
			case ch == '(':
				depth++
			case ch == ')':
				depth--
				if depth == 0 && i != len(expr)-1 {
					whole = false
				}
			}
		}
		if !whole {
			break
		}
		expr = strings.TrimSpace(expr[1 : len(expr)-1])
	}
	return expr
}

// keyDetail is the sentence of the finding for a key nothing backs, and what
// to do about it.
func keyDetail(t *dbschema.Table, key lintKey, filter string, version int, c candidate) string {
	def := c.index.Definition()
	same := sameWords(key.cols)
	switch c.reason {
	case reasonUnsure:
		return fmt.Sprintf("cannot tell whether %s backs %s: %s, and the predicates as written do not settle it. "+
			"Repeat the index's predicate in the model's where to settle it, or %s", def, key.label, c.why,
			createIndex(t, key, filter, version))
	case reasonNullable:
		if c.exprNull {
			return fmt.Sprintf("%s is NULL where %s is, and %s holds NULLs distinct, so any number of rows "+
				"may hold the same key with %s NULL: %s%s", strings.Join(c.nulls, " and "),
				orWords(nullableCols(key)), def, orWords(nullableCols(key)), nullsRemedy(t, key, c, version),
				partialToo(c, filter))
		}
		verb := "is"
		if len(c.nulls) > 1 {
			verb = "are"
		}
		rest := restOf(key.cols, c.nulls)
		hold := "the same key"
		if len(rest) > 0 {
			hold = "the same " + sameWords(rest)
		}
		return fmt.Sprintf("%s %s nullable and %s holds NULLs distinct, so any number of rows may hold %s with "+
			"%s NULL: %s%s", andWords(c.nulls), verb, def, hold, orWords(c.nulls), nullsRemedy(t, key, c, version),
			partialToo(c, filter))
	case reasonPartial:
		return fmt.Sprintf("%s holds only where %s, and %s: %s", def, trimPredicate(c.index.Predicate),
			outsideWords(filter),
			partialRemedy(t, key, filter, version, c.index))
	case reasonSuperset:
		if c.index.Plain() {
			// Keyed on a key_any_of group's other column, it would be
			// another key; the index is no help to this one.
			rekey := ""
			if !key.ref && !anyIn(c.outside, key.anyOf) {
				rekey = "key on [" + strings.Join(append(append([]string{}, key.cols...), c.outside...), ", ") +
					"], or "
			}
			return fmt.Sprintf("%s is over more columns than %s, so two rows may share %s: %s%s", def, key.label,
				same, rekey, createIndex(t, key, filter, version))
		}
		return fmt.Sprintf("%s reads columns outside %s, so two rows may share %s: %s", def, key.label, same,
			createIndex(t, key, filter, version))
	case reasonInvalid:
		return fmt.Sprintf("unique index %s is invalid, left by a CREATE INDEX CONCURRENTLY that failed, and "+
			"backs nothing: delete the duplicate rows it failed on, then REINDEX INDEX CONCURRENTLY %s",
			c.index.Name, sqlIndexName(t, c.index.Name))
	}
	what := "every change to it then fails as a duplicate key"
	if key.ref {
		what = "every reference to it then fails to resolve"
	}
	return fmt.Sprintf("no unique index or constraint backs %s: the database lets a second row with this %s in, "+
		"and %s. %s", key.label, strings.TrimPrefix(strings.SplitN(key.label, " [", 2)[0], "ref "), what,
		createIndex(t, key, filter, version))
}

// partialToo is the second half of the sentence on an index that fails on
// its NULLs and on its predicate too.
func partialToo(c candidate, filter string) string {
	if !c.partialToo {
		return ""
	}
	return fmt.Sprintf(". It also holds only where %s, and %s", trimPredicate(c.index.Predicate), outsideWords(filter))
}

// outsideWords says that a model's rows are not all within a predicate.
func outsideWords(filter string) string {
	if filter == "" {
		return "this model reads every row"
	}
	return "the rows this model reads, where " + showFilter(filter) + ", are not all within it"
}

// partialRemedy is what to do about a partial index whose predicate the
// model's rows are not all within. A model whose field is bun's soft_delete
// gets its live rows from its soft-delete column, which is where to say so.
func partialRemedy(t *dbschema.Table, key lintKey, filter string, version int, index dbschema.KeyIndex) string {
	return softDeleteRemedy(t, index) + "give the model a where that implies it, if the rows outside it are no " +
		"master data, or " + createIndex(t, key, filter, version)
}

// softDeleteRemedy is the first remedy for a partial index over the live rows
// of a table bun soft-deletes, "(deleted_at IS NULL)": set soft_delete, so the
// model reads its live rows. "" for an index over any other predicate.
func softDeleteRemedy(t *dbschema.Table, index dbschema.KeyIndex) string {
	col := liveRowsPredicate(t, index.Predicate)
	if col == "" {
		return ""
	}
	return "set soft_delete: " + col + " if the model's field is bun's soft_delete, or "
}

// nullsRemedy is what to do about a nullable key column an index holds
// NULLs distinct in: NULLS NOT DISTINCT from PostgreSQL 15 on, an index
// over COALESCE before, or NOT NULL.
func nullsRemedy(t *dbschema.Table, key lintKey, c candidate, version int) string {
	cols := c.nulls
	if c.exprNull {
		cols = nullableCols(key)
	}
	notNull := "make " + andWords(cols) + " NOT NULL"
	switch {
	case c.index.Exclusion && !c.index.Unique:
		if version >= 150000 {
			return notNull + ", or back the key with a unique index NULLS NOT DISTINCT in its place"
		}
		return notNull + ", or back the key with a unique index over COALESCE(" + cols[0] + ", ...) in its place"
	case version >= 150000:
		return "declare it NULLS NOT DISTINCT (PostgreSQL 15 and later), or " + notNull
	}
	if c.exprNull {
		return notNull + ", or have the expression map NULL to a value, as COALESCE does"
	}
	return "index " + coalesced(t, c.index) + " in its place, or " + notNull
}

// coalesced is an index's column list with each nullable plain column of it
// in COALESCE, NULL mapped to the zero of its type, the way to hold NULLs
// equal before PostgreSQL 15.
func coalesced(t *dbschema.Table, index dbschema.KeyIndex) string {
	parts := make([]string, 0, len(index.Columns))
	for _, col := range index.Columns {
		if col.Column == "" {
			parts = append(parts, col.Expr)
			continue
		}
		name := sqlName(col.Column)
		if c, ok := t.Column(col.Column); ok && c.Nullable {
			zero := "<a value it never holds>"
			if z, ok := c.ZeroText(); ok {
				zero = z
				if c.Type != "bool" && !numericType(c.Type) {
					zero = "'" + strings.ReplaceAll(z, "'", "''") + "'"
				}
			}
			name = "COALESCE(" + name + ", " + zero + ")"
		}
		parts = append(parts, name)
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// createIndex is the CREATE UNIQUE INDEX that backs a key: over its columns,
// NULLS NOT DISTINCT where one may be NULL, or COALESCE before PostgreSQL 15,
// and over the model's rows only when it has a where.
func createIndex(t *dbschema.Table, key lintKey, filter string, version int) string {
	cols := make([]string, 0, len(key.cols))
	nullable := false
	for _, col := range key.cols {
		cols = append(cols, sqlName(col))
		if key.nullable[col] {
			nullable = true
		}
	}
	list := "(" + strings.Join(cols, ", ") + ")"
	if nullable && version < 150000 {
		index := dbschema.KeyIndex{Unique: true}
		for _, col := range key.cols {
			index.Columns = append(index.Columns, dbschema.IndexColumn{Column: col})
		}
		list = coalesced(t, index)
	}
	out := "CREATE UNIQUE INDEX ON " + tableName(t) + " " + list
	if nullable && version >= 150000 {
		out += " NULLS NOT DISTINCT"
	}
	// The rows a key_any_of key names hold a value in its group's column.
	var where []string
	for _, col := range key.cols {
		if key.anyOf[col] {
			where = append(where, sqlName(col)+" IS NOT NULL")
		}
	}
	if filter != "" {
		where = append(where, showFilter(filter))
	}
	if len(where) > 0 {
		out += " WHERE " + strings.Join(where, " AND ")
	}
	return out
}

// anyIn reports whether any of cols is in set.
func anyIn(cols []string, set map[string]bool) bool {
	for _, col := range cols {
		if set[col] {
			return true
		}
	}
	return false
}

// showFilter is a row filter as a message shows it.
func showFilter(filter string) string {
	return stripParens(strings.ReplaceAll(filter, "\n)", ")"))
}

// trimPredicate is a predicate as pg_get_expr writes it, without the
// parentheses around the whole.
func trimPredicate(pred string) string { return stripParens(strings.TrimSpace(pred)) }

var lowerIdent = regexp.MustCompile(`^[a-z_][a-z0-9_$]*$`)

// sqlName is a name as SQL needs it written: as it is when it is a plain
// lower-case identifier, double-quoted otherwise.
func sqlName(name string) string {
	if lowerIdent.MatchString(name) {
		return name
	}
	return sqlIdent(name)
}

// tableName is a table as a message names it: without the schema when that
// is public.
func tableName(t *dbschema.Table) string {
	if t.Schema == "public" {
		return sqlName(t.Name)
	}
	return sqlName(t.Schema) + "." + sqlName(t.Name)
}

// sqlIndexName is an index of t as SQL names it.
func sqlIndexName(t *dbschema.Table, name string) string {
	if t.Schema == "public" {
		return sqlName(name)
	}
	return sqlName(t.Schema) + "." + sqlName(name)
}

func nullableCols(key lintKey) []string {
	var out []string
	for _, col := range key.cols {
		if key.nullable[col] {
			out = append(out, col)
		}
	}
	return out
}

func restOf(cols, without []string) []string {
	var out []string
	for _, col := range cols {
		if !contains(without, col) {
			out = append(out, col)
		}
	}
	return out
}

// andWords is "a", "a and b", "a, b and c".
func andWords(words []string) string { return joinWords(words, "and") }

// orWords is "a", "a or b", "a, b or c".
func orWords(words []string) string { return joinWords(words, "or") }

func joinWords(words []string, last string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " " + last + " " + words[len(words)-1]
}

// sameWords names a key's columns as "the same" does: "code", "plan_id and
// code".
func sameWords(cols []string) string { return andWords(cols) }

// serverVersion is server_version_num: 160013 for 16.13.
func serverVersion(ctx context.Context, db bun.IDB) (int, error) {
	var version int
	err := db.QueryRowContext(ctx, "SELECT current_setting('server_version_num')::int").Scan(&version)
	return version, err
}

// sqlProbe answers keyProbe's questions in a transaction, each in a
// savepoint it rolls back: an expression that fails leaves the transaction
// usable, and the planner's settings go with the savepoint.
type sqlProbe struct {
	ctx   context.Context
	tx    bun.Tx
	table *dbschema.Table
}

// keySavepoint is the savepoint each of the lint's questions runs in.
const keySavepoint = "bfm_keylint"

// asked runs fn in a savepoint and rolls it back, whatever fn did. An error
// PostgreSQL raised over the question is an errCannotTell; any other, a
// connection lost say, comes back as it is.
func (p *sqlProbe) asked(fn func() error, after func() error) error {
	if _, err := p.tx.ExecContext(p.ctx, "SAVEPOINT "+keySavepoint); err != nil {
		return err
	}
	err := fn()
	bg := context.WithoutCancel(p.ctx)
	if _, rerr := p.tx.ExecContext(bg, "ROLLBACK TO SAVEPOINT "+keySavepoint); rerr != nil {
		return rerr
	}
	if after != nil {
		if aerr := after(); aerr != nil {
			return aerr
		}
	}
	if _, rerr := p.tx.ExecContext(bg, "RELEASE SAVEPOINT "+keySavepoint); rerr != nil {
		return rerr
	}
	if err != nil && valueError(err) {
		return errCannotTell{msg: valueMessage(err), state: pgerr.State(err)}
	}
	return err
}

// keyRow is a one-row table whose columns are the table's columns cols,
// every one NULL and of the column's own type, a domain's included: an outer
// join of the table, of which it reads no row.
func (p *sqlProbe) keyRow(cols []string) string {
	quoted := make([]string, 0, len(cols))
	for _, col := range cols {
		quoted = append(quoted, sqlIdent(col))
	}
	return "(SELECT 1 AS bfm__) AS bfm_one LEFT JOIN (SELECT " + strings.Join(quoted, ", ") + " FROM " +
		sqlIdent(p.table.Schema) + "." + sqlIdent(p.table.Name) + " WHERE false) AS bfm_key ON true"
}

func (p *sqlProbe) readsOnly(expr string, cols []string) (bool, error) {
	err := p.asked(func() error {
		_, err := p.tx.ExecContext(p.ctx, "SELECT ("+expr+") FROM "+p.keyRow(cols)+" WHERE false")
		return err
	}, nil)
	if err == nil {
		return true, nil
	}
	if undefinedColumn(err) {
		return false, nil
	}
	return false, err
}

// undefinedColumn reports the error of an expression naming a column the row
// it was evaluated over does not have.
func undefinedColumn(err error) bool {
	var cannot errCannotTell
	return errors.As(err, &cannot) && cannot.state == pgerr.UndefinedColumn
}

func (p *sqlProbe) nullOver(expr string, cols []string) (bool, error) {
	var null bool
	err := p.asked(func() error {
		return p.tx.QueryRowContext(p.ctx, "SELECT ("+expr+") IS NULL FROM "+p.keyRow(cols)).Scan(&null)
	}, nil)
	return null, err
}

// statementSeq numbers the statements the planner is asked about, so no two
// share a name in a session.
var statementSeq atomic.Int64

func (p *sqlProbe) planned(index dbschema.KeyIndex, key lintKey, filter string) ([]string, error) {
	var conds []string
	n := 0
	for _, col := range index.Columns {
		n++
		switch {
		case col.Column == "":
			conds = append(conds, fmt.Sprintf("(%s) = $%d", col.Expr, n))
		case key.nullable[col.Column]:
			// The tool looks a NULL up as NULL, which proves no
			// predicate on the column.
			conds = append(conds, fmt.Sprintf("%s IS NOT DISTINCT FROM $%d", sqlIdent(col.Column), n))
		default:
			conds = append(conds, fmt.Sprintf("%s = $%d", sqlIdent(col.Column), n))
		}
	}
	if filter != "" {
		conds = append(conds, "("+filter+"\n)")
	}
	name := "bfm_keylint_" + strconv.FormatInt(statementSeq.Add(1), 10)
	args := strings.TrimSuffix(strings.Repeat("NULL, ", n), ", ")
	prepared := false
	var plan string
	err := p.asked(func() error {
		// A generic plan keeps the parameters from being folded into
		// constants, and with sequential scans off the planner takes an
		// index wherever it may. Both settings go with the savepoint.
		if _, err := p.tx.ExecContext(p.ctx, "SELECT set_config('enable_seqscan', 'off', true), "+
			"set_config('plan_cache_mode', 'force_generic_plan', true)"); err != nil {
			return err
		}
		if _, err := p.tx.ExecContext(p.ctx, "PREPARE "+name+" AS SELECT 1 FROM "+sqlIdent(p.table.Schema)+"."+
			sqlIdent(p.table.Name)+" WHERE "+strings.Join(conds, " AND ")); err != nil {
			return err
		}
		prepared = true
		execute := "EXECUTE " + name
		if n > 0 {
			execute += "(" + args + ")"
		}
		return p.tx.QueryRowContext(p.ctx, "EXPLAIN (FORMAT JSON) "+execute).Scan(&plan)
	}, func() error {
		// A prepared statement belongs to the session, whatever becomes
		// of the transaction.
		if !prepared {
			return nil
		}
		_, err := p.tx.ExecContext(context.WithoutCancel(p.ctx), "DEALLOCATE "+name)
		return err
	})
	if err != nil {
		return nil, err
	}
	return planIndexes(plan)
}

// planIndexes are the indexes an EXPLAIN (FORMAT JSON) names.
func planIndexes(plan string) ([]string, error) {
	var doc any
	if err := json.Unmarshal([]byte(plan), &doc); err != nil {
		return nil, fmt.Errorf("read the plan: %w", err)
	}
	var used []string
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if name, ok := v["Index Name"].(string); ok && !contains(used, name) {
				used = append(used, name)
			}
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(v[k])
			}
		case []any:
			for _, e := range v {
				walk(e)
			}
		}
	}
	walk(doc)
	return used, nil
}

// reportStricter groups the fixture rows of a key by an index that backs it
// and is stricter than it, evaluating the index's columns over the rows'
// keys, and reports the rows it holds equal although their keys differ:
// UNIQUE (lower(email)) holds Ann@example.com and ann@example.com equal, so
// dbfixture cannot load both, and a migration inserting the second fails on
// the index. The keys are cast to their columns' types, in a VALUES list
// whose columns carry the key's names; nothing is written. A key column that
// is a reference holds the name of the row it points at here, not its id, so
// an index expression reading one is passed over.
func reportStricter(ctx context.Context, tx bun.Tx, probe *sqlProbe, model string, t *dbschema.Table, key lintKey,
	index dbschema.KeyIndex, snap *Snapshot) error {

	want := append([]string{}, key.cols...)
	sort.Strings(want)
	var entries []*Entry
	refs := map[string]bool{}
	for _, e := range snap.Entries[model] {
		if strings.Join(sortedColumns(e.Key), "\x00") != strings.Join(want, "\x00") {
			continue
		}
		entries = append(entries, e)
		for col, v := range e.Key {
			if v.Ref != nil {
				refs[col] = true
			}
		}
	}
	if len(entries) < 2 {
		return nil
	}
	var own []string
	for _, col := range key.cols {
		if !refs[col] {
			own = append(own, col)
		}
	}
	var groupBy, present []string
	for _, c := range index.Columns {
		item := sqlIdent(c.Column)
		if c.Column == "" {
			// bun reads a ? as a placeholder in a query with arguments.
			if strings.Contains(c.Expr, "?") {
				return nil
			}
			if len(refs) > 0 {
				ok, err := probe.readsOnly(c.Expr, own)
				if err != nil && !errors.As(err, &errCannotTell{}) {
					return err
				}
				if err != nil || !ok {
					return nil
				}
			}
			item = "(" + c.Expr + ")"
		}
		groupBy = append(groupBy, item)
		present = append(present, item+" IS NOT NULL")
	}
	names := []string{"bfm_i"}
	selects := []string{"v.bfm_i"}
	for i, col := range key.cols {
		names = append(names, fmt.Sprintf("c%d", i))
		column, ok := t.Column(col)
		switch {
		case refs[col]:
			selects = append(selects, fmt.Sprintf("v.c%d AS %s", i, sqlIdent(col)))
		case !ok || isJSON(column):
			return nil
		default:
			selects = append(selects, fmt.Sprintf("v.c%d::%s AS %s", i, castType(column), sqlIdent(col)))
		}
	}
	row := "(?::int" + strings.Repeat(", ?::text", len(key.cols)) + ")"
	args := make([]any, 0, len(entries)*(len(key.cols)+1))
	for i, e := range entries {
		args = append(args, i)
		for _, col := range key.cols {
			v := e.Key[col]
			switch {
			case v.IsNull:
				args = append(args, nil)
			case v.Ref != nil:
				args = append(args, v.Ref.Key)
			default:
				args = append(args, v.Lit)
			}
		}
	}
	query := "SELECT array_to_string(array_agg(bfm_i ORDER BY bfm_i), ',') FROM (SELECT " +
		strings.Join(selects, ", ") + " FROM (VALUES " + strings.TrimSuffix(strings.Repeat(row+",", len(entries)), ",") +
		") AS v(" + strings.Join(names, ", ") + ")) AS bfm_key"
	// An index that holds NULLs distinct holds no row with a NULL in it equal
	// to another.
	if !index.NullsNotDistinct {
		query += " WHERE " + strings.Join(present, " AND ")
	}
	query += " GROUP BY " + strings.Join(groupBy, ", ") + " HAVING count(*) > 1 ORDER BY min(bfm_i)"
	var groups [][]*Entry
	err := tx.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var list string
			if err := rows.Scan(&list); err != nil {
				return err
			}
			var group []*Entry
			for _, part := range strings.Split(list, ",") {
				i, err := strconv.Atoi(part)
				if err != nil || i < 0 || i >= len(entries) {
					return fmt.Errorf("unexpected group %q", list)
				}
				group = append(group, entries[i])
			}
			groups = append(groups, group)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return rows.Close()
	})
	if err != nil {
		// A value the column's type refuses is a finding of its own.
		if valueError(err) {
			return nil
		}
		return err
	}
	for _, group := range groups {
		keys := map[string]bool{}
		var shown, ids []string
		for _, e := range group {
			k := e.foldKey(model)
			if k == "" {
				k = e.KeyStr
			}
			if !keys[k] {
				keys[k] = true
				shown = append(shown, keyLabelOf(e.Key))
			}
			id := e.ID
			if id == "" {
				id = "(no id)"
			}
			ids = append(ids, id)
		}
		// One key, or keys its type holds equal, is what reportDuplicates
		// and reportEqualKeys say.
		if len(keys) < 2 {
			continue
		}
		snap.Findings = append(snap.Findings, Finding{
			Kind: FindingDuplicateKey, Model: model, Row: group[0].label(model),
			Detail: fmt.Sprintf("%s holds the natural keys %s equal, so dbfixture cannot load these %s (%s), and "+
				"a migration inserting one after the other fails on the index: make them differ as the index "+
				"compares them", index.Definition(), strings.Join(shown, " and "), plural(len(group), "row"),
				strings.Join(ids, ", ")),
		})
	}
	return nil
}

// indexAdvice is what to do about a natural key, cols, that more than one row
// of a table holds: name the index that should have kept the second row out
// and say why it did not, or, when the table has none, fallback ("give the
// table a unique index").
func indexAdvice(t *dbschema.Table, cols []string, fallback string) string {
	if t == nil {
		return fallback
	}
	inKey := map[string]bool{}
	for _, col := range cols {
		inKey[col] = true
	}
	var nullable, partial, invalid *dbschema.KeyIndex
	var nulls []string
	for i := range t.KeyIndexes {
		index := &t.KeyIndexes[i]
		if !index.Equality() || !index.Plain() || !plainWithin(*index, inKey) {
			continue
		}
		switch {
		case !index.Valid:
			if invalid == nil {
				invalid = index
			}
		case index.Predicate != "":
			if partial == nil {
				partial = index
			}
		case !index.NullsNotDistinct:
			var cs []string
			for _, c := range index.Columns {
				if column, ok := t.Column(c.Column); ok && column.Nullable {
					cs = append(cs, c.Column)
				}
			}
			if len(cs) > 0 && nullable == nil {
				nullable, nulls = index, cs
			}
		default:
			// A valid index over every row that holds NULLs equal keeps
			// any second row out; the rows are equal only to the tool.
			return fallback
		}
	}
	switch {
	case nullable != nil:
		return fmt.Sprintf("%s holds NULLs distinct, and lets in a second row with %s NULL: declare it NULLS NOT "+
			"DISTINCT (PostgreSQL 15 and later), or make %s NOT NULL", nullable.Definition(), orWords(nulls),
			andWords(nulls))
	case partial != nil:
		return fmt.Sprintf("%s holds only where %s, and lets in the rows outside it: %sgive the model a where "+
			"that implies it, or give the table a unique index over every row", partial.Definition(),
			trimPredicate(partial.Predicate), softDeleteRemedy(t, *partial))
	case invalid != nil:
		return fmt.Sprintf("unique index %s is invalid, left by a CREATE INDEX CONCURRENTLY that failed: delete "+
			"the duplicate rows, then REINDEX INDEX CONCURRENTLY %s", invalid.Name, sqlIndexName(t, invalid.Name))
	}
	return fallback
}

// keyAdvice ends the diff's refusal of a key two rows hold: ", and give the
// table a unique index", or what is wrong with the index it has, from
// whichever snapshot read the table.
func keyAdvice(old, next *Snapshot, model string, e *Entry) string {
	t := old.tables[model]
	if t == nil {
		t = next.tables[model]
	}
	const fallback = "give the table a unique index"
	if advice := indexAdvice(t, sortedColumns(e.Key), fallback); advice != fallback {
		return "; " + advice
	}
	return ", and " + fallback
}
