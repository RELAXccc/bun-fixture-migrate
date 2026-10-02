package dbtest_test

// Every generated file kept in testdata/generated, compiled against today's
// fixtureapply and run by bun's migrator: up, down, and up again. A generated
// migration lives in an application's repository for good, and each later
// version of this module has to compile and run it the way it was meant.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
)

const corpusMigrations = `package migrations

import "github.com/uptrace/bun/migrate"

var Migrations = migrate.NewMigrations()
`

const corpusMain = `package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

%s
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/migrate"
)

var sets = []*migrate.Migrations{%s}

func main() {
	set := flag.Int("set", 0, "which directory of the corpus")
	down := flag.Bool("down", false, "roll back instead of migrating")
	flag.Parse()
	ctx := context.Background()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(os.Getenv("DSN")))), pgdialect.New())
	defer db.Close()
	m := migrate.NewMigrator(db, sets[*set])
	if err := m.Init(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "init:", err)
		os.Exit(2)
	}
	var err error
	if *down {
		_, err = m.Rollback(ctx)
	} else {
		_, err = m.Migrate(ctx)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}
`

func TestEveryGeneratedFileCompilesAndRuns(t *testing.T) {
	db := connect(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no Go toolchain to build with")
	}
	dirs, err := filepath.Glob(filepath.Join("..", "testdata", "generated", "*"))
	if err != nil || len(dirs) < 4 {
		t.Fatalf("the corpus of generated files is missing: %v %v", dirs, err)
	}
	sort.Strings(dirs)

	// One package per directory, all built into one program, inside this
	// module so it resolves bun and the library through its own go.mod.
	build := "corpuscheck"
	t.Cleanup(func() { os.RemoveAll(build) })
	var imports, sets, files []string
	for i, dir := range dirs {
		found, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(found) != 1 {
			t.Fatalf("%s: want one generated file, got %v %v", dir, found, err)
		}
		files = append(files, found[0])
		pkg := filepath.Join(build, fmt.Sprintf("c%d", i))
		if err := os.MkdirAll(pkg, 0o755); err != nil {
			t.Fatal(err)
		}
		src, err := os.ReadFile(found[0])
		if err != nil {
			t.Fatal(err)
		}
		for name, content := range map[string][]byte{"migrations.go": []byte(corpusMigrations), filepath.Base(found[0]): src} {
			if err := os.WriteFile(filepath.Join(pkg, name), content, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		imports = append(imports, fmt.Sprintf("\tc%d \"github.com/RELAXccc/bun-fixture-migrate/dbtest/%s/c%d\"", i, build, i))
		sets = append(sets, fmt.Sprintf("c%d.Migrations", i))
	}
	main := fmt.Sprintf(corpusMain, strings.Join(imports, "\n")+"\n", strings.Join(sets, ", "))
	if err := os.WriteFile(filepath.Join(build, "main.go"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "corpus")
	if out, err := exec.Command("go", "build", "-o", bin, "./"+build).CombinedOutput(); err != nil {
		t.Fatalf("a generated file of an earlier version does not build: %v\n%s", err, out)
	}

	for i, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			ctx := context.Background()
			setup, err := os.ReadFile(filepath.Join(dir, "setup.sql"))
			if err != nil {
				t.Fatal(err)
			}
			for _, stmt := range strings.Split(string(setup), ";\n") {
				if strings.TrimSpace(stmt) == "" {
					continue
				}
				if _, err := db.ExecContext(ctx, stmt); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
			}
			queries, err := os.ReadFile(filepath.Join(dir, "dump.sql"))
			if err != nil {
				t.Fatal(err)
			}
			dump := func() string {
				var out []string
				for _, q := range strings.Split(strings.TrimSpace(string(queries)), ";\n") {
					out = append(out, scan[string](t, db, strings.TrimSuffix(q, ";")))
				}
				return strings.Join(out, "\n")
			}
			migrate := func(what string, args ...string) {
				t.Helper()
				cmd := exec.Command(bin, append([]string{"-set", fmt.Sprint(i)}, args...)...)
				cmd.Env = append(os.Environ(), "DSN="+os.Getenv("BUN_FIXTURE_MIGRATE_POSTGRES"))
				var out bytes.Buffer
				cmd.Stdout, cmd.Stderr = &out, &out
				if err := cmd.Run(); err != nil {
					t.Fatalf("%s: %v\n%s", what, err, out.String())
				}
				if strings.Contains(out.String(), "SKIPPED") || strings.Contains(out.String(), "nothing to") ||
					!strings.Contains(out.String(), "applied (1 row)") {
					t.Fatalf("%s has to make every change:\n%s", what, out.String())
				}
			}
			before := dump()
			migrate("up")
			after := dump()
			if after == before {
				t.Fatalf("up changed nothing:\n%s", after)
			}
			migrate("down", "-down")
			if got := dump(); got != before {
				t.Fatalf("down did not restore the database\n--- got ---\n%s\n--- want ---\n%s", got, before)
			}
			migrate("up again")
			if got := dump(); got != after {
				t.Fatalf("up after down differs\n--- got ---\n%s\n--- want ---\n%s", got, after)
			}
			// A set with an audit table recorded each of the three runs.
			src, err := os.ReadFile(files[i])
			if err != nil {
				t.Fatal(err)
			}
			if set, _, err := fixturemigrate.ReadChangeSet(src); err != nil {
				t.Fatal(err)
			} else if set.AuditTable != "" {
				got := scan[string](t, db, "SELECT string_agg(direction, ',' ORDER BY id) FROM "+set.AuditTable+
					" WHERE set_name = ?", set.Name)
				if got != "up,down,up" {
					t.Fatalf("%s holds %q", set.AuditTable, got)
				}
			}
		})
	}
}
