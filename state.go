package fixturemigrate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	// Fixture is the fixture file's content, byte for byte.
	Fixture []byte
	// Migration names the migration that last wrote the state, or "baseline".
	Migration string
}

// ErrNoState is what ReadState returns for a state file that does not exist.
var ErrNoState = errors.New("no state file")

const (
	stateMarker = "# ----- the fixture file, as the migrations leave a database -----"
	stateHeader = `# bun-fixture-migrate state file. Do not edit it by hand.
#
# It is the fixture file as the generated migrations leave a database.
# "generate" diffs the fixture file against it, not against git, and rewrites
# it with every migration it writes. "baseline" rewrites it without writing a
# migration, for a change you migrated by hand.
#
# Two branches that each generate a migration both change the two lines below,
# so their merge conflicts here, on purpose. Keep both migrations, check with
# "bun-fixture-migrate plan" against a copy of production that they do not
# change the same rows, then run "bun-fixture-migrate baseline" on the merged
# fixture file.
#
`
)

// Encode renders the state file.
func (s State) Encode() []byte {
	body := normalizeNewlines(s.Fixture)
	var b bytes.Buffer
	b.WriteString(stateHeader)
	fmt.Fprintf(&b, "# migration: %s\n", s.Migration)
	fmt.Fprintf(&b, "# sha256: %s\n", checksum(body))
	b.WriteString(stateMarker + "\n")
	b.Write(body)
	return b.Bytes()
}

// DecodeState reads a state file and checks it against its own checksum. A
// state that does not match is refused rather than used: a wrong base state
// produces a migration that looks right and is not.
func DecodeState(data []byte) (State, error) {
	data = normalizeNewlines(data)
	i := bytes.Index(data, []byte("\n"+stateMarker+"\n"))
	if i < 0 {
		return State{}, fmt.Errorf("this is not a state file bun-fixture-migrate wrote: the marker line is missing")
	}
	header, body := string(data[:i]), data[i+len(stateMarker)+2:]
	var s State
	var sum string
	for _, line := range strings.Split(header, "\n") {
		if v, ok := strings.CutPrefix(line, "# migration: "); ok {
			s.Migration = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(line, "# sha256: "); ok {
			sum = strings.TrimSpace(v)
		}
	}
	if sum == "" {
		return State{}, fmt.Errorf("the state file has no checksum line")
	}
	if checksum(body) != sum {
		return State{}, fmt.Errorf("the state file does not match its own checksum, so it was edited by hand " +
			"or merged line by line. Take it back from git, or rewrite it with baseline once you know which " +
			"state the migrations leave a database in")
	}
	s.Fixture = body
	return s, nil
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
func WriteFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
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
