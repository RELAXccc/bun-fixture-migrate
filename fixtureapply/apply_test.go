package fixtureapply

import (
	"runtime"
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

func tables() fixturechange.Tables {
	return fixturechange.Tables{
		"Plan":    {Name: "plans", ID: "id", Key: "name"},
		"Feature": {Name: "features", ID: "id", Key: "code", Serial: true},
	}
}

func TestValidateAcceptsAGoodSet(t *testing.T) {
	set := fixturechange.Set{
		Name:           "20260921120000_fixture_prices",
		SeedGuardTable: "plans",
		Tables:         tables(),
		Changes: []fixturechange.Change{
			{Model: "Plan", Kind: fixturechange.Update,
				Key: fixturechange.Values{"name": fixturechange.Lit("team")},
				Old: fixturechange.Values{"price_cents": fixturechange.Lit("2000")},
				New: fixturechange.Values{"price_cents": fixturechange.Lit("2500")}},
			{Model: "Feature", Kind: fixturechange.Insert,
				Key: fixturechange.Values{"code": fixturechange.Lit("sso")},
				New: fixturechange.Values{"code": fixturechange.Lit("sso"), "plan_id": fixturechange.RefTo("Plan", "team")}},
		},
	}
	if err := Validate(set); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		set  fixturechange.Set
		want string
	}{
		"unknown model": {
			fixturechange.Set{Tables: tables(), Changes: []fixturechange.Change{
				{Model: "Coupon", Kind: fixturechange.Update,
					Key: fixturechange.Values{"code": fixturechange.Lit("x")}},
			}}, "unknown model"},
		"unknown kind": {
			fixturechange.Set{Tables: tables(), Changes: []fixturechange.Change{
				{Model: "Plan", Kind: "replace", Key: fixturechange.Values{"name": fixturechange.Lit("x")}},
			}}, "unknown kind"},
		"no key": {
			fixturechange.Set{Tables: tables(), Changes: []fixturechange.Change{
				{Model: "Plan", Kind: fixturechange.Delete},
			}}, "no key"},
		"column that is not an identifier": {
			fixturechange.Set{Tables: tables(), Changes: []fixturechange.Change{
				{Model: "Plan", Kind: fixturechange.Delete,
					Key: fixturechange.Values{"name\"; DROP TABLE plans; --": fixturechange.Lit("x")}},
			}}, "plain SQL identifier"},
		// bun v1.2.18 drops a NUL from a bound string, so the database
		// would hold something else than the file says.
		"NUL in a value": {
			fixturechange.Set{Tables: tables(), Changes: []fixturechange.Change{
				{Model: "Plan", Kind: fixturechange.Insert,
					Key: fixturechange.Values{"name": fixturechange.Lit("te\x00am")},
					New: fixturechange.Values{"name": fixturechange.Lit("te\x00am")}},
			}}, "NUL"},
		"NUL in a reference": {
			fixturechange.Set{Tables: tables(), Changes: []fixturechange.Change{
				{Model: "Feature", Kind: fixturechange.Insert,
					Key: fixturechange.Values{"code": fixturechange.Lit("sso")},
					New: fixturechange.Values{"plan_id": fixturechange.RefTo("Plan", "a\x00")}},
			}}, "NUL"},
		"lock timeout without a unit": {
			fixturechange.Set{Tables: tables(), LockTimeout: "5000"}, "ms, s, min, h or d"},
		"lock timeout in Go's spelling": {
			fixturechange.Set{Tables: tables(), LockTimeout: "1m30s"}, "ms, s, min, h or d"},
		"lock timeout with SQL in it": {
			fixturechange.Set{Tables: tables(), LockTimeout: "5s'; DROP TABLE x; --"}, "ms, s, min, h or d"},
		"duplicate key policy": {
			fixturechange.Set{Tables: tables(), Policy: fixturechange.Policy{DuplicateKey: "ignore"}}, "DuplicateKey"},
		"table that is not an identifier": {
			fixturechange.Set{Tables: fixturechange.Tables{"Plan": {Name: "plans; DROP TABLE x", ID: "id", Key: "name"}}},
			"plain SQL identifier"},
		// Without the old value the statement would overwrite whatever is
		// there, which is the one thing this tool promises not to do.
		"update without the old value": {
			fixturechange.Set{Tables: tables(), Changes: []fixturechange.Change{
				{Model: "Plan", Kind: fixturechange.Update,
					Key: fixturechange.Values{"name": fixturechange.Lit("team")},
					New: fixturechange.Values{"price_cents": fixturechange.Lit("2500")}},
			}}, "without its old value"},
		"insert with old values": {
			fixturechange.Set{Tables: tables(), Changes: []fixturechange.Change{
				{Model: "Plan", Kind: fixturechange.Insert,
					Key: fixturechange.Values{"name": fixturechange.Lit("team")},
					Old: fixturechange.Values{"price_cents": fixturechange.Lit("1")},
					New: fixturechange.Values{"price_cents": fixturechange.Lit("2")}},
			}}, "insert with old values"},
		"reference to an unknown model": {
			fixturechange.Set{Tables: tables(), Changes: []fixturechange.Change{
				{Model: "Plan", Kind: fixturechange.Delete,
					Key: fixturechange.Values{"name": fixturechange.RefTo("Coupon", "x")}},
			}}, "unknown model"},
		// The natural key alone would delete whatever carries the name now,
		// including a row somebody has since edited into something else.
		"delete without the row it removes": {
			fixturechange.Set{Tables: tables(), Changes: []fixturechange.Change{
				{Model: "Plan", Kind: fixturechange.Delete,
					Key: fixturechange.Values{"name": fixturechange.Lit("team")}},
			}}, "delete without the row it removes"},
		// A misspelled policy reads as one of the two settings, and which one
		// depends on the field: strict here, lenient in ChangedRow.
		"policy nobody can read": {
			fixturechange.Set{Tables: tables(), Policy: fixturechange.Policy{MissingRow: "warm"}},
			"policy MissingRow"},
		"migrations table that is not an identifier": {
			fixturechange.Set{Tables: tables(), MigrationsTable: "bun_migrations; DROP TABLE plans"},
			"migrations table"},
		"policy that is not offered for this field": {
			fixturechange.Set{Tables: tables(), Policy: fixturechange.Policy{MissingRow: fixturechange.ModeIgnore}},
			"policy MissingRow"},
	} {
		err := Validate(tc.set)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: expected an error containing %q, got %v", name, tc.want, err)
		}
	}
}

func TestValidateAcceptsLockTimeouts(t *testing.T) {
	for _, timeout := range []string{"", "500ms", "5s", "2min", "1h", "1d"} {
		if err := Validate(fixturechange.Set{Tables: tables(), LockTimeout: timeout}); err != nil {
			t.Errorf("%q: %v", timeout, err)
		}
	}
}

func TestQuoteIdent(t *testing.T) {
	for in, want := range map[string]string{
		"items":         `"items"`,
		"public.items":  `"public"."items"`,
		"_x1":           `"_x1"`,
		`a" OR "1`:      "",
		"":              "",
		"items; DROP x": "",
	} {
		got, err := quoteIdent(in)
		if want == "" {
			if err == nil {
				t.Errorf("quoteIdent(%q) should have failed, got %q", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("quoteIdent(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestInvert(t *testing.T) {
	ins := fixturechange.Change{Model: "Plan", Kind: fixturechange.Insert,
		Key: fixturechange.Values{"name": fixturechange.Lit("pro")},
		New: fixturechange.Values{"name": fixturechange.Lit("pro")}}
	back := invert(ins)
	if back.Kind != fixturechange.Delete || len(back.Old) != 1 || len(back.New) != 0 {
		t.Fatalf("an insert reverts to a guarded delete, got %+v", back)
	}
	upd := fixturechange.Change{Model: "Plan", Kind: fixturechange.Update,
		Key: fixturechange.Values{"name": fixturechange.Lit("pro")},
		Old: fixturechange.Values{"seats": fixturechange.Lit("1")},
		New: fixturechange.Values{"seats": fixturechange.Lit("2")}}
	back = invert(upd)
	if back.Old["seats"].Lit != "2" || back.New["seats"].Lit != "1" {
		t.Fatalf("an update reverts by swapping, got %+v", back)
	}
	if back.Key["name"].Lit != "pro" {
		t.Fatalf("an update that leaves the key alone keeps it, got %+v", back)
	}
}

// A rename leaves its row under the new key, so that is where the revert
// finds it. Keyed on the old one, it looked for a row named both ways at once.
func TestInvertARename(t *testing.T) {
	rename := fixturechange.Change{Model: "Feature", Kind: fixturechange.Update, ID: "7",
		Key: fixturechange.Values{"plan_id": fixturechange.RefTo("Plan", "team"), "code": fixturechange.Lit("api")},
		Old: fixturechange.Values{"code": fixturechange.Lit("api")},
		New: fixturechange.Values{"code": fixturechange.Lit("rest")}}
	back := invert(rename)
	if back.Key["code"].Lit != "rest" || back.Key["plan_id"].Ref == nil || back.Key["plan_id"].Ref.Key != "team" {
		t.Fatalf("the revert has to find the row under the key the rename gave it, got %+v", back.Key)
	}
	if back.ID != "7" || back.Old["code"].Lit != "rest" || back.New["code"].Lit != "api" {
		t.Fatalf("the revert renames it back under the same id guard, got %+v", back)
	}
	if rename.Key["code"].Lit != "api" {
		t.Fatal("the change itself has to be left alone")
	}
	if again := invert(back); keyLabel(again.Key) != keyLabel(rename.Key) {
		t.Fatalf("inverting twice is the rename again, got %s", keyLabel(again.Key))
	}
}

func TestMovedKey(t *testing.T) {
	key := fixturechange.Values{"name": fixturechange.Lit("team"), "region": fixturechange.Lit("eu")}
	if got, moved := movedKey(key, fixturechange.Values{"price": fixturechange.Lit("2")}); moved ||
		keyLabel(got) != "name=team,region=eu" {
		t.Fatalf("no key column written: %s, %v", keyLabel(got), moved)
	}
	if got, moved := movedKey(key, fixturechange.Values{"name": fixturechange.Lit("crew")}); !moved ||
		keyLabel(got) != "name=crew,region=eu" {
		t.Fatalf("a rename: %s, %v", keyLabel(got), moved)
	}
}

// A guard that matched nothing is not success. bun records a migration as
// applied the moment the function returns nil, so the default for every reason
// a statement could not do its job is to fail and roll back.
func TestModeForIsStrictByDefault(t *testing.T) {
	var strict fixturechange.Policy
	for _, pr := range []problem{problemMissing, problemIDDrift, problemDuplicate, problemReferenced} {
		if got := modeFor(strict, pr); got != "error" {
			t.Errorf("modeFor(zero, %q) = %q, want error", pr, got)
		}
	}
	// Except a row somebody edited here: their edit is kept, and an update
	// that would overwrite it is skipped with a warning.
	if got := modeFor(strict, problemChanged); got != "warn" {
		t.Errorf("modeFor(zero, changed) = %q, want warn", got)
	}
	// And the database already holding what the change wanted is never a
	// problem at all.
	if got := modeFor(strict, problemBenign); got != "warn" {
		t.Errorf("modeFor(zero, benign) = %q, want warn", got)
	}
}

func TestModeForFollowsThePolicy(t *testing.T) {
	p := fixturechange.Policy{MissingRow: "warn", ChangedRow: "error", IDDrift: "ignore", DuplicateKey: "warn"}
	if got := modeFor(p, problemDuplicate); got != "warn" {
		t.Errorf("duplicate: %q", got)
	}
	if got := modeFor(p, problemMissing); got != "warn" {
		t.Errorf("missing: %q", got)
	}
	if got := modeFor(p, problemChanged); got != "error" {
		t.Errorf("changed: %q", got)
	}
	if got := modeFor(p, problemIDDrift); got != "warn" {
		t.Errorf("id drift: %q", got)
	}
}

func TestValidateWantsAnIDColumnForAnIDGuard(t *testing.T) {
	set := fixturechange.Set{
		Name:   "rename",
		Tables: fixturechange.Tables{"Plan": {Name: "plans"}},
		Changes: []fixturechange.Change{{Model: "Plan", Kind: fixturechange.Update, ID: "2",
			Key: fixturechange.Values{"name": fixturechange.Lit("team")},
			Old: fixturechange.Values{"name": fixturechange.Lit("team")},
			New: fixturechange.Values{"name": fixturechange.Lit("crew")}}},
	}
	if err := Validate(set); err == nil || !strings.Contains(err.Error(), "id column") {
		t.Fatalf("a change guarded by an id needs the id column, got %v", err)
	}
	set.Tables["Plan"] = fixturechange.Table{Name: "plans", ID: "id"}
	if err := Validate(set); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRefusesAnInsertWithAnIDGuard(t *testing.T) {
	set := fixturechange.Set{
		Name:   "bad",
		Tables: tables(),
		Changes: []fixturechange.Change{{Model: "Plan", Kind: fixturechange.Insert, ID: "2",
			Key: fixturechange.Values{"name": fixturechange.Lit("team")},
			New: fixturechange.Values{"name": fixturechange.Lit("team")}}},
	}
	if err := Validate(set); err == nil {
		t.Fatal("an insert has nothing to guard on yet")
	}
}

// The empty policy is the one a hand-written set carries, and it has to pass.
func TestValidateAcceptsAnEmptyPolicy(t *testing.T) {
	if err := (fixturechange.Policy{}).Validate(); err != nil {
		t.Fatalf("the zero policy is the default, not a mistake: %v", err)
	}
	full := fixturechange.Policy{
		MissingRow: fixturechange.ModeError, ChangedRow: fixturechange.ModeWarn, IDDrift: fixturechange.ModeIgnore}
	if err := full.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestKeyLabelIsStableWhateverTheMapOrder(t *testing.T) {
	key := fixturechange.Values{
		"code":    fixturechange.Lit("api"),
		"plan_id": fixturechange.RefTo("Plan", "team"),
		"note":    fixturechange.Null(),
	}
	const want = "code=api,note=NULL,plan_id=Plan(team)"
	for i := 0; i < 20; i++ {
		if got := keyLabel(key); got != want {
			t.Fatalf("keyLabel = %q, want %q", got, want)
		}
	}
}

// The id is written by the insert but left out of the check for a row that is
// already there: the row is the same row whatever id this database gave it.
func TestWithoutColumn(t *testing.T) {
	values := fixturechange.Values{
		"id": fixturechange.Lit("3"), "name": fixturechange.Lit("pro")}
	out := withoutColumn(values, "id")
	if len(out) != 1 || out["name"].Lit != "pro" {
		t.Fatalf("withoutColumn dropped the wrong thing: %+v", out)
	}
	if len(values) != 2 {
		t.Fatal("the original has to be left alone")
	}
}

func TestRowCount(t *testing.T) {
	for n, want := range map[int64]string{0: "0 rows", 1: "1 row", 2: "2 rows"} {
		if got := rowCount(n); got != want {
			t.Errorf("rowCount(%d) = %q, want %q", n, got, want)
		}
	}
}

// Outside bun's migrator there is no record to take back, so Apply called from
// a test, a tool or a dry run must not go looking for one, whatever the file
// calling it is named.
func TestTheMigrationNameIsOnlyReadUnderTheMigrator(t *testing.T) {
	const app = "example.com/app/migrations"
	frame := func(fn, file string) runtime.Frame { return runtime.Frame{Function: fn, File: file} }
	apply := frame("github.com/RELAXccc/bun-fixture-migrate/fixtureapply.Apply", "/x/fixtureapply/apply.go")
	generated := frame(app+".init.0.func1", "/app/migrations/20260921120000_fixture_prices.go")
	helper := frame(app+".applyFixtures", "/app/migrations/helpers.go")
	migrator := frame("github.com/uptrace/bun/migrate.(*Migrator).Migrate", "/mod/bun/migrate/migrator.go")
	main := frame("main.main", "/app/cmd/20260101000000_tool.go")

	for name, tc := range map[string]struct {
		frames []runtime.Frame
		want   string
	}{
		"generated file under the migrator":       {[]runtime.Frame{apply, generated, migrator, main}, "20260921120000"},
		"through a helper in another file":        {[]runtime.Frame{apply, helper, generated, migrator}, "20260921120000"},
		"a migration-named file, no migrator":     {[]runtime.Frame{apply, generated, main}, ""},
		"only the tool's own file named like one": {[]runtime.Frame{apply, main}, ""},
	} {
		if got := migrationFromFrames(tc.frames); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
	if got := migrationFromStack(); got != "" {
		t.Fatalf("no migrator on this stack, got %q", got)
	}
}

func TestBunsMigrationFilePattern(t *testing.T) {
	for file, want := range map[string]string{
		"20260921120000_fixture_plan_prices.go": "20260921120000",
		"1_init.up.sql":                         "1",
		"20260921120000_Fixture.go":             "",
		"migrations.go":                         "",
		"20260921120000.go":                     "",
	} {
		got := ""
		if m := bunMigrationFile.FindStringSubmatch(file); m != nil {
			got = m[1]
		}
		if got != want {
			t.Errorf("%s: %q, want %q", file, got, want)
		}
	}
}

// A model's Where is written into every statement for it, inside parentheses.
// One that could reach outside them would change which rows a guard matches,
// so a migration would write rows it was never generated for.
func TestValidateChecksAWhere(t *testing.T) {
	for _, ok := range []string{
		"tenant_id IS NULL",
		"(kind = 'global') AND deleted_at IS NULL",
		"flags ? 'global'",
		"note <> 'a) OR (b;'",
		`"weird)name" IS NULL`,
		"note <> E'it\\'s ) here'",
		"note <> $$ ) ; $$ AND x <> $t$ ( $t$",
		"tenant_id IS NULL -- global rows only ) ;",
		"tenant_id IS NULL /* not (a tenant's ;) /* nested ) */ */",
		"price$usd > 0",
	} {
		set := fixturechange.Set{Tables: fixturechange.Tables{"Tag": {Name: "tags", ID: "id", Where: ok}}}
		if err := Validate(set); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for bad, want := range map[string]string{
		"tenant_id IS NULL) OR (true":                     "closes a parenthesis",
		"(tenant_id IS NULL":                              "does not close",
		"tenant_id IS NULL; DROP TABLE tags":              "holds a ;",
		"note <> 'unterminated":                           "quote that does not end",
		"note <> E'it\\'s":                                "quote that does not end",
		`"unterminated IS NULL`:                           "quoted name",
		"x <> $q$ never closed":                           "dollar quote",
		"x /* never closed":                               "comment that does not end",
		"(x -- the closing parenthesis is in a comment )": "does not close",
		"x\x00": "NUL",
	} {
		set := fixturechange.Set{Tables: fixturechange.Tables{"Tag": {Name: "tags", ID: "id", Where: bad}}}
		if err := Validate(set); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want an error containing %q, got %v", bad, want, err)
		}
	}
}

// A set written for a format this version does not know is refused rather than
// run with a meaning it may not have; 0, a set from before formats were
// numbered, is format 1.
func TestValidateKnowsItsFormats(t *testing.T) {
	for _, format := range []int{0, fixturechange.CurrentFormat} {
		if err := Validate(fixturechange.Set{Format: format, Tables: tables()}); err != nil {
			t.Errorf("format %d: %v", format, err)
		}
	}
	for _, format := range []int{-1, fixturechange.CurrentFormat + 1} {
		err := Validate(fixturechange.Set{Format: format, Tables: tables()})
		if err == nil || !strings.Contains(err.Error(), "upgrade github.com/RELAXccc/bun-fixture-migrate") {
			t.Errorf("format %d: %v", format, err)
		}
	}
}

// standInForUp calls registeringMigration from where Up does.
func standInForUp() string { return registeringMigration() }

// Up reads the migration's name from the file that calls it, the file bun's
// Register reads it from, so a failure never has to find it on the stack.
func TestUpReadsTheNameOfTheFileThatRegisters(t *testing.T) {
	if got := fromAMigrationFile(); got != "20260921120000" {
		t.Fatalf("from a migration's file: %q", got)
	}
	if got := standInForUp(); got != "" {
		t.Fatalf("from a file not named like a migration: %q", got)
	}
}
