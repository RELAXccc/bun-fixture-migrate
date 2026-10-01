package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What apply refuses before it connects: no file, a file that is not a
// generated migration, and -record for a file bun would not name.
func TestApplyRefusesWhatItCannotRun(t *testing.T) {
	cfg, _ := project(t, oldFixture, oldFixture)
	dir := filepath.Dir(cfg)
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	set := `package migrations

import "github.com/RELAXccc/bun-fixture-migrate/fixturechange"

var set = fixturechange.Set{Name: "x", Tables: fixturechange.Tables{"Plan": {Name: "plans", ID: "id"}}}
`
	for want, args := range map[string][]string{
		"apply needs -file":           {},
		"a .go file":                  {"-file", write("20260101000000_up.sql", "SELECT 1")},
		"holds no fixture change set": {"-file", write("20260101000000_other.go", "package migrations\n")},
		"no such file":                {"-file", filepath.Join(dir, "20260101000000_gone.go")},
		"no name to record it under":  {"-file", write("fixture_x.go", set), "-record"},
		"Tables is not written out as a literal": {"-file", write("20260101000001_bad.go",
			strings.Replace(set, `Tables: fixturechange.Tables{"Plan": {Name: "plans", ID: "id"}}`, "Tables: tables()", 1))},
		"flag provided but not defined: -y": {"-file", write("20260101000002_x.go", set), "-y"},
	} {
		code, stdout, stderr := call(t, append([]string{"apply", "-config", cfg}, args...)...)
		if code != 1 || stdout != "" || !strings.Contains(stderr, want) {
			t.Errorf("%v: exit %d, want 1 and %q\n%s%s", args, code, want, stdout, stderr)
		}
	}
	// It connects to the database the configuration or -dsn names, like
	// every command that writes there.
	code, _, stderr := call(t, "apply", "-h")
	if code != 0 || !strings.Contains(stderr, "-dsn") || !strings.Contains(stderr, "-record") ||
		!strings.Contains(stderr, "-revert") || !strings.Contains(stderr, "-yes") {
		t.Fatalf("apply -h: exit %d\n%s", code, stderr)
	}
}
