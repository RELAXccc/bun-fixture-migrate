package fixturemigrate

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// A model's changed_row, missing_row, id_drift and duplicate_key override the
// policy block, and an audit_table is a table name; both are read from the
// file and checked.
func TestTheConfigurationReadsModelPoliciesAndTheAuditTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture-migrate.yml")
	if err := os.WriteFile(path, []byte(`fixture: fixtures/fixture.yml
audit_table: ops.bun_fixture_audit
policy:
  changed_row: error
models:
  Price:
    table: prices
  Translation:
    table: translations
    key: [key]
    changed_row: warn
    missing_row: warn
    id_drift: ignore
    duplicate_key: warn
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuditTable != "ops.bun_fixture_audit" {
		t.Fatalf("audit_table: %q", cfg.AuditTable)
	}
	if got := cfg.ModelPolicy("Price"); got != cfg.Policy {
		t.Fatalf("a model without its own runs under the policy block: %+v", got)
	}
	want := cfg.Policy
	want.ChangedRow, want.MissingRow, want.IDDrift, want.DuplicateKey = ModeWarn, ModeWarn, ModeIgnore, ModeWarn
	if got := cfg.ModelPolicy("Translation"); got != want {
		t.Fatalf("ModelPolicy(Translation) = %+v, want %+v", got, want)
	}
	if p := cfg.TablePolicy("Price"); p != nil {
		t.Fatalf("a model without its own carries none into the change set: %+v", p)
	}
	if p := cfg.TablePolicy("Translation"); p == nil || *p != (fixturechange.Policy{MissingRow: "warn",
		ChangedRow: "warn", IDDrift: "ignore", DuplicateKey: "warn"}) {
		t.Fatalf("TablePolicy(Translation) = %+v", p)
	}
	// Only what the model sets: the rest is the set's, so that changing the
	// policy block in a migration file still reaches this model.
	cfg.Models["Translation"].MissingRow, cfg.Models["Translation"].IDDrift = "", ""
	cfg.Models["Translation"].DuplicateKey = ""
	if p := cfg.TablePolicy("Translation"); *p != (fixturechange.Policy{ChangedRow: "warn"}) {
		t.Fatalf("TablePolicy(Translation) = %+v", p)
	}
}

func TestTheConfigurationRefusesAModelPolicyItDoesNotKnow(t *testing.T) {
	for name, m := range map[string]*Model{
		"changed_row":   {Table: "t", ChangedRow: "ignore"},
		"missing_row":   {Table: "t", MissingRow: "ignore"},
		"id_drift":      {Table: "t", IDDrift: "maybe"},
		"duplicate_key": {Table: "t", DuplicateKey: "ignore"},
	} {
		cfg := &Config{Models: map[string]*Model{"Plan": m}}
		err := cfg.Prepare()
		if err == nil || !strings.Contains(err.Error(), `model "Plan": `+name) {
			t.Errorf("%s: %v", name, err)
		}
	}
	cfg := &Config{AuditTable: "audit; DROP TABLE x", Models: map[string]*Model{"Plan": {Table: "t"}}}
	if err := cfg.Prepare(); err == nil || !strings.Contains(err.Error(), "audit_table") {
		t.Fatalf("audit_table: %v", err)
	}
}

// The generate-time reading of id_drift is the model's: one model's ids may
// drift while another's are refused.
func TestIDDriftIsTheModelsOwn(t *testing.T) {
	next := replace(t, base, "      id: 2\n      name: team\n", "      id: 7\n      name: team\n")
	cfg := testConfig(t)
	cfg.Models["Plan"].IDDrift = ModeWarn
	res := computeWith(t, cfg, base, next)
	if len(res.Refusals) != 0 || len(res.Warnings) != 1 {
		t.Fatalf("Plan's id_drift warn: %+v / %+v", res.Warnings, res.Refusals)
	}
	// The warning names the id_drift that decided: the model's.
	if r := res.Warnings[0].Reason; !strings.Contains(r, "the model's id_drift is warn") || strings.Contains(r, "policy.") {
		t.Fatalf("got %s", r)
	}
	cfg.Models["Plan"].IDDrift = ModeIgnore
	if res := computeWith(t, cfg, base, next); len(res.Refusals)+len(res.Warnings) != 0 {
		t.Fatalf("Plan's id_drift ignore: %+v / %+v", res.Warnings, res.Refusals)
	}
	// And the other way round: the policy block says warn, the model error,
	// and setting the policy block's to warn would change nothing.
	cfg = testConfig(t)
	cfg.Policy.IDDrift = ModeWarn
	cfg.Models["Plan"].IDDrift = ModeError
	res = computeWith(t, cfg, base, next)
	if len(res.Refusals) != 1 || !strings.Contains(res.Refusals[0].Reason, "or set the model's id_drift to warn") ||
		strings.Contains(res.Refusals[0].Reason, "policy.") {
		t.Fatalf("Plan's id_drift error: %+v / %+v", res.Warnings, res.Refusals)
	}
	// Where the model sets nothing, the policy block's decides, and is the
	// one named; one the model sets of another key changes nothing.
	cfg = testConfig(t)
	cfg.Policy.IDDrift = ModeWarn
	cfg.Models["Plan"].ChangedRow = ModeError
	if res := computeWith(t, cfg, base, next); len(res.Warnings) != 1 ||
		!strings.Contains(res.Warnings[0].Reason, "; policy.id_drift is warn") {
		t.Fatalf("got %+v", res.Warnings)
	}
	cfg.Policy.IDDrift = ModeError
	if res := computeWith(t, cfg, base, next); len(res.Refusals) != 1 ||
		!strings.Contains(res.Refusals[0].Reason, "or set policy.id_drift to warn") {
		t.Fatalf("got %+v", res.Refusals)
	}
}

// A refused delete names the deletes that refused it: the model's own, or
// the policy block's it inherits.
func TestARefusedDeleteNamesItsDeletes(t *testing.T) {
	old := "- model: Currency\n  rows:\n    - {id: 1, code: EUR}\n    - {id: 2, code: USD}\n"
	next := "- model: Currency\n  rows:\n    - {id: 1, code: EUR}\n"
	for _, tc := range []struct {
		policy, model DeletePolicy
		want          string
	}{
		{DeleteRefuse, "", "refused by the configuration, policy.deletes: refuse, because"},
		{DeleteAllow, DeleteRefuse, "refused by the configuration, the model's deletes: refuse, because"},
	} {
		cfg := &Config{Policy: Policy{Deletes: tc.policy}, Models: map[string]*Model{
			"Currency": {Table: "currencies", Ref: "code", Key: []string{"code"}, Deletes: tc.model}}}
		if err := cfg.Prepare(); err != nil {
			t.Fatal(err)
		}
		res := computeWith(t, cfg, old, next)
		if len(res.Refusals) != 1 || !strings.Contains(res.Refusals[0].Reason, tc.want) {
			t.Errorf("%s/%s: expected %q in %+v", tc.policy, tc.model, tc.want, res.Refusals)
		}
	}
}

// A duplicate key in one model can be a warning while it is an error in the
// rest.
func TestADuplicateKeyFindingIsTheModels(t *testing.T) {
	cfg := testConfig(t)
	cfg.Models["Feature"].DuplicateKey = ModeWarn
	findings := []Finding{{Kind: FindingDuplicateKey, Model: "Feature", Row: "code=api", Detail: "two rows"}}
	if mode, kept := cfg.Worst(findings); mode != ModeWarn || len(kept) != 1 {
		t.Fatalf("Feature's duplicate_key warn: %q %d", mode, len(kept))
	}
	if got := cfg.ModeOf(findings[0]); got != ModeWarn {
		t.Fatalf("ModeOf: %q", got)
	}
	findings = append(findings, Finding{Kind: FindingDuplicateKey, Model: "Plan", Row: "name=team"})
	if mode, _ := cfg.Worst(findings); mode != ModeError {
		t.Fatalf("Plan's is the policy block's error: %q", mode)
	}
	if got := cfg.ModeOf(Finding{Kind: FindingZeroDefault, Model: "Feature"}); got != cfg.Policy.ZeroDefault {
		t.Fatalf("other kinds are the policy block's: %q", got)
	}
}

// A migration carries the audit table and each model's own policy, written
// only when set, and reads back as the set it runs.
func TestAGeneratedFileCarriesTheAuditTableAndTheModelsPolicy(t *testing.T) {
	cfg := testConfig(t)
	next := replace(t, base, "      price_cents: 2000\n", "      price_cents: 2500\n")
	next = replace(t, next, "      quota: 100\n", "      quota: 150\n")

	plain, err := Render(cfg, "prices", "20260921120000", computeWith(t, cfg, base, next))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "AuditTable") || strings.Contains(string(plain), "&fixturechange.Policy") {
		t.Fatalf("written only when set:\n%s", plain)
	}

	cfg.AuditTable = "bun_fixture_audit"
	cfg.Schema = "app"
	cfg.Models["Plan"].ChangedRow = ModeError
	cfg.Models["Feature"].ChangedRow = ModeWarn
	cfg.Models["Feature"].MissingRow = ModeWarn
	res := computeWith(t, cfg, base, next)
	src, err := Render(cfg, "prices", "20260921120000", res)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(src)), " ")
	for _, want := range []string{
		`AuditTable: "app.bun_fixture_audit",`,
		`"Feature": {Name: "app.features", ID: "id", Serial: true, Policy: &fixturechange.Policy{MissingRow: "warn", ChangedRow: "warn"}},`,
		`Policy: &fixturechange.Policy{ChangedRow: "error"}},`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %s:\n%s", want, src)
		}
	}
	got, isFixture, err := ReadChangeSet(src)
	if err != nil || !isFixture {
		t.Fatalf("ReadChangeSet: %v %v", isFixture, err)
	}
	if got.AuditTable != "app.bun_fixture_audit" || !reflect.DeepEqual(got.Tables, res.Tables) {
		t.Fatalf("read back as %+v", got)
	}
	if err := fixtureapply.Validate(got); err != nil {
		t.Fatal(err)
	}
	if p := got.PolicyFor("Feature"); p.ChangedRow != "warn" || p.MissingRow != "warn" || p.IDDrift != "error" {
		t.Fatalf("Feature runs under %+v", p)
	}
	// The hash status compares is the same for the file read back as for
	// the set it compiles to.
	set := fixturechange.Set{Name: got.Name, SeedGuardTable: got.SeedGuardTable,
		MigrationsTable: cfg.MigrationsTable, AuditTable: "app.bun_fixture_audit", Tables: res.Tables,
		Changes: res.Changes, Policy: got.Policy}
	if fixtureapply.SetSHA256(set) != fixtureapply.SetSHA256(got) {
		t.Fatal("the file read back hashes differently from the set it runs")
	}
}

func TestAHandWrittenTablePolicyReadsBack(t *testing.T) {
	src := []byte(`package migrations

import fc "github.com/RELAXccc/bun-fixture-migrate/fixturechange"

var set = fc.Set{
	Name:       "x",
	AuditTable: "audit",
	Tables: fc.Tables{
		"Plan": {Name: "plans", ID: "id", Policy: &fc.Policy{ChangedRow: fc.ModeError}},
		"Tag":  {Name: "tags", ID: "id", Policy: nil},
	},
	Changes: []fc.Change{
		{Model: "Plan", Kind: fc.Update,
			Key: fc.Values{"name": fc.Lit("team")},
			Old: fc.Values{"note": fc.Null()},
			New: fc.Values{"note": fc.Lit("x")}},
	},
}
`)
	set, _, err := ReadChangeSet(src)
	if err != nil {
		t.Fatal(err)
	}
	if set.AuditTable != "audit" || set.Tables["Plan"].Policy == nil ||
		set.Tables["Plan"].Policy.ChangedRow != fixturechange.ModeError || set.Tables["Tag"].Policy != nil {
		t.Fatalf("%+v", set)
	}
	bad := strings.Replace(string(src), "Policy: &fc.Policy{", "Policy: fc.Policy{", 1)
	if _, _, err := ReadChangeSet([]byte(bad)); err == nil || !strings.Contains(err.Error(), "&fixturechange.Policy") {
		t.Fatalf("a table policy that is not a pointer: %v", err)
	}
}

// A refusal of findings names the policies that make them errors, each where
// its value comes from: a model's own duplicate_key, the policy block's
// zero_default. An invalid value no policy makes an error is only fixed.
func TestARefusalOfFindingsNamesThePolicies(t *testing.T) {
	cfg := testConfig(t)
	cfg.Policy.DuplicateKey = ModeWarn
	cfg.Models["Plan"].DuplicateKey = ModeError
	findings := []Finding{
		{Kind: FindingDuplicateKey, Model: "Plan", Row: "Plan/name=team"},
		{Kind: FindingDuplicateKey, Model: "Feature", Row: "Feature/code=api"},
		{Kind: FindingZeroDefault, Model: "Feature", Row: "Feature/code=api"},
		{Kind: FindingInvalidValue, Model: "Plan", Row: "Plan/name=team"},
	}
	if got := cfg.warnTo(findings); got != ", or set Plan's duplicate_key and policy.zero_default to warn" {
		t.Fatalf("got %q", got)
	}
	if got := cfg.warnTo(findings[3:]); got != "" {
		t.Fatalf("an invalid value has no policy to set: %q", got)
	}
	_, err := refuseFindings(cfg, &Snapshot{Findings: findings})
	if err == nil || !strings.HasSuffix(err.Error(), "nothing written. Fix them, or set Plan's duplicate_key and "+
		"policy.zero_default to warn") {
		t.Fatalf("got %v", err)
	}
}
