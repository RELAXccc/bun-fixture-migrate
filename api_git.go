package fixturemigrate

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitFiles reads the fixture files as of a git revision. A file the revision
// does not have yet is empty there: at that revision, nothing loaded it.
func (p *Project) gitFiles(rev string) ([]FixtureFile, error) {
	paths := p.FixturePaths()
	files := make([]FixtureFile, 0, len(paths))
	for i, path := range paths {
		data, err := gitShow(path, rev)
		if err != nil {
			if !missingAt(err) {
				return nil, err
			}
			data = nil
		}
		files = append(files, FixtureFile{Path: p.Config.Fixtures[i], Data: data})
	}
	return files, nil
}

// missingAt reports git's answer for a path the revision does not have.
func missingAt(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "exists on disk, but not in") || strings.Contains(msg, "does not exist in")
}

// gitShow reads a file as of a revision. The path is resolved inside the
// repository the file itself belongs to, so it does not matter where the
// program was started.
func gitShow(path, rev string) ([]byte, error) {
	dir := filepath.Dir(path)
	top, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%s is not in a git repository, pass -old instead: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// Resolve symlinks on both sides: a temporary directory on macOS is a
	// symlink, and git reports the resolved top level.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	topDir := strings.TrimSpace(string(top))
	if resolved, err := filepath.EvalSymlinks(topDir); err == nil {
		topDir = resolved
	}
	rel, err := filepath.Rel(topDir, abs)
	if err != nil {
		return nil, err
	}
	data, err := git(dir, "show", rev+":"+filepath.ToSlash(rel))
	if err != nil {
		return nil, fmt.Errorf("git show %s:%s: %w", rev, filepath.ToSlash(rel), err)
	}
	return data, nil
}

// git runs git in a directory and returns what it printed. Its error is what
// git said, on one line.
func git(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		// git ends a sentence with a period, which a message that goes on
		// after it would double.
		if msg := strings.TrimSuffix(strings.TrimSpace(stderr.String()), "."); msg != "" {
			return nil, errors.New(joinLines(msg))
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

// joinLines is a message of several lines, from git, as one: its lines
// joined with "; ".
func joinLines(msg string) string {
	var parts []string
	for _, line := range strings.Split(msg, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, "; ")
}
