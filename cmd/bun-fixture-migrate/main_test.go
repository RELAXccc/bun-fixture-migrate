package main

// The commands that need no database, driven the way a shell drives them.
// generate against two files is the whole pipeline — configuration, fixture
// parsing, diff, rendering — and it is the one command a project runs without
// a DSN in reach.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const config = `fixture: fixtures/fixture.yml
out: migrations
package: migrations
seed_guard_table: plans
models:
  Currency:
    table: currencies
    ref: code
    key: [code]
  Plan:
    table: plans
    serial: true
    key: [name]
    references:
      currency_id: Currency
`

const oldFixture = `- model: Currency
  rows:
    - _id: eur
      id: 1
      code: EUR
      symbol: "E"
- model: Plan
  rows:
    - _id: team
      id: 2
      name: team
      currency_id: '{{ $.Currency.eur.ID }}'
      price_cents: 2000
`

const newFixture = `- model: Currency
  rows:
    - _id: eur
      id: 1
      code: EUR
      symbol: "E"
- model: Plan
  rows:
    - _id: team
      id: 2
      name: team
      currency_id: '{{ $.Currency.eur.ID }}'
      price_cents: 2500
`

// project writes a configuration, the fixture file and the base revision of it
// into a temporary directory and returns the paths of the two.
func project(t *testing.T, head, base string) (configPath, basePath string) {
	t.Helper()
	return projectWith(t, config, head, base)
}

func projectWith(t *testing.T, config, head, base string) (configPath, basePath string) {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) string {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write("fixtures/fixture.yml", head)
	if err := os.Mkdir(filepath.Join(dir, "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	return write("fixture-migrate.yml", config), write("base.yml", base)
}

// call runs one command and returns its exit code and its two streams.
func call(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestGenerateAgainstAFileNeedsNoDatabase(t *testing.T) {
	cfg, base := project(t, newFixture, oldFixture)
	code, stdout, stderr := call(t, "generate", "-config", cfg, "-old", base, "-name", "plan prices", "-dry-run")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	if !strings.HasPrefix(stdout, "Plan: 1 update\n") {
		t.Fatalf("expected the summary first:\n%s", stdout)
	}
	flat := strings.Join(strings.Fields(stdout), " ")
	for _, want := range []string{
		"package migrations",
		`SeedGuardTable: "plans"`,
		`fixturechange.Lit("2000")`,
		`fixturechange.Lit("2500")`,
	} {
		if !strings.Contains(flat, want) {
			t.Fatalf("the migration is missing %q:\n%s", want, stdout)
		}
	}
	// -dry-run means what it says.
	if entries, err := os.ReadDir(filepath.Join(filepath.Dir(cfg), "migrations")); err != nil || len(entries) != 0 {
		t.Fatalf("a dry run wrote %v (%v)", entries, err)
	}
}

func TestGenerateWritesTheFileItNames(t *testing.T) {
	cfg, base := project(t, newFixture, oldFixture)
	code, stdout, stderr := call(t, "generate", "-config", cfg, "-old", base, "-name", "plan prices")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	dir := filepath.Join(filepath.Dir(cfg), "migrations")
	entries, err := os.ReadDir(dir)
	// The migration and the state file it leaves behind.
	if err != nil || len(entries) != 2 || entries[1].Name() != "fixture_state.yml" {
		t.Fatalf("expected the migration and the state file, got %v (%v)", entries, err)
	}
	name := entries[0].Name()
	if !strings.HasSuffix(name, "_fixture_plan_prices.go") {
		t.Fatalf("unexpected file name %q", name)
	}
	if !strings.Contains(stdout, filepath.Join(dir, name)) {
		t.Fatalf("the path has to be printed:\n%s", stdout)
	}
}

// id_drift: warn reports a renumbered row and carries on: before, generate
// put the warning among the refusals and refused the whole migration, with a
// message telling the user to set the policy it already had.
func TestIDDriftWarnIsAWarning(t *testing.T) {
	renumbered := strings.Replace(newFixture, "      id: 2\n", "      id: 7\n", 1)
	for policy, want := range map[string]int{"error": 2, "warn": 0} {
		cfg, base := projectWith(t, config+"policy:\n  id_drift: "+policy+"\n", renumbered, oldFixture)
		code, stdout, stderr := call(t, "generate", "-config", cfg, "-old", base, "-name", "prices", "-dry-run")
		if code != want {
			t.Fatalf("id_drift: %s: exit %d, want %d\n%s%s", policy, code, want, stdout, stderr)
		}
		if policy == "warn" && (!strings.Contains(stderr, "warning:") || strings.Contains(stderr, "refused:") ||
			!strings.Contains(stdout, "price_cents")) {
			t.Fatalf("id_drift: warn has to warn and write the price change:\n%s%s", stdout, stderr)
		}
	}
}

// -at makes the output the same on every run: a release script, a test that
// replays a project's history, a reviewer regenerating to compare.
func TestGenerateAtATimeIsReproducible(t *testing.T) {
	cfg, base := project(t, newFixture, oldFixture)
	var files [2]string
	for i, at := range []string{"20260102030405", "2026-01-02T03:04:05Z"} {
		code, stdout, stderr := call(t, "generate", "-config", cfg, "-old", base, "-name", "plan prices", "-at", at, "-dry-run")
		if code != 0 {
			t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
		}
		files[i] = stdout
	}
	if files[0] != files[1] || !strings.Contains(files[0], "fixtureChanges20260102030405PlanPrices") {
		t.Fatalf("one time spelled two ways has to write one file:\n%s\n---\n%s", files[0], files[1])
	}
	code, stdout, stderr := call(t, "generate", "-config", cfg, "-old", base, "-name", "plan prices", "-at", "20260102030405")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg), "migrations", "20260102030405_fixture_plan_prices.go")); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := call(t, "generate", "-config", cfg, "-old", base, "-name", "x", "-at", "yesterday"); code != 1 ||
		!strings.Contains(stderr, "YYYYMMDDHHMMSS") {
		t.Fatalf("a bad -at has to be an error naming the format, got %d: %s", code, stderr)
	}
}

// Nothing to do is not a failure, and it must not leave a migration behind
// that registers an empty change set.
func TestGenerateWritesNothingWhenNothingChanged(t *testing.T) {
	cfg, base := project(t, oldFixture, oldFixture)
	code, stdout, _ := call(t, "generate", "-config", cfg, "-old", base, "-name", "nothing")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stdout)
	}
	if !strings.Contains(stdout, "nothing changed") {
		t.Fatalf("expected it to say so:\n%s", stdout)
	}
	if entries, _ := os.ReadDir(filepath.Join(filepath.Dir(cfg), "migrations")); len(entries) != 0 {
		t.Fatalf("nothing to write, but %v was written", entries)
	}
}

// A refusal is exit code 2 and an empty migrations directory: the point of
// refusing is that nothing half-right gets written.
func TestARefusedDifferenceIsExitCodeTwo(t *testing.T) {
	renamed := strings.Replace(newFixture, "name: team", "name: crew", 1)
	cfg, base := project(t, renamed, oldFixture)
	code, _, stderr := call(t, "generate", "-config", cfg, "-old", base, "-name", "rename")
	if code != 2 {
		t.Fatalf("exit %d, want 2\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "refused:") || !strings.Contains(stderr, "renamed from") {
		t.Fatalf("the refusal has to say what it is:\n%s", stderr)
	}
	if entries, _ := os.ReadDir(filepath.Join(filepath.Dir(cfg), "migrations")); len(entries) != 0 {
		t.Fatalf("a refusal must write nothing, found %v", entries)
	}
}

func TestTwoBaseStatesAreRefused(t *testing.T) {
	cfg, base := project(t, newFixture, oldFixture)
	code, _, stderr := call(t, "generate", "-config", cfg, "-old", base, "-from-db", "-name", "x")
	if code != 1 || !strings.Contains(stderr, "different base states") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}

func TestAModelTheConfigurationDoesNotListStopsTheCommand(t *testing.T) {
	head := newFixture + `- model: Coupon
  rows:
    - _id: x
      code: X
`
	cfg, base := project(t, head, oldFixture)
	code, _, stderr := call(t, "generate", "-config", cfg, "-old", base, "-name", "x")
	if code != 1 || !strings.Contains(stderr, `model "Coupon"`) {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}

func TestUnknownCommandAndNoCommand(t *testing.T) {
	if code, _, stderr := call(t, "migrate"); code != 1 || !strings.Contains(stderr, `no command "migrate"`) {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if code, _, stderr := call(t); code != 1 || !strings.Contains(stderr, "bun-fixture-migrate <command>") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if code, stdout, _ := call(t, "help"); code != 0 || !strings.Contains(stdout, "scaffold") {
		t.Fatalf("exit %d: %s", code, stdout)
	}
	if code, stdout, _ := call(t, "version"); code != 0 || !strings.HasPrefix(stdout, "bun-fixture-migrate ") {
		t.Fatalf("exit %d: %s", code, stdout)
	}
}

// -h prints the flags of a command and is not an error.
func TestCommandHelpIsNotAFailure(t *testing.T) {
	code, _, stderr := call(t, "generate", "-h")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr, "-allow-partial") {
		t.Fatalf("expected the flags:\n%s", stderr)
	}
}

// The base revision comes out of git, and it comes out of the repository the
// fixture file belongs to rather than the working directory the command was
// started in. (The library's tests read git directly.)
func TestGenerateReadsTheBaseAsOfARevision(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	cfg, _ := project(t, newFixture, oldFixture)
	dir := filepath.Dir(cfg)
	fixture := filepath.Join(dir, "fixtures", "fixture.yml")
	if err := os.WriteFile(fixture, []byte(oldFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"add", "."},
		{"commit", "-q", "-m", "the base state"},
	} {
		if out, err := git(dir, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(fixture, []byte(newFixture), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := call(t, "generate", "-config", cfg, "-name", "prices", "-dry-run")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `fixturechange.Lit("2500")`) {
		t.Fatalf("the diff against HEAD is the price change:\n%s", stdout)
	}
	code, stdout, stderr = call(t, "generate", "-config", cfg, "-name", "prices", "-dry-run", "-base", "no-such-revision")
	if code != 1 || !strings.Contains(stderr, "no-such-revision") {
		t.Fatalf("a revision that does not exist has to be an error: exit %d\n%s%s", code, stdout, stderr)
	}
}

// A bad flag is one line, with the command's name in front, saying where the
// flags are listed; neither the flag package's own print of the error nor the
// whole usage comes on top. -h still prints the usage.
func TestABadFlagIsOneError(t *testing.T) {
	cfg, _ := project(t, oldFixture, oldFixture)
	code, stdout, stderr := call(t, "status", "-config", cfg, "-nope")
	if code != 1 || stdout != "" || strings.Count(stderr, "\n") != 1 || strings.Contains(stderr, "Usage") ||
		!strings.HasPrefix(stderr, "bun-fixture-migrate: flag provided but not defined: -nope") ||
		!strings.Contains(stderr, `"bun-fixture-migrate status -h"`) {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	code, _, stderr = call(t, "scaffold", "-schema")
	if code != 1 || strings.Count(stderr, "\n") != 1 || !strings.Contains(stderr, "flag needs an argument: -schema") {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	code, _, stderr = call(t, "plan", "-h")
	if code != 0 || !strings.HasPrefix(stderr, "Usage of plan:") || !strings.Contains(stderr, "-dsn") ||
		!strings.Contains(stderr, "-with-sql") {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	// baseline connects to ask whether a difference is only spelling, so it
	// takes -dsn like every command that connects.
	code, _, stderr = call(t, "baseline", "-h")
	if code != 0 || !strings.Contains(stderr, "-dsn") {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
}

// An argument where a flag was meant is repeated in the error, unless it is a
// DSN with a password in it.
func TestAStrayArgumentDoesNotRepeatAPassword(t *testing.T) {
	cfg, _ := project(t, oldFixture, oldFixture)
	for _, arg := range []string{"postgres://app:s3cret@db/x", "host=db password=s3cret", "postgres://app:s3cret@[::1/x",
		"app:s3cret@db:5432/x"} {
		code, _, stderr := call(t, "check", "-config", cfg, arg)
		if code != 1 || !strings.Contains(stderr, "unexpected argument") || strings.Contains(stderr, "s3cret") {
			t.Errorf("%s: exit %d\n%s", arg, code, stderr)
		}
	}
	if _, _, stderr := call(t, "check", "-config", cfg, "extra"); !strings.Contains(stderr, `unexpected argument "extra"`) {
		t.Errorf("%s", stderr)
	}
}

// Whatever the exit code, the line that says why starts with the command's
// name.
func TestEveryErrorStartsWithTheCommandsName(t *testing.T) {
	renamed, renamedBase := project(t, strings.Replace(newFixture, "name: team", "name: crew", 1), oldFixture)
	cfg, base := project(t, newFixture, oldFixture)
	for want, args := range map[int][]string{
		2: {"generate", "-config", renamed, "-old", renamedBase, "-name", "rename"},
		1: {"generate", "-config", cfg, "-old", base, "-name", "x", "-at", "yesterday"},
	} {
		code, _, stderr := call(t, args...)
		lines := strings.Split(strings.TrimSpace(stderr), "\n")
		if code != want || !strings.HasPrefix(lines[len(lines)-1], "bun-fixture-migrate: ") {
			t.Errorf("%v: exit %d\n%s", args, code, stderr)
		}
	}
}

// -dsn wins over the configuration's database, takes env:NAME as the
// configuration does, and is never repeated with its password.
func TestDSNWinsOverTheConfiguration(t *testing.T) {
	cfg, _ := projectWith(t, config+"database: postgres://app:fromconfig@127.0.0.1:1/x?sslmode=disable\n",
		oldFixture, oldFixture)
	t.Setenv("BFM_TEST_FLAG_DSN", "postgres://app:fromenv@127.0.0.1:3/z?sslmode=disable")
	// status needs something to compare the fixture file with before it
	// connects.
	if code, _, stderr := call(t, "baseline", "-config", cfg, "-offline"); code != 0 {
		t.Fatal(stderr)
	}
	for dsn, where := range map[string]string{
		"postgres://app:fromflag@127.0.0.1:2/y?sslmode=disable": "127.0.0.1:2",
		"env:BFM_TEST_FLAG_DSN":                                 "127.0.0.1:3",
	} {
		for _, cmd := range []string{"check", "export", "plan", "sync", "status"} {
			code, _, stderr := call(t, cmd, "-config", cfg, "-dsn", dsn)
			if code != 1 || !strings.Contains(stderr, "connect to postgres://app:xxxxx@"+where) ||
				strings.Contains(stderr, "127.0.0.1:1/") || strings.Contains(stderr, "from") {
				t.Errorf("%s -dsn %s: exit %d\n%s", cmd, dsn, code, stderr)
			}
		}
	}
	// baseline asks the database whether a difference is only spelling.
	writeFixture(t, cfg, newFixture)
	code, _, stderr := call(t, "baseline", "-config", cfg, "-dsn", "env:BFM_TEST_FLAG_DSN")
	if code != 1 || !strings.Contains(stderr, "connect to postgres://app:xxxxx@127.0.0.1:3") {
		t.Errorf("baseline -dsn: exit %d\n%s", code, stderr)
	}
	// A project without a database in its configuration gets one.
	none, _ := project(t, oldFixture, oldFixture)
	if code, _, stderr := call(t, "baseline", "-config", none); code != 0 {
		t.Fatal(stderr)
	}
	code, _, stderr = call(t, "status", "-config", none, "-require-applied", "-dsn", "postgres://app@127.0.0.1:4/z")
	if code != 1 || !strings.Contains(stderr, "127.0.0.1:4") {
		t.Errorf("status -require-applied -dsn: exit %d\n%s", code, stderr)
	}
	for _, args := range [][]string{{"check", "-config", none}, {"scaffold"}} {
		code, _, stderr := call(t, append(args, "-dsn", "env:BFM_TEST_FLAG_DSN_UNSET")...)
		if code != 1 || !strings.Contains(stderr, "environment variable BFM_TEST_FLAG_DSN_UNSET, which is not set") {
			t.Errorf("%v with an unset variable: exit %d\n%s", args, code, stderr)
		}
	}
}

// $BUN_FIXTURE_MIGRATE_CONFIG is where the configuration is unless -config
// says otherwise.
func TestTheConfigurationPathFromTheEnvironment(t *testing.T) {
	cfg, _ := project(t, oldFixture, oldFixture)
	t.Setenv("BUN_FIXTURE_MIGRATE_CONFIG", cfg)
	if code, stdout, stderr := call(t, "baseline"); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg), "migrations", "fixture_state.yml")); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing.yml")
	if code, _, stderr := call(t, "baseline", "-config", missing); code != 1 || !strings.Contains(stderr, missing) {
		t.Fatalf("-config wins: exit %d\n%s", code, stderr)
	}
}

// Output a script redirects that did not all arrive fails the command.
func TestWriteOutFailsWithTheWriter(t *testing.T) {
	if err := writeOut(failingWriter{}, []byte("x")); err == nil || !strings.Contains(err.Error(), "standard output") {
		t.Fatalf("%v", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

// A migration without a seed guard runs on a database that was never seeded,
// before the seed, and fails there; one guarded by a table the fixture files
// do not fill does nothing on a seeded database where that table is empty.
// generate says so, and writes the migration all the same.
func TestGenerateWarnsAboutTheSeedGuard(t *testing.T) {
	for guard, want := range map[string]string{
		"seed_guard_table: plans\n":        "",
		"seed_guard_table: public.plans\n": "",
		"":                                 "warning: no seed_guard_table: on a database that was never seeded this migration runs before the seed and fails",
		"seed_guard_table: \"\"\n":         "warning: no seed_guard_table",
		"seed_guard_table: users\n":        "warning: seed_guard_table users is the table of no model",
	} {
		cfg, base := projectWith(t, strings.Replace(config, "seed_guard_table: plans\n", guard, 1), newFixture, oldFixture)
		code, out, errs := call(t, "generate", "-config", cfg, "-old", base, "-name", "x")
		if code != 0 || (want == "") != !strings.Contains(errs, "seed_guard_table") || !strings.Contains(errs, want) {
			t.Errorf("%q: exit %d\n%s%s", guard, code, out, errs)
		}
	}
}

// The last line of a failed command says why, starting with the command's
// name, however many lines the error came in.
func TestAnErrorIsOneLine(t *testing.T) {
	cfg, _ := projectWith(t, strings.Replace(config, "package: migrations\n",
		"package: migrations\nseed_guard_tabel: plans\nmigrator_name: M\n", 1), oldFixture, oldFixture)
	code, _, stderr := call(t, "status", "-config", cfg, "-offline")
	if code != 1 || strings.Count(stderr, "\n") != 1 || !strings.Contains(stderr, "field seed_guard_tabel not found") ||
		!strings.Contains(stderr, "; line ") {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if got := oneLine("a\n  b\n\nc"); got != "a; b; c" {
		t.Fatal(got)
	}
	// git's own sentence, in the middle of one of ours, keeps one period.
	if _, err := exec.LookPath("git"); err != nil {
		return
	}
	cfg, _ = project(t, oldFixture, oldFixture)
	gitIn(t, filepath.Dir(cfg), false, "init", "-q")
	code, _, stderr = call(t, "status", "-config", cfg, "-offline")
	if code != 1 || !strings.Contains(stderr, "invalid object name 'HEAD'. So nothing says") {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
}

// Small things a message gets right: the flag that would have helped, the
// file plan -file takes, where a configuration nobody named on the command
// line came from.
func TestMessagesSayWhatToDo(t *testing.T) {
	cfg, base := projectWith(t, config+"database: env:BFM_TEST_UNSET_DATABASE_URL\n", newFixture, oldFixture)
	code, _, stderr := call(t, "generate", "-config", cfg, "-old", base, "-name", "x")
	if code != 1 || !strings.Contains(stderr, "BFM_TEST_UNSET_DATABASE_URL, which is not set") ||
		!strings.Contains(stderr, "-no-lint generates without the database") {
		t.Errorf("exit %d\n%s", code, stderr)
	}
	if code, _, stderr := call(t, "generate", "-config", cfg, "-old", base, "-name", "x", "-no-lint"); code != 0 {
		t.Errorf("exit %d\n%s", code, stderr)
	}

	sql := filepath.Join(filepath.Dir(cfg), "migrations", "20260101000000_schema.up.sql")
	if err := os.WriteFile(sql, []byte("CREATE TABLE x (id int);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = call(t, "plan", "-config", cfg, "-file", sql)
	if code != 1 || !strings.Contains(stderr, "plan -file takes a fixture migration generate wrote, a .go file") {
		t.Errorf("exit %d\n%s", code, stderr)
	}

	t.Setenv("BUN_FIXTURE_MIGRATE_CONFIG", filepath.Join(t.TempDir(), "nowhere.yml"))
	code, _, stderr = call(t, "status", "-offline")
	if code != 1 || !strings.Contains(stderr, "nowhere.yml") || !strings.Contains(stderr, "$BUN_FIXTURE_MIGRATE_CONFIG") {
		t.Errorf("exit %d\n%s", code, stderr)
	}
	// Named with -config, it is not the variable's.
	if _, _, stderr := call(t, "status", "-offline", "-config", filepath.Join(t.TempDir(), "x.yml")); strings.Contains(stderr, "$BUN_") {
		t.Errorf("%s", stderr)
	}
}
