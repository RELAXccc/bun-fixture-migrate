package fixturemigrate

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// Whatever Render writes, ReadChangeSet reads back as the same set. status and
// plan work from the files on disk, so a file that reads back differently from
// what it runs would make both of them lie.
func TestAGeneratedFileReadsBackAsTheSetItRuns(t *testing.T) {
	cfg := testConfig(t)
	cfg.Policy.Renames = RenameUpdate
	cfg.Models["Currency"].Deletes = DeleteCascade
	next := replace(t, base, "      price_cents: 2000\n", "      price_cents: 2500\n")
	next = replace(t, next, "      name: team\n", "      name: crew\n")
	next = replace(t, next, "      quota: 100\n", "      quota: 100\n      note: ~\n")
	next = strings.Replace(next, "- model: Feature\n  rows:\n", `- model: Currency
  rows:
    - _id: usd
      id: 2
      code: "US\"D\n"
      symbol: "$"
- model: Feature
  rows:
    - plan_id: '{{ $.Plan.free.ID }}'
      code: sso
      quota: 1
      note: "{\"sso\":true,\"why\":\"a \x60backquote\x60\"}"
    - plan_id: '{{ $.Plan.free.ID }}'
      code: audit
      quota: 1
      note: '{"audit":true}'
`, 1)
	old := replace(t, base, "      quota: 100\n", "      quota: 100\n      note: x\n")
	res := computeWith(t, cfg, old, next)
	if len(res.Refusals) != 0 {
		t.Fatalf("refusals: %+v", res.Refusals)
	}
	src, err := Render(cfg, "all kinds", "20260921120000", res)
	if err != nil {
		t.Fatal(err)
	}
	got, isFixture, err := ReadChangeSet(src)
	if err != nil || !isFixture {
		t.Fatalf("ReadChangeSet: %v, %v\n%s", isFixture, err, src)
	}
	want := fixturechange.Set{
		Name:            "20260921120000_fixture_all_kinds",
		SeedGuardTable:  cfg.SeedGuardTable,
		MigrationsTable: cfg.MigrationsTable,
		Tables:          res.Tables,
		Changes:         res.Changes,
		Policy: fixturechange.Policy{
			MissingRow: fixturechange.Mode(cfg.Policy.MissingRow),
			ChangedRow: fixturechange.Mode(cfg.Policy.ChangedRow),
			IDDrift:    fixturechange.Mode(cfg.Policy.IDDrift),
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("read back differently\n got: %+v\nwant: %+v\n%s", got, want, src)
	}
	var kinds []string
	for _, c := range got.Changes {
		kinds = append(kinds, string(c.Kind))
	}
	// JSON is written as a raw string, unless it holds a backquote.
	if !strings.Contains(string(src), "fixturechange.Lit(`{\"audit\":true}`)") ||
		!strings.Contains(string(src), `\"why\":\"a `+"`backquote`"+`\"}")`) {
		t.Fatalf("literals:\n%s", src)
	}
	if strings.Join(kinds, ",") != "update,insert,insert,insert,update,update" {
		t.Fatalf("expected a rename, inserts and updates, got %v", kinds)
	}
}

// A person dropping a change or fixing a value by hand, with the names the
// package exports rather than the ones Render uses, still reads back.
func TestAHandEditedFileReadsBack(t *testing.T) {
	src := []byte(`package migrations

import fc "github.com/RELAXccc/bun-fixture-migrate/fixturechange"

var set = fc.Set{
	Name:   "x",
	Tables: fc.Tables{"Plan": fc.Table{Name: "plans", ID: "id", Serial: false}},
	Policy: fc.Policy{MissingRow: fc.ModeWarn},
	Changes: []fc.Change{
		{Model: "Plan", Kind: "update",
			Key: fc.Values{"name": fc.Lit("team")},
			Old: fc.Values{"note": fc.Null()},
			New: fc.Values{"note": fc.Lit("x")}},
	},
}
`)
	set, isFixture, err := ReadChangeSet(src)
	if err != nil || !isFixture {
		t.Fatalf("%v %v", isFixture, err)
	}
	if set.Policy.MissingRow != fixturechange.ModeWarn || set.Changes[0].Kind != fixturechange.Update ||
		!set.Changes[0].Old["note"].IsNull {
		t.Fatalf("%+v", set)
	}
}

func TestReadChangeSetSaysWhereItStopped(t *testing.T) {
	for name, tc := range map[string]struct{ src, want string }{
		"computed value": {`package m
import "github.com/RELAXccc/bun-fixture-migrate/fixturechange"
var s = fixturechange.Set{Changes: []fixturechange.Change{{Key: fixturechange.Values{"a": value()}}}}
`, "line 3: a value that is not Lit"},
		"unknown field": {`package m
import "github.com/RELAXccc/bun-fixture-migrate/fixturechange"
var s = fixturechange.Set{
	Owner: "x",
}
`, "line 4: unknown field Owner"},
		"two sets": {`package m
import "github.com/RELAXccc/bun-fixture-migrate/fixturechange"
var a, b = fixturechange.Set{}, fixturechange.Set{}
`, "2 change sets"},
	} {
		_, isFixture, err := ReadChangeSet([]byte(tc.src))
		if !isFixture || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, %v; want %q", name, isFixture, err, tc.want)
		}
	}
	// Any other migration is not a fixture migration, and not an error.
	_, isFixture, err := ReadChangeSet([]byte("package m\n\nfunc init() {}\n"))
	if isFixture || err != nil {
		t.Fatalf("%v %v", isFixture, err)
	}
}

func TestReadMigrations(t *testing.T) {
	cfg := testConfig(t)
	src, err := Render(cfg, "prices", "20260921120000", compute(t, base,
		replace(t, base, "      price_cents: 2000\n", "      price_cents: 2500\n")))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, content := range map[string]string{
		"migrations.go":                     "package migrations\n",
		"20260921120000_fixture_prices.go":  string(src),
		"20260920000000_schema.up.sql":      "SELECT 1",
		"20260920000000_schema.down.sql":    "SELECT 1",
		"20260922000000_backfill.go":        "package migrations\n\nfunc init() {}\n",
		"20260922000000_backfill_test.go":   "package migrations\n",
		"20260923000000_other.tx.up.sql":    "SELECT 1",
		"README.md":                         "",
		"20260924000000_fixture_x.go":       "package migrations\n\nimport \"github.com/RELAXccc/bun-fixture-migrate/fixturechange\"\n\nvar x = fixturechange.Set{Name: y}\n",
		"20260924000000_same_name.down.sql": "SELECT 1",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ms, err := ReadMigrations(dir)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range ms.List {
		ids = append(ids, m.ID())
	}
	if got := strings.Join(ids, " "); got !=
		"20260920000000_schema 20260921120000_fixture_prices 20260922000000_backfill "+
			"20260923000000_other 20260924000000_fixture_x 20260924000000_same_name" {
		t.Fatalf("migrations: %s", got)
	}
	if len(ms.List[0].Files) != 2 {
		t.Fatalf("the up and down files are one migration: %+v", ms.List[0])
	}
	fixtures := ms.Fixtures()
	if len(fixtures) != 1 || fixtures[0].Fixture.Name != "20260921120000_fixture_prices" {
		t.Fatalf("fixtures: %+v", fixtures)
	}
	problems := strings.Join(ms.Problems, "\n")
	if !strings.Contains(problems, "20260924000000_fixture_x.go holds a change set this tool cannot read back") {
		t.Fatalf("the unreadable file has to be named:\n%s", problems)
	}
	if !strings.Contains(problems, "share the name 20260924000000") {
		t.Fatalf("the shared name has to be reported:\n%s", problems)
	}
}

func TestNextStampRunsAfterEverythingThere(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		existing []string
		want     string
	}{
		{nil, "20260921120000"},
		{[]string{"20260101000000", "1"}, "20260921120000"},
		// Taken: bun would record two migrations as one.
		{[]string{"20260921120000"}, "20260921120001"},
		// A migration dated ahead of this clock still runs first.
		{[]string{"20260921120000", "20260930235959"}, "20261001000000"},
		// Not a date, but fourteen digits, so it sorts like one.
		{[]string{"99999999999998"}, "99999999999999"},
	} {
		if got := NextStamp(now, tc.existing); got != tc.want {
			t.Errorf("NextStamp(%v) = %s, want %s", tc.existing, got, tc.want)
		}
	}
}

func TestCheckPackage(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if w, err := CheckPackage(dir, "migrations", "Migrations"); err != nil || len(w) != 0 {
		t.Fatalf("an empty directory: %v %v", w, err)
	}
	write("migrations.go", "package migrations\n\nvar (\n\tMigrations = 1\n)\n")
	write("x_test.go", "package migrations_test\n")
	if w, err := CheckPackage(dir, "migrations", "Migrations"); err != nil || len(w) != 0 {
		t.Fatalf("%v %v", w, err)
	}
	if w, _ := CheckPackage(dir, "migrations", "All"); len(w) != 1 || !strings.Contains(w[0], "variable All") {
		t.Fatalf("expected a warning about All, got %v", w)
	}
	if _, err := CheckPackage(dir, "db", "Migrations"); err == nil || !strings.Contains(err.Error(), "package migrations") {
		t.Fatalf("expected the package clause to be refused, got %v", err)
	}
}
