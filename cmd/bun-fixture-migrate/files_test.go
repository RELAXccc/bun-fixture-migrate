package main

// Several fixture files, loaded the way an application loads them with one
// fixture.Load(ctx, fsys, "currencies.yml", "plans.yml"): in order, one scope
// of anchors.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const filesConfig = `fixtures: [fixtures/currencies.yml, fixtures/plans.yml]
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

const currenciesFile = `- model: Currency
  rows:
    - _id: eur
      id: 1
      code: EUR
      symbol: "E"
`

const plansFile = `- model: Plan
  rows:
    - _id: team
      id: 2
      name: team
      currency_id: '{{ $.Currency.eur.ID }}'
      price_cents: 2000
`

func filesProject(t *testing.T, config string) string {
	t.Helper()
	cfg, _ := projectWith(t, config, "", "")
	dir := filepath.Dir(cfg)
	for name, content := range map[string]string{"currencies.yml": currenciesFile, "plans.yml": plansFile} {
		if err := os.WriteFile(filepath.Join(dir, "fixtures", name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

func TestAReferenceAcrossFilesAndAStateOfBoth(t *testing.T) {
	cfg := filesProject(t, filesConfig)
	if code, out, errs := call(t, "baseline", "-config", cfg); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	state := readFile(t, filepath.Join(filepath.Dir(cfg), "migrations", "fixture_state.yml"))
	if !strings.Contains(state, " lines of fixtures/currencies.yml -----") ||
		!strings.Contains(state, " lines of fixtures/plans.yml -----") {
		t.Fatalf("the state holds both files:\n%s", state)
	}
	plans := filepath.Join(filepath.Dir(cfg), "fixtures", "plans.yml")
	if err := os.WriteFile(plans, []byte(strings.Replace(plansFile, "2000", "2500", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := call(t, "generate", "-config", cfg, "-name", "price")
	if code != 0 || !strings.Contains(out, "Plan: 1 update") {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	if code, _, _ := call(t, "status", "-config", cfg, "-offline"); code != 0 {
		t.Fatal("after generate, nothing is uncovered")
	}
}

// dbfixture loads the files in order, so a row cannot name one of a file
// listed after its own.
func TestFileOrderIsLoadOrder(t *testing.T) {
	cfg := filesProject(t, strings.Replace(filesConfig,
		"[fixtures/currencies.yml, fixtures/plans.yml]", "[fixtures/plans.yml, fixtures/currencies.yml]", 1))
	code, _, errs := call(t, "baseline", "-config", cfg)
	if code != 1 || !strings.Contains(errs, "further down") {
		t.Fatalf("exit %d: %s", code, errs)
	}
}

// Splitting one fixture file into two changes no row, so there is nothing to
// migrate.
func TestSplittingTheFileIsNoChange(t *testing.T) {
	cfg, _ := project(t, oldFixture, oldFixture)
	if code, _, errs := call(t, "baseline", "-config", cfg); code != 0 {
		t.Fatal(errs)
	}
	dir := filepath.Dir(cfg)
	parts := strings.SplitN(oldFixture, "- model: Plan", 2)
	for name, content := range map[string]string{"a.yml": parts[0], "b.yml": "- model: Plan" + parts[1]} {
		if err := os.WriteFile(filepath.Join(dir, "fixtures", name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	split := strings.Replace(readFile(t, cfg), "fixture: fixtures/fixture.yml", "fixtures: [fixtures/a.yml, fixtures/b.yml]", 1)
	if err := os.WriteFile(cfg, []byte(split), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := call(t, "status", "-config", cfg, "-offline")
	if code != 0 || !strings.Contains(out, "not migrated  nothing") {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
}

// A file added since the git revision is empty at that revision: its rows are
// all new.
func TestAFileNewSinceTheRevisionIsEmptyThere(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	cfg := filesProject(t, filesConfig)
	dir := filepath.Dir(cfg)
	plans := filepath.Join(dir, "fixtures", "plans.yml")
	if err := os.Remove(plans); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"}, {"add", "."}, {"commit", "-q", "-m", "currencies only"}} {
		if out, err := git(dir, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(plans, []byte(plansFile), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := call(t, "generate", "-config", cfg, "-name", "plans", "-base", "HEAD", "-dry-run")
	if code != 0 || !strings.Contains(out, "Plan: 1 insert") {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
}

func TestConfigRefusesFixtureAndFixturesThatDisagree(t *testing.T) {
	cfg, _ := projectWith(t, "fixture: a.yml\n"+filesConfig, "", "")
	if code, _, errs := call(t, "status", "-config", cfg, "-offline"); code != 1 || !strings.Contains(errs, "both set") {
		t.Fatalf("exit %d: %s", code, errs)
	}
}
