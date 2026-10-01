package dbtest_test

// Long-running projects: examples/saas and examples/commerce, two applications
// whose master data evolves over a dozen releases each, replayed from their
// history and deployed, release by release, to several long-lived databases
// kept in different states. This file is the replay; each example's
// longrun_<name>_test.go says what its application does to the databases
// between releases.
//
// examples/<name>/timeline holds the releases as a developer makes them: the
// fixture files, models and SQL migrations each release changes, and a
// release.yml that says what the developer runs (generate with a fixed -at,
// baseline, a merge of two branches), what admins and the application do to
// the databases between releases, and what each step is expected to say. The
// generated files are not in the timeline: this test produces them by running
// the real command, in a copy of the project, and at the end compares what it
// produced with the committed example, byte for byte. So the committed
// example is the real output of the tool over a real history.
//
// BFM_LONGRUN_UPDATE=1 rewrites the committed example instead of comparing.
// BFM_LONGRUN_WORK=<dir> keeps the work directory (project, binaries, logs),
// in <dir>/<name>.
// BFM_LONGRUN_KEEP=1 keeps the databases (bfm_<name>_*).
// BFM_LONGRUN_UNTIL=r05 stops after that release, without the comparison.
//
// The environments, each a database of its own:
//
//	prod         created at r01, deployed every release, with application
//	             traffic between releases and admin edits
//	staging      created at r02, deployed every other release, so it catches
//	             up two releases at once
//	onprem       (saas) created at r01, deployed only at r06 and r12, so it
//	outlet       (commerce) catches up many releases in one migrate
//	dev          created fresh at every release: migrate, seed, sequences
//	workstation  the developer's database, migrated before every generate so
//	             generate lints against the release's schema
//
// After every deploy: status -require-applied is clean, check is clean unless
// the release expects drift, the application's own rows still point at the
// same master rows, an application insert into every serial master table gets
// an id the sequence had not handed out, and the master data equals the fresh
// dev database's.
//
// A "known: Fn" note marks a workaround for a finding of the tool, in the
// release.yml and here, so the fix can remove it.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/uptrace/bun"
)

// lrExample is a long-running example project: its directory under
// examples/, which also names its databases, and what its application does to
// them. Each example's longrun_<name>_test.go declares one.
type lrExample struct {
	name string
	// minVersion is the oldest PostgreSQL server_version_num the example's
	// schema runs on, and why.
	minVersion int
	minReason  string
	// appData are queries, each returning one text, of the application's own
	// rows as the master rows they point at, by natural key: what a deploy
	// must never change.
	appData []string
	// appInserts insert into the master tables the way the application or an
	// admin UI would, without an id; each is rolled back.
	appInserts []lrInsert
	// traffic is the application at work in an environment between releases.
	traffic func(lr *longrun, env *lrEnv)
}

type lrInsert struct {
	sql string
	// notWithTraffic leaves the insert out where the application works, whose
	// own rows, kept rather than rolled back, are this insert already.
	notWithTraffic bool
	// table, when set, leaves the insert out while the database has no such
	// table: before the release that creates it, after the one that drops it.
	table string
}

// lrManifest is a release's release.yml.
type lrManifest struct {
	Description string `yaml:"description"`
	// Steps are what the developer does, in order, in the project. Without
	// any, the release is "overlay ." and nothing else.
	Steps []lrStep `yaml:"steps"`
	// Create are the environments this release creates.
	Create []string `yaml:"create"`
	// Deploy are the long-lived environments this release is deployed to;
	// dev and the workstation always are.
	Deploy []string `yaml:"deploy"`
	// Replicas deploys prod from this many processes at once.
	Replicas int `yaml:"replicas"`
	// BeforeDeploy is SQL per environment that an admin UI or the
	// application runs before the deploy.
	BeforeDeploy map[string][]string `yaml:"before_deploy"`
	// Plan are commands run against environments before the deploy.
	Plan   []lrStep `yaml:"plan"`
	Expect struct {
		// Migrate is what the deploy of an environment says.
		Migrate map[string]lrOutcome `yaml:"migrate"`
		// Check is what check says after the deploy; exit 0 unless
		// given here.
		Check map[string]lrOutcome `yaml:"check"`
	} `yaml:"expect"`
	// After are steps run after the deploys, against the project.
	After []lrStep `yaml:"after"`
	// Probes try something on a copy of the project, and of a database,
	// that the release did not do: what would have happened.
	Probes []lrProbe `yaml:"probes"`
	// Known names the findings this release works around.
	Known []string `yaml:"known"`
}

// lrOutcome is what a command is expected to do.
type lrOutcome struct {
	Exit int `yaml:"exit"`
	// Output must all appear, Absent must not, in the output with runs of
	// spaces collapsed.
	Output []string `yaml:"output"`
	Absent []string `yaml:"absent"`
	// Known names the finding that makes this the outcome.
	Known string `yaml:"known"`
}

// lrStep is one thing done to the project or a database. Exactly one of the
// action fields is set.
type lrStep struct {
	// Overlay copies a directory of the release over the project: "." is
	// the release directory itself (its fixtures/, migrations/,
	// models.go.txt and fixture-migrate.yml), anything else a
	// subdirectory laid out the same way.
	Overlay string `yaml:"overlay"`
	// Remove deletes files of the project.
	Remove []string `yaml:"remove"`
	// Generate runs generate -name -at against the workstation, migrated
	// to the project's schema first.
	Generate *lrGenerate `yaml:"generate"`
	// CLI runs the command with these arguments, DATABASE_URL set to Env
	// (default the workstation).
	CLI []string `yaml:"cli"`
	// App runs the application built from the project against Env.
	App []string `yaml:"app"`
	// SQL runs against Env; with Want, it is a query whose single value
	// must be Want.
	SQL  string  `yaml:"sql"`
	Want *string `yaml:"want"`
	// Edit replaces text in one file of the project.
	Edit *lrEdit `yaml:"edit"`
	// Merge makes branches with git and merges them.
	Merge *lrMerge `yaml:"merge"`
	// File checks a file of the project: Output must be in it, Absent not.
	File string `yaml:"file"`

	Env string `yaml:"env"`
	// Vars are environment variables for CLI and App, NAME=value.
	Vars      []string `yaml:"vars"`
	Note      string   `yaml:"note"`
	lrOutcome `yaml:",inline"`
}

type lrGenerate struct {
	Name  string   `yaml:"name"`
	At    string   `yaml:"at"`
	Flags []string `yaml:"flags"`
}

type lrEdit struct {
	File string `yaml:"file"` // a glob, relative to the project, matching one file
	Old  string `yaml:"old"`
	New  string `yaml:"new"`
	// Regexp reads Old as a regular expression, for text with ids in it.
	Regexp bool `yaml:"regexp"`
}

// lrMerge is two branches made from the project as it stands, each with
// its own steps, merged with git into the project.
type lrMerge struct {
	Branches []struct {
		Name  string   `yaml:"name"`
		Steps []lrStep `yaml:"steps"`
	} `yaml:"branches"`
	// Conflicts are the files git must report conflicting.
	Conflicts []string `yaml:"conflicts"`
	// Resolve is overlaid on the conflicted files: the resolution.
	Resolve string `yaml:"resolve"`
	// State is the side of the state file taken: ours or theirs.
	State string `yaml:"state"`
}

type lrProbe struct {
	Name string `yaml:"name"`
	// From is "start" to copy the project as it was before the release's
	// steps, rather than as they left it.
	From string `yaml:"from"`
	// When is "before-deploy" to run before the release is deployed,
	// rather than after.
	When string `yaml:"when"`
	// Clone copies this environment's database, or with "fresh" makes an
	// empty one; steps name it "clone". Clone2 is a second one, "clone2".
	Clone  string   `yaml:"clone"`
	Clone2 string   `yaml:"clone2"`
	Steps  []lrStep `yaml:"steps"`
	Known  string   `yaml:"known"`
}

type lrEnv struct {
	name, db, dsn string
	conn          *bun.DB
	// traffic: the application writes into it between releases.
	traffic bool
	// deployed is the last release deployed.
	deployed string
}

type longrun struct {
	t        *testing.T
	ctx      context.Context
	ex       lrExample
	admin    *bun.DB
	base     *url.URL
	repo     string // the bun-fixture-migrate module
	example  string // examples/<name>
	work     string
	project  string
	tool     string
	envs     map[string]*lrEnv
	release  string
	builds   int
	timings  []string
	logs     string
	traffics int
}

// replayExample replays an example's timeline release by release, then
// compares what it produced with the committed example.
func replayExample(t *testing.T, ex lrExample) {
	if testing.Short() {
		t.Skip("replays a dozen releases against several databases; skipped with -short")
	}
	lr := newLongrun(t, ex)
	started := time.Now()
	entries, err := os.ReadDir(filepath.Join(lr.example, "timeline"))
	if err != nil {
		t.Fatal(err)
	}
	var releases []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "r") {
			releases = append(releases, e.Name())
		}
	}
	sort.Strings(releases)
	if len(releases) == 0 {
		t.Fatal("no releases in the timeline")
	}
	until := os.Getenv("BFM_LONGRUN_UNTIL")
	for _, rel := range releases {
		lr.replay(rel)
		if rel == until {
			t.Logf("stopped after %s (BFM_LONGRUN_UNTIL); the example was not compared", rel)
			t.Log("timings:\n  " + strings.Join(lr.timings, "\n  "))
			return
		}
	}
	lr.finish()
	lr.timings = append(lr.timings, fmt.Sprintf("total %s, %d application builds", time.Since(started).Round(time.Millisecond), lr.builds))
	t.Log("timings:\n  " + strings.Join(lr.timings, "\n  "))
}

func newLongrun(t *testing.T, ex lrExample) *longrun {
	t.Helper()
	admin := connect(t)
	var version int
	if err := admin.QueryRowContext(context.Background(), "SHOW server_version_num").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < ex.minVersion {
		t.Skip(ex.minReason)
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no Go toolchain to build with")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git to merge with")
	}
	base, err := url.Parse(os.Getenv("BUN_FIXTURE_MIGRATE_POSTGRES"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/RELAXccc/bun-fixture-migrate").Output()
	if err != nil {
		t.Fatalf("find the module: %v", err)
	}
	repo := strings.TrimSpace(string(out))
	work := os.Getenv("BFM_LONGRUN_WORK")
	if work == "" {
		work = t.TempDir()
	} else {
		work = filepath.Join(work, ex.name)
		os.RemoveAll(work)
		if err := os.MkdirAll(work, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	lr := &longrun{t: t, ctx: context.Background(), ex: ex, admin: admin, base: base, repo: repo,
		example: filepath.Join(repo, "examples", ex.name), work: work,
		project: filepath.Join(work, "project"), tool: filepath.Join(work, "bin", "bun-fixture-migrate"),
		envs: map[string]*lrEnv{}, logs: filepath.Join(work, "logs")}
	for _, dir := range []string{lr.project, filepath.Join(work, "bin"), lr.logs} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("go", "build", "-o", lr.tool,
		"github.com/RELAXccc/bun-fixture-migrate/cmd/bun-fixture-migrate").CombinedOutput(); err != nil {
		t.Fatalf("build the command: %v\n%s", err, out)
	}
	// The project starts as the parts of the example no release changes:
	// the deploy step, the module, the migrations package.
	for _, rel := range []string{"main.go", "go.sum", filepath.Join("migrations", "migrations.go")} {
		lrCopyFile(t, filepath.Join(lr.example, rel), filepath.Join(lr.project, rel))
	}
	mod, err := os.ReadFile(filepath.Join(lr.example, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	mod = bytes.Replace(mod, []byte("=> ../.."), []byte("=> "+repo), 1)
	lrWrite(t, filepath.Join(lr.project, "go.mod"), mod)
	t.Cleanup(func() {
		for _, env := range lr.envs {
			if env.conn != nil {
				env.conn.Close()
			}
			if os.Getenv("BFM_LONGRUN_KEEP") == "" {
				lr.dropDB(env.db)
			}
		}
	})
	return lr
}

// replay makes one release: the developer's steps, the build, the deploys,
// and everything checked after them.
func (lr *longrun) replay(rel string) {
	t := lr.t
	lr.release = rel
	started := time.Now()
	dir := filepath.Join(lr.example, "timeline", rel)
	data, err := os.ReadFile(filepath.Join(dir, "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var m lrManifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("%s/release.yml: %v", rel, err)
	}
	t.Logf("=== %s: %s", rel, strings.SplitN(strings.TrimSpace(m.Description), "\n", 2)[0])
	for _, k := range m.Known {
		t.Logf("%s: known: %s", rel, k)
	}

	start := filepath.Join(lr.work, "start-"+rel)
	lrCopyTree(t, lr.project, start)
	steps := m.Steps
	if len(steps) == 0 {
		steps = []lrStep{{Overlay: "."}}
	}
	for i, s := range steps {
		lr.step(lr.project, dir, fmt.Sprintf("step %d", i+1), s, nil, true)
	}
	app := lr.build(lr.project, rel)
	developed := time.Since(started)

	for _, name := range m.Create {
		env := lr.createEnv(name)
		env.traffic = true
	}
	if _, ok := lr.envs["workstation"]; !ok {
		lr.createEnv("workstation")
	}
	lr.createEnv("dev")

	for _, name := range lrSortedKeys(m.BeforeDeploy) {
		env := lr.env(name)
		for _, stmt := range m.BeforeDeploy[name] {
			run(t, env.conn, stmt)
		}
	}
	for _, p := range m.Probes {
		if p.When == "before-deploy" {
			lr.probe(dir, start, p)
		}
	}
	for i, p := range m.Plan {
		lr.step(lr.project, dir, fmt.Sprintf("plan %d", i+1), p, nil, false)
	}

	targets := append([]string{"dev"}, m.Deploy...)
	targets = append(targets, "workstation")
	var timings []string
	before := map[string]string{}
	for _, name := range targets {
		before[name] = lr.appData(lr.env(name))
	}
	for _, name := range targets {
		env := lr.env(name)
		deployStarted := time.Now()
		want := m.Expect.Migrate[name]
		var out string
		if name == "prod" && m.Replicas > 1 {
			out = lr.replicas(env, app, m.Replicas)
		} else {
			out = lr.expect(fmt.Sprintf("deploy %s", name), want, app, lr.project, env.dsn, nil, "migrate")
		}
		lr.checkOutcome("deploy "+name, want, out)
		env.deployed = rel
		timings = append(timings, fmt.Sprintf("%s %s", name, time.Since(deployStarted).Round(time.Millisecond)))
	}
	for _, name := range targets {
		lr.afterDeploy(lr.env(name), before[name], m.Expect.Check[name])
	}
	// Master data equals the fresh database's, wherever check found the
	// database agrees with the file.
	devMaster := lr.master(lr.env("dev"))
	for _, name := range targets[1:] {
		if m.Expect.Check[name].Exit != 0 {
			continue
		}
		if got := lr.master(lr.env(name)); got != devMaster {
			t.Fatalf("%s: the master data of %s differs from a fresh database's:\n%s", rel, name, lrDiff(devMaster, got))
		}
	}
	for i, s := range m.After {
		lr.step(lr.project, dir, fmt.Sprintf("after %d", i+1), s, nil, false)
	}
	for _, p := range m.Probes {
		if p.When == "" {
			lr.probe(dir, start, p)
		}
	}
	for _, name := range lrSortedKeys(lr.envs) {
		if env := lr.envs[name]; env.traffic && env.deployed == rel {
			lr.traffic(env)
		}
	}
	lr.timings = append(lr.timings, fmt.Sprintf("%s: %s (developer steps and build %s; deploys %s)",
		rel, time.Since(started).Round(time.Millisecond), developed.Round(time.Millisecond), strings.Join(timings, ", ")))
}

// step runs one step in a project directory. main is false for a probe's
// copy, whose generate must not migrate the workstation.
func (lr *longrun) step(project, relDir, label string, s lrStep, clones map[string]string, main bool) {
	t := lr.t
	t.Helper()
	label = lr.release + " " + label
	if s.Note != "" {
		t.Logf("%s: %s", label, s.Note)
	}
	if s.Known != "" {
		t.Logf("%s: known: %s", label, s.Known)
	}
	dsn := func() string {
		switch s.Env {
		case "", "workstation":
			if _, ok := lr.envs["workstation"]; !ok {
				lr.createEnv("workstation")
			}
			return lr.env("workstation").dsn
		case "clone", "clone2":
			db, ok := clones[s.Env]
			if !ok {
				t.Fatalf("%s: env %s outside a probe that makes it", label, s.Env)
			}
			return lr.dsnOf(db)
		}
		return lr.env(s.Env).dsn
	}
	switch {
	case s.Overlay != "":
		lr.overlay(filepath.Join(relDir, s.Overlay), project)
	case len(s.Remove) > 0:
		for _, rel := range s.Remove {
			if err := os.Remove(filepath.Join(project, rel)); err != nil {
				t.Fatalf("%s: %v", label, err)
			}
		}
	case s.Generate != nil:
		if main {
			lr.migrateWorkstation(project)
		}
		args := []string{"generate", "-name", s.Generate.Name, "-at", s.Generate.At}
		args = append(args, s.Generate.Flags...)
		out := lr.expect(label+" generate", s.lrOutcome, lr.tool, project, dsn(), nil, args...)
		lr.checkOutcome(label+" generate", s.lrOutcome, out)
	case len(s.CLI) > 0:
		out := lr.expect(label+" "+strings.Join(s.CLI, " "), s.lrOutcome, lr.tool, project, dsn(), s.Vars, s.CLI...)
		lr.checkOutcome(label, s.lrOutcome, out)
	case len(s.App) > 0:
		app := lr.build(project, lr.release+"-"+filepath.Base(project))
		out := lr.expect(label+" app "+strings.Join(s.App, " "), s.lrOutcome, app, project, dsn(), s.Vars, s.App...)
		lr.checkOutcome(label, s.lrOutcome, out)
	case s.SQL != "":
		db := openDB(t, dsn(), nil)
		defer db.Close()
		if s.Want == nil {
			run(t, db, s.SQL)
			break
		}
		var got string
		if err := db.QueryRowContext(lr.ctx, s.SQL).Scan(&got); err != nil {
			t.Fatalf("%s: %s: %v", label, s.SQL, err)
		}
		if got != *s.Want {
			t.Fatalf("%s: %s\n got %q\nwant %q", label, s.SQL, got, *s.Want)
		}
	case s.Edit != nil:
		matches, err := filepath.Glob(filepath.Join(project, s.Edit.File))
		if err != nil || len(matches) != 1 {
			t.Fatalf("%s: %s matches %v (%v), want one file", label, s.Edit.File, matches, err)
		}
		data, err := os.ReadFile(matches[0])
		if err != nil {
			t.Fatal(err)
		}
		if s.Edit.Regexp {
			re := regexp.MustCompile(s.Edit.Old)
			if n := len(re.FindAllIndex(data, -1)); n != 1 {
				t.Fatalf("%s: %q matches %s %d times, want once", label, s.Edit.Old, matches[0], n)
			}
			lrWrite(t, matches[0], re.ReplaceAll(data, []byte(s.Edit.New)))
			break
		}
		if bytes.Count(data, []byte(s.Edit.Old)) != 1 {
			t.Fatalf("%s: %q is not in %s exactly once", label, s.Edit.Old, matches[0])
		}
		lrWrite(t, matches[0], bytes.Replace(data, []byte(s.Edit.Old), []byte(s.Edit.New), 1))
	case s.Merge != nil:
		lr.merge(project, relDir, label, s.Merge)
	case s.File != "":
		data, err := os.ReadFile(filepath.Join(project, s.File))
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		lr.checkOutcome(label+" "+s.File, s.lrOutcome, lrCollapse(string(data)))
	default:
		t.Fatalf("%s: a step that does nothing", label)
	}
}

// overlay copies the project files of a release directory over a project.
// Go sources are kept as .go.txt in the timeline, so the go tool does not
// build a dozen copies of package main; the suffix comes off here.
func (lr *longrun) overlay(src, project string) {
	t := lr.t
	t.Helper()
	for _, part := range []string{"fixtures", "migrations", "models.go.txt", "fixture-migrate.yml"} {
		from := filepath.Join(src, part)
		info, err := os.Stat(from)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			lrCopyFile(t, from, filepath.Join(project, strings.TrimSuffix(part, ".txt")))
			continue
		}
		err = filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(src, path)
			lrCopyFile(t, path, filepath.Join(project, strings.TrimSuffix(rel, ".txt")))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// build compiles the application of a project.
func (lr *longrun) build(project, name string) string {
	lr.t.Helper()
	bin := filepath.Join(lr.work, "bin", "app-"+name)
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = project
	if out, err := cmd.CombinedOutput(); err != nil {
		lr.t.Fatalf("%s: build the application: %v\n%s", lr.release, err, out)
	}
	lr.builds++
	return bin
}

// migrateWorkstation brings the developer's database to the project's
// schema before a generate, the way a developer runs the application after
// adding a SQL migration: generate lints the fixture files against it.
func (lr *longrun) migrateWorkstation(project string) {
	ws, ok := lr.envs["workstation"]
	if !ok {
		ws = lr.createEnv("workstation")
	}
	app := lr.build(project, lr.release+"-workstation")
	lr.expect(lr.release+" migrate the workstation", lrOutcome{}, app, project, ws.dsn, nil, "migrate")
}

// expect runs a program in dir with DATABASE_URL and fails unless it exits
// as want says. It returns the output with runs of spaces collapsed.
func (lr *longrun) expect(label string, want lrOutcome, bin, dir, dsn string, env []string, args ...string) string {
	lr.t.Helper()
	code, out := lr.exec(bin, dir, dsn, env, args...)
	if code != want.Exit {
		lr.t.Fatalf("%s: exit %d, want %d\n%s", label, code, want.Exit, out)
	}
	return lrCollapse(out)
}

func (lr *longrun) exec(bin, dir, dsn string, env []string, args ...string) (int, string) {
	lr.t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "DATABASE_URL="+dsn), env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	started := time.Now()
	err := cmd.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		lr.t.Fatal(err)
	}
	lr.log(fmt.Sprintf("$ %s %s  [exit %d, %s]\n%s", filepath.Base(bin), strings.Join(args, " "), code,
		time.Since(started).Round(time.Millisecond), out.String()))
	return code, out.String()
}

func (lr *longrun) log(text string) {
	f, err := os.OpenFile(filepath.Join(lr.logs, lr.release+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		lr.t.Fatal(err)
	}
	defer f.Close()
	fmt.Fprintln(f, text)
}

func (lr *longrun) checkOutcome(label string, want lrOutcome, out string) {
	lr.t.Helper()
	for _, s := range want.Output {
		if !strings.Contains(out, lrCollapse(s)) {
			lr.t.Fatalf("%s: the output does not say %q:\n%s", label, s, out)
		}
	}
	for _, s := range want.Absent {
		if strings.Contains(out, lrCollapse(s)) {
			lr.t.Fatalf("%s: the output says %q:\n%s", label, s, out)
		}
	}
}

// replicas deploys prod from several processes at once. bun's lock does not
// wait; the application retries it, so every replica succeeds and exactly one
// of them migrates. (known: saas F7 -- the retry is the example's main.go, not
// bun's or the tool's; the docs say bun's lock serialises replicas.)
func (lr *longrun) replicas(env *lrEnv, app string, n int) string {
	t := lr.t
	type result struct {
		code int
		out  string
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			cmd := exec.Command(app, "migrate")
			cmd.Dir = lr.project
			cmd.Env = append(os.Environ(), "DATABASE_URL="+env.dsn)
			out, err := cmd.CombinedOutput()
			code := 0
			if exit, ok := err.(*exec.ExitError); ok {
				code = exit.ExitCode()
			} else if err != nil {
				code = -1
				out = append(out, err.Error()...)
			}
			results[i] = result{code, string(out)}
		}(i)
	}
	// Hold bun's lock while they start, as a deploy in progress would, so
	// every replica has to wait for it.
	run(t, env.conn, "INSERT INTO bun_migration_locks (table_name) VALUES ('bun_migrations')")
	close(start)
	time.Sleep(500 * time.Millisecond)
	run(t, env.conn, "DELETE FROM bun_migration_locks")
	wg.Wait()
	migrated := 0
	var all []string
	for i, r := range results {
		lr.log(fmt.Sprintf("$ replica %d: migrate  [exit %d]\n%s", i, r.code, r.out))
		if r.code != 0 {
			t.Fatalf("%s: replica %d of %d: exit %d\n%s", lr.release, i, n, r.code, r.out)
		}
		if strings.Contains(r.out, "migrated to group") {
			migrated++
		}
		if !strings.Contains(r.out, "waited") {
			t.Fatalf("%s: replica %d did not wait for the lock:\n%s", lr.release, i, r.out)
		}
		all = append(all, r.out)
	}
	if migrated != 1 {
		t.Fatalf("%s: %d replicas migrated, want exactly 1:\n%s", lr.release, migrated, strings.Join(all, "\n---\n"))
	}
	return lrCollapse(strings.Join(all, "\n"))
}

// afterDeploy checks an environment the release was just deployed to.
func (lr *longrun) afterDeploy(env *lrEnv, before string, check lrOutcome) {
	t := lr.t
	label := lr.release + " " + env.name
	lr.expect(label+" status -require-applied", lrOutcome{}, lr.tool, lr.project, env.dsn, nil, "status", "-require-applied")
	out := lr.expect(label+" check", check, lr.tool, lr.project, env.dsn, nil, "check")
	lr.checkOutcome(label+" check", check, out)
	if after := lr.appData(env); before != "" && after != before {
		t.Fatalf("%s: the application's data changed in the deploy:\n%s", label, lrDiff(before, after))
	}
	lr.appInserts(env)
}

// appData is the application's own rows as the master rows they point at,
// by natural key: what a deploy must never change.
func (lr *longrun) appData(env *lrEnv) string {
	parts := []string{"app data"}
	for _, q := range lr.ex.appData {
		var s string
		if err := env.conn.QueryRowContext(lr.ctx, q).Scan(&s); err != nil {
			if strings.Contains(err.Error(), "does not exist") {
				return "" // not migrated yet
			}
			lr.t.Fatalf("%s %s: %v", lr.release, env.name, err)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n")
}

// appInserts inserts into every serial master table the way the
// application or an admin UI would, without an id, and rolls back: an id the
// sequence hands out that a row already holds fails here.
func (lr *longrun) appInserts(env *lrEnv) {
	for _, ins := range lr.ex.appInserts {
		if env.traffic && ins.notWithTraffic {
			continue
		}
		if ins.table != "" {
			var exists bool
			if err := env.conn.QueryRowContext(lr.ctx, "SELECT to_regclass(?) IS NOT NULL", ins.table).Scan(&exists); err != nil {
				lr.t.Fatal(err)
			}
			if !exists {
				continue
			}
		}
		tx, err := env.conn.BeginTx(lr.ctx, nil)
		if err != nil {
			lr.t.Fatal(err)
		}
		_, err = tx.ExecContext(lr.ctx, ins.sql)
		tx.Rollback()
		if err != nil {
			lr.t.Fatalf("%s %s: the application's insert fails, the sequence is behind: %s: %v",
				lr.release, env.name, ins.sql, err)
		}
	}
}

// traffic is the application at work in an environment between releases.
func (lr *longrun) traffic(env *lrEnv) {
	lr.traffics++
	lr.ex.traffic(lr, env)
}

// master is a database's master data as export writes it, made comparable
// between two databases: no comment lines, no ids (a uuid or a serial id
// differs between a database that migrated and one that was seeded, and
// check compares the ids that the files name), rows in a fixed order.
func (lr *longrun) master(env *lrEnv) string {
	t := lr.t
	// known: saas F2 -- export -stdout is not comparable between two databases
	// holding the same master data: it carries the time of the export, the
	// database's own ids (a gen_random_uuid() key included), and rows in id
	// order. Normalised here.
	code, raw := lr.exec(lr.tool, lr.project, env.dsn, nil, "export", "-stdout")
	if code != 0 {
		t.Fatalf("%s %s: export -stdout: exit %d\n%s", lr.release, env.name, code, raw)
	}
	type model struct {
		Model string           `yaml:"model"`
		Rows  []map[string]any `yaml:"rows"`
	}
	result := map[string][]string{}
	file := ""
	var part strings.Builder
	flush := func() {
		var models []model
		if err := yaml.Unmarshal([]byte(part.String()), &models); err != nil {
			t.Fatalf("%s %s: read the export: %v\n%s", lr.release, env.name, err, part.String())
		}
		for _, m := range models {
			var rows []string
			for _, row := range m.Rows {
				delete(row, "id")
				b, err := json.Marshal(row)
				if err != nil {
					t.Fatal(err)
				}
				rows = append(rows, string(b))
			}
			sort.Strings(rows)
			result[file+" "+m.Model] = rows
		}
		part.Reset()
	}
	for _, line := range strings.Split(raw, "\n") {
		if name, ok := strings.CutPrefix(line, "# ==> "); ok {
			flush()
			file = strings.TrimSuffix(name, " <==")
			continue
		}
		part.WriteString(line + "\n")
	}
	flush()
	var b strings.Builder
	for _, k := range lrSortedKeys(result) {
		b.WriteString(k + "\n")
		for _, row := range result[k] {
			b.WriteString("  " + row + "\n")
		}
	}
	return b.String()
}

// probe tries something on a copy of the project, and optionally on a
// copy of a database.
func (lr *longrun) probe(relDir, start string, p lrProbe) {
	t := lr.t
	t.Logf("%s probe: %s", lr.release, p.Name)
	if p.Known != "" {
		t.Logf("%s probe: known: %s", lr.release, p.Known)
	}
	copyDir := filepath.Join(lr.work, "probe-"+lr.release+"-"+lrSlug(p.Name))
	os.RemoveAll(copyDir)
	switch p.From {
	case "":
		lrCopyTree(t, lr.project, copyDir)
	case "start":
		lrCopyTree(t, start, copyDir)
	default:
		t.Fatalf("%s probe %q: from %q, want start or nothing", lr.release, p.Name, p.From)
	}
	if p.When != "" && p.When != "before-deploy" {
		t.Fatalf("%s probe %q: when %q, want before-deploy or nothing", lr.release, p.Name, p.When)
	}
	clones := map[string]string{}
	for name, from := range map[string]string{"clone": p.Clone, "clone2": p.Clone2} {
		db := "bfm_" + lr.ex.name + "_probe_" + name
		switch from {
		case "":
			continue
		case "fresh":
			lr.dropDB(db)
			run(t, lr.admin, "CREATE DATABASE "+db)
		default:
			lr.cloneDB(lr.env(from), db)
		}
		clones[name] = db
		defer lr.dropDB(db)
	}
	for i, s := range p.Steps {
		lr.step(copyDir, relDir, fmt.Sprintf("probe %q step %d", p.Name, i+1), s, clones, false)
	}
}

// merge makes each branch from the project as it stands, commits it, and
// merges them all into the project with git, then resolves the conflicts.
func (lr *longrun) merge(project, relDir, label string, m *lrMerge) {
	t := lr.t
	git := func(args ...string) (int, string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=dev", "GIT_AUTHOR_EMAIL=dev@example.com",
			"GIT_COMMITTER_NAME=dev", "GIT_COMMITTER_EMAIL=dev@example.com", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		lr.log(fmt.Sprintf("$ git %s  [exit %d]\n%s", strings.Join(args, " "), code, out))
		return code, string(out)
	}
	must := func(args ...string) string {
		code, out := git(args...)
		if code != 0 {
			t.Fatalf("%s: git %v: exit %d\n%s", label, args, code, out)
		}
		return out
	}
	if _, err := os.Stat(filepath.Join(project, ".git")); err != nil {
		must("init", "-q", "-b", "main")
	}
	must("add", "-A")
	must("commit", "-q", "--allow-empty", "-m", "before "+lr.release)
	for _, b := range m.Branches {
		must("checkout", "-q", "-b", b.Name, "main")
		for i, s := range b.Steps {
			lr.step(project, relDir, fmt.Sprintf("%s branch %s step %d", label, b.Name, i+1), s, nil, true)
		}
		must("add", "-A")
		must("commit", "-q", "-m", b.Name)
	}
	must("checkout", "-q", "main")
	var conflicts []string
	for _, b := range m.Branches {
		if code, _ := git("merge", "--no-ff", "-q", "-m", "merge "+b.Name, b.Name); code != 0 {
			out := must("diff", "--name-only", "--diff-filter=U")
			conflicts = append(conflicts, strings.Fields(out)...)
		}
	}
	sort.Strings(conflicts)
	want := append([]string{}, m.Conflicts...)
	sort.Strings(want)
	if strings.Join(conflicts, " ") != strings.Join(want, " ") {
		t.Fatalf("%s: the merge conflicts in %v, want %v", label, conflicts, want)
	}
	if len(conflicts) == 0 {
		return
	}
	lr.overlay(filepath.Join(relDir, m.Resolve), project)
	switch m.State {
	case "ours", "theirs":
		must("checkout", "--"+m.State, "--", "migrations/fixture_state.yml")
	default:
		t.Fatalf("%s: state %q, want ours or theirs", label, m.State)
	}
	must("add", "-A")
	must("commit", "-q", "-m", "merge")
}

func (lr *longrun) env(name string) *lrEnv {
	env, ok := lr.envs[name]
	if !ok {
		lr.t.Fatalf("%s: no environment %s yet", lr.release, name)
	}
	return env
}

func (lr *longrun) dsnOf(db string) string {
	u := *lr.base
	u.Path = "/" + db
	return u.String()
}

// createEnv makes an empty database for an environment, replacing one of
// the same name.
func (lr *longrun) createEnv(name string) *lrEnv {
	if old, ok := lr.envs[name]; ok && old.conn != nil {
		old.conn.Close()
	}
	db := "bfm_" + lr.ex.name + "_" + name
	lr.dropDB(db)
	run(lr.t, lr.admin, "CREATE DATABASE "+db)
	env := &lrEnv{name: name, db: db, dsn: lr.dsnOf(db)}
	env.conn = openDB(lr.t, env.dsn, nil)
	if old, ok := lr.envs[name]; ok {
		env.traffic = old.traffic
	}
	lr.envs[name] = env
	return env
}

func (lr *longrun) dropDB(db string) {
	ctx := context.Background()
	lr.admin.ExecContext(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = ?", db)
	lr.admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+db)
}

// cloneDB copies an environment's database, as a restore of its backup
// would.
func (lr *longrun) cloneDB(src *lrEnv, db string) {
	src.conn.Close()
	lr.dropDB(db)
	run(lr.t, lr.admin, fmt.Sprintf("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s'", src.db))
	run(lr.t, lr.admin, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", db, src.db))
	src.conn = openDB(lr.t, src.dsn, nil)
}

// finish compares the project the history produced with the committed
// example, or with BFM_LONGRUN_UPDATE rewrites the example.
func (lr *longrun) finish() {
	t := lr.t
	update := os.Getenv("BFM_LONGRUN_UPDATE") != ""
	var problems []string
	for _, part := range []string{"migrations", "fixtures", "fixture-migrate.yml", "models.go"} {
		produced := lrFiles(t, filepath.Join(lr.project, part))
		committed := lrFiles(t, filepath.Join(lr.example, part))
		for _, rel := range lrSortedKeys(produced) {
			have, ok := committed[rel]
			switch {
			case !ok:
				problems = append(problems, "missing in the example: "+filepath.Join(part, rel))
			case !bytes.Equal(have, produced[rel]):
				problems = append(problems, "differs: "+filepath.Join(part, rel)+"\n"+lrDiff(string(have), string(produced[rel])))
			default:
				continue
			}
			if update {
				lrWrite(t, filepath.Join(lr.example, part, rel), produced[rel])
			}
		}
		for _, rel := range lrSortedKeys(committed) {
			if _, ok := produced[rel]; ok {
				continue
			}
			problems = append(problems, "not produced by the history: "+filepath.Join(part, rel))
			if update {
				if err := os.Remove(filepath.Join(lr.example, part, rel)); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	switch {
	case len(problems) == 0:
	case update:
		t.Logf("rewrote examples/%s:\n%s", lr.ex.name, strings.Join(problems, "\n"))
	default:
		t.Fatalf("examples/%s is not what its history produces (BFM_LONGRUN_UPDATE=1 rewrites it):\n%s",
			lr.ex.name, strings.Join(problems, "\n"))
	}
}

// lrFiles reads a file, or every file under a directory, by path relative
// to it.
func lrFiles(t *testing.T, root string) map[string][]byte {
	out := map[string][]byte{}
	info, err := os.Stat(root)
	if os.IsNotExist(err) {
		return out
	}
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		data, err := os.ReadFile(root)
		if err != nil {
			t.Fatal(err)
		}
		out[""] = data
		return out
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out[rel] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func lrCopyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	lrWrite(t, to, data)
}

func lrCopyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		lrCopyFile(t, path, filepath.Join(to, rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func lrWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func lrCollapse(s string) string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		lines = append(lines, strings.Join(strings.Fields(line), " "))
	}
	return strings.Join(lines, "\n")
}

func lrSortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func lrSlug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}

// lrDiff shows the lines only one side has.
func lrDiff(want, got string) string {
	wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
	inWant, inGot := map[string]int{}, map[string]int{}
	for _, l := range wl {
		inWant[l]++
	}
	for _, l := range gl {
		inGot[l]++
	}
	var b strings.Builder
	for _, l := range wl {
		if inGot[l] == 0 {
			b.WriteString("- " + l + "\n")
		}
	}
	for _, l := range gl {
		if inWant[l] == 0 {
			b.WriteString("+ " + l + "\n")
		}
	}
	if b.Len() > 4000 {
		return b.String()[:4000] + "\n..."
	}
	return b.String()
}
