package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
	"github.com/RELAXccc/bun-fixture-migrate/internal/pgerr"
)

// A literal, NULL and a reference have to be told apart by a program, which
// the text report cannot do for the literal "NULL".
func TestCheckJSONTellsValuesApart(t *testing.T) {
	res := &fixturemigrate.CheckResult{Result: &fixturemigrate.Result{Changes: []fixturechange.Change{{
		Model: "Plan", Kind: fixturechange.Update,
		Key: fixturechange.Values{"name": fixturechange.Lit("team")},
		Old: fixturechange.Values{"note": fixturechange.Lit("NULL"), "currency_id": fixturechange.RefTo("Currency", "EUR")},
		New: fixturechange.Values{"note": fixturechange.Null(), "currency_id": fixturechange.RefTo("Currency", "USD")},
	}}}}
	var buf bytes.Buffer
	if err := writeJSON(&buf, checkJSON(policyConfig(t, ""), res)); err != nil {
		t.Fatal(err)
	}
	var back struct {
		Agree   bool
		Changes []struct {
			Database map[string]any
			File     map[string]any
		}
	}
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	c := back.Changes[0]
	if back.Agree || c.Database["note"] != "NULL" || c.File["note"] != nil {
		t.Fatalf("%s", buf.String())
	}
	ref, ok := c.File["currency_id"].(map[string]any)
	if !ok || ref["model"] != "Currency" || ref["key"] != "USD" {
		t.Fatalf("a reference is an object: %s", buf.String())
	}
	if jsonValues(nil) != nil {
		t.Fatal("no values is no object")
	}
}

// policyConfig is a prepared configuration with one policy line.
func policyConfig(t *testing.T, policy string) *fixturemigrate.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.yml")
	if err := os.WriteFile(path, []byte("fixture: f.yml\n"+policy+"models:\n  Plan: {table: plans}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := fixturemigrate.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// A finding the policy makes a warning is reported and is not disagreement,
// in the JSON as in the exit code; one it makes an error is.
func TestCheckJSONAgreesDespiteAWarning(t *testing.T) {
	res := &fixturemigrate.CheckResult{Result: &fixturemigrate.Result{}, Findings: []fixturemigrate.Finding{
		{Kind: fixturemigrate.FindingZeroDefault, Model: "Plan", Row: "name=x", Detail: "a zero"}}}
	for policy, want := range map[string]checkFinding{
		"policy: {zero_default: warn}\n":  {Kind: "zero against a default", Level: "warn"},
		"policy: {zero_default: error}\n": {Kind: "zero against a default", Level: "error"},
	} {
		report := checkJSON(policyConfig(t, policy), res)
		if len(report.Findings) != 1 || report.Findings[0].Level != want.Level ||
			report.Agree != (want.Level == "warn") {
			t.Errorf("%s: %+v", policy, report)
		}
	}
}

func TestResolveDSN(t *testing.T) {
	t.Setenv("BFM_TEST_DSN_SET", "postgres://x")
	if dsn, err := resolveDSN("env:BFM_TEST_DSN_SET"); err != nil || dsn != "postgres://x" {
		t.Fatalf("%q %v", dsn, err)
	}
	if _, err := resolveDSN("env:BFM_TEST_DSN_UNSET"); err == nil || !strings.Contains(err.Error(), "BFM_TEST_DSN_UNSET") {
		t.Fatalf("an unset variable is named: %v", err)
	}
	if _, err := resolveDSN(""); err == nil {
		t.Fatal("no DSN is an error")
	}
	if _, err := resolveDSN("env:"); err == nil || !strings.Contains(err.Error(), "write env:NAME") ||
		strings.Contains(err.Error(), "variable ,") {
		t.Fatalf("env: names no variable: %v", err)
	}
	if dsn, _ := resolveDSN("postgres://y"); dsn != "postgres://y" {
		t.Fatal(dsn)
	}
}

func TestRedactAndScrub(t *testing.T) {
	u, _ := url.Parse("postgres://app:p%40ss@db:5432/app?sslmode=require&password=q1&application_name=x")
	got := redact(u)
	if strings.Contains(got, "p%40ss") || strings.Contains(got, "q1") || !strings.Contains(got, "application_name=x") {
		t.Fatalf("redact: %s", got)
	}
	msg := scrub("auth failed for p@ss and p%40ss and q1", u)
	if strings.Contains(msg, "p@ss") || strings.Contains(msg, "p%40ss") || strings.Contains(msg, "q1") {
		t.Fatalf("scrub: %s", msg)
	}
}

func TestInconclusiveErrors(t *testing.T) {
	for _, err := range []error{context.Canceled, fmt.Errorf("x: %w", context.DeadlineExceeded)} {
		if !inconclusive(err) {
			t.Errorf("%v is no verdict on the migration", err)
		}
	}
	if inconclusive(errors.New("no row of items has name=anvil")) {
		t.Fatal("a missing row is a verdict")
	}
	if pgerr.State(errors.New("x")) != "" {
		t.Fatal("no SQLSTATE in a plain error")
	}
}

func TestFileListAndUpSQL(t *testing.T) {
	var f fileList
	f.Set("a.go")
	f.Set("b.go")
	if f.String() != "a.go,b.go" {
		t.Fatal(f.String())
	}
	m := fixturemigrate.MigrationFile{Files: []string{"x/1_a.down.sql", "x/1_a.up.sql"}}
	if upSQL(m) != "x/1_a.up.sql" || upSQL(fixturemigrate.MigrationFile{Files: []string{"x/1_a.go"}}) != "" {
		t.Fatal("upSQL")
	}
}

func TestPrintPlanSaysWhatHappened(t *testing.T) {
	var out bytes.Buffer
	o := streams{ctx: context.Background(), stdout: &out, stderr: &out}
	printPlan(o, &planReport{
		RowsLocked: 1, LockedSeconds: 7.25,
		NotSimulated: []string{"20260101000000_backfill"},
		Notes:        []string{"20260101000000_backfill.up.sql holds a template"},
		Problems:     []string{"a.go and b.sql share the name 1"},
		Migrations: []plannedMigration{
			{ID: "1_schema", Kind: "sql", Result: "succeeds", Notes: []string{"a blank line"}},
			{ID: "2_fixture_a", Kind: "fixture", Result: "fails", Error: "boom", After: []string{"20260101000000_backfill"},
				Changes: []fixtureapply.Outcome{
					{Index: 0, Model: "Plan", Key: "name=pro", Kind: fixturechange.Insert, Status: fixtureapply.StatusApplied, Rows: 1},
					{Index: 1, Model: "Plan", Key: "name=team", Kind: fixturechange.Update, Status: fixtureapply.StatusFailed,
						Problem: fixtureapply.ProblemMissingRow, Message: "gone"},
					{Index: -1, Model: "Plan", Status: fixtureapply.StatusSequence, Message: "moved"},
				}},
			{ID: "3_fixture_b", Kind: "fixture", Result: "not reached"},
			{ID: "4_fixture_c", Kind: "fixture", Result: "inconclusive", Error: "lock timeout"},
			{ID: "5_fixture_d", Kind: "fixture", Result: "unseeded"},
		},
	})
	text := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{
		"1_schema (SQL): would succeed", "2_fixture_a: would FAIL", "applied Plan name=pro insert (1 row)",
		"failed Plan name=team update [missing row]: gone", "sequence moved", "boom",
		"pending before it and not simulated: 20260101000000_backfill", "3_fixture_b: not reached",
		"4_fixture_c: could not be planned", "5_fixture_d: would do nothing", "not simulated, not fixture migrations",
		"note: a blank line", "note: 20260101000000_backfill.up.sql holds a template",
		"problems a.go and b.sql share the name 1",
		"except sequences, which no rollback undoes",
		"for 7.25s it held locked the 1 row it wrote and what the SQL migrations locked",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}

	// The error of a migration that failed on one change is that change's
	// message, which is printed once.
	out.Reset()
	printPlan(o, &planReport{Migrations: []plannedMigration{{ID: "2_fixture_a", Kind: "fixture", Result: "fails",
		Error: "2_fixture_a: Plan name=team update: no row of plans has name=team",
		Changes: []fixtureapply.Outcome{{Index: 0, Model: "Plan", Key: "name=team", Kind: fixturechange.Update,
			Status: fixtureapply.StatusFailed, Problem: fixtureapply.ProblemMissingRow, Message: "no row of plans has name=team"}}}}})
	if n := strings.Count(out.String(), "no row of plans"); n != 1 {
		t.Errorf("the failure is printed %d times:\n%s", n, out.String())
	}
	if strings.Contains(out.String(), "an id an insert drew") || strings.Contains(out.String(), "held locked") {
		t.Errorf("nothing was inserted:\n%s", out.String())
	}

	// Fixture migrations alone: an insert's id, and the rows it held.
	out.Reset()
	printPlan(o, &planReport{RowsLocked: 2, LockedSeconds: 0.042, Migrations: []plannedMigration{{ID: "2_fixture_a",
		Kind: "fixture", Result: "succeeds", Changes: []fixtureapply.Outcome{{Index: 0, Model: "Plan", Key: "name=pro",
			Kind: fixturechange.Insert, Status: fixtureapply.StatusApplied, Rows: 2}}}}})
	text = strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{"rolled back: nothing was changed, except that an id an insert drew",
		"for 42ms it held locked the 2 rows it wrote; other sessions writing them waited"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}

func TestPrintSync(t *testing.T) {
	var out bytes.Buffer
	o := streams{ctx: context.Background(), stdout: &out, stderr: &out}
	printSync(o, syncReport{DryRun: true, Changes: []fixtureapply.Outcome{
		{Index: 0, Model: "Plan", Key: "name=pro", Kind: fixturechange.Insert, Status: fixtureapply.StatusApplied}}})
	if !strings.Contains(out.String(), "would apply  Plan name=pro insert") {
		t.Fatalf("%s", out.String())
	}
	out.Reset()
	printSync(o, syncReport{})
	if !strings.Contains(out.String(), "already holds") {
		t.Fatalf("%s", out.String())
	}
	out.Reset()
	printSync(o, syncReport{Applied: true, Refusals: []checkRefusal{{"Plan", "x", "renamed"}},
		Findings: []checkFinding{{Kind: "invalid value", Model: "Plan", Row: "x", Detail: "bad"}},
		Changes:  []fixtureapply.Outcome{{Index: 0, Status: fixtureapply.StatusApplied}}})
	for _, want := range []string{"refused: Plan x: renamed", "invalid value: Plan x: bad", "applied 1 change"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
}

// Every command that talks to a database says so before connecting when
// there is none.
func TestCommandsThatNeedADatabaseSaySo(t *testing.T) {
	cfg, _ := project(t, oldFixture, oldFixture)
	for _, cmd := range []string{"check", "export", "plan", "sync"} {
		code, _, errs := call(t, cmd, "-config", cfg)
		if code != 1 || !strings.Contains(errs, "no database in the configuration") {
			t.Errorf("%s: exit %d: %s", cmd, code, errs)
		}
	}
	if code, _, errs := call(t, "scaffold"); code != 1 || !strings.Contains(errs, "DATABASE_URL") {
		t.Errorf("scaffold: exit %d: %s", code, errs)
	}
	if code, _, errs := call(t, "status", "-config", cfg, "-require-applied"); code != 1 || !strings.Contains(errs, "needs the database") {
		t.Errorf("status -require-applied: exit %d: %s", code, errs)
	}
}

// export with several files writes each in place, so one -o cannot hold them.
func TestExportOfSeveralFilesRefusesOneOutput(t *testing.T) {
	cfg := filesProject(t, filesConfig)
	code, _, errs := call(t, "export", "-config", cfg, "-o", filepath.Join(t.TempDir(), "x.yml"))
	if code != 1 || !strings.Contains(errs, "-o writes one file") {
		t.Fatalf("exit %d: %s", code, errs)
	}
}

func TestScaffoldDoesNotOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture-migrate.yml")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL", "postgres://u:p@127.0.0.1:1/x?sslmode=disable")
	if code, _, errs := call(t, "scaffold", "-o", path); code != 1 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if got, _ := os.ReadFile(path); string(got) != "x" {
		t.Fatal("scaffold overwrote a file")
	}
}

// An export is written anew from the database, so the comments of the file
// it replaces are counted, to say they are gone.
func TestDroppedComments(t *testing.T) {
	old := "# master data\n- model: Plan\n  rows:\n    # the cheap one\n    - name: free # forever\n      note: \"a # b\"\n"
	exported := "# Exported by bun-fixture-migrate from a live database.\n- model: Plan\n  rows:\n    - name: free\n" +
		"      note: \"a # b\"\n"
	if n := droppedComments([]byte(old), []byte(exported)); n != 3 {
		t.Fatalf("got %d, want the three comments", n)
	}
	if n := droppedComments([]byte(exported), []byte(exported)); n != 0 {
		t.Fatalf("the export's own header is kept: %d", n)
	}
}
