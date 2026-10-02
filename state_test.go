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

// A project adopting the tool has neither its fixtures directory nor its
// migrations directory yet: the first export and the first baseline make
// them. An error names the file being written, not the temporary one.
func TestWriteFileAtomicMakesTheDirectory(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "fixtures", "master", "fixture.yml")
	if err := WriteFileAtomic(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "x\n" {
		t.Fatalf("%q %v", data, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("expected only the file, got %v", entries)
	}
	// A file where a directory has to be.
	blocked := filepath.Join(root, "plain")
	if err := os.WriteFile(blocked, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(blocked, "fixture.yml")
	err := WriteFileAtomic(target, []byte("x\n"), 0o644)
	if err == nil || !strings.HasPrefix(err.Error(), "write "+target+": ") || strings.Contains(err.Error(), ".tmp") {
		t.Fatalf("got %v", err)
	}
	// A directory where the file has to be: the rename fails, and the
	// temporary file goes.
	dirTarget := filepath.Join(root, "fixtures", "master")
	err = WriteFileAtomic(dirTarget, []byte("x\n"), 0o644)
	if err == nil || !strings.HasPrefix(err.Error(), "write "+dirTarget+": ") || strings.Contains(err.Error(), ".tmp") {
		t.Fatalf("got %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "fixtures")); len(entries) != 1 {
		t.Fatalf("a temporary file was left behind: %v", entries)
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

// A fixture file can hold any line, including one that looks like the line
// the state file puts before a file, in this format or the last one. It is
// data, and the files come back as they went in.
func TestAMarkerLineInsideAFixtureFileIsData(t *testing.T) {
	for _, marker := range []string{
		"# ----- fixture file: c.yml -----",
		"# ----- 1 line of c.yml -----",
		"# ----- 0 lines of c.yml -----",
		stateMarker,
		"# sha256: 0000",
		"# format: 2",
	} {
		files := []FixtureFile{
			{Path: "a.yml", Data: []byte(marker + "\n- model: X\n  rows: []\n")},
			{Path: "b.yml", Data: []byte("[]\n" + marker + "\n")},
		}
		back, err := DecodeState(State{Files: files, Migration: "m"}.Encode())
		if err != nil {
			t.Fatalf("%q: %v", marker, err)
		}
		if len(back.Files) != 2 || back.Files[0].Path != "a.yml" || back.Files[1].Path != "b.yml" ||
			string(back.Files[0].Data) != string(files[0].Data) || string(back.Files[1].Data) != string(files[1].Data) {
			t.Fatalf("%q: the files came back as %+v", marker, back.Files)
		}
	}
}

// Everything the state says is under its checksum: which migration wrote it,
// what it was built on, what was left out, its format.
func TestTheFieldsAreUnderTheChecksum(t *testing.T) {
	s := State{Files: []FixtureFile{{Path: "fixtures/fixture.yml", Data: []byte(base)}},
		Migration: "20260921120000_fixture_prices", Covers: "20260921120000_fixture_prices",
		Base: "20260920120000_fixture_seats", LeftOut: []string{"Plan name=team: its id changed from 2 to 7"}}
	encoded := string(s.Encode())
	back, err := DecodeState([]byte(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if back.Migration != s.Migration || back.Covers != s.Covers || back.Base != s.Base || len(back.LeftOut) != 1 ||
		back.LeftOut[0] != s.LeftOut[0] ||
		back.Format != StateFormat {
		t.Fatalf("got %+v", back)
	}
	for _, edit := range [][2]string{
		{"# migration: 20260921120000_fixture_prices", "# migration: 20260921120001_fixture_prices"},
		{"# covers: 20260921120000_fixture_prices", "# covers: 20260921120001_fixture_prices"},
		{"# base: 20260920120000_fixture_seats", "# base: 20260920120001_fixture_seats"},
		{"# left out: Plan name=team", "# left out: Plan name=crew"},
		{"# format: 2", "# format: 2 "},
		{"lines of fixtures/fixture.yml -----", "lines of fixtures/other.yml -----"},
	} {
		if !strings.Contains(encoded, edit[0]) {
			t.Fatalf("no %q in\n%s", edit[0], encoded)
		}
		edited := strings.Replace(encoded, edit[0], edit[1], 1)
		if _, err := DecodeState([]byte(edited)); err == nil || !strings.Contains(err.Error(), "checksum") {
			t.Errorf("%q: expected the edit to be caught, got %v", edit[1], err)
		}
	}
	// Dropping a left-out change is an edit too.
	dropped := strings.Replace(encoded, "# left out: Plan name=team: its id changed from 2 to 7\n", "", 1)
	if _, err := DecodeState([]byte(dropped)); err == nil {
		t.Fatal("a left-out change that disappeared has to be caught")
	}
}

// A state of no fixture files at all is still a state file.
func TestAStateOfNoFiles(t *testing.T) {
	back, err := DecodeState(State{Migration: "baseline"}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Files) != 0 || back.Migration != "baseline" {
		t.Fatalf("got %+v", back)
	}
}

// A format this release does not know is refused with a sentence saying so,
// rather than read as the nearest one it does.
func TestAnUnknownFormatIsRefused(t *testing.T) {
	encoded := string(State{Files: []FixtureFile{{Data: []byte(base)}}, Migration: "baseline"}.Encode())
	for format, want := range map[string]string{"3": "newer bun-fixture-migrate", "1": "no release", "two": "no release"} {
		edited := strings.Replace(encoded, "# format: 2\n", "# format: "+format+"\n", 1)
		if _, err := DecodeState([]byte(edited)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("format %s: %v", format, err)
		}
	}
	unknown := strings.Replace(encoded, "# migration: baseline\n", "# migration: baseline\n# owner: x\n", 1)
	if _, err := DecodeState([]byte(unknown)); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Errorf("an unknown field: %v", err)
	}
}

// A value that would end its line early, or read back as something else, is
// quoted; anything else is written as it is, for a reader.
func TestStateValuesThatNeedQuoting(t *testing.T) {
	s := State{
		Files:     []FixtureFile{{Path: "fix\ntures/a -----.yml", Data: []byte("[]\n")}, {Path: `"b".yml`, Data: nil}},
		Migration: "baseline", Base: " x",
		LeftOut: []string{"Plan name=a\nb: renamed", `"quoted" at the start`, "", "plain: it stays plain"},
	}
	encoded := s.Encode()
	back, err := DecodeState(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if back.Files[0].Path != s.Files[0].Path || back.Files[1].Path != s.Files[1].Path || back.Base != s.Base ||
		strings.Join(back.LeftOut, "|") != strings.Join(s.LeftOut, "|") {
		t.Fatalf("got %+v", back)
	}
	if !strings.Contains(string(encoded), "# left out: plain: it stays plain\n") {
		t.Fatalf("a plain sentence is written plain:\n%s", encoded)
	}
}

// An editor that drops the final newline, or a checkout that writes CRLF, has
// not edited the state.
func TestAStateWithoutItsFinalNewline(t *testing.T) {
	for _, files := range [][]FixtureFile{nil, {{Data: []byte(base)}}, {{Path: "a", Data: []byte("[]")}, {Path: "b", Data: []byte("[]")}}} {
		encoded := State{Files: files, Migration: "baseline"}.Encode()
		trimmed := strings.TrimSuffix(strings.ReplaceAll(string(encoded), "\n", "\r\n"), "\r\n")
		back, err := DecodeState([]byte(trimmed))
		if err != nil {
			t.Fatalf("%d files: %v", len(files), err)
		}
		if !SameFiles(back.Files, files) {
			t.Fatalf("got %+v", back.Files)
		}
	}
}

// A merge that stopped in the state file says so, and what to do.
func TestAConflictedStateSaysWhatToDo(t *testing.T) {
	ours := string(State{Files: []FixtureFile{{Data: []byte(base)}}, Migration: "20260921120000_fixture_a"}.Encode())
	theirs := string(State{Files: []FixtureFile{{Data: []byte(base)}}, Migration: "20260921120001_fixture_b"}.Encode())
	oursLines, theirsLines := strings.SplitAfter(ours, "\n"), strings.SplitAfter(theirs, "\n")
	var merged strings.Builder
	for i := range oursLines {
		if oursLines[i] == theirsLines[i] {
			merged.WriteString(oursLines[i])
			continue
		}
		merged.WriteString("<<<<<<< HEAD\n" + oursLines[i] + "=======\n" + theirsLines[i] + ">>>>>>> b\n")
	}
	_, err := DecodeState([]byte(merged.String()))
	if err == nil || !strings.Contains(err.Error(), "conflict markers") || !strings.Contains(err.Error(), "generate again") {
		t.Fatalf("got %v", err)
	}
}

// State files written before the format was numbered are what that release
// vouched for, and still read: these two were written by it.
func TestAStateFileOfFormat1StillReads(t *testing.T) {
	one, err := ReadState(filepath.Join("testdata", "state-format1-one-file.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if one.Format != 1 || one.Migration != "20260921120000_fixture_prices" || one.Base != "" || len(one.Files) != 1 ||
		one.Files[0].Path != "" || !strings.HasPrefix(string(one.Files[0].Data), "- model: Currency\n") ||
		!strings.HasSuffix(string(one.Files[0].Data), "      enabled: true\n") {
		t.Fatalf("got %+v", one)
	}
	several, err := ReadState(filepath.Join("testdata", "state-format1-several-files.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if several.Format != 1 || several.Migration != "baseline" || len(several.Files) != 2 ||
		several.Files[0].Path != "fixtures/currencies.yml" || several.Files[1].Path != "fixtures/plans.yml" ||
		!strings.HasSuffix(string(several.Files[1].Data), "price_cents: 2000\n") {
		t.Fatalf("got %+v", several)
	}
	doc, err := ParseFiles(several.Files)
	if err != nil || len(doc) != 2 || doc[1].Name != "Plan" {
		t.Fatalf("%v %+v", err, doc)
	}
	// Written again, it is format 2 and the same files.
	for _, s := range []State{one, several} {
		again, err := DecodeState(s.Encode())
		if err != nil || again.Format != StateFormat || !SameFiles(again.Files, s.Files) || again.Migration != s.Migration {
			t.Fatalf("%v %+v", err, again)
		}
	}
	// And an edit of one is still caught.
	data, err := os.ReadFile(filepath.Join("testdata", "state-format1-several-files.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeState([]byte(strings.Replace(string(data), "2000", "2500", 1))); err == nil ||
		!strings.Contains(err.Error(), "checksum") {
		t.Fatalf("got %v", err)
	}
}

// The state's history says which fixture migrations of the directory it
// includes. One generated on another branch, against the state this one was
// generated against or an older one, sorts between the two or after both.
func TestWhichMigrationsAStateIncludes(t *testing.T) {
	dir := func(ids ...string) []MigrationFile {
		var out []MigrationFile
		for _, id := range ids {
			name, comment, _ := strings.Cut(id, "_")
			out = append(out, MigrationFile{Name: name, Comment: comment})
		}
		return out
	}
	ids := func(ms []MigrationFile) string {
		var out []string
		for _, m := range ms {
			out = append(out, m.ID())
		}
		return strings.Join(out, ",")
	}
	const x, a, b = "20261001000000_fixture_x", "20261001100000_fixture_a", "20261001110000_fixture_b"
	for _, c := range []struct {
		name  string
		state State
		dir   []MigrationFile
		want  string
		known bool
	}{
		{"linear", State{Covers: b, Base: a}, dir(x, a, b), "", true},
		{"kept the newer side", State{Covers: b, Base: x}, dir(x, a, b), a, true},
		{"kept the older side", State{Covers: a, Base: x}, dir(x, a, b), b, true},
		{"nothing before it", State{Covers: b}, dir(a, b), a, true},
		{"a baseline", State{Migration: "baseline", Covers: b, Base: b}, dir(x, a, b), "", true},
		{"a baseline of nothing", State{Migration: "baseline"}, dir(x), x, true},
		{"format 1, generated", State{Format: 1, Migration: a}, dir(x, a, b), b, true},
		{"format 1, baseline", State{Format: 1, Migration: "baseline"}, dir(x, a, b), "", false},
	} {
		got, known := c.state.Unaccounted(c.dir)
		if ids(got) != c.want || known != c.known {
			t.Errorf("%s: got %q %v, want %q %v", c.name, ids(got), known, c.want, c.known)
		}
	}
	if CompareMigrations("9_x", "10_x") <= 0 || CompareMigrations("", "1_a") >= 0 || CompareMigrations("1_a", "1_b") >= 0 {
		t.Fatal("migrations compare as bun orders them: by name as a string, then the rest")
	}
}

// The migration a state says it includes last: what a deleted migration is
// checked against.
func TestTheMigrationAStateCovers(t *testing.T) {
	for _, c := range []struct {
		state State
		want  string
	}{
		{State{Format: 2, Migration: "2_fixture_b", Covers: "2_fixture_b", Base: "1_fixture_a"}, "2_fixture_b"},
		{State{Format: 2, Migration: "baseline", Covers: "2_fixture_b", Base: "2_fixture_b"}, "2_fixture_b"},
		{State{Format: 2, Migration: "baseline"}, ""},
		{State{Format: 1, Migration: "2_fixture_b"}, "2_fixture_b"},
		{State{Format: 1, Migration: "baseline"}, ""},
		{State{Format: 1}, ""},
	} {
		if got := c.state.Covered(); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.state, got, c.want)
		}
	}
	// And a conflict is told apart from every other state that does not read.
	if _, err := DecodeState([]byte("<<<<<<< HEAD\n# format: 2\n=======\n>>>>>>> b\n")); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("got %v", err)
	}
}

// The comment on top is nobody's to read, and a blank line in it changes
// nothing; a file without a format line says so, not that a marker of an
// older release is missing.
func TestABlankLineInTheStateFilesComment(t *testing.T) {
	data := string(State{Files: []FixtureFile{{Data: []byte(base)}}, Migration: "baseline"}.Encode())
	edited := strings.Replace(data, "#\n", "\n", 1)
	if edited == data {
		t.Fatal("no blank comment line to replace")
	}
	if _, err := DecodeState([]byte(edited)); err != nil {
		t.Fatalf("got %v", err)
	}
	if _, err := DecodeState([]byte("# a comment\n\n- model: Plan\n")); err == nil ||
		!strings.Contains(err.Error(), `no "# format:" line`) {
		t.Fatalf("got %v", err)
	}
}
