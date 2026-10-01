package dbtest_test

// The state file, and the commands built on it, against a real database and
// bun's real migrator.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// generate -from-db -allow-partial records what it left out like generate
// against the state does: status fails on it until baseline -force.
func TestAPartialGenerateFromTheDatabase(t *testing.T) {
	db := itemDB(t)
	loadFixture(t, db, itemFixture)
	c := buildCLI(t)
	c.must(0, "baseline")
	edited := replaceOnce(t, itemFixture, "      cost: 120\n", "      cost: 130\n")
	c.write("fixtures/fixture.yml", replaceOnce(t, edited, `name: "rope"`, `name: "cord"`))
	c.must(2, "generate", "-from-db", "-name", "cost")
	c.must(0, "generate", "-from-db", "-name", "cost", "-allow-partial")
	if out := c.must(3, "status", "-offline"); !strings.Contains(out, "left out") || !strings.Contains(out, "renamed") {
		t.Fatalf("status:\n%s", out)
	}
	if out := c.must(0, "generate", "-name", "again"); !strings.Contains(out, "nothing changed") {
		t.Fatalf("the accepted change is not generated twice:\n%s", out)
	}
	c.must(2, "baseline")
	c.must(0, "baseline", "-force")
	c.must(0, "status", "-offline")
}

// 1.5 written 1.50 is no change in a double precision column, and only the
// database can say so. status asks it, baseline asks it, and generate records
// the new spelling, so status -offline in CI does not fail on it forever.
func TestARespelledValueIsNoChange(t *testing.T) {
	db := itemDB(t)
	loadFixture(t, db, itemFixture)
	c := buildCLI(t)
	c.must(0, "baseline")
	respelled := replaceOnce(t, itemFixture, "      ratio: 1.5\n", "      ratio: 1.50\n")
	c.write("fixtures/fixture.yml", respelled)

	// Without the column's type, 1.50 could be a string field's text.
	c.must(3, "status", "-offline")
	if out := c.must(0, "status"); !strings.Contains(out, "only in how values are written") {
		t.Fatalf("status:\n%s", out)
	}
	c.must(2, "baseline", "-offline")

	out := c.must(0, "generate", "-name", "ratio")
	if !strings.Contains(out, "nothing changed") || !strings.Contains(out, "only in how it is written") {
		t.Fatalf("generate:\n%s", out)
	}
	entries, _ := os.ReadDir(filepath.Join(c.dir, "migrations"))
	for _, e := range entries {
		if strings.Contains(e.Name(), "_fixture_") {
			t.Fatalf("a migration for no change: %s", e.Name())
		}
	}
	c.must(0, "status", "-offline")

	c.write("fixtures/fixture.yml", replaceOnce(t, respelled, "      ratio: 1.50\n", "      ratio: 1.500\n"))
	if out := c.must(0, "baseline"); !strings.Contains(out, "only in how values are written") {
		t.Fatalf("baseline:\n%s", out)
	}
	c.must(0, "status", "-offline")
}
