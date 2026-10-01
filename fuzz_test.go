package fixturemigrate

import (
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
)

// The readers below take files a person edits, merges and resolves conflicts
// in. Whatever they are handed, they return an error rather than panic, and
// what they accept means one thing.

// A state file that decodes encodes back to one that decodes the same.
func FuzzDecodeState(f *testing.F) {
	f.Add(State{Files: []FixtureFile{{Data: []byte(base)}}, Migration: "baseline"}.Encode())
	f.Add(State{Files: []FixtureFile{{Path: "a.yml", Data: []byte("[]")}, {Path: "b.yml", Data: []byte("- model: X\n")}},
		Migration: "20260101000000_fixture_x"}.Encode())
	f.Add(State{Files: []FixtureFile{{Path: "a.yml", Data: []byte("# ----- 1 line of b.yml -----\n")}},
		Migration: "20260101000000_fixture_x", Base: "20250101000000_fixture_w", LeftOut: []string{"X a: b", "\"c\n"}}.Encode())
	f.Add(State{Migration: "baseline"}.Encode())
	f.Add([]byte(stateMarker + "\n"))
	for _, golden := range []string{"state-format1-one-file.yml", "state-format1-several-files.yml"} {
		data, err := os.ReadFile(filepath.Join("testdata", golden))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := DecodeState(data)
		if err != nil {
			return
		}
		again, err := DecodeState(s.Encode())
		if err != nil {
			t.Fatalf("a decoded state does not decode once encoded: %v", err)
		}
		if !SameFiles(again.Files, s.Files) || again.Migration != s.Migration || again.Base != s.Base ||
			strings.Join(again.LeftOut, "\x00") != strings.Join(s.LeftOut, "\x00") || len(again.LeftOut) != len(s.LeftOut) {
			t.Fatalf("%+v became %+v", s, again)
		}
		for i := range s.Files {
			if len(s.Files) > 1 && again.Files[i].Path != s.Files[i].Path {
				t.Fatalf("%q became %q", s.Files[i].Path, again.Files[i].Path)
			}
		}
	})
}

// A migration file that reads as a change set is one Apply would accept or
// refuse up front, never one it trips over.
func FuzzReadChangeSet(f *testing.F) {
	cfg := &Config{Fixture: "fixture.yml", Out: "migrations", Models: map[string]*Model{
		"Plan": {Table: "plans", Key: []string{"name"}},
	}}
	if err := cfg.Prepare(); err != nil {
		f.Fatal(err)
	}
	f.Add([]byte("package migrations\n"))
	f.Add([]byte("package migrations\nvar x = fixturechange.Set{Name: \"x\"}\n"))
	f.Add([]byte("package migrations\nvar x = fixturechange.Set{Changes: []fixturechange.Change{{Model: `Plan`, " +
		"Kind: fixturechange.Insert, Key: fixturechange.Values{\"name\": fixturechange.Lit(`{\"a\":1}`)}}}}\n"))
	f.Fuzz(func(t *testing.T, src []byte) {
		set, ok, err := ReadChangeSet(src)
		if err != nil || !ok {
			return
		}
		_ = fixtureapply.Validate(set)
	})
}

// Any fixture file either reads into a snapshot or says why not.
func FuzzFixtureSnapshot(f *testing.F) {
	f.Add([]byte(base))
	f.Add([]byte("- model: Plan\n  rows:\n    - _id: a\n      name: '{{ $.Plan.a.ID }}'\n"))
	f.Add([]byte("- model: Currency\n  rows:\n    - code: [1, {a: b}]\n      id: 0x1F\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		cfg := testConfig(t)
		doc, err := ParseDoc(data)
		if err != nil {
			return
		}
		_, _ = FixtureSnapshot(cfg, doc, "fuzz")
	})
}

// A YAML integer, in any of the notations yaml.v3 reads, is written as the
// decimal number it means.
func FuzzYamlInt(f *testing.F) {
	for _, seed := range []string{"0", "017", "0x1F", "0o17", "0b101", "1_000", "-0x_ff", "+5", "99999999999999999999"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out, ok := yamlInt(in)
		if !ok {
			return
		}
		want, _ := new(big.Int).SetString(strings.ReplaceAll(in, "_", ""), 0)
		got, ok := new(big.Int).SetString(out, 10)
		if !ok || got.Cmp(want) != 0 {
			t.Fatalf("%q is %s, written %q", in, want, out)
		}
		if again, _ := yamlInt(out); again != out {
			t.Fatalf("%q is not a fixed point: %q", out, again)
		}
	})
}
