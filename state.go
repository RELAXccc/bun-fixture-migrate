package fixturemigrate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// State is the state file: the fixture file as the generated migrations leave
// a database.
//
// generate diffs the fixture file against it rather than against git and
// rewrites it with every migration it writes. Against git, two ordinary
// sequences of events go wrong without a word. Commit a fixture edit before
// generating its migration, and HEAD already holds the edit, so there is
// nothing to generate and the change never reaches a seeded database. Generate
// twice before committing, and the second migration carries the first one's
// changes again, guarded by values the first one has since replaced, so on a
// database where the first ran they are skipped as somebody's edit.
//
// It also turns a race into a merge conflict. Two branches that each generate a
// migration both rewrite the header of this file, so the merge stops there
// instead of producing two migrations that each assume they are the only one.
type State struct {
	// Files are the fixture files, in load order, byte for byte. A
	// configuration with one fixture file has one, whose Path a state file
	// written before several files were supported leaves empty.
	Files []FixtureFile
	// Migration names the migration that last wrote the state, or "baseline".
	Migration string
	// Covers is the newest fixture migration whose changes Files include: the
	// one generate wrote with this state, or for a state baseline wrote, the
	// newest fixture migration of the directory as it was. Empty when there
	// was none.
	Covers string
	// Base is what Covers was generated against: the newest fixture migration
	// whose changes the state before it included. generate names a migration
	// after every other one, so as long as nothing was merged no fixture
	// migration sorts between Base and Covers, nor after Covers; one that does
	// was generated on another branch, against a state this one never saw.
	// Equal to Covers when no generated migration is in question.
	Base string
	// LeftOut are the changes generate -allow-partial refused, one sentence
	// each. Files already holds them, so generate does not see them again;
	// they are listed here so that nothing forgets them until baseline -force
	// says a migration somebody wrote by hand makes them.
	LeftOut []string
	// Format is the format DecodeState read: 1 for a state file written
	// before the format was numbered. Encode always writes StateFormat.
	Format int
}

// FixtureFile is one fixture file: its path as the configuration spells it,
// and its content.
type FixtureFile struct {
	Path string
	Data []byte
}

// ErrNoState is what ReadState returns for a state file that does not exist.
var ErrNoState = errors.New("no state file")

// StateFormat is the format Encode writes.
//
// Format 1 put each fixture file after a marker line, and a reader found the
// next file by looking for the next marker, so a fixture file holding such a
// line was split in the wrong place under a checksum that still matched; and
// the name of the migration that wrote it was outside the checksum. Format 2
// says how many lines each file has and covers everything but the comment on
// top with the checksum.
const StateFormat = 2

const (
	stateHeader = `# bun-fixture-migrate state file. Do not edit it by hand.
#
# It is the fixture file as the generated migrations leave a database.
# "generate" diffs the fixture file against it, not against git, and rewrites
# it with every migration it writes. "baseline" rewrites it without writing a
# migration, for a change you migrated by hand.
#
# Two branches that each generate a migration both change the lines below, so
# their merge conflicts here, on purpose: each migration expects the rows as
# they were before it, and whichever runs second finds the other's changes.
# Keep the migration a database already applied and delete the other, take
# this file as the one you kept left it, then run "bun-fixture-migrate
# generate" on the merged fixture file.
#
`
	formatPrefix    = "# format: "
	migrationPrefix = "# migration: "
	coversPrefix    = "# covers: "
	basePrefix      = "# base: "
	leftOutPrefix   = "# left out: "
	sumPrefix       = "# sha256: "
	sectionPrefix   = "# ----- "
	sectionSuffix   = " -----"

	// The markers of format 1.
	stateMarker     = "# ----- the fixture file, as the migrations leave a database -----"
	fileMarkerStart = "# ----- fixture file: "
	fileMarkerEnd   = " -----"
)

// Encode renders the state file: a comment saying what it is, the fields,
// the checksum, and each fixture file after a line giving its length and its
// path. The checksum covers every byte after the comment but its own line.
// A fixture file is copied verbatim, so the state file parses as the fixture
// files do; it ends in a newline, which changes nothing.
func (s State) Encode() []byte {
	var meta bytes.Buffer
	fmt.Fprintf(&meta, "%s%d\n", formatPrefix, StateFormat)
	fmt.Fprintf(&meta, "%s%s\n", migrationPrefix, stateValue(s.Migration))
	if s.Covers != "" {
		fmt.Fprintf(&meta, "%s%s\n", coversPrefix, stateValue(s.Covers))
	}
	if s.Base != "" {
		fmt.Fprintf(&meta, "%s%s\n", basePrefix, stateValue(s.Base))
	}
	for _, line := range s.LeftOut {
		fmt.Fprintf(&meta, "%s%s\n", leftOutPrefix, stateValue(line))
	}
	var files bytes.Buffer
	for _, f := range s.Files {
		data := withFinalNewline(normalizeNewlines(f.Data))
		fmt.Fprintf(&files, "%s%s of %s%s\n", sectionPrefix, lineCount(bytes.Count(data, []byte("\n"))),
			stateValue(f.Path), sectionSuffix)
		files.Write(data)
	}
	var b bytes.Buffer
	b.WriteString(stateHeader)
	b.Write(meta.Bytes())
	fmt.Fprintf(&b, "%s%s\n", sumPrefix, checksum(append(meta.Bytes(), files.Bytes()...)))
	b.Write(files.Bytes())
	return b.Bytes()
}

func lineCount(n int) string {
	if n == 1 {
		return "1 line"
	}
	return strconv.Itoa(n) + " lines"
}

// stateValue is a field as the state file writes it: as it is when that reads
// back as the same text, quoted as a Go string otherwise, so that no value can
// end its line early or start with a quote it did not have.
func stateValue(v string) string {
	plain := v != "" && v == strings.TrimSpace(v) && !strings.HasPrefix(v, `"`) && utf8.ValidString(v)
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			plain = false
		}
	}
	if plain {
		return v
	}
	return strconv.Quote(v)
}

func readStateValue(v string) (string, error) {
	if !strings.HasPrefix(v, `"`) {
		return v, nil
	}
	return strconv.Unquote(v)
}

// DecodeState reads a state file and checks it against its own checksum. A
// state that does not match is refused rather than used: a wrong base state
// produces a migration that looks right and is not.
//
// It reads both formats. A state file of format 1 is still what the release
// that wrote it vouched for, and is replaced by format 2 the next time
// generate or baseline writes it.
func DecodeState(data []byte) (State, error) {
	data = normalizeNewlines(data)
	s, err := decodeAnyState(data)
	if err != nil && conflicted(data) {
		return State{}, ErrStateConflict
	}
	return s, err
}

func decodeAnyState(data []byte) (State, error) {
	offset := 0
	for _, raw := range bytes.SplitAfter(data, []byte("\n")) {
		line := strings.TrimSuffix(string(raw), "\n")
		switch {
		case strings.HasPrefix(line, formatPrefix):
			return decodeState(data[offset:])
		case line == stateMarker || strings.HasPrefix(line, fileMarkerStart):
			return decodeStateFormat1(data)
		case strings.TrimSpace(line) == "":
			// A blank line in the comment, which nothing reads.
		case !strings.HasPrefix(line, "#"):
			// The comment on top is over, and no field was in it.
			return State{}, errNotState
		}
		offset += len(raw)
	}
	return State{}, errNotState
}

var errNotState = errors.New("this is not a state file bun-fixture-migrate wrote: no \"# format:\" line follows " +
	"the comment on top, nor the marker line of an older release")

// ErrStateConflict is what DecodeState and ReadState return, wrapped, for a
// merge that stopped in the state file, which it does on purpose when two
// branches each generated a migration.
var ErrStateConflict = errors.New("the state file holds git's conflict markers: two branches each generated a " +
	"migration from the same state, and whichever runs second would find the other's changes. Keep the migration " +
	"a database already applied and delete the other, take the state file as the one you kept left it " +
	"(git checkout --ours or --theirs), then generate again on the merged fixture file")

// conflicted reports whether git left conflict markers in a file. A fixture
// file cannot hold such a line: at the start of a line of a YAML sequence it
// is not YAML.
func conflicted(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		if line == "=======" || strings.HasPrefix(line, "<<<<<<< ") || strings.HasPrefix(line, ">>>>>>> ") ||
			strings.HasPrefix(line, "||||||| ") {
			return true
		}
	}
	return false
}

// errStateEdited is the checksum's verdict.
var errStateEdited = errors.New("the state file does not match its own checksum, so it was edited by hand " +
	"or merged line by line. Take it back from git, or rewrite it with baseline once you know which " +
	"state the migrations leave a database in")

// decodeState reads format 2 from its format line on. Every line ends in a
// newline, so an editor that drops the last one has changed nothing.
func decodeState(data []byte) (State, error) {
	lines := bytes.SplitAfter(withFinalNewline(data), []byte("\n"))
	lines = lines[:len(lines)-1] // what follows the last newline: nothing
	s := State{}
	var meta bytes.Buffer
	sum, i := "", 0
	for ; i < len(lines) && sum == ""; i++ {
		line := strings.TrimSuffix(string(lines[i]), "\n")
		if v, ok := strings.CutPrefix(line, sumPrefix); ok {
			sum = strings.TrimSpace(v)
			if sum == "" {
				return State{}, fmt.Errorf("the state file has an empty checksum line")
			}
			continue
		}
		meta.Write(lines[i])
		if v, ok := strings.CutPrefix(line, formatPrefix); ok && i == 0 {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 2 {
				return State{}, fmt.Errorf("the state file says it is format %q, which no release of bun-fixture-migrate "+
					"wrote", strings.TrimSpace(v))
			}
			if n > StateFormat {
				return State{}, fmt.Errorf("the state file is format %d, written by a newer bun-fixture-migrate than this "+
					"one, which reads up to format %d. Use the release the project uses", n, StateFormat)
			}
			s.Format = n
			continue
		}
		var field *string
		var value string
		switch {
		case strings.HasPrefix(line, migrationPrefix):
			field, value = &s.Migration, line[len(migrationPrefix):]
		case strings.HasPrefix(line, coversPrefix):
			field, value = &s.Covers, line[len(coversPrefix):]
		case strings.HasPrefix(line, basePrefix):
			field, value = &s.Base, line[len(basePrefix):]
		case strings.HasPrefix(line, leftOutPrefix):
			s.LeftOut = append(s.LeftOut, "")
			field, value = &s.LeftOut[len(s.LeftOut)-1], line[len(leftOutPrefix):]
		default:
			return State{}, fmt.Errorf("the state file has a line this release does not know where its fields are: %q", line)
		}
		v, err := readStateValue(value)
		if err != nil {
			return State{}, fmt.Errorf("the state file has a field that is not a valid quoted string: %q", line)
		}
		*field = v
	}
	if sum == "" {
		return State{}, fmt.Errorf("the state file has no checksum line")
	}
	rest := bytes.Join(lines[i:], nil)
	if checksum(append(meta.Bytes(), rest...)) != sum {
		return State{}, errStateEdited
	}
	for i < len(lines) {
		line := strings.TrimSuffix(string(lines[i]), "\n")
		n, path, ok := parseSection(line)
		if !ok {
			return State{}, fmt.Errorf("the state file has %q where a fixture file should start", line)
		}
		i++
		if i+n > len(lines) {
			return State{}, fmt.Errorf("the state file ends inside the fixture file %s", path)
		}
		s.Files = append(s.Files, FixtureFile{Path: path, Data: bytes.Join(lines[i:i+n], nil)})
		i += n
	}
	return s, nil
}

// parseSection reads the line before a fixture file in format 2:
// "# ----- 12 lines of fixtures/plans.yml -----".
func parseSection(line string) (n int, path string, ok bool) {
	rest, ok := strings.CutPrefix(line, sectionPrefix)
	if !ok || !strings.HasSuffix(rest, sectionSuffix) {
		return 0, "", false
	}
	rest = strings.TrimSuffix(rest, sectionSuffix)
	count, rest, ok := strings.Cut(rest, " ")
	if !ok {
		return 0, "", false
	}
	n, err := strconv.Atoi(count)
	if err != nil || n < 0 || strconv.Itoa(n) != count {
		return 0, "", false
	}
	word := "lines of "
	if n == 1 {
		word = "line of "
	}
	if rest, ok = strings.CutPrefix(rest, word); !ok {
		return 0, "", false
	}
	path, err = readStateValue(rest)
	if err != nil {
		return 0, "", false
	}
	return n, path, true
}

// decodeStateFormat1 reads a state file written before the format was
// numbered, as that release read it.
func decodeStateFormat1(data []byte) (State, error) {
	s := State{Format: 1}
	var sum string
	start := -1
	lines := bytes.SplitAfter(data, []byte("\n"))
	offset := 0
	for _, raw := range lines {
		line := strings.TrimSuffix(string(raw), "\n")
		if line == stateMarker || strings.HasPrefix(line, fileMarkerStart) {
			start = offset
			break
		}
		if v, ok := strings.CutPrefix(line, migrationPrefix); ok {
			s.Migration = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(line, sumPrefix); ok {
			sum = strings.TrimSpace(v)
		}
		offset += len(raw)
	}
	if start < 0 {
		return State{}, errNotState
	}
	if sum == "" {
		return State{}, fmt.Errorf("the state file has no checksum line")
	}
	rest := data[start:]
	if checksum(bodyOf(rest)) != sum {
		return State{}, errStateEdited
	}
	if bytes.HasPrefix(rest, []byte(stateMarker+"\n")) {
		s.Files = []FixtureFile{{Data: bodyOf(rest)}}
		return s, nil
	}
	var current *FixtureFile
	for _, raw := range bytes.SplitAfter(rest, []byte("\n")) {
		line := strings.TrimSuffix(string(raw), "\n")
		if strings.HasPrefix(line, fileMarkerStart) && strings.HasSuffix(line, fileMarkerEnd) {
			path := strings.TrimSuffix(strings.TrimPrefix(line, fileMarkerStart), fileMarkerEnd)
			s.Files = append(s.Files, FixtureFile{Path: path})
			current = &s.Files[len(s.Files)-1]
			continue
		}
		if current == nil {
			return State{}, fmt.Errorf("the state file has %q where a fixture file should start", line)
		}
		current.Data = append(current.Data, raw...)
	}
	return s, nil
}

// bodyOf is what the checksum of format 1 covers: for one file, its content
// after the marker line; for several, the markers and the files.
func bodyOf(rest []byte) []byte {
	if bytes.HasPrefix(rest, []byte(stateMarker+"\n")) {
		return rest[len(stateMarker)+1:]
	}
	return rest
}

// Unaccounted is the fixture migrations, of those given, whose changes the
// state does not include as far as its history says: one that sorts after
// Covers, or between Base and Covers. known is false when the state does not
// say, which is a state file of format 1 that baseline wrote; one of format 1
// that generate wrote says only that nothing after its migration is included.
func (s State) Unaccounted(fixtures []MigrationFile) (out []MigrationFile, known bool) {
	covers, base, between := s.Covers, s.Base, true
	if s.Format == 1 {
		if s.Migration == "baseline" || s.Migration == "" {
			return nil, false
		}
		covers, between = s.Migration, false
	}
	for _, m := range fixtures {
		id := m.ID()
		if id == covers {
			continue
		}
		if CompareMigrations(id, covers) > 0 ||
			(between && CompareMigrations(id, base) > 0 && CompareMigrations(id, covers) < 0) {
			out = append(out, m)
		}
	}
	return out, true
}

// Covered is the newest fixture migration whose changes the state says it
// includes: Covers, or for a state file of format 1 the migration that wrote
// it. "" when it names none, as a state baseline wrote before any fixture
// migration, or one of format 1 that baseline wrote, says nothing.
//
// Unaccounted finds a migration the state does not include; this is the
// other way round. Deleted from the directory, the migration is no longer
// there to make its changes, while the state, which generate diffs against,
// still says they are made: they reach no database, and every gate built on
// the state stays green.
func (s State) Covered() string {
	if s.Format == 1 {
		if s.Migration == "baseline" {
			return ""
		}
		return s.Migration
	}
	return s.Covers
}

// CompareMigrations orders two migrations as bun runs them, by the name bun
// records and then by the rest of the file name, as ReadMigrations lists
// them: "20260921120000_fixture_prices". The empty string sorts first.
func CompareMigrations(a, b string) int {
	nameA, restA, _ := strings.Cut(a, "_")
	nameB, restB, _ := strings.Cut(b, "_")
	if c := strings.Compare(nameA, nameB); c != 0 {
		return c
	}
	return strings.Compare(restA, restB)
}

// SameFiles reports whether two lists of fixture files hold the same content,
// ignoring line endings and a missing final newline, which change nothing.
func SameFiles(a, b []FixtureFile) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a) > 1 && a[i].Path != b[i].Path {
			return false
		}
		if !bytes.Equal(withFinalNewline(normalizeNewlines(a[i].Data)), withFinalNewline(normalizeNewlines(b[i].Data))) {
			return false
		}
	}
	return true
}

// ParseFiles parses fixture files as dbfixture loads them with one
// fixture.Load: in order, as one document, one scope of anchors across all of
// them.
func ParseFiles(files []FixtureFile) (Doc, error) {
	var doc Doc
	for _, f := range files {
		part, err := ParseDoc(f.Data)
		if err != nil {
			if f.Path != "" {
				return nil, fmt.Errorf("%s: %w", f.Path, err)
			}
			return nil, err
		}
		doc = append(doc, part...)
	}
	return doc, nil
}

func withFinalNewline(b []byte) []byte {
	if len(b) == 0 || b[len(b)-1] == '\n' {
		return b
	}
	return append(append([]byte{}, b...), '\n')
}

// ReadState reads a state file, ErrNoState when there is none.
func ReadState(path string) (State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, ErrNoState
	}
	if err != nil {
		return State{}, err
	}
	s, err := DecodeState(data)
	if err != nil {
		return State{}, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// WriteState writes a state file.
func WriteState(path string, s State) error {
	return WriteFileAtomic(path, s.Encode(), 0o644)
}

// WriteFileAtomic writes a file so that a reader sees the old content or the
// new one, never half of either: a temporary file in the same directory,
// synced, then renamed over the target. An interrupted export must not leave
// half a fixture file behind for the next seed to load.
//
// The directory is created when it is not there yet, as a project adopting
// the tool has neither its fixtures nor its migrations directory. An error
// names the file being written, never the temporary one, which the person
// reading it has never heard of.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	fail := func(err error) error {
		var pathErr *os.PathError
		var linkErr *os.LinkError
		switch {
		case errors.As(err, &linkErr):
			err = linkErr.Err
		case errors.As(err, &pathErr):
			err = pathErr.Err
		}
		return fmt.Errorf("write %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fail(err)
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			err = fail(err)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Chmod(perm); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func checksum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// normalizeNewlines turns CRLF into LF, so a checkout that converts line
// endings does not break the checksum.
func normalizeNewlines(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}
