package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
)

// The example project is what a reader copies, so it has to be what the tool
// would leave: its fixture file fully migrated, its migrations directory
// readable, and its generated migration the one generate writes today.
func TestTheExampleProjectIsConsistent(t *testing.T) {
	config := filepath.Join("..", "..", "examples", "basic", "fixture-migrate.yml")
	code, stdout, stderr := call(t, "status", "-offline", "-config", config)
	if code != 0 {
		t.Fatalf("status: exit %d\n%s%s", code, stdout, stderr)
	}
	for _, want := range []string{
		"not migrated  nothing",
		"20260930160000_schema",
		"20260930165255_fixture_plan_prices  fixture, 3 changes",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status does not say %q:\n%s", want, stdout)
		}
	}

	// The migration reads back and renders to the same bytes: an example
	// written by an older generate would teach an older format.
	path := filepath.Join("..", "..", "examples", "basic", "migrations", "20260930165255_fixture_plan_prices.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	set, _, err := fixturemigrate.ReadChangeSet(src)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := fixturemigrate.LoadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	order, err := cfg.DependencyOrder()
	if err != nil {
		t.Fatal(err)
	}
	again, err := fixturemigrate.Render(cfg, "plan prices", "20260930165255", &fixturemigrate.Result{
		Changes: set.Changes, Tables: set.Tables, Order: order,
		Base: "the state after baseline", Head: "fixtures/fixture.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(src) {
		t.Fatalf("generate writes it differently now; regenerate the example:\n%s", again)
	}

	// So does the state file, comment and all: one in an older format, or
	// with an older comment, would teach the merge advice that went with it.
	statePath := filepath.Join("..", "..", "examples", "basic", "migrations", "fixture_state.yml")
	state, err := fixturemigrate.ReadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.Format != fixturemigrate.StateFormat || state.Covered() != "20260930165255_fixture_plan_prices" {
		t.Fatalf("the example's state file: %+v", state)
	}
	if data, _ := os.ReadFile(statePath); string(data) != string(state.Encode()) {
		t.Fatalf("the example's state file is not what the tool writes now:\n%s", state.Encode())
	}
}
