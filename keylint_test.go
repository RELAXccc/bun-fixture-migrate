package fixturemigrate

import (
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
)

// fakeProbe answers the lint's questions the way PostgreSQL would for the
// expressions and indexes of a test: an expression reads the columns reads
// gives it, is NULL over NULLs where nulls says, and the planner uses the
// indexes plans gives for an index under a filter.
type fakeProbe struct {
	reads map[string][]string
	nulls map[string]bool
	plans map[string][]string // index name + "|" + filter -> indexes used
	fail  map[string]bool     // expressions PostgreSQL cannot evaluate
	// nullFail are expressions PostgreSQL cannot evaluate over NULLs.
	nullFail map[string]bool
	asked    []string
}

func (p *fakeProbe) readsOnly(expr string, cols []string) (bool, error) {
	if p.fail[expr] {
		return false, errCannotTell{msg: "function norm(text) does not exist", state: "42883"}
	}
	for _, col := range p.reads[expr] {
		if !contains(cols, col) {
			return false, nil
		}
	}
	return true, nil
}

func (p *fakeProbe) nullOver(expr string, cols []string) (bool, error) {
	if p.nullFail[expr] {
		return false, errCannotTell{msg: "division by zero", state: "22012"}
	}
	return p.nulls[expr], nil
}

func (p *fakeProbe) planned(index dbschema.KeyIndex, key lintKey, filter string) ([]string, error) {
	p.asked = append(p.asked, index.Name)
	return p.plans[index.Name+"|"+filter], nil
}

func keyCol(name, typ string, nullable bool) dbschema.Column {
	return dbschema.Column{Name: name, Type: typ, FullType: typ, Nullable: nullable}
}

func unique(name string, cols ...string) dbschema.KeyIndex {
	k := dbschema.KeyIndex{Name: name, Unique: true, Valid: true}
	for _, c := range cols {
		k.Columns = append(k.Columns, dbschema.IndexColumn{Column: c})
	}
	return k
}

func expr(name string, exprs ...string) dbschema.KeyIndex {
	k := dbschema.KeyIndex{Name: name, Unique: true, Valid: true}
	for _, e := range exprs {
		if strings.Contains(e, "(") {
			k.Columns = append(k.Columns, dbschema.IndexColumn{Expr: e})
		} else {
			k.Columns = append(k.Columns, dbschema.IndexColumn{Column: e})
		}
	}
	return k
}

// withReads gives an index the columns pg_depend says it reads.
func withReads(k dbschema.KeyIndex, cols ...string) dbschema.KeyIndex {
	k.Reads = cols
	return k
}

// keyTable is a table of every case: id, the key columns, a nullable
// parent_id and email, a soft-delete column and a status.
func keyTable(indexes ...dbschema.KeyIndex) *dbschema.Table {
	pk := unique("t_pkey", "id")
	pk.Primary = true
	return &dbschema.Table{Schema: "public", Name: "t", Columns: []dbschema.Column{
		keyCol("id", "int8", false), keyCol("code", "text", false), keyCol("plan_id", "int8", false),
		keyCol("parent_id", "int8", true), keyCol("email", "text", true), keyCol("login", "text", false),
		keyCol("deleted_at", "timestamptz", true), keyCol("status", "text", false), keyCol("during", "tstzrange", false),
		keyCol("tenant_id", "int8", true),
	}, KeyIndexes: append([]dbschema.KeyIndex{pk}, indexes...)}
}

func verdict(t *testing.T, table *dbschema.Table, cols []string, filter string, version int,
	p *fakeProbe) KeyVerdict {

	t.Helper()
	if p == nil {
		p = &fakeProbe{}
	}
	key := newLintKey(table, cols, nil, "key ["+strings.Join(cols, ", ")+"]")
	v, err := judgeKey(table, key, filter, version, p)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Every row of the spec's verdict table, against the catalog's structs and
// a probe answering as PostgreSQL did in the repros.
func TestTheVerdictOnAKey(t *testing.T) {
	exprs := &fakeProbe{
		reads: map[string][]string{
			"lower(email)": {"email"}, "lower(login)": {"login"}, "COALESCE(parent_id, 0)": {"parent_id"},
			"(code || plan_id::text)": {"code", "plan_id"},
		},
		nulls: map[string]bool{"lower(email)": true, "lower(login)": true},
	}
	deferrable := unique("t_code_key", "code")
	deferrable.Deferrable = true
	exclusion := dbschema.KeyIndex{Name: "t_code_excl", Exclusion: true, Valid: true,
		Columns: []dbschema.IndexColumn{{Column: "code", Operator: "="}}}
	ranged := dbschema.KeyIndex{Name: "t_code_during_excl", Exclusion: true, Valid: true,
		Columns: []dbschema.IndexColumn{{Column: "code", Operator: "="}, {Column: "during", Operator: "&&"}}}
	invalid := unique("t_code_idx", "code")
	invalid.Valid = false
	nnd := unique("t_parent_id_code_key", "parent_id", "code")
	nnd.NullsNotDistinct = true

	for _, tc := range []struct {
		what     string
		indexes  []dbschema.KeyIndex
		key      []string
		backed   bool
		index    string
		stricter bool
		detail   string
	}{
		{"exactly the key", []dbschema.KeyIndex{unique("t_plan_id_code_key", "plan_id", "code")},
			[]string{"code", "plan_id"}, true, "t_plan_id_code_key", false, ""},
		{"over a part of the key, stricter", []dbschema.KeyIndex{unique("t_code_key", "code")},
			[]string{"plan_id", "code"}, true, "t_code_key", true, ""},
		{"over an expression of the key, stricter", []dbschema.KeyIndex{expr("t_lower", "lower(login)")},
			[]string{"login"}, true, "t_lower", true, ""},
		{"over more columns than the key", []dbschema.KeyIndex{unique("t_code_plan_id_key", "code", "plan_id")},
			[]string{"code"}, false, "t_code_plan_id_key", false,
			"UNIQUE (code, plan_id) is over more columns than key [code], so two rows may share code: key on " +
				"[code, plan_id], or CREATE UNIQUE INDEX ON t (code)"},
		{"no index but the primary key", nil, []string{"plan_id", "code"}, false, "", false,
			"no unique index or constraint backs key [plan_id, code]: the database lets a second row with this " +
				"key in, and every change to it then fails as a duplicate key. CREATE UNIQUE INDEX ON t (plan_id, code)"},
		{"a nullable column, NULLs distinct", []dbschema.KeyIndex{unique("t_parent_id_code_key", "parent_id", "code")},
			[]string{"parent_id", "code"}, false, "t_parent_id_code_key", false,
			"parent_id is nullable and UNIQUE (parent_id, code) holds NULLs distinct, so any number of rows may " +
				"hold the same code with parent_id NULL: declare it NULLS NOT DISTINCT (PostgreSQL 15 and later), " +
				"or make parent_id NOT NULL"},
		{"NULLS NOT DISTINCT", []dbschema.KeyIndex{nnd}, []string{"parent_id", "code"}, true,
			"t_parent_id_code_key", false, ""},
		{"COALESCE maps NULL to a value", []dbschema.KeyIndex{expr("t_coal", "COALESCE(parent_id, 0)", "code")},
			[]string{"parent_id", "code"}, true, "t_coal", true, ""},
		{"an expression NULL lets through", []dbschema.KeyIndex{expr("t_lower_email", "lower(email)")},
			[]string{"email"}, false, "t_lower_email", false,
			"lower(email) is NULL where email is, and UNIQUE (lower(email)) holds NULLs distinct, so any number of " +
				"rows may hold the same key with email NULL: declare it NULLS NOT DISTINCT (PostgreSQL 15 and later), " +
				"or make email NOT NULL"},
		{"an expression reading a column outside the key",
			[]dbschema.KeyIndex{withReads(expr("t_cat", "(code || plan_id::text)"), "code", "plan_id")},
			[]string{"code"}, false, "t_cat", false,
			"UNIQUE ((code || plan_id::text)) reads columns outside key [code], so two rows may share code: " +
				"CREATE UNIQUE INDEX ON t (code)"},
		{"a deferrable constraint", []dbschema.KeyIndex{deferrable}, []string{"code"}, true, "t_code_key", false, ""},
		{"an exclusion constraint WITH =", []dbschema.KeyIndex{exclusion}, []string{"code"}, true, "t_code_excl",
			false, ""},
		{"an exclusion constraint with another operator", []dbschema.KeyIndex{ranged}, []string{"code"}, false, "",
			false, "no unique index or constraint backs key [code]"},
		{"an invalid index", []dbschema.KeyIndex{invalid}, []string{"code"}, false, "t_code_idx", false,
			"unique index t_code_idx is invalid, left by a CREATE INDEX CONCURRENTLY that failed, and backs " +
				"nothing: delete the duplicate rows it failed on, then REINDEX INDEX CONCURRENTLY t_code_idx"},
		{"an invalid index beside a valid one", []dbschema.KeyIndex{invalid, unique("t_code_key", "code")},
			[]string{"code"}, true, "t_code_key", false, ""},
		{"the primary key", nil, []string{"id"}, true, "t_pkey", false, ""},
	} {
		t.Run(tc.what, func(t *testing.T) {
			v := verdict(t, keyTable(tc.indexes...), tc.key, "", 160013, exprs)
			if v.Backed != tc.backed || v.Index != tc.index || v.Unsure {
				t.Fatalf("backed %v by %q unsure %v, want backed %v by %q:\n%s", v.Backed, v.Index, v.Unsure,
					tc.backed, tc.index, v.Detail)
			}
			if (len(v.stricter) > 0) != tc.stricter {
				t.Fatalf("stricter %v, want %v", v.stricter, tc.stricter)
			}
			if !strings.HasPrefix(v.Detail, tc.detail) || tc.detail == "" && v.Detail != "" {
				t.Fatalf("detail:\n%s\nwant:\n%s", v.Detail, tc.detail)
			}
		})
	}
}

// Before PostgreSQL 15 there is no NULLS NOT DISTINCT, and the remedy is an
// index over COALESCE, in a new index too.
func TestTheNullRemedyFollowsTheServer(t *testing.T) {
	table := keyTable(unique("t_parent_id_code_key", "parent_id", "code"))
	v := verdict(t, table, []string{"parent_id", "code"}, "", 140011, nil)
	if !strings.HasSuffix(v.Detail, "index (COALESCE(parent_id, 0), code) in its place, or make parent_id NOT NULL") {
		t.Fatalf("PostgreSQL 14:\n%s", v.Detail)
	}
	v = verdict(t, keyTable(), []string{"parent_id", "code"}, "", 140011, nil)
	if !strings.HasSuffix(v.Detail, "CREATE UNIQUE INDEX ON t (COALESCE(parent_id, 0), code)") {
		t.Fatalf("PostgreSQL 14, no index:\n%s", v.Detail)
	}
	v = verdict(t, keyTable(), []string{"parent_id", "code"}, RowFilter("status = 'live'", ""), 170000, nil)
	if !strings.HasSuffix(v.Detail, "CREATE UNIQUE INDEX ON t (parent_id, code) NULLS NOT DISTINCT WHERE "+
		"status = 'live'") {
		t.Fatalf("PostgreSQL 17, a where:\n%s", v.Detail)
	}
	// An exclusion constraint has no NULLS NOT DISTINCT.
	excl := dbschema.KeyIndex{Name: "t_excl", Exclusion: true, Valid: true,
		Columns: []dbschema.IndexColumn{{Column: "parent_id", Operator: "="}, {Column: "code", Operator: "="}}}
	v = verdict(t, keyTable(excl), []string{"parent_id", "code"}, "", 160000, nil)
	if !strings.Contains(v.Detail, "EXCLUDE (parent_id WITH =, code WITH =) holds NULLs distinct") ||
		!strings.HasSuffix(v.Detail, "make parent_id NOT NULL, or back the key with a unique index NULLS NOT "+
			"DISTINCT in its place") {
		t.Fatalf("exclusion constraint:\n%s", v.Detail)
	}
}

// A partial index backs a key when the model's filter implies its
// predicate: as written for a soft-delete filter, by the planner otherwise,
// and never when the filter does not name the predicate's columns.
func TestAPartialIndex(t *testing.T) {
	live := unique("t_live", "code")
	live.Predicate = "(deleted_at IS NULL)"
	notRetired := unique("t_not_retired", "tenant_id", "code")
	notRetired.Predicate = "(status <> 'retired'::text)"
	nnRetired := notRetired
	nnRetired.NullsNotDistinct = true

	soft := RowFilter("", "deleted_at")
	p := &fakeProbe{}
	if v := verdict(t, keyTable(live), []string{"code"}, soft, 160000, p); !v.Backed || len(p.asked) > 0 {
		t.Fatalf("a soft-delete filter under a live-rows index: %+v, planner asked %v", v, p.asked)
	}
	// Spelled another way, with the where around it.
	if v := verdict(t, keyTable(live), []string{"code"}, RowFilter("Deleted_At is null and code <> ''", ""), 160000,
		p); !v.Backed || len(p.asked) > 0 {
		t.Fatalf("a where repeating the predicate: %+v, planner asked %v", v, p.asked)
	}

	// No filter: the predicate names deleted_at, which nothing in the lookup
	// does, so no planner is asked.
	v := verdict(t, keyTable(live), []string{"code"}, "", 160000, p)
	if v.Backed || len(p.asked) > 0 || !strings.HasPrefix(v.Detail, "UNIQUE (code) WHERE deleted_at IS NULL holds "+
		"only where deleted_at IS NULL, and this model reads every row: give the model a where that implies it") {
		t.Fatalf("no filter: %+v, planner asked %v", v, p.asked)
	}

	active := RowFilter("status = 'active'", "")
	p = &fakeProbe{plans: map[string][]string{"t_not_retired|" + active: {"t_not_retired"}}}
	if v := verdict(t, keyTable(nnRetired), []string{"tenant_id", "code"}, active, 160000, p); !v.Backed ||
		len(p.asked) != 1 {
		t.Fatalf("the planner says status = 'active' implies status <> 'retired': %+v", v)
	}
	archived := RowFilter("status <> 'archived'", "")
	p = &fakeProbe{plans: map[string][]string{}}
	v = verdict(t, keyTable(nnRetired), []string{"tenant_id", "code"}, archived, 160000, p)
	if v.Backed || v.Unsure || !strings.Contains(v.Detail, "the rows this model reads, where status <> "+
		"'archived', are not all within it") {
		t.Fatalf("the planner uses no index: %+v", v)
	}
	// The planner chose another index: nothing says whether this one would
	// have done, and the verdict says so.
	p = &fakeProbe{plans: map[string][]string{"t_not_retired|" + active: {"t_tenant_code_idx"}}}
	v = verdict(t, keyTable(nnRetired), []string{"tenant_id", "code"}, active, 160000, p)
	if v.Backed || !v.Unsure || !strings.HasPrefix(v.Detail, "cannot tell whether UNIQUE NULLS NOT DISTINCT "+
		"(tenant_id, code) WHERE status <> 'retired'::text backs key [tenant_id, code]: the planner chose "+
		"t_tenant_code_idx for the lookup instead") {
		t.Fatalf("the planner chose another index: %+v", v)
	}
	// Failing on NULLs and on its predicate, it says both.
	v = verdict(t, keyTable(notRetired), []string{"tenant_id", "code"}, archived, 160000,
		&fakeProbe{plans: map[string][]string{}})
	if !strings.Contains(v.Detail, "tenant_id is nullable") || !strings.HasSuffix(v.Detail, "It also holds only "+
		"where status <> 'retired'::text, and the rows this model reads, where status <> 'archived', are not all "+
		"within it") {
		t.Fatalf("both: %s", v.Detail)
	}
}

// A predicate the model's rows are not all within settles the verdict,
// whatever else could not be evaluated.
func TestAPartialIndexFailsWhateverIsUndecided(t *testing.T) {
	live := expr("t_live", "norm(email)")
	live.Predicate = "(deleted_at IS NULL)"
	p := &fakeProbe{reads: map[string][]string{"norm(email)": {"email"}},
		nullFail: map[string]bool{"norm(email)": true}}
	v := verdict(t, keyTable(withReads(live, "email", "deleted_at")), []string{"email"}, "", 160000, p)
	if v.Backed || v.Unsure || !strings.HasPrefix(v.Detail, "UNIQUE (norm(email)) WHERE deleted_at IS NULL holds "+
		"only where") {
		t.Fatalf("%+v", v)
	}
}

// A key_any_of key is each combination of the key and one column of each
// group, and the column a group adds is never NULL: col = $n in the lookup.
func TestTheKeysOfAKeyAnyOfModel(t *testing.T) {
	table := keyTable()
	m := &Model{Key: []string{"plan_id"}, KeyAnyOf: [][]string{{"parent_id", "tenant_id"}}}
	keys := effectiveKeys(m, table)
	if len(keys) != 2 || keys[0].label != "key [plan_id, parent_id]" || keys[1].label != "key [plan_id, tenant_id]" {
		t.Fatalf("keys: %+v", keys)
	}
	for _, k := range keys {
		if len(k.nullable) != 0 {
			t.Fatalf("%s: nullable %v", k.label, k.nullable)
		}
	}
	// A column the table does not have is left to checkModels.
	if keys := effectiveKeys(&Model{Key: []string{"nope"}}, table); len(keys) != 0 {
		t.Fatalf("a missing column: %+v", keys)
	}
}

// What PostgreSQL could not evaluate leaves the key undecided, which the
// policy makes a warning at most.
func TestAnExpressionPostgreSQLCannotEvaluate(t *testing.T) {
	p := &fakeProbe{fail: map[string]bool{"norm(code)": true}}
	v := verdict(t, keyTable(expr("t_norm", "norm(code)")), []string{"code"}, "", 160000, p)
	if v.Backed || !v.Unsure || !strings.Contains(v.Detail, "PostgreSQL could not evaluate norm(code) over the "+
		"key's columns: function norm(text) does not exist") {
		t.Fatalf("%+v", v)
	}
	cfg := &Config{Models: map[string]*Model{"T": {Table: "t"}}, Policy: Policy{KeyIndex: ModeError}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	f := Finding{Kind: FindingUnbackedKey, Model: "T", Row: "key [code]", Detail: v.Detail, unsure: true}
	if got := cfg.ModeOf(f); got != ModeWarn {
		t.Fatalf("an undecided key under key_index: error is %s", got)
	}
	f.unsure = false
	if got := cfg.ModeOf(f); got != ModeError {
		t.Fatalf("an unbacked key under key_index: error is %s", got)
	}
}

func TestPredicatesAsWritten(t *testing.T) {
	for _, tc := range []struct {
		filter, pred string
		want         bool
	}{
		{`"deleted_at" IS NULL`, "(deleted_at IS NULL)", true},
		{"(tenant_id is null) AND \"deleted_at\" IS NULL", "((deleted_at IS NULL) AND (tenant_id IS NULL))", true},
		{"deleted_at IS NULL", "((deleted_at IS NULL) AND (tenant_id IS NULL))", false},
		{"status <> 'retired'", "(status <> 'retired'::text)", true},
		{"status <> 'Retired'", "(status <> 'retired'::text)", false},
		{"kind = 'a and b'", "(kind = 'a and b'::text)", true},
		{"(kind = 'x' or kind = 'y')", "((kind = 'x'::text) OR (kind = 'y'::text))", false},
		{"", "(deleted_at IS NULL)", false},
		{"x::character varying = 'a'", "((x)::character varying = 'a'::character varying)", false},
	} {
		if got := impliedAsWritten(tc.filter, tc.pred); got != tc.want {
			t.Errorf("%q implies %q: %v, want %v (%q, %q)", tc.filter, tc.pred, got, tc.want,
				conjuncts(tc.filter), conjuncts(tc.pred))
		}
	}
}

func TestRowFilter(t *testing.T) {
	for _, tc := range []struct{ where, soft, want string }{
		{"", "", ""},
		{"tenant_id IS NULL", "", "(tenant_id IS NULL)"},
		{"", "deleted_at", `"deleted_at" IS NULL`},
		{"kind = 'a' -- master", "deleted_at", "(kind = 'a' -- master\n) AND \"deleted_at\" IS NULL"},
	} {
		if got := RowFilter(tc.where, tc.soft); got != tc.want {
			t.Errorf("RowFilter(%q, %q) = %q, want %q", tc.where, tc.soft, got, tc.want)
		}
	}
}

// The duplicate-key messages name the index that let the second row in, and
// ask for one only where the table has none (E11).
func TestDuplicateKeyAdvice(t *testing.T) {
	const fallback = "give the table a unique index"
	live := unique("t_live", "code")
	live.Predicate = "(deleted_at IS NULL)"
	invalid := unique("t_code_idx", "code")
	invalid.Valid = false
	for _, tc := range []struct {
		what    string
		indexes []dbschema.KeyIndex
		key     []string
		want    string
	}{
		{"no index", nil, []string{"code"}, fallback},
		{"NULLs distinct", []dbschema.KeyIndex{unique("t_parent_id_code_key", "parent_id", "code")},
			[]string{"code", "parent_id"}, "UNIQUE (parent_id, code) holds NULLs distinct, and lets in a second row " +
				"with parent_id NULL: declare it NULLS NOT DISTINCT (PostgreSQL 15 and later), or make parent_id NOT NULL"},
		{"partial", []dbschema.KeyIndex{live}, []string{"code"}, "UNIQUE (code) WHERE deleted_at IS NULL holds only " +
			"where deleted_at IS NULL, and lets in the rows outside it: give the model a where that implies it, or " +
			"give the table a unique index over every row"},
		{"invalid", []dbschema.KeyIndex{invalid}, []string{"code"}, "unique index t_code_idx is invalid, left by a " +
			"CREATE INDEX CONCURRENTLY that failed: delete the duplicate rows, then REINDEX INDEX CONCURRENTLY t_code_idx"},
		{"an index over more columns", []dbschema.KeyIndex{unique("t_code_plan_id_key", "code", "plan_id")},
			[]string{"code"}, fallback},
	} {
		if got := indexAdvice(keyTable(tc.indexes...), tc.key, fallback); got != tc.want {
			t.Errorf("%s:\n%s\nwant:\n%s", tc.what, got, tc.want)
		}
	}
	if got := indexAdvice(nil, []string{"code"}, fallback); got != fallback {
		t.Errorf("no catalog: %s", got)
	}
}

func TestTheKeyIndexPolicy(t *testing.T) {
	cfg := &Config{Models: map[string]*Model{"A": {Table: "a"}, "B": {Table: "b", KeyIndex: ModeError}}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	if cfg.Policy.KeyIndex != ModeWarn {
		t.Fatalf("default %q", cfg.Policy.KeyIndex)
	}
	if got := cfg.ModeOf(Finding{Kind: FindingUnbackedKey, Model: "A"}); got != ModeWarn {
		t.Fatalf("A: %s", got)
	}
	if got := cfg.ModeOf(Finding{Kind: FindingUnbackedKey, Model: "B"}); got != ModeError {
		t.Fatalf("B: %s", got)
	}
	if got := cfg.ModelPolicy("B").KeyIndex; got != ModeError {
		t.Fatalf("ModelPolicy(B): %s", got)
	}
	// Generate-time only: no migration carries it.
	if cfg.TablePolicy("B") != nil {
		t.Fatalf("TablePolicy(B) = %+v", cfg.TablePolicy("B"))
	}
	bad := &Config{Models: map[string]*Model{"A": {Table: "a", KeyIndex: "sometimes"}}}
	if err := bad.Prepare(); err == nil || !strings.Contains(err.Error(), `model "A": key_index is "sometimes"`) {
		t.Fatalf("a bad model value: %v", err)
	}
	bad = &Config{Policy: Policy{KeyIndex: "sometimes"}, Models: map[string]*Model{"A": {Table: "a"}}}
	if err := bad.Prepare(); err == nil || !strings.Contains(err.Error(), `policy key_index is "sometimes"`) {
		t.Fatalf("a bad policy value: %v", err)
	}
}
