package main

// The state file, and the commands built on it, without a database: the two
// ways diffing against git loses or doubles a change, and how the state file
// closes both.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func migrationsOf(t *testing.T, cfg string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(cfg), "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".go") {
			out = append(out, e.Name())
		}
	}
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeFixture(t *testing.T, cfg, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(filepath.Dir(cfg), "fixtures", "fixture.yml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Generating twice before anything is committed. Against git, the second
// migration would carry the first one's change again, guarded by the value the
// first one replaced. Against the state file it carries only what is new.
func TestASecondGenerateCarriesOnlyWhatIsNew(t *testing.T) {
	cfg, base := project(t, newFixture, oldFixture)
	if code, out, errs := call(t, "generate", "-config", cfg, "-old", base, "-name", "first"); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	code, out, _ := call(t, "generate", "-config", cfg, "-name", "again")
	if code != 0 || !strings.Contains(out, "nothing changed in fixtures/fixture.yml since the state after") {
		t.Fatalf("exit %d: the state already covers the change:\n%s", code, out)
	}

	writeFixture(t, cfg, strings.Replace(newFixture, "price_cents: 2500", "price_cents: 3000", 1))
	code, out, errs := call(t, "generate", "-config", cfg, "-name", "second")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	files := migrationsOf(t, cfg)
	if len(files) != 2 {
		t.Fatalf("expected two migrations, got %v", files)
	}
	second := readFile(t, filepath.Join(filepath.Dir(cfg), "migrations", files[1]))
	if !strings.Contains(second, `"price_cents": fixturechange.Lit("2500")`) ||
		!strings.Contains(second, `"price_cents": fixturechange.Lit("3000")`) ||
		strings.Contains(second, `Lit("2000")`) {
		t.Fatalf("the second migration has to go from 2500 to 3000 and nothing else:\n%s", second)
	}
	if files[1] <= files[0] {
		t.Fatalf("the second migration has to run after the first: %v", files)
	}
}

// Committing the fixture edit before generating its migration. Against HEAD
// there is nothing left to generate, and the change never reaches a seeded
// database. Against the state file it is still there.
func TestAFixtureEditCommittedWithoutAMigrationIsStillFound(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	cfg, _ := project(t, oldFixture, oldFixture)
	dir := filepath.Dir(cfg)
	if code, out, errs := call(t, "baseline", "-config", cfg); code != 0 {
		t.Fatalf("baseline: exit %d\n%s%s", code, out, errs)
	}
	writeFixture(t, cfg, newFixture)
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"add", "."},
		{"commit", "-q", "-m", "fixture edit, no migration"},
	} {
		if out, err := git(dir, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	code, out, errs := call(t, "generate", "-config", cfg, "-name", "late", "-dry-run")
	if code != 0 || !strings.Contains(out, `fixturechange.Lit("2500")`) {
		t.Fatalf("exit %d: the committed edit has to be found\n%s%s", code, out, errs)
	}
	// HEAD, asked for explicitly, shows the problem this avoids.
	if _, out, _ := call(t, "generate", "-config", cfg, "-name", "late", "-base", "HEAD"); !strings.Contains(out, "nothing changed") {
		t.Fatalf("against HEAD there is nothing to generate:\n%s", out)
	}
}

func TestBaselineRefusesToSwallowAChange(t *testing.T) {
	cfg, _ := project(t, oldFixture, oldFixture)
	if code, out, _ := call(t, "baseline", "-config", cfg); code != 0 || !strings.Contains(out, "wrote") {
		t.Fatalf("exit %d: %s", code, out)
	}
	if code, out, _ := call(t, "baseline", "-config", cfg); code != 0 || !strings.Contains(out, "already records") {
		t.Fatalf("exit %d: %s", code, out)
	}
	writeFixture(t, cfg, newFixture)
	code, out, errs := call(t, "baseline", "-config", cfg)
	if code != 2 || !strings.Contains(out, "Plan: 1 update") || !strings.Contains(errs, "-force") {
		t.Fatalf("exit %d: a change with no migration is not a baseline\n%s%s", code, out, errs)
	}
	if code, _, errs := call(t, "baseline", "-config", cfg, "-force"); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if _, out, _ := call(t, "generate", "-config", cfg, "-name", "x"); !strings.Contains(out, "nothing changed") {
		t.Fatalf("after the baseline, generate has nothing to do:\n%s", out)
	}
}

// status is the gate a CI job runs: a fixture edit with no migration fails it.
func TestStatusFailsOnAChangeNoMigrationMakes(t *testing.T) {
	cfg, base := project(t, newFixture, oldFixture)
	if code, _, errs := call(t, "baseline", "-config", cfg, "-old", base); code != 0 {
		t.Fatalf("baseline: %s", errs)
	}
	code, out, errs := call(t, "status", "-config", cfg, "-offline")
	if code != 3 || !strings.Contains(out, "not migrated  Plan: 1 update") ||
		!strings.Contains(errs, "no migration makes") {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	if code, out, errs := call(t, "generate", "-config", cfg, "-name", "prices"); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	code, out, errs = call(t, "status", "-config", cfg, "-offline")
	if code != 0 || !strings.Contains(out, "every change since the state file has a migration") ||
		!strings.Contains(out, "_fixture_prices  fixture, 1 change") {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}

	code, out, _ = call(t, "status", "-config", cfg, "-offline", "-json")
	var report statusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil || code != 0 {
		t.Fatalf("exit %d, %v:\n%s", code, err, out)
	}
	if len(report.Migrations) != 1 || !report.Migrations[0].Fixture || report.Migrations[0].Applied != nil ||
		report.State == nil || !strings.HasSuffix(report.State.Migration, "_fixture_prices") {
		t.Fatalf("%+v", report)
	}
	// An empty list is [], which a program can range over without a check.
	for _, field := range []string{`"uncovered": []`, `"refused": []`, `"problems": []`, `"notes": []`} {
		if !strings.Contains(out, field) {
			t.Errorf("no %s in\n%s", field, out)
		}
	}
}

// Two migrations bun would record under one name: one of them never runs.
func TestStatusReportsASharedMigrationName(t *testing.T) {
	cfg, base := project(t, newFixture, oldFixture)
	call(t, "generate", "-config", cfg, "-old", base, "-name", "prices")
	files := migrationsOf(t, cfg)
	stamp := files[0][:14]
	sql := filepath.Join(filepath.Dir(cfg), "migrations", stamp+"_add_column.up.sql")
	if err := os.WriteFile(sql, []byte("SELECT 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := call(t, "status", "-config", cfg, "-offline")
	if code != 3 || !strings.Contains(out, "share the name "+stamp) {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

// A migration dated ahead of this clock, or one generated a moment ago, still
// runs before the new one.
func TestGenerateDatesTheMigrationAfterEveryOther(t *testing.T) {
	cfg, base := project(t, newFixture, oldFixture)
	ahead := filepath.Join(filepath.Dir(cfg), "migrations", "29990101000000_ahead.up.sql")
	if err := os.WriteFile(ahead, []byte("SELECT 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := call(t, "generate", "-config", cfg, "-old", base, "-name", "prices"); code != 0 {
		t.Fatal(errs)
	}
	if files := migrationsOf(t, cfg); len(files) != 1 || files[0] != "29990101000001_fixture_prices.go" {
		t.Fatalf("got %v", files)
	}
}

func TestGenerateRefusesAPackageItWouldNotCompileIn(t *testing.T) {
	cfg, base := project(t, newFixture, oldFixture)
	other := filepath.Join(filepath.Dir(cfg), "migrations", "migrations.go")
	if err := os.WriteFile(other, []byte("package db\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := call(t, "generate", "-config", cfg, "-old", base, "-name", "prices")
	if code != 1 || !strings.Contains(errs, "is package db and the configuration says package migrations") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if files := migrationsOf(t, cfg); len(files) != 1 {
		t.Fatalf("nothing may be written: %v", files)
	}
}

// An argument that is not a flag is a mistake, such as a -name of two words
// without quotes, and not something to drop quietly.
func TestAStrayArgumentIsAnError(t *testing.T) {
	cfg, base := project(t, newFixture, oldFixture)
	code, _, errs := call(t, "generate", "-config", cfg, "-old", base, "-name", "plan", "prices")
	if code != 1 || !strings.Contains(errs, `unexpected argument "prices"`) {
		t.Fatalf("exit %d: %s", code, errs)
	}
}

// A DSN pgdriver cannot read makes it panic, and url.Parse repeats the whole
// DSN in its error. Neither may reach the terminal.
func TestABadDSNIsASentenceWithoutThePassword(t *testing.T) {
	for name, dsn := range map[string]string{
		"libpq keywords":    "host=localhost dbname=app password=s3cret",
		"broken URL":        "postgres://app:s3cret@[::1/app",
		"unreachable host":  "postgres://app:s3cret@127.0.0.1:1/app?sslmode=disable",
		"other scheme":      "mysql://app:s3cret@localhost/app",
		"unix without path": "unix://app:s3cret@mydb",
		"query password":    "postgres://app@127.0.0.1:1/app?sslmode=disable&password=s3cret",
		"escaped password":  "postgres://app:s3cr%40et@127.0.0.1:1/app?sslmode=disable&password=s3cret",
	} {
		cfg, _ := projectWith(t, config+"database: "+dsn+"\n", oldFixture, oldFixture)
		code, out, errs := call(t, "check", "-config", cfg)
		if code != 1 {
			t.Errorf("%s: exit %d\n%s%s", name, code, out, errs)
		}
		if strings.Contains(errs, "s3cr") || strings.Contains(errs, "goroutine") {
			t.Errorf("%s: the password or a stack trace reached the terminal:\n%s", name, errs)
		}
	}
}

// bun's reading of a SQL migration: split at --bun:split, blank lines
// dropped, any other directive refused.
func TestSplitSQLReadsLikeBun(t *testing.T) {
	queries, err := splitSQL([]byte("ALTER TABLE a ADD b int;\n\n--bun:split\r\nUPDATE a SET b = 1;\n  \n--bun:split\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 2 || queries[0] != "ALTER TABLE a ADD b int;\n" || queries[1] != "UPDATE a SET b = 1;\n" {
		t.Fatalf("%q", queries)
	}
	if _, err := splitSQL([]byte("--bun:nope\nSELECT 1")); err == nil || !strings.Contains(err.Error(), "unknown directive") {
		t.Fatalf("expected the directive to be refused: %v", err)
	}
}

// With no state file and no git to read the fixture file's last revision
// from, nothing says what the fixture file changes. A gate that passed on that
// would pass anything.
func TestStatusWithNothingToCompareWithFails(t *testing.T) {
	cfg, _ := project(t, newFixture, oldFixture)
	t.Setenv("PATH", "")
	code, out, errs := call(t, "status", "-config", cfg, "-offline")
	if code != 1 || !strings.Contains(errs, "Run bun-fixture-migrate baseline") || !strings.Contains(errs, "git is not installed") {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
}

// generate -allow-partial writes what it can and records the rest as left out.
// The next generate does not write the accepted change again, and status keeps
// failing on the refused one until baseline -force says a migration somebody
// wrote makes it.
func TestWhatAPartialGenerateLeftOutStaysVisible(t *testing.T) {
	edited := strings.Replace(newFixture, `symbol: "E"`, `symbol: "€"`, 1)
	edited = strings.Replace(edited, "name: team", "name: crew", 1)
	cfg, _ := project(t, oldFixture, oldFixture)
	if code, _, errs := call(t, "baseline", "-config", cfg); code != 0 {
		t.Fatal(errs)
	}
	writeFixture(t, cfg, edited)
	if code, out, errs := call(t, "generate", "-config", cfg, "-name", "symbol"); code != 2 {
		t.Fatalf("without -allow-partial a refusal writes nothing: exit %d\n%s%s", code, out, errs)
	}
	code, out, errs := call(t, "generate", "-config", cfg, "-name", "symbol", "-allow-partial")
	if code != 0 || !strings.Contains(errs, "refused:") || !strings.Contains(out, "1 change left out") {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	if files := migrationsOf(t, cfg); len(files) != 1 {
		t.Fatalf("got %v", files)
	}

	code, out, errs = call(t, "status", "-config", cfg, "-offline")
	if code != 3 || !strings.Contains(out, "left out") || !strings.Contains(out, "renamed from") ||
		!strings.Contains(out, "baseline -force") || !strings.Contains(errs, "1 change left out") {
		t.Fatalf("a refused change has to stay visible: exit %d\n%s%s", code, out, errs)
	}
	_, out, _ = call(t, "status", "-config", cfg, "-offline", "-json")
	var report statusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil || len(report.LeftOut) != 1 ||
		!strings.Contains(report.LeftOut[0], "renamed from") {
		t.Fatalf("%v\n%s", err, out)
	}

	// The accepted change is not generated a second time.
	code, out, _ = call(t, "generate", "-config", cfg, "-name", "again", "-allow-partial")
	if code != 0 || !strings.Contains(out, "nothing changed") || len(migrationsOf(t, cfg)) != 1 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	// Another change carries what was left out forward.
	writeFixture(t, cfg, strings.Replace(edited, "price_cents: 2500", "price_cents: 2600", 1))
	if code, out, errs := call(t, "generate", "-config", cfg, "-name", "price"); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	if code, _, _ := call(t, "status", "-config", cfg, "-offline"); code != 3 {
		t.Fatalf("exit %d: what was left out is still not migrated", code)
	}

	// The migration for it is written by hand; baseline says so.
	code, out, errs = call(t, "baseline", "-config", cfg)
	if code != 2 || !strings.Contains(out, "left out: Plan") || !strings.Contains(errs, "-force") {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	if code, _, errs := call(t, "baseline", "-config", cfg, "-force"); code != 0 {
		t.Fatal(errs)
	}
	if code, out, errs := call(t, "status", "-config", cfg, "-offline"); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
}
