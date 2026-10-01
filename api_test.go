package fixturemigrate

// The Project methods without a database: what generate, baseline and status
// do against files, the refusals and the errors a program tells apart, and
// the JSON each result encodes to.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

const apiConfig = `fixture: fixtures/fixture.yml
out: migrations
package: migrations
seed_guard_table: plans
models:
  Currency:
    table: currencies
    ref: code
    key: [code]
  Plan:
    table: plans
    serial: true
    key: [name]
    references:
      currency_id: Currency
`

const apiOld = `- model: Currency
  rows:
    - _id: eur
      id: 1
      code: EUR
      symbol: "E"
- model: Plan
  rows:
    - _id: team
      id: 2
      name: team
      currency_id: '{{ $.Currency.eur.ID }}'
      price_cents: 2000
`

var apiNew = strings.Replace(apiOld, "price_cents: 2000", "price_cents: 2500", 1)

// apiProject writes a project into a temporary directory: the configuration,
// the fixture file, the base file next to it and a migrations package.
func apiProject(t *testing.T, config, fixture string) string {
	t.Helper()
	dir := t.TempDir()
	apiWrite(t, dir, "fixture-migrate.yml", config)
	apiWrite(t, dir, "fixtures/fixture.yml", fixture)
	apiWrite(t, dir, "base.yml", apiOld)
	apiWrite(t, dir, "migrations/migrations.go", "package migrations\n\nvar Migrations = 1\n")
	return dir
}

func apiWrite(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func loadProject(t *testing.T, dir string) *Project {
	t.Helper()
	p, err := LoadProject(filepath.Join(dir, "fixture-migrate.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadProjectResolvesPathsAsTheCommandDoes(t *testing.T) {
	dir := apiProject(t, apiConfig, apiNew)
	p := loadProject(t, dir)
	if p.Dir != dir || p.OutDir() != filepath.Join(dir, "migrations") ||
		p.StatePath() != filepath.Join(dir, "migrations", "fixture_state.yml") ||
		strings.Join(p.FixturePaths(), ",") != filepath.Join(dir, "fixtures", "fixture.yml") {
		t.Fatalf("%s %s %s %v", p.Dir, p.OutDir(), p.StatePath(), p.FixturePaths())
	}
	files, err := p.Files()
	if err != nil || len(files) != 1 || files[0].Path != "fixtures/fixture.yml" || string(files[0].Data) != apiNew {
		t.Fatalf("%v %+v", err, files)
	}

	// A fixture file that is not there is the error of the methods that
	// need it, not of loading: export writes it.
	if err := os.Remove(filepath.Join(dir, "fixtures", "fixture.yml")); err != nil {
		t.Fatal(err)
	}
	if err := p.ReadFiles(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadFiles: %v", err)
	}
	p = loadProject(t, dir)
	if _, err := p.Files(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Files: %v", err)
	}
	if _, err := p.Generate(context.Background(), nil, GenerateOptions{Name: "x"}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := LoadProject(filepath.Join(dir, "nowhere.yml")); err == nil {
		t.Fatal("a configuration that is not there is an error")
	}

	// A configuration built in code is prepared.
	cfg := &Config{Fixture: "fixtures/fixture.yml", Models: map[string]*Model{"Plan": {Table: "plans"}}}
	_, err = NewProject(cfg, dir)
	if err != nil || cfg.Fixtures[0] != "fixtures/fixture.yml" || cfg.Models["Plan"].Ref != "name" {
		t.Fatalf("%v %+v", err, cfg)
	}
	if _, err := NewProject(&Config{}, dir); err == nil {
		t.Fatal("a configuration without models is refused")
	}
}

// The methods that cannot work without a database say so, after what is wrong
// with the files.
func TestMethodsThatNeedADatabaseSaySo(t *testing.T) {
	p := loadProject(t, apiProject(t, apiConfig, apiNew))
	ctx := context.Background()
	if _, err := p.Check(ctx, nil); err == nil || !strings.Contains(err.Error(), "Check needs a database") {
		t.Errorf("Check: %v", err)
	}
	if exp, err := p.Export(ctx, nil, ExportOptions{}); exp == nil || err == nil ||
		!strings.Contains(err.Error(), "Export needs a database") {
		t.Errorf("Export: %v", err)
	}
	if _, err := p.Sync(ctx, nil, SyncOptions{}); err == nil || !strings.Contains(err.Error(), "Sync needs a database") {
		t.Errorf("Sync: %v", err)
	}
	if _, err := p.Generate(ctx, nil, GenerateOptions{FromDB: true, Name: "x"}); err == nil ||
		!strings.Contains(err.Error(), "needs a database") {
		t.Errorf("Generate FromDB: %v", err)
	}
	if _, err := p.Status(ctx, nil, StatusOptions{RequireApplied: true}); err == nil {
		t.Error("RequireApplied needs a database")
	}
	if err := ReadOnly(ctx, nil, nil); err == nil {
		t.Error("ReadOnly without a database")
	}
}

func TestGenerateWritesNothingUntilWrite(t *testing.T) {
	dir := apiProject(t, apiConfig, apiNew)
	p := loadProject(t, dir)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	g, err := p.Generate(context.Background(), nil, GenerateOptions{Name: "plan prices", At: at,
		Old: filepath.Join(dir, "base.yml")})
	if err != nil {
		t.Fatal(err)
	}
	if g.ID != "20261001120000_fixture_plan_prices" || g.Path != filepath.Join(dir, "migrations", g.ID+".go") ||
		!strings.Contains(string(g.Source), `fixturechange.Lit("2500")`) || g.State == nil ||
		g.State.Migration != g.ID || strings.Join(g.Diff.Summary(), ";") != "Plan: 1 update" {
		t.Fatalf("%+v", g)
	}
	if _, err := os.Stat(g.Path); err == nil {
		t.Fatal("Generate wrote the migration")
	}
	if _, err := os.Stat(p.StatePath()); err == nil {
		t.Fatal("Generate wrote the state file")
	}
	written, err := g.Write()
	if err != nil || strings.Join(written, ",") != g.Path+","+p.StatePath() {
		t.Fatalf("%v %v", err, written)
	}
	if data, _ := os.ReadFile(g.Path); string(data) != string(g.Source) {
		t.Fatal("the migration written is not the source")
	}
	state, err := ReadState(p.StatePath())
	if err != nil || state.Covers != g.ID || string(state.Files[0].Data) != apiNew {
		t.Fatalf("%v %+v", err, state)
	}
	if _, err := g.Write(); err == nil || !strings.Contains(err.Error(), "exists already") {
		t.Fatalf("a second Write: %v", err)
	}

	// Against the state it wrote, nothing is left.
	g, err = p.Generate(context.Background(), nil, GenerateOptions{Name: "again"})
	if err != nil || len(g.Diff.Changes) != 0 || g.Source != nil || g.State != nil ||
		g.Diff.Base != "the state after "+state.Migration {
		t.Fatalf("%v %+v", err, g)
	}
}

func TestGenerateDryRunIsNotWritten(t *testing.T) {
	dir := apiProject(t, apiConfig, apiNew)
	p := loadProject(t, dir)
	g, err := p.Generate(context.Background(), nil, GenerateOptions{Name: "x", Old: filepath.Join(dir, "base.yml"),
		DryRun: true})
	if err != nil || g.Source == nil {
		t.Fatalf("%v %+v", err, g)
	}
	if _, err := g.Write(); err == nil {
		t.Fatal("a dry run was written")
	}
}

// A program tells a refusal from a failure with errors.Is, and finds what was
// refused with errors.As.
func TestGenerateRefusalsAreTyped(t *testing.T) {
	ctx := context.Background()
	// A rename, which the policy refuses, and a currency's symbol.
	renamed := strings.Replace(strings.Replace(apiOld, "name: team", "name: crew", 1), `symbol: "E"`, `symbol: "EUR"`, 1)
	dir := apiProject(t, apiConfig, renamed)
	p := loadProject(t, dir)
	base := filepath.Join(dir, "base.yml")

	g, err := p.Generate(ctx, nil, GenerateOptions{Name: "rename", Old: base})
	var refused *RefusedError
	if !errors.Is(err, ErrRefused) || !errors.As(err, &refused) || len(refused.Refusals) != 1 ||
		errors.Is(err, ErrFindings) || errors.Is(err, ErrLineage) || g == nil || len(g.Diff.Refusals) != 1 ||
		len(g.Diff.Changes) != 1 || g.Source != nil {
		t.Fatalf("a rename: %v %+v", err, g)
	}
	if _, werr := g.Write(); werr != err {
		t.Fatalf("a refused result is not written: %v", werr)
	}

	// Allowed to be partial, the rest is written and the rename is left out.
	g, err = p.Generate(ctx, nil, GenerateOptions{Name: "rename", Old: base, AllowPartial: true})
	if err != nil || g.State == nil || len(g.LeftOut) != 1 || !strings.Contains(g.LeftOut[0], "crew") {
		t.Fatalf("%v %+v", err, g)
	}

	// No name, and a migration to write.
	if _, err := p.Generate(ctx, nil, GenerateOptions{Old: base, AllowPartial: true}); !errors.Is(err, ErrNameRequired) {
		t.Fatalf("no name: %v", err)
	}

	// Two rows sharing a key, which the policy makes an error.
	twice := apiOld + "    - _id: team2\n      id: 3\n      name: team\n      currency_id: '{{ $.Currency.eur.ID }}'\n"
	p = loadProject(t, apiProject(t, apiConfig, twice))
	g, err = p.Generate(ctx, nil, GenerateOptions{Name: "x", Old: base})
	if !errors.Is(err, ErrFindings) || !errors.Is(err, ErrRefused) || !errors.As(err, &refused) ||
		len(refused.Findings) == 0 || refused.Findings[0].Kind != FindingDuplicateKey || len(g.Findings) == 0 {
		t.Fatalf("a duplicate key: %v %+v", err, g)
	}
}

// A fixture migration the state file does not include refuses generate,
// baseline and fails status, and each says which.
func TestTheStateFilesHistoryIsALineageError(t *testing.T) {
	ctx := context.Background()
	dir := apiProject(t, apiConfig, apiOld)
	p := loadProject(t, dir)
	b, err := p.Baseline(ctx, nil, BaselineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write(); err != nil {
		t.Fatal(err)
	}
	baselined, err := os.ReadFile(p.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	// A migration made a change and the state file went back to before it:
	// the migration is not in its history. Its file says it was written by
	// hand.
	apiWrite(t, dir, "fixtures/fixture.yml", apiNew)
	p = loadProject(t, dir)
	g, err := p.Generate(ctx, nil, GenerateOptions{Name: "by hand", At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Write(); err != nil {
		t.Fatal(err)
	}
	apiWrite(t, dir, "migrations/fixture_state.yml", string(baselined))
	apiWrite(t, dir, "migrations/20260101000000_fixture_by_hand.go",
		strings.Replace(string(g.Source), "// Generated by bun-fixture-migrate: ", "// Written by hand: ", 1))
	p = loadProject(t, dir)

	g, err = p.Generate(ctx, nil, GenerateOptions{Name: "x"})
	var refused *RefusedError
	if !errors.Is(err, ErrLineage) || !errors.As(err, &refused) || len(refused.Problems) != 1 ||
		strings.Join(g.NotInState, ",") != "20260101000000_fixture_by_hand" {
		t.Fatalf("generate: %v %+v", err, g)
	}
	g, err = p.Generate(ctx, nil, GenerateOptions{Name: "x", DryRun: true})
	if err != nil || len(g.Problems) != 1 || g.Source == nil {
		t.Fatalf("generate, dry run: %v %+v", err, g)
	}
	r, err := p.Status(ctx, nil, StatusOptions{})
	if err != nil || strings.Join(r.NotInState, ",") != "20260101000000_fixture_by_hand" || len(r.Failures) == 0 {
		t.Fatalf("status: %v %+v", err, r)
	}
	ms, err := ReadMigrations(p.OutDir())
	if err != nil {
		t.Fatal(err)
	}
	if problems := p.LineageProblems(ms); len(problems) != 1 || !strings.Contains(problems[0], "baseline -force") {
		t.Fatalf("lineage problems: %q", problems)
	}
	// Written by hand, it is recorded with Force, and the change with it.
	b, err = p.Baseline(ctx, nil, BaselineOptions{})
	if !errors.Is(err, ErrLineage) || strings.Join(b.NotInState, ",") != "20260101000000_fixture_by_hand" {
		t.Fatalf("baseline: %v %+v", err, b)
	}
	b, err = p.Baseline(ctx, nil, BaselineOptions{Force: true})
	if err != nil || b.State == nil || b.State.Covers != "20260101000000_fixture_by_hand" {
		t.Fatalf("baseline -force: %v %+v", err, b)
	}
}

func TestBaselineRecordsTheFilesOnWrite(t *testing.T) {
	ctx := context.Background()
	dir := apiProject(t, apiConfig, apiOld)
	p := loadProject(t, dir)
	b, err := p.Baseline(ctx, nil, BaselineOptions{})
	if err != nil || b.State == nil || b.Unchanged || b.Recorded != "fixtures/fixture.yml" {
		t.Fatalf("%v %+v", err, b)
	}
	if _, err := os.Stat(p.StatePath()); err == nil {
		t.Fatal("Baseline wrote the state file")
	}
	if written, err := b.Write(); err != nil || strings.Join(written, ",") != p.StatePath() {
		t.Fatalf("%v %v", err, written)
	}
	b, err = p.Baseline(ctx, nil, BaselineOptions{})
	if err != nil || !b.Unchanged || b.State != nil {
		t.Fatalf("again: %v %+v", err, b)
	}
	if written, err := b.Write(); err != nil || len(written) != 0 {
		t.Fatalf("nothing to write: %v %v", err, written)
	}

	// A change of content is refused without Force: no migration makes it.
	apiWrite(t, dir, "fixtures/fixture.yml", apiNew)
	p = loadProject(t, dir)
	b, err = p.Baseline(ctx, nil, BaselineOptions{})
	if !errors.Is(err, ErrUnmigrated) || !errors.Is(err, ErrRefused) || b.Diff == nil ||
		strings.Join(b.Diff.Summary(), ";") != "Plan: 1 update" {
		t.Fatalf("%v %+v", err, b)
	}
	if b, err = p.Baseline(ctx, nil, BaselineOptions{Force: true}); err != nil || b.State == nil {
		t.Fatalf("Force: %v %+v", err, b)
	}
}

func TestStatusOffline(t *testing.T) {
	ctx := context.Background()
	dir := apiProject(t, apiConfig, apiOld)
	p := loadProject(t, dir)
	b, err := p.Baseline(ctx, nil, BaselineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write(); err != nil {
		t.Fatal(err)
	}
	r, err := p.Status(ctx, nil, StatusOptions{})
	if err != nil || len(r.Failures) != 0 || r.Base != "the state file" || r.Database != nil {
		t.Fatalf("%v %+v", err, r)
	}
	apiWrite(t, dir, "fixtures/fixture.yml", apiNew)
	p = loadProject(t, dir)
	if r, err = p.Status(ctx, nil, StatusOptions{}); err != nil ||
		strings.Join(r.Uncovered, ";") != "Plan: 1 update" ||
		strings.Join(r.Failures, ";") != "the fixture file has changes no migration makes" {
		t.Fatalf("%v %+v", err, r)
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"refused":[]`, `"problems":[]`, `"migrations":[]`, `"findings":[]`,
		`"database":null`, `"failures":["the fixture file has changes no migration makes"]`} {
		if !strings.Contains(string(data), field) {
			t.Errorf("no %s in %s", field, data)
		}
	}
}

// Every list of a report is [] in JSON when it is empty, as a program ranging
// over it expects.
func TestTheJSONOfAReportHasNoNullLists(t *testing.T) {
	for name, v := range map[string]any{
		"check": &CheckReport{}, "sync": &SyncReport{}, "sync result": &SyncReport{SyncResult: &SyncResult{}},
	} {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(string(data), "null") {
			t.Errorf("%s: %s", name, data)
		}
	}
}

// A literal, NULL and a reference have to be told apart by a program, which
// the text report cannot do for the literal "NULL".
func TestCheckJSONTellsValuesApart(t *testing.T) {
	cfg := policyConfig(t, "")
	report := newCheckReport(cfg, &CheckResult{Result: &Result{Changes: []fixturechange.Change{{
		Model: "Plan", Kind: fixturechange.Update,
		Key: fixturechange.Values{"name": fixturechange.Lit("team")},
		Old: fixturechange.Values{"note": fixturechange.Lit("NULL"), "currency_id": fixturechange.RefTo("Currency", "EUR")},
		New: fixturechange.Values{"note": fixturechange.Null(), "currency_id": fixturechange.RefTo("Currency", "USD")},
	}}}})
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Agree   bool
		Changes []struct {
			Database map[string]any
			File     map[string]any
		}
	}
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	c := back.Changes[0]
	if back.Agree || c.Database["note"] != "NULL" || c.File["note"] != nil {
		t.Fatalf("%s", data)
	}
	ref, ok := c.File["currency_id"].(map[string]any)
	if !ok || ref["model"] != "Currency" || ref["key"] != "USD" {
		t.Fatalf("a reference is an object: %s", data)
	}
	if jsonValues(nil) != nil {
		t.Fatal("no values is no object")
	}
}

// A finding the policy makes a warning is reported and is not disagreement,
// in the JSON as in the exit code; one it makes an error is.
func TestCheckJSONAgreesDespiteAWarning(t *testing.T) {
	for policy, level := range map[string]Mode{
		"policy: {zero_default: warn}\n":  ModeWarn,
		"policy: {zero_default: error}\n": ModeError,
	} {
		report := newCheckReport(policyConfig(t, policy), &CheckResult{Result: &Result{}, Findings: []Finding{
			{Kind: FindingZeroDefault, Model: "Plan", Row: "name=x", Detail: "a zero"}}})
		data, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		var back struct {
			Agree    bool
			Findings []ReportedFinding
		}
		if err := json.Unmarshal(data, &back); err != nil || len(back.Findings) != 1 ||
			back.Findings[0].Level != level || back.Agree != (level == ModeWarn) || report.Agree != back.Agree {
			t.Errorf("%s: %v %s", policy, err, data)
		}
	}
}

// policyConfig is a prepared configuration with one policy line.
func policyConfig(t *testing.T, policy string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.yml")
	if err := os.WriteFile(path, []byte("fixture: f.yml\n"+policy+"models:\n  Plan: {table: plans}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The refusal of a sync is a RefusedError that is also ErrSyncRefused, so a
// program written against either finds it.
func TestARefusedErrorUnwraps(t *testing.T) {
	err := error(&RefusedError{Message: "sync refused: x", err: ErrSyncRefused, reason: ErrFindings})
	if !errors.Is(err, ErrRefused) || !errors.Is(err, ErrSyncRefused) || !errors.Is(err, ErrFindings) ||
		errors.Is(err, ErrLineage) || err.Error() != "sync refused: x" {
		t.Fatal(err)
	}
}

// An export is written anew from the database, so the comments of the file
// it replaces are counted, to say they are gone.
func TestDroppedComments(t *testing.T) {
	old := "# master data\n- model: Plan\n  rows:\n    # the cheap one\n    - name: free # forever\n      note: \"a # b\"\n"
	exported := "# Exported by bun-fixture-migrate from a live database.\n- model: Plan\n  rows:\n    - name: free\n" +
		"      note: \"a # b\"\n"
	if n := droppedComments([]byte(old), []byte(exported)); n != 3 {
		t.Fatalf("got %d, want the three comments", n)
	}
	if n := droppedComments([]byte(exported), []byte(exported)); n != 0 {
		t.Fatalf("the export's own header is kept: %d", n)
	}
}

// Without a database, every row that writes a value only the column's type can
// settle is refused; status says so once per model and column rather than
// once per row, and leaves every other refusal as it is.
func TestStatusGroupsValuesOnlyTheDatabaseCanSettle(t *testing.T) {
	p := loadProject(t, apiProject(t, apiConfig, apiOld))
	snap := func(text string) *Snapshot {
		s, err := p.snapshotOf([]FixtureFile{{Data: []byte(text)}}, "f")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	head := strings.Replace(apiOld, "name: team", "name: crew", 1)
	for _, name := range []string{"a", "b", "c"} {
		head += "    - name: " + name + "\n      currency_id: '{{ $.Currency.eur.ID }}'\n      price_cents: 29.00\n"
	}
	res, err := Compute(p.Config, snap(apiOld), snap(head))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Refusals) != 4 {
		t.Fatalf("expected a rename and three undecided values, got %v", res.Refusals)
	}
	got := groupUndecided(res.Refusals)
	if len(got) != 2 || !strings.Contains(got[0], "renamed") ||
		!strings.HasPrefix(got[1], "Plan.price_cents is written like 29.00 in 3 rows, Plan/name=a the first of them, which ") ||
		!strings.Contains(got[1], "with the database configured") {
		t.Fatalf("got %q", got)
	}
	// One row reads as it did.
	if one := groupUndecided(res.Refusals[:1]); len(one) != 1 {
		t.Fatalf("got %q", one)
	}
	var single []Refusal
	for _, r := range res.Refusals {
		if strings.Contains(r.Key, "name=b") {
			single = append(single, r)
		}
	}
	if one := groupUndecided(single); len(one) != 1 || one[0] != single[0].String() {
		t.Fatalf("got %q, want %q", one, single[0].String())
	}
}

// A pending migration named before one the database applied is marked, and
// what the table records that the directory does not have is listed.
func TestStatusMarksOrder(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	r := &StatusReport{Directory: "migrations", Database: &StatusDatabase{Table: "bun_migrations"},
		Migrations: []StatusMigration{{ID: "1_a", Name: "1"}, {ID: "2_b", Name: "2"}, {ID: "3_c", Name: "3"},
			{ID: "4_d", Name: "4"}}}
	markApplied(r, map[string]Applied{"1": {Name: "1", GroupID: 1, MigratedAt: at},
		"3": {Name: "3", GroupID: 2, MigratedAt: at}, "9": {Name: "9", GroupID: 2, MigratedAt: at}})
	if r.Database.NewestApplied != "3" || !r.Migrations[1].OutOfOrder || r.Migrations[3].OutOfOrder ||
		r.Migrations[0].OutOfOrder || strings.Join(r.Database.NotInDirectory, ",") != "9" ||
		r.Migrations[2].Applied == nil || r.Migrations[2].Applied.Group != 2 || len(r.Notes) != 1 ||
		!strings.Contains(r.Notes[0], "2_b is pending and sorts before 3") {
		t.Fatalf("%+v %+v %q", r.Database, r.Migrations, r.Notes)
	}
}

// The base revision comes out of git, and out of the repository the fixture
// file belongs to rather than the working directory.
func TestGitShowReadsTheFileAsOfARevision(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := apiProject(t, apiConfig, apiOld)
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"}, {"add", "."}, {"commit", "-q", "-m", "the base state"}} {
		if out, err := git(dir, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	fixture := filepath.Join(dir, "fixtures", "fixture.yml")
	apiWrite(t, dir, "fixtures/fixture.yml", apiNew)
	data, err := gitShow(fixture, "HEAD")
	if err != nil || string(data) != apiOld {
		t.Fatalf("%v\n%s", err, data)
	}
	if _, err := gitShow(fixture, "no-such-revision"); err == nil {
		t.Fatal("a revision that does not exist has to be an error")
	}
	// The project diffs against HEAD while there is no state file, and says
	// so.
	p := loadProject(t, dir)
	g, err := p.Generate(context.Background(), nil, GenerateOptions{Name: "x", DryRun: true})
	if err != nil || len(g.Notes) != 1 || !strings.Contains(g.Notes[0], "diffs against git HEAD") ||
		g.Diff.Base != "HEAD:fixtures/fixture.yml" || len(g.Diff.Changes) != 1 {
		t.Fatalf("%v %+v", err, g)
	}
}

func TestGitShowOutsideARepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	// A temporary directory can sit inside a repository; one that does not
	// exist cannot.
	_, err := gitShow(filepath.Join(t.TempDir(), "nowhere", "fixture.yml"), "HEAD")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "pass -old") {
		t.Fatalf("the message has to name the way out: %v", err)
	}
}
