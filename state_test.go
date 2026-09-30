package fixturemigrate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStateRoundTrips(t *testing.T) {
	s := State{Files: []FixtureFile{{Data: []byte(base)}}, Migration: "20260921120000_fixture_prices"}
	back, err := DecodeState(s.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if string(back.Files[0].Data) != base || back.Migration != s.Migration {
		t.Fatalf("got %+v", back)
	}
	// The body is the fixture file verbatim, so it parses as one.
	if _, err := ParseDoc(s.Encode()); err != nil {
		t.Fatalf("the state file is a fixture file with a comment on top: %v", err)
	}
}

// A checkout that turns LF into CRLF is not an edit.
func TestStateSurvivesCRLF(t *testing.T) {
	encoded := strings.ReplaceAll(string(State{Files: []FixtureFile{{Data: []byte(base)}}, Migration: "baseline"}.Encode()), "\n", "\r\n")
	back, err := DecodeState([]byte(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if string(back.Files[0].Data) != base {
		t.Fatalf("got %q", back.Files[0].Data)
	}
}

// A state that was edited or merged line by line is not a state anybody can
// vouch for.
func TestAnEditedStateIsRefused(t *testing.T) {
	encoded := string(State{Files: []FixtureFile{{Data: []byte(base)}}, Migration: "baseline"}.Encode())
	edited := strings.Replace(encoded, "price_cents: 2000", "price_cents: 2500", 1)
	if _, err := DecodeState([]byte(edited)); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("expected the edit to be caught, got %v", err)
	}
	if _, err := DecodeState([]byte(base)); err == nil || !strings.Contains(err.Error(), "marker") {
		t.Fatalf("a fixture file is not a state file: %v", err)
	}
}

func TestReadAndWriteState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture_state.yml")
	if _, err := ReadState(path); !errors.Is(err, ErrNoState) {
		t.Fatalf("expected ErrNoState, got %v", err)
	}
	if err := WriteState(path, State{Files: []FixtureFile{{Data: []byte(base)}}, Migration: "baseline"}); err != nil {
		t.Fatal(err)
	}
	s, err := ReadState(path)
	if err != nil || string(s.Files[0].Data) != base {
		t.Fatalf("%v %+v", err, s.Files)
	}
	// Nothing is left behind next to it.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("expected only the state file, got %v", entries)
	}
}

// Several fixture files are one state, in their load order, under one
// checksum.
func TestAStateOfSeveralFiles(t *testing.T) {
	files := []FixtureFile{
		{Path: "fixtures/currencies.yml", Data: []byte("- model: Currency\n  rows: []\n")},
		{Path: "fixtures/plans.yml", Data: []byte("- model: Plan\n  rows: []")},
	}
	encoded := State{Files: files, Migration: "baseline"}.Encode()
	back, err := DecodeState(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !SameFiles(back.Files, files) || back.Files[1].Path != "fixtures/plans.yml" {
		t.Fatalf("got %+v", back.Files)
	}
	// Moving content between files is an edit too.
	edited := strings.Replace(string(encoded), "- model: Plan", "- model: Plans", 1)
	if _, err := DecodeState([]byte(edited)); err == nil {
		t.Fatal("an edit of the second file has to be caught")
	}
	// And a list of files parses as one document.
	doc, err := ParseFiles(back.Files)
	if err != nil || len(doc) != 2 || doc[1].Name != "Plan" {
		t.Fatalf("%v %+v", err, doc)
	}
	if SameFiles(files, files[:1]) || !SameFiles(files, back.Files) {
		t.Fatal("SameFiles")
	}
}

// A state file written for one fixture file reads as before.
func TestAStateOfOneFileKeepsItsFormat(t *testing.T) {
	encoded := string(State{Files: []FixtureFile{{Path: "fixtures/fixture.yml", Data: []byte(base)}}, Migration: "x"}.Encode())
	if !strings.Contains(encoded, stateMarker+"\n") || strings.Contains(encoded, fileMarkerStart) {
		t.Fatalf("one file keeps the single marker:\n%s", encoded)
	}
}
