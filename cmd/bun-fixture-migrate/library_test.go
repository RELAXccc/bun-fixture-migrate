package main

// What a command prints with -json is what the library's method returns,
// encoded: a program calling the library and one reading the command see the
// same thing. dbtest does the same for the commands that need a database.

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
)

// libraryJSON is a result as the command prints it.
func libraryJSON(t *testing.T, v any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := writeJSON(&buf, v); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func loadProject(t *testing.T, cfg string) *fixturemigrate.Project {
	t.Helper()
	p, err := fixturemigrate.LoadProject(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGenerateJSONIsTheLibrarysResult(t *testing.T) {
	cfg, base := project(t, newFixture, oldFixture)
	code, out, errs := call(t, "generate", "-config", cfg, "-old", base, "-name", "prices", "-at", "20261001120000",
		"-dry-run", "-json")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	g, err := loadProject(t, cfg).Generate(context.Background(), nil, fixturemigrate.GenerateOptions{Name: "prices",
		Old: base, At: at, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := libraryJSON(t, g); out != want {
		t.Fatalf("the command printed\n%s\nthe library returned\n%s", out, want)
	}
	for _, field := range []string{`"migration": "20261001120000_fixture_prices"`, `"dry_run": true`, `"written": []`,
		`"summary": [`, `"Plan: 1 update"`, `"refusals": []`, `"source": "package migrations`} {
		if !strings.Contains(out, field) {
			t.Errorf("no %s in\n%s", field, out)
		}
	}

	// Written, it lists what it wrote; refused, it says why and exits 2.
	code, out, errs = call(t, "generate", "-config", cfg, "-old", base, "-name", "prices", "-json")
	if code != 0 || !strings.Contains(out, "_fixture_prices.go\",") || !strings.Contains(out, "fixture_state.yml\"") ||
		strings.Contains(out, `"source"`) {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	writeFixture(t, cfg, strings.Replace(newFixture, "name: team", "name: crew", 1))
	code, out, errs = call(t, "generate", "-config", cfg, "-name", "rename", "-json")
	if code != 2 || !strings.Contains(out, `"reason": "renamed from`) || !strings.Contains(out, `"written": []`) ||
		!strings.HasPrefix(errs, "bun-fixture-migrate: 1 change refused") {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	// An error that is no refusal has no report.
	writeFixture(t, cfg, newFixture)
	if code, out, errs := call(t, "generate", "-config", cfg, "-old", base, "-json"); code != 1 || out != "" ||
		!strings.Contains(errs, "-name is required") {
		t.Fatalf("no name: exit %d\n%s%s", code, out, errs)
	}
}

func TestBaselineJSONIsTheLibrarysResult(t *testing.T) {
	cmdCfg, _ := project(t, oldFixture, oldFixture)
	libCfg, _ := project(t, oldFixture, oldFixture)
	code, out, errs := call(t, "baseline", "-config", cmdCfg, "-json")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	b, err := loadProject(t, libCfg).Baseline(context.Background(), nil, fixturemigrate.BaselineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write(); err != nil {
		t.Fatal(err)
	}
	want := strings.ReplaceAll(libraryJSON(t, b), filepath.Dir(libCfg), filepath.Dir(cmdCfg))
	if out != want {
		t.Fatalf("the command printed\n%s\nthe library returned\n%s", out, want)
	}
	if !strings.Contains(out, `"written": [`+"\n") || !strings.Contains(out, `"unchanged": false`) {
		t.Fatalf("%s", out)
	}

	// Recorded already, nothing is written; a change is refused, with what it
	// is, and exits 2.
	if code, out, _ := call(t, "baseline", "-config", cmdCfg, "-json"); code != 0 ||
		!strings.Contains(out, `"unchanged": true`) || !strings.Contains(out, `"written": []`) {
		t.Fatalf("exit %d\n%s", code, out)
	}
	writeFixture(t, cmdCfg, newFixture)
	code, out, errs = call(t, "baseline", "-config", cmdCfg, "-json")
	if code != 2 || !strings.Contains(out, `"Plan: 1 update"`) || !strings.Contains(errs, "pass -force") {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
}

func TestStatusJSONIsTheLibrarysReport(t *testing.T) {
	cfg, _ := project(t, oldFixture, oldFixture)
	if code, _, errs := call(t, "baseline", "-config", cfg); code != 0 {
		t.Fatal(errs)
	}
	writeFixture(t, cfg, newFixture)
	code, out, errs := call(t, "status", "-config", cfg, "-offline", "-json")
	if code != 3 {
		t.Fatalf("exit %d\n%s%s", code, out, errs)
	}
	r, err := loadProject(t, cfg).Status(context.Background(), nil, fixturemigrate.StatusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := libraryJSON(t, r); out != want {
		t.Fatalf("the command printed\n%s\nthe library returned\n%s", out, want)
	}
	// The exit code's sentence is the report's failures.
	if !strings.HasSuffix(errs, "bun-fixture-migrate: "+strings.Join(r.Failures, "; ")+"\n") {
		t.Fatalf("%s", errs)
	}
}

// export -json reports what export wrote, which -stdout writes instead.
func TestExportJSONAndStdoutAreOneOrTheOther(t *testing.T) {
	cfg, _ := project(t, oldFixture, oldFixture)
	code, _, errs := call(t, "export", "-config", cfg, "-json", "-stdout")
	if code != 1 || !strings.Contains(errs, "-json and -stdout both write to standard output") {
		t.Fatalf("exit %d: %s", code, errs)
	}
}
