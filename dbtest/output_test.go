package dbtest_test

// What the commands say and write, as a script or a CI job reads it: the exit
// code and the JSON agree with each other, an export is the same file every
// time, and output that does not arrive fails the command.

import (
	"encoding/json"
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
