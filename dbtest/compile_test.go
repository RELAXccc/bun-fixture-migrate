package dbtest_test

// The generated file has to compile, and against a real migrator.
//
// The library's own test parses it and checks it is gofmt-clean, which catches
// a file that is not Go. It does not catch one that names a field that has
// since been renamed, imports a package that moved, or registers itself with a
// signature bun no longer has -- and those are exactly what changes here break.
// The one way to know is to build it.
//
// It needs no database, only a Go toolchain and the module cache this module
// already uses.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
)

const migratorFile = `package buildcheck

import "github.com/uptrace/bun/migrate"

// Migrations is what the generated file registers with, spelled the way the
// configuration says.
var Migrations = migrate.NewMigrations()
`

func TestTheGeneratedFileCompiles(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no Go toolchain to build with")
	}
	cfg := itemConfig(t)
	cfg.Package = "buildcheck"
	cfg.Migrator = "Migrations"

	old := fixtureSnapshot(t, cfg, itemFixture, "HEAD:fixture.yml")
	// One of each kind, so the file carries an insert with an explicit id and a
	// reference, an update, and a delete.
	changed := replaceOnce(t, itemFixture, "      cost: 120\n", "      cost: 130\n")
	changed = replaceOnce(t, changed, `    - _id: rope`, `    - _id: hammer
      id: 3
      region_id: '{{ $.Region.us.ID }}'
      name: "hammer"
      cost: 7
      production_max: 1
      ratio: 1
      active: true
      note: ~
    - _id: rope`)
	head := fixtureSnapshot(t, cfg, changed, "fixture.yml")

	res, err := fixturemigrate.Compute(cfg, old, head)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if len(res.Refusals) != 0 {
		t.Fatalf("unexpected refusals: %+v", res.Refusals)
	}
	src, err := fixturemigrate.Render(cfg, "item costs", "20260921120000", res)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// The package is built inside this module so it resolves bun and the
	// library through the module's own go.mod, with no network and no second
	// go.sum to keep in step.
	dir := "buildcheck"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	for name, content := range map[string][]byte{
		"migrator.go": []byte(migratorFile),
		fixturemigrate.FileName("20260921120000", "item costs"): src,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command("go", "build", "./"+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the generated migration does not build: %v\n%s\n%s", err, out, src)
	}
}

// replaceOnce is strings.Replace with a test failure when it matched nothing,
// so a fixture edited out from under one of these tests says so.
func replaceOnce(t *testing.T, text, old, new string) string {
	t.Helper()
	out := strings.Replace(text, old, new, 1)
	if out == text {
		t.Fatalf("%q is not in the fixture any more", old)
	}
	return out
}
