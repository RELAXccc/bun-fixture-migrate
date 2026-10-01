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
// started in.
func TestGitShowReadsTheFileAsOfARevision(t *testing.T) {
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

	data, err := gitShow(fixture, "HEAD")
	if err != nil {
		t.Fatalf("gitShow: %v", err)
	}
	if string(data) != oldFixture {
		t.Fatalf("expected the committed revision, got:\n%s", data)
	}

	code, stdout, stderr := call(t, "generate", "-config", cfg, "-name", "prices", "-dry-run")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `fixturechange.Lit("2500")`) {
		t.Fatalf("the diff against HEAD is the price change:\n%s", stdout)
	}
	if _, err := gitShow(fixture, "no-such-revision"); err == nil {
		t.Fatal("a revision that does not exist has to be an error")
	}
}

func TestGitShowOutsideARepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	// A temporary directory can sit inside a repository; one that does not
	// exist cannot.
	_, err := gitShow(filepath.Join(t.TempDir(), "nowhere", "fixture.yml"), "HEAD")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "pass -old") {
		t.Fatalf("the message has to name the way out: %v", err)
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
