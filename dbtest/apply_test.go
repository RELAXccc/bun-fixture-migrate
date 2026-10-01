package dbtest_test

// apply, the escape hatch for migrating by hand: one generated migration run
// outside bun's migrator, and with -record recorded as the migrator records
// it, so that the migrator afterwards treats it as applied and does not run it
// again. And status, reading what the audit table says each run did.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A migrator that says what bun thinks of every migration, through
// MigrationsWithStatus, and with -migrate runs Migrate and says how many it
// ran.
const statusMigratorMain = `package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

	"github.com/RELAXccc/bun-fixture-migrate/dbtest/applymigratorcheck/migrations"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/migrate"
)

func main() {
	run := flag.Bool("migrate", false, "run Migrate after printing the status")
	flag.Parse()
	ctx := context.Background()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(os.Getenv("DSN")))), pgdialect.New())
	defer db.Close()
	m := migrate.NewMigrator(db, migrations.Migrations)
	if err := m.Init(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "init:", err)
		os.Exit(2)
	}
	ms, err := m.MigrationsWithStatus(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "status:", err)
		os.Exit(2)
	}
	for _, mig := range ms {
		fmt.Printf("status %s applied=%v group=%d\n", mig.Name, mig.IsApplied(), mig.GroupID)
	}
	if !*run {
		return
	}
	group, err := m.Migrate(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
	fmt.Printf("migrated %d\n", len(group.Migrations))
}
`

func buildStatusMigrator(t *testing.T, dir string) string {
	t.Helper()
	const pkg = "applymigratorcheck"
	t.Cleanup(func() { os.RemoveAll(pkg) })
	files := map[string]string{"main.go": statusMigratorMain, "migrations/migrations.go": migratorPackage}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if name := e.Name(); strings.HasSuffix(name, ".go") && name != "migrations.go" {
			files["migrations/"+name] = readFileT(t, filepath.Join(dir, name))
		}
	}
	for path, content := range files {
		full := filepath.Join(pkg, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(t.TempDir(), "migrator")
	if out, err := exec.Command("go", "build", "-o", bin, "./"+pkg).CombinedOutput(); err != nil {
		t.Fatalf("the migrations do not build: %v\n%s", err, out)
	}
	return bin
}

func runStatusMigrator(t *testing.T, bin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "DSN="+os.Getenv("BUN_FIXTURE_MIGRATE_POSTGRES"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the migrator: %v\n%s", err, out)
	}
	return string(out)
}

func TestApplyByHandIsWhatBunsMigratorWouldHaveDone(t *testing.T) {
	db := itemDB(t)
	run(t, db, "DROP TABLE IF EXISTS bun_migrations, bun_migration_locks, bfm_cli_audit")
	t.Cleanup(func() { run(t, db, "DROP TABLE IF EXISTS bfm_cli_audit") })
	loadFixture(t, db, itemFixture)
	c := buildCLI(t)
	c.write("fixture-migrate.yml", strings.Replace(cliConfig, "seed_guard_table: items\n",
		"seed_guard_table: items\naudit_table: bfm_cli_audit\n", 1))
	c.must(0, "baseline")
	// The audit table does not exist before a migration with one ran: that
	// is no error.
	if out := c.must(0, "status"); !strings.Contains(out, "bfm_cli_audit does not exist yet") {
		t.Fatalf("status before the first run:\n%s", out)
	}

	edited := replaceOnce(t, itemFixture, "      cost: 120\n", "      cost: 130\n")
	edited = replaceOnce(t, edited, "    - _id: rope", `    - _id: hammer
      id: 3
      region_id: '{{ $.Region.us.ID }}'
      name: "hammer"
      cost: 7
      production_max: 1
      ratio: 1
      active: true
      note: ~
    - _id: rope`)
	c.write("fixtures/fixture.yml", edited)
	c.must(0, "generate", "-name", "hammer", "-at", "20300101000000")
	file := filepath.Join(c.dir, "migrations", "20300101000000_fixture_hammer.go")
	if src := readFileT(t, file); !strings.Contains(src, `AuditTable:      "bfm_cli_audit"`) {
		t.Fatalf("the migration carries the audit table:\n%s", src)
	}
	bin := buildStatusMigrator(t, filepath.Join(c.dir, "migrations"))

	// Without -yes it is plan -file: the same report, and nothing changed.
	out := c.must(0, "apply", "-file", file)
	if !strings.Contains(out, "20300101000000_fixture_hammer: would succeed") ||
		!strings.Contains(out, "applied Item name=hammer insert (1 row)") ||
		!strings.Contains(out, "rolled back: nothing was changed") ||
		!strings.Contains(out, "run it again with -yes") {
		t.Fatalf("apply without -yes:\n%s", out)
	}
	if got := scan[int64](t, db, "SELECT count(*) FROM items WHERE name = 'hammer' OR cost = 130"); got != 0 {
		t.Fatal("apply without -yes changed the database")
	}
	// -record needs bun's table, which the migrator's Init creates.
	if out := c.must(1, "apply", "-file", file, "-yes", "-record"); !strings.Contains(out, "bun_migrations does not exist") {
		t.Fatalf("apply -record without the migrations table:\n%s", out)
	}
	if out := runStatusMigrator(t, bin); !strings.Contains(out, "status 20300101000000 applied=false") {
		t.Fatalf("before apply:\n%s", out)
	}

	// An admin edited the anvil, so its update is skipped; the hammer goes in.
	run(t, db, "UPDATE items SET cost = 125 WHERE name = 'anvil'")
	out = c.must(0, "apply", "-file", file, "-yes", "-record")
	if !strings.Contains(out, "applied Item name=hammer insert (1 row)") ||
		!strings.Contains(out, "skipped Item name=anvil update [changed row]") ||
		!strings.Contains(out, "20300101000000_fixture_hammer: applied and recorded as applied (group 1), committed") {
		t.Fatalf("apply -yes -record:\n%s", out)
	}
	if got := scan[string](t, db, "SELECT name || ' ' || group_id FROM bun_migrations"); got != "20300101000000 1" {
		t.Fatalf("bun_migrations: %s", got)
	}

	// bun's own migrator takes the record for its own: the migration is
	// applied, in group 1, and Migrate has nothing to run.
	out = runStatusMigrator(t, bin, "-migrate")
	if !strings.Contains(out, "status 20300101000000 applied=true group=1") || !strings.Contains(out, "migrated 0") ||
		strings.Contains(out, "fixture change") || strings.Contains(out, "applied (") {
		t.Fatalf("bun's migrator after apply -record:\n%s", out)
	}
	if out := c.must(2, "apply", "-file", file, "-yes", "-record"); !strings.Contains(out, "is recorded in bun_migrations already") {
		t.Fatalf("a second apply -record:\n%s", out)
	}

	// status reads the audit table: what the run did here, and that the file
	// is the one that ran.
	out = c.must(0, "status")
	if !strings.Contains(out, "what the fixture migrations did here, according to bfm_cli_audit") ||
		!strings.Contains(out, "20300101000000_fixture_hammer: applied") ||
		!strings.Contains(out, "1 applied, 0 unchanged, 1 skipped") ||
		!strings.Contains(out, "skipped Item name=anvil update [changed row]") || strings.Contains(out, "edited after") {
		t.Fatalf("status with the audit table:\n%s", out)
	}
	var report struct {
		Migrations []struct {
			ID    string
			Audit *struct {
				Direction                   string
				Applied, Unchanged, Skipped int
				SkippedChanges              []struct {
					Index                     int
					Model, Key, Kind, Problem string
				} `json:"skipped_changes"`
				Edited bool
			}
		}
		Database struct {
			Audit struct {
				Table  string
				Exists bool
			}
		}
	}
	_, stdout, _ := c.run("status", "-json")
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || len(report.Migrations) != 1 {
		t.Fatalf("status -json: %v\n%s", err, stdout)
	}
	a := report.Migrations[0].Audit
	if a == nil || a.Direction != "up" || a.Applied != 1 || a.Skipped != 1 || len(a.SkippedChanges) != 1 ||
		a.SkippedChanges[0].Problem != "changed row" || a.SkippedChanges[0].Model != "Item" || a.Edited ||
		report.Database.Audit.Table != "bfm_cli_audit" || !report.Database.Audit.Exists {
		t.Fatalf("status -json:\n%s", stdout)
	}

	// Edited after it ran here.
	src := readFileT(t, file)
	c.write("migrations/20300101000000_fixture_hammer.go", strings.Replace(src, `fixturechange.Lit("7")`,
		`fixturechange.Lit("8")`, 1))
	out = c.must(0, "status")
	if !strings.Contains(out, "edited after it ran here") {
		t.Fatalf("status of an edited migration:\n%s", out)
	}
	c.write("migrations/20300101000000_fixture_hammer.go", src)

	// Reverted by hand, with its record: only the hammer the run inserted
	// goes, and bun runs the migration again.
	out = c.must(0, "apply", "-file", file, "-revert")
	if !strings.Contains(out, "Revert inverts the 1 change bfm_cli_audit says the migration made") ||
		!strings.Contains(out, "applied Item name=hammer delete (1 row)") {
		t.Fatalf("apply -revert without -yes:\n%s", out)
	}
	out = c.must(0, "apply", "-file", file, "-revert", "-yes", "-record")
	if !strings.Contains(out, "reverted and its record deleted, committed") ||
		!strings.Contains(out, "not reverted: the migration did not make it in this database") {
		t.Fatalf("apply -revert -yes -record:\n%s", out)
	}
	if got := scan[string](t, db, "SELECT string_agg(name || '=' || cost, ' ' ORDER BY name) FROM items"); got != "anvil=125 rope=0" {
		t.Fatalf("after the revert: %s", got)
	}
	if out := c.must(2, "apply", "-file", file, "-revert", "-yes", "-record"); !strings.Contains(out, "is not recorded") {
		t.Fatalf("a second revert -record:\n%s", out)
	}
	out = runStatusMigrator(t, bin, "-migrate")
	if !strings.Contains(out, "status 20300101000000 applied=false") || !strings.Contains(out, "migrated 1") {
		t.Fatalf("bun's migrator after apply -revert -record:\n%s", out)
	}
	if out := c.must(0, "status"); !strings.Contains(out, "20300101000000_fixture_hammer: applied") {
		t.Fatalf("status after the migrator ran it again:\n%s", out)
	}
}
