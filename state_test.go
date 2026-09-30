package fixturemigrate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStateRoundTrips(t *testing.T) {
	s := State{Fixture: []byte(base), Migration: "20260921120000_fixture_prices"}
	back, err := DecodeState(s.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if string(back.Fixture) != base || back.Migration != s.Migration {
		t.Fatalf("got %+v", back)
	}
	// The body is the fixture file verbatim, so it parses as one.
	if _, err := ParseDoc(s.Encode()); err != nil {
		t.Fatalf("the state file is a fixture file with a comment on top: %v", err)
	}
}

// A checkout that turns LF into CRLF is not an edit.
func TestStateSurvivesCRLF(t *testing.T) {
	encoded := strings.ReplaceAll(string(State{Fixture: []byte(base), Migration: "baseline"}.Encode()), "\n", "\r\n")
	back, err := DecodeState([]byte(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if string(back.Fixture) != base {
		t.Fatalf("got %q", back.Fixture)
	}
}

// A state that was edited or merged line by line is not a state anybody can
// vouch for.
func TestAnEditedStateIsRefused(t *testing.T) {
	encoded := string(State{Fixture: []byte(base), Migration: "baseline"}.Encode())
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
	if err := WriteState(path, State{Fixture: []byte(base), Migration: "baseline"}); err != nil {
		t.Fatal(err)
	}
	s, err := ReadState(path)
	if err != nil || string(s.Fixture) != base {
		t.Fatalf("%v %q", err, s.Fixture)
	}
	// Nothing is left behind next to it.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("expected only the state file, got %v", entries)
	}
}
