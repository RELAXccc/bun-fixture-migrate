package dbtest_test

// The state file, and the commands built on it, against a real database and
// bun's real migrator.

import (
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
