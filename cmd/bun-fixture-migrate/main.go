// Command bun-fixture-migrate writes the bun migration that brings a seeded
// database from an older revision of a dbfixture YAML file to the one in the
// working tree.
//
// Exit codes: 0 when a file was written or nothing changed, 1 on an error,
// 2 when something was refused and nothing was written.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
)

func main() {
	if err := run(); err != nil {
		var refused refusedError
		if errors.As(err, &refused) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "bun-fixture-migrate:", err)
		os.Exit(1)
	}
}

type refusedError struct{ n int }

func (e refusedError) Error() string {
	return fmt.Sprintf("%s refused, nothing written; write them yourself or re-run with -allow-partial",
		plural(e.n, "change"))
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func run() error {
	var (
		configPath   = flag.String("config", "fixture-migrate.yml", "configuration file")
		name         = flag.String("name", "", "short name for the migration, required")
		base         = flag.String("base", "HEAD", "git revision to diff the fixture file against")
		oldPath      = flag.String("old", "", "read the old revision from this file instead of git")
		out          = flag.String("out", "", "directory for the generated file (default: the out of the config file)")
		dryRun       = flag.Bool("dry-run", false, "print the file instead of writing it")
		allowPartial = flag.Bool("allow-partial", false, "write the changes that were accepted even when others were refused")
	)
	flag.Parse()

	cfg, err := fixturemigrate.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	root := filepath.Dir(*configPath)
	if cfg.Fixture == "" {
		return fmt.Errorf("%s: no fixture file configured", *configPath)
	}
	fixturePath := filepath.Join(root, cfg.Fixture)

	newData, err := os.ReadFile(fixturePath)
	if err != nil {
		return err
	}
	var oldData []byte
	baseLabel := *base
	if *oldPath != "" {
		baseLabel = *oldPath
		oldData, err = os.ReadFile(*oldPath)
	} else {
		oldData, err = gitShow(fixturePath, *base)
	}
	if err != nil {
		return err
	}

	oldDoc, err := fixturemigrate.ParseDoc(oldData)
	if err != nil {
		return fmt.Errorf("%s of %s: %w", baseLabel, cfg.Fixture, err)
	}
	newDoc, err := fixturemigrate.ParseDoc(newData)
	if err != nil {
		return fmt.Errorf("%s: %w", fixturePath, err)
	}

	res, err := fixturemigrate.Compute(cfg, oldDoc, newDoc)
	if err != nil {
		return err
	}
	for _, line := range res.Summary() {
		fmt.Println(line)
	}
	for _, r := range res.Refusals {
		fmt.Fprintln(os.Stderr, "refused:", r.String())
	}
	if len(res.Changes) == 0 {
		if len(res.Refusals) > 0 {
			return refusedError{len(res.Refusals)}
		}
		fmt.Printf("nothing changed in %s since %s\n", cfg.Fixture, baseLabel)
		return nil
	}
	if len(res.Refusals) > 0 && !*allowPartial {
		return refusedError{len(res.Refusals)}
	}

	if *name == "" {
		return fmt.Errorf("-name is required")
	}
	stamp := time.Now().UTC().Format(fixturemigrate.Stamp)
	src, err := fixturemigrate.Render(cfg, *name, stamp, baseLabel, res)
	if err != nil {
		return err
	}
	if *dryRun {
		os.Stdout.Write(src)
		return nil
	}

	dir := *out
	if dir == "" {
		dir = filepath.Join(root, cfg.Out)
	}
	target := filepath.Join(dir, fixturemigrate.FileName(stamp, *name))
	if err := os.WriteFile(target, src, 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", target)
	fmt.Println("read it, then run your migrations")
	return nil
}

// gitShow reads a file as of a revision. The path is resolved inside the
// repository the file itself belongs to, so it does not matter where the
// command was started.
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
	rel, err := filepath.Rel(strings.TrimSpace(string(top)), abs)
	if err != nil {
		return nil, err
	}
	data, err := git(dir, "show", rev+":"+filepath.ToSlash(rel))
	if err != nil {
		return nil, fmt.Errorf("git show %s:%s: %w", rev, filepath.ToSlash(rel), err)
	}
	return data, nil
}

func git(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, errors.New(msg)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}
