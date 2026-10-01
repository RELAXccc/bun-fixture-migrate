package fixtureapply

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

var identPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*$`)

// lockTimeoutPattern is a lock_timeout PostgreSQL reads as a duration. A
// bare number would be milliseconds, which is too easy to misread.
var lockTimeoutPattern = regexp.MustCompile(`^[0-9]+(ms|s|min|h|d)$`)

// quoteIdent double-quotes a plain, optionally schema-qualified identifier and
// rejects anything else. Generated files only ever contain names that came from
// the configuration, but this is the line between the file and the database and
// it is cheap to hold.
func quoteIdent(name string) (string, error) {
	parts := strings.Split(name, ".")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if !identPattern.MatchString(p) {
			return "", fmt.Errorf("%q is not a plain SQL identifier", name)
		}
		out = append(out, `"`+p+`"`)
	}
	return strings.Join(out, "."), nil
}

// Validate checks a change set without touching the database: known models,
// plain identifiers, a kind that exists, and the shape each kind needs. Apply
// and Revert call it first, and the generator's tests call it on their output.
func Validate(set fixturechange.Set) error {
	if err := set.Policy.Validate(); err != nil {
		return err
	}
	// A model nobody points at needs no key column, so an empty one is only an
	// error where a reference would use it.
	referenced := map[string]bool{}
	guardsID := map[string]bool{}
	for _, c := range set.Changes {
		if c.ID != "" {
			guardsID[c.Model] = true
		}
		for _, values := range []fixturechange.Values{c.Key, c.Old, c.New} {
			for _, v := range values {
				if v.Ref != nil {
					referenced[v.Ref.Model] = true
				}
			}
		}
	}
	for _, model := range sortedModels(set.Tables) {
		t := set.Tables[model]
		parts := []struct{ what, name string }{{"table", t.Name}}
		if t.Serial || referenced[model] || guardsID[model] {
			parts = append(parts, struct{ what, name string }{"id column", t.ID})
		}
		if referenced[model] {
			parts = append(parts, struct{ what, name string }{"key column", t.Key})
		}
		for _, part := range parts {
			if part.name == "" {
				return fmt.Errorf("model %q: no %s", model, part.what)
			}
			if _, err := quoteIdent(part.name); err != nil {
				return fmt.Errorf("model %q: %s %w", model, part.what, err)
			}
		}
	}
	if set.SeedGuardTable != "" {
		if _, err := quoteIdent(set.SeedGuardTable); err != nil {
			return fmt.Errorf("seed guard table %w", err)
		}
	}
	if set.MigrationsTable != "" {
		if _, err := quoteIdent(set.MigrationsTable); err != nil {
			return fmt.Errorf("migrations table %w", err)
		}
	}
	if set.LockTimeout != "" && !lockTimeoutPattern.MatchString(set.LockTimeout) {
		return fmt.Errorf("lock timeout %q is not a whole number with a unit PostgreSQL knows: ms, s, min, h or d, "+
			"as in 5s", set.LockTimeout)
	}
	for i, c := range set.Changes {
		if _, ok := set.Tables[c.Model]; !ok {
			return fmt.Errorf("change %d: unknown model %q", i, c.Model)
		}
		if len(c.Key) == 0 {
			return fmt.Errorf("change %d (%s): no key", i, c.Model)
		}
		for _, values := range []fixturechange.Values{c.Key, c.Old, c.New} {
			for _, col := range sortedColumns(values) {
				if _, err := quoteIdent(col); err != nil {
					return fmt.Errorf("change %d (%s): %w", i, c.Model, err)
				}
				v := values[col]
				if ref := v.Ref; ref != nil {
					if _, ok := set.Tables[ref.Model]; !ok {
						return fmt.Errorf("change %d (%s.%s): reference to unknown model %q", i, c.Model, col, ref.Model)
					}
				}
				// PostgreSQL's text cannot hold a NUL. bun v1.2.18 drops it from
				// a bound string without a word, so the database would hold
				// something other than the file; later versions refuse it.
				if strings.ContainsRune(v.Lit, 0) || (v.Ref != nil && strings.ContainsRune(v.Ref.Key, 0)) {
					return fmt.Errorf("change %d (%s.%s): the value holds a NUL character, which PostgreSQL "+
						"cannot store", i, c.Model, col)
				}
			}
		}
		switch c.Kind {
		case fixturechange.Insert:
			if len(c.New) == 0 {
				return fmt.Errorf("change %d (%s): insert without columns", i, c.Model)
			}
			if len(c.Old) != 0 {
				return fmt.Errorf("change %d (%s): insert with old values", i, c.Model)
			}
			if c.ID != "" {
				return fmt.Errorf("change %d (%s): insert with an id guard", i, c.Model)
			}
		case fixturechange.Update:
			if len(c.New) == 0 {
				return fmt.Errorf("change %d (%s): update without columns", i, c.Model)
			}
			// Every written column must carry the value it is replacing,
			// otherwise the statement would overwrite a row somebody else
			// changed.
			for _, col := range sortedColumns(c.New) {
				if _, ok := c.Old[col]; !ok {
					return fmt.Errorf("change %d (%s): update of %q without its old value", i, c.Model, col)
				}
			}
		case fixturechange.Delete:
			if len(c.New) != 0 {
				return fmt.Errorf("change %d (%s): delete with new values", i, c.Model)
			}
			// A delete guards on the whole row it is removing. Without that it
			// would take the natural key's word for it and remove a row
			// somebody had since edited into something else.
			if len(c.Old) == 0 {
				return fmt.Errorf("change %d (%s): delete without the row it removes", i, c.Model)
			}
		default:
			return fmt.Errorf("change %d (%s): unknown kind %q", i, c.Model, c.Kind)
		}
	}
	return nil
}
