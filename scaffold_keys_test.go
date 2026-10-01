package fixturemigrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
)

// keyedTables are the tables of the spec's scaffold repro (c6, c8): keys
// only a partial, an expression or an exclusion index gives, a nullable key
// column, and an invalid index.
func keyedTables() map[string]*dbschema.Table {
	pk := unique("pkey", "id")
	pk.Primary = true
	table := func(name string, cols []dbschema.Column, indexes ...dbschema.KeyIndex) *dbschema.Table {
		t := &dbschema.Table{Schema: "public", Name: name, PrimaryKey: []string{"id"},
			Columns:    append([]dbschema.Column{keyCol("id", "int8", false)}, cols...),
			KeyIndexes: append([]dbschema.KeyIndex{pk}, indexes...)}
		t.Uniques = [][]string{{"id"}}
		for _, k := range indexes {
			if k.Valid && k.Plain() && k.Predicate == "" && k.Equality() {
				var cols []string
				for _, c := range k.Columns {
					cols = append(cols, c.Column)
				}
				t.Uniques = append(t.Uniques, cols)
			}
		}
		return t
	}
	live := unique("regions_code_live", "code")
	live.Predicate = "(deleted_at IS NULL)"
	retired := unique("plans_code_not_retired", "code")
	retired.Predicate = "(status <> 'retired'::text)"
	invalid := unique("k_invalid_code", "code")
	invalid.Valid = false
	excl := dbschema.KeyIndex{Name: "k_excl_code_excl", Exclusion: true, Valid: true,
		Columns: []dbschema.IndexColumn{{Column: "code", Operator: "="}}}
	lower := withReads(expr("admins_email_lower", "lower(email)"), "email")
	nnd := unique("k_nnd_key", "parent_id", "code")
	nnd.NullsNotDistinct = true
	tables := map[string]*dbschema.Table{}
	for _, t := range []*dbschema.Table{
		table("regions", []dbschema.Column{keyCol("code", "text", false),
			keyCol("deleted_at", "timestamptz", true)}, live),
		table("plans", []dbschema.Column{keyCol("code", "text", false), keyCol("status", "text", false)}, retired),
		table("admins", []dbschema.Column{keyCol("email", "text", false)}, lower),
		table("k_excl", []dbschema.Column{keyCol("code", "text", false)}, excl),
		table("categories", []dbschema.Column{keyCol("parent_id", "int8", true), keyCol("code", "text", false)},
			unique("categories_parent_id_code_key", "parent_id", "code")),
		table("k_nnd", []dbschema.Column{keyCol("parent_id", "int8", true), keyCol("code", "text", false)}, nnd),
		table("k_invalid", []dbschema.Column{keyCol("code", "text", false)}, invalid),
		// A plain index beats the partial one, and a narrower one a wider.
		table("tags", []dbschema.Column{keyCol("code", "text", false), keyCol("plan_id", "int8", false),
			keyCol("deleted_at", "timestamptz", true)}, live, unique("tags_code_plan_id_key", "code", "plan_id")),
	} {
		tables["public."+t.Name] = t
	}
	return tables
}

func TestScaffoldGuessesKeysFromEveryKindOfIndex(t *testing.T) {
	text := string(Scaffold(keyedTables(), nil, "public", ScaffoldOptions{}))
	block := func(model string) string {
		i := strings.Index(text, "\n  "+model+":\n")
		if i < 0 {
			i = strings.Index(text, "\n  # "+model+":\n")
		}
		if i < 0 {
			t.Fatalf("no %s in:\n%s", model, text)
		}
		rest := text[i+1:]
		if j := strings.Index(rest, "\n\n"); j >= 0 {
			rest = rest[:j]
		}
		// The words of the block, comment marks and line breaks taken out.
		var words []string
		for _, line := range strings.Split(rest, "\n") {
			words = append(words, strings.Fields(strings.TrimLeft(line, " #"))...)
		}
		return strings.Join(words, " ")
	}
	for model, wants := range map[string][]string{
		// A live-rows index is a table bun soft-deletes, which soft_delete
		// says rather than a where.
		"Region": {"GUESS: taken from UNIQUE (code) WHERE deleted_at IS NULL, which holds only where deleted_at IS " +
			"NULL: the live rows of a table bun soft-deletes, which the soft_delete below says.",
			"key: [code]", "GUESS: UNIQUE (code) WHERE deleted_at IS NULL holds only where deleted_at IS NULL, which " +
				"is how a table keeps the rows bun's soft delete deleted", "soft_delete: deleted_at"},
		"Plan": {"GUESS: taken from UNIQUE (code) WHERE status <> 'retired'::text",
			"key: [code] where: status <> 'retired'::text"},
		"Admin": {"GUESS: UNIQUE (lower(email)) backs key [email] and is stricter: values lower(email) maps to one " +
			"value collide", "key: [email]"},
		"KExcl": {"GUESS: taken from EXCLUDE (code WITH =), an exclusion constraint", "key: [code]"},
		"Category": {"GUESS: parent_id is nullable, and UNIQUE (parent_id, code) holds NULLs distinct", "Declare it " +
			"NULLS NOT DISTINCT (PostgreSQL 15 and later), index COALESCE(parent_id, ...) in its place, or make " +
			"parent_id NOT NULL.", "key: [parent_id, code]"},
		"KNnd": {"key: [parent_id, code]"},
		"KInvalid": {"GUESS: unique index k_invalid_code is invalid, left by a CREATE INDEX CONCURRENTLY that failed, so no " +
			"key is taken from it", "key: [name]"},
		"Tag": {"key: [code, plan_id]"},
	} {
		got := block(model)
		for _, want := range wants {
			if !strings.Contains(got, want) {
				t.Errorf("%s: missing %q in:\n%s", model, want, got)
			}
		}
	}
	for _, model := range []string{"KNnd", "Tag"} {
		if strings.Contains(block(model), "GUESS: taken from") || strings.Contains(block(model), "nullable") {
			t.Errorf("%s is keyed on a plain unique index and needs no word:\n%s", model, block(model))
		}
	}
	if !strings.Contains(text, "\n  # public.k_invalid has no valid unique index besides its primary key "+
		"(k_invalid_code is invalid) and no name column, so\n") {
		t.Errorf("the headline over k_invalid:\n%s", text)
	}
	if !strings.Contains(text, "\n  key_index: error\n") {
		t.Errorf("a new configuration is strict about unbacked keys:\n%s", text)
	}

	path := filepath.Join(t.TempDir(), "fixture-migrate.yml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("the scaffold has to load: %v", err)
	}
	if cfg.Policy.KeyIndex != ModeError || cfg.Models["Region"].Where != "" {
		t.Fatalf("policy %+v, Region %+v", cfg.Policy, cfg.Models["Region"])
	}
	if _, ok := cfg.Models["KInvalid"]; ok {
		t.Fatal("a table keyed only by an invalid index is no model")
	}
}

func TestWrapComment(t *testing.T) {
	got := wrapComment("GUESS: "+strings.Repeat("word ", 30)+"end", "    # ")
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if len(line) > 80 || !strings.HasPrefix(line, "    # ") {
			t.Fatalf("line %q of:\n%s", line, got)
		}
	}
	if !strings.HasSuffix(got, "end\n") {
		t.Fatalf("%q", got)
	}
}
