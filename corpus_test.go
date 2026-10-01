package fixturemigrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// A generated migration lives in an application's repository for good, and is
// read back by status and plan, and compiled against fixtureapply, by every
// later version of this tool. testdata/generated keeps files that earlier
// versions wrote, exactly as they wrote them: one directory each, named after
// the commit that wrote it, with the database it runs against (setup.sql) and
// what to compare before and after (dump.sql). The dbtest module compiles and
// runs every one of them under bun's migrator.
//
// Never regenerate a file there; a version that writes a new shape adds a
// directory of its own.
func corpus(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "generated", "*", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 4 {
		t.Fatalf("the corpus of generated files is missing: %v", files)
	}
	return files
}

func TestEveryGeneratedFileReadsBackAndValidates(t *testing.T) {
	for _, path := range corpus(t) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		set, isFixture, err := ReadChangeSet(src)
		if err != nil || !isFixture {
			t.Fatalf("%s: %v, %v", path, isFixture, err)
		}
		if err := fixtureapply.Validate(set); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if want := strings.TrimSuffix(filepath.Base(path), ".go"); set.Name != want || len(set.Changes) == 0 {
			t.Fatalf("%s: read back as %q with %d changes", path, set.Name, len(set.Changes))
		}
		if set.Format != 0 && set.Format != fixturechange.CurrentFormat {
			t.Fatalf("%s: format %d", path, set.Format)
		}
		for _, name := range []string{"setup.sql", "dump.sql"} {
			if _, err := os.Stat(filepath.Join(filepath.Dir(path), name)); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
		}
	}
}
