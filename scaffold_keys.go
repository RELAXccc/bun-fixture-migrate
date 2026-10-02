package fixturemigrate

import (
	"fmt"
	"strings"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
)

// keyGuess is the natural key Scaffold proposes for a table, with what it
// took it from and what a person has to check about it.
type keyGuess struct {
	// key is the guessed key, nil when nothing gives one.
	key []string
	// comment are the lines that say what the key was taken from when that
	// was not a plain unique index over columns that are never NULL, each a
	// GUESS to check.
	comment []string
	// where is the predicate of the partial index the key was taken from,
	// which the configuration proposes, commented out, as the model's where.
	where string
	// invalid names a unique index the guess passed over because it is
	// invalid, "" when there is none.
	invalid string
}

// Kinds of index a key is guessed from, the most trusted first.
const (
	// guessPlain: a unique index over columns that are NOT NULL or held
	// NULLS NOT DISTINCT.
	guessPlain = iota
	// guessLive: a partial unique index over the rows where one column is
	// NULL, as a live-rows index of a soft-delete table is.
	guessLive
	// guessExclusion: an exclusion constraint whose every operator is =.
	guessExclusion
	// guessExpression: a unique index over expressions.
	guessExpression
	// guessNullable: a unique index that holds NULLs distinct in a nullable
	// column.
	guessNullable
	// guessPartial: a partial unique index over any other predicate.
	guessPartial
)

// guessKeyFrom guesses a table's natural key from its unique indexes and
// exclusion constraints, the primary key and any index over the id left out,
// and an invalid index never used: in order of preference, the narrowest
// first among each,
//
//  1. a unique index over columns that are NOT NULL or NULLS NOT DISTINCT;
//  2. a partial one over the rows where a column is NULL, a live-rows index;
//  3. an exclusion constraint whose every operator is =;
//  4. a unique index over expressions, keyed by the columns they read;
//  5. a unique index with a nullable column it holds NULLs distinct in;
//  6. a partial one over any other predicate.
//
// Everything but the first is a GUESS, which the comment says.
func guessKeyFrom(t *dbschema.Table, id string) keyGuess {
	if len(t.KeyIndexes) == 0 {
		return keyGuess{key: guessKeyFromUniques(t, id)}
	}
	var g keyGuess
	best, bestKind := (*dbschema.KeyIndex)(nil), 0
	var bestKey []string
	for i := range t.KeyIndexes {
		index := &t.KeyIndexes[i]
		if !index.Equality() || readsColumn(*index, id) {
			continue
		}
		if !index.Valid {
			if g.invalid == "" {
				g.invalid = index.Name
			}
			continue
		}
		kind, key := guessKind(t, *index)
		if key == nil {
			continue
		}
		if best == nil || kind < bestKind || kind == bestKind && len(key) < len(bestKey) {
			best, bestKind, bestKey = index, kind, key
		}
	}
	if g.invalid != "" {
		g.comment = append(g.comment, fmt.Sprintf("GUESS: unique index %s is invalid, left by a CREATE INDEX "+
			"CONCURRENTLY that failed, so no key is taken from it: it refuses nothing, and the duplicates it "+
			"failed on are still there. Delete them, then REINDEX INDEX CONCURRENTLY %s.", g.invalid, g.invalid))
	}
	if best == nil {
		return g
	}
	g.key = bestKey
	def := best.Definition()
	nulls := nullsDistinct(t, *best)
	switch col := liveRowsPredicate(t, best.Predicate); {
	case bestKind == guessLive && softDeleteColumn(t, col):
		// The live rows of a table bun soft-deletes, which soft_delete
		// says, as scaffoldSoftDelete proposes.
		g.comment = append(g.comment, fmt.Sprintf("GUESS: taken from %s, which holds only where %s: the live "+
			"rows of a table bun soft-deletes, which the soft_delete below says.", def,
			trimPredicate(best.Predicate)))
	case bestKind == guessLive || bestKind == guessPartial:
		pred := trimPredicate(best.Predicate)
		g.where = pred
		g.comment = append(g.comment, fmt.Sprintf("GUESS: taken from %s, which holds only where %s. If those "+
			"are the master rows, uncomment the where below; without it the key lint reports the key "+
			"unbacked, since the rows outside it may share a key.", def, pred))
	case bestKind == guessExclusion:
		g.comment = append(g.comment, fmt.Sprintf("GUESS: taken from %s, an exclusion constraint that refuses "+
			"two rows equal in these columns, as a unique index does.", def))
	case bestKind == guessExpression:
		var exprs []string
		for _, c := range best.Columns {
			if c.Column == "" {
				exprs = append(exprs, c.Expr)
			}
		}
		g.comment = append(g.comment, fmt.Sprintf("GUESS: %s backs key [%s] and is stricter: values %s maps to "+
			"one value collide, so two rows the key tells apart can still be refused by it.", def,
			strings.Join(bestKey, ", "), strings.Join(exprs, " and ")))
	}
	if len(nulls) > 0 {
		g.comment = append(g.comment, fmt.Sprintf("GUESS: %s nullable, and %s holds NULLs distinct, so it lets "+
			"any number of rows hold the same key with %s NULL, which no lookup by the key tells apart. Declare it "+
			"NULLS NOT DISTINCT (PostgreSQL 15 and later), index COALESCE(%s, ...) in its place, or make %s "+
			"NOT NULL.", isAre(nulls), def, orWords(nulls), nulls[0], andWords(nulls)))
	}
	return g
}

// guessKind is which of guessKeyFrom's kinds an index is, and the key it
// gives: its columns, or for an index over expressions its plain columns and
// the columns its expressions read. nil for an index that gives none, a
// partial one over expressions, whose predicate's columns pg_depend lists
// among those its expressions read.
func guessKind(t *dbschema.Table, index dbschema.KeyIndex) (int, []string) {
	var key []string
	for _, c := range index.Columns {
		switch {
		case c.Column != "":
			if !contains(key, c.Column) {
				key = append(key, c.Column)
			}
		case index.Predicate != "":
			return 0, nil
		default:
			// The columns an expression reads, in the order pg_depend
			// lists them, at its place in the index.
			named := columnsNamed(t, c.Expr)
			for _, col := range index.Reads {
				if named[col] && !contains(key, col) {
					key = append(key, col)
				}
			}
		}
	}
	if !index.Plain() {
		for _, col := range index.Reads {
			if !contains(key, col) {
				key = append(key, col)
			}
		}
		return guessExpression, key
	}
	nullable := len(nullsDistinct(t, index)) > 0
	switch {
	case index.Predicate != "" && liveRowsPredicate(t, index.Predicate) != "":
		return guessLive, key
	case index.Predicate != "":
		return guessPartial, key
	case nullable:
		return guessNullable, key
	case index.Exclusion && !index.Unique:
		return guessExclusion, key
	}
	return guessPlain, key
}

// liveRowsPredicate is the column of a predicate "(<col> IS NULL)" over a
// nullable timestamp column, the live rows of a table that soft-deletes
// them; "" for any other predicate.
func liveRowsPredicate(t *dbschema.Table, pred string) string {
	conj := conjuncts(pred)
	if len(conj) != 1 || !strings.HasSuffix(conj[0], " is null") {
		return ""
	}
	name := strings.TrimSuffix(conj[0], " is null")
	c, ok := t.Column(name)
	if !ok || !c.Nullable || c.Type != "timestamptz" && c.Type != "timestamp" {
		return ""
	}
	return name
}

// nullsDistinct are the nullable plain columns of an index that holds NULLs
// distinct.
func nullsDistinct(t *dbschema.Table, index dbschema.KeyIndex) []string {
	if index.NullsNotDistinct {
		return nil
	}
	var out []string
	for _, c := range index.Columns {
		if column, ok := t.Column(c.Column); ok && c.Column != "" && column.Nullable {
			out = append(out, c.Column)
		}
	}
	return out
}

// readsColumn reports whether an index has col among its columns, or reads
// it in an expression.
func readsColumn(index dbschema.KeyIndex, col string) bool {
	for _, c := range index.Columns {
		if c.Column == col {
			return true
		}
	}
	return !index.Plain() && contains(index.Reads, col)
}

func isAre(cols []string) string {
	if len(cols) == 1 {
		return cols[0] + " is"
	}
	return andWords(cols) + " are"
}

// guessKeyFromUniques is the narrowest unique index that is not the primary
// key and does not contain the id, which is very often exactly the natural
// key: the guess for a table read without its KeyIndexes.
func guessKeyFromUniques(t *dbschema.Table, id string) []string {
	var best []string
	for _, cols := range t.Uniques {
		if contains(cols, id) || len(cols) == 0 {
			continue
		}
		if best == nil || len(cols) < len(best) {
			best = cols
		}
	}
	return best
}

// noKeyHeadline is the first line of the comment over a table Scaffold
// writes commented out, as it has nothing to key it on.
func noKeyHeadline(t *dbschema.Table) string {
	if g := guessKeyFrom(t, scaffoldID(t)); g.invalid != "" {
		return fmt.Sprintf("%s has no valid unique index besides its primary key (%s is invalid) and no "+
			"name column, so", t.Qualified(), g.invalid)
	}
	return t.Qualified() + " has no unique index besides its primary key and no name column, so"
}

// wrapComment writes text as YAML comment lines of at most 80 columns, each
// starting with prefix, "    # ". A word longer than a line, an index's
// predicate say, gets a line of its own.
func wrapComment(text, prefix string) string {
	var b strings.Builder
	line := prefix
	for _, word := range strings.Fields(strings.Join(commentLines(text), " ")) {
		if line != prefix && len(line)+1+len(word) > 80 {
			b.WriteString(line + "\n")
			line = prefix
		}
		if line != prefix {
			line += " "
		}
		line += word
	}
	if line != prefix {
		b.WriteString(line + "\n")
	}
	return b.String()
}
