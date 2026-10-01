package dbtest_test

// What the commands say and write, as a script or a CI job reads it: the exit
// code and the JSON agree with each other, an export is the same file every
// time, and output that does not arrive fails the command.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A finding the policy makes a warning is reported, and the database and the
// file still agree: exit 0, and "agree": true. Made an error, it is
// disagreement.
func TestCheckWarningsAreNotDisagreement(t *testing.T) {
	db := itemDB(t)
	zero := strings.Replace(itemFixture, "      production_max: 3\n", "      production_max: 0\n", 1)
	loadFixture(t, db, zero)
	// bun wrote the default for the zero; somebody put the zero back, so
	// the database holds what the file says.
	run(t, db, "UPDATE items SET production_max = 0 WHERE name = 'rope'")
	c := buildCLI(t)
	c.write("fixtures/fixture.yml", zero)

	var report struct {
		Agree    bool
		Findings []struct{ Kind, Level string }
	}
	c.write("fixture-migrate.yml", cliConfig+"policy: {zero_default: warn}\n")
	out := c.must(0, "check")
	if !strings.Contains(out, "Zero against a default:") || !strings.Contains(out, "the policy makes the findings above warnings") {
		t.Fatalf("check:\n%s", out)
	}
	_, stdout, _ := c.run("check", "-json")
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || !report.Agree || len(report.Findings) != 1 ||
		report.Findings[0].Level != "warn" {
		t.Fatalf("check -json under warn: %v\n%s", err, stdout)
	}

	c.write("fixture-migrate.yml", cliConfig)
	c.must(3, "check")
	code, stdout, _ := c.run("check", "-json")
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || code != 3 || report.Agree ||
		report.Findings[0].Level != "error" {
		t.Fatalf("check -json under error: exit %d %v\n%s", code, err, stdout)
	}
}

// Two exports of one database are the same bytes, so CI can diff an export
// against the committed file.
func TestAnExportIsTheSameEveryTime(t *testing.T) {
	db := itemDB(t)
	loadFixture(t, db, itemFixture)
	c := buildCLI(t)
	_, first, errs := c.run("export", "-stdout")
	if first == "" {
		t.Fatalf("no export:\n%s", errs)
	}
	if stamp := regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d`).FindString(first); stamp != "" {
		t.Fatalf("the export carries the time %s:\n%s", stamp, first)
	}
	if _, second, _ := c.run("export", "-stdout"); second != first {
		t.Fatalf("two exports differ:\n%s\n---\n%s", first, second)
	}
}

// Standard output on a full disk: export -stdout and scaffold, whose output
// is the product, fail instead of exiting 0 with nothing written.
func TestOutputThatDoesNotArriveFailsTheCommand(t *testing.T) {
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Skip("no /dev/full to write to")
	}
	defer full.Close()
	db := itemDB(t)
	loadFixture(t, db, itemFixture)
	c := buildCLI(t)
	for _, args := range [][]string{
		{"export", "-config", filepath.Join(c.dir, "fixture-migrate.yml"), "-stdout"},
		{"scaffold", "-dsn", os.Getenv("BUN_FIXTURE_MIGRATE_POSTGRES")},
	} {
		cmd := exec.Command(c.bin, args...)
		cmd.Env = append(os.Environ(), "BFM_TEST_DSN="+os.Getenv("BUN_FIXTURE_MIGRATE_POSTGRES"))
		var stderr strings.Builder
		cmd.Stdout, cmd.Stderr = full, &stderr
		err := cmd.Run()
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 ||
			!strings.Contains(stderr.String(), "bun-fixture-migrate: write to standard output") {
			t.Fatalf("%v: %v\n%s", args, err, stderr.String())
		}
	}
}

// A model whose table is in another schema than the configuration's
// schema: is read from there by check and export as by everything else.
func TestCommandsReadATableInAnotherSchema(t *testing.T) {
	db := connect(t)
	run(t, db, "DROP SCHEMA IF EXISTS o_catalog CASCADE", "CREATE SCHEMA o_catalog",
		"CREATE TABLE o_catalog.products (id bigint PRIMARY KEY, sku text UNIQUE NOT NULL, price bigint NOT NULL)",
		"INSERT INTO o_catalog.products VALUES (1, 'A-1', 100)")
	c := buildCLI(t)
	c.write("fixture-migrate.yml", `fixture: fixtures/fixture.yml
out: migrations
seed_guard_table: o_catalog.products
database: env:BFM_TEST_DSN
models:
  Product: {table: o_catalog.products, ref: sku, key: [sku]}
`)
	c.write("fixtures/fixture.yml", "- model: Product\n  rows:\n    - {id: 1, sku: A-1, price: 100}\n")
	c.must(0, "check")
	if out := c.must(0, "export", "-stdout"); !strings.Contains(out, `sku: "A-1"`) {
		t.Fatalf("export:\n%s", out)
	}
	c.must(0, "sync")
}

// A sync that fails before it changes anything says so and nothing else; a
// report of no changes would read as "the database already holds the files".
func TestASyncThatFailsSaysOnlyWhy(t *testing.T) {
	db := itemDB(t)
	loadFixture(t, db, itemFixture)
	c := buildCLI(t)
	c.write("fixture-migrate.yml", strings.Replace(cliConfig, "    key: [name]\n",
		"    key: [name]\n    where: \"no_such_column = 1\"\n", 1))
	for _, args := range [][]string{{"sync"}, {"sync", "-json"}} {
		code, stdout, stderr := c.run(args...)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "no_such_column") {
			t.Fatalf("%v: exit %d\n%s%s", args, code, stdout, stderr)
		}
	}
}
