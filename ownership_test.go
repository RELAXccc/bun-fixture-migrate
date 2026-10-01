package fixturemigrate

import (
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// ownedConfig is testConfig with a change to its models, prepared again:
// Prepare is repeatable, and a deletes it filled in from the policy is not
// one a model sets.
func ownedConfig(t *testing.T, change func(cfg *Config)) *Config {
	t.Helper()
	cfg := testConfig(t)
	change(cfg)
	if err := cfg.Prepare(); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return cfg
}

// The pair most of these tests diff: a feature leaves the file, the quota and
// the flag of another change, and a third is new.
func featurePair(t *testing.T) (string, string) {
	t.Helper()
	old := base + `    - plan_id: '{{ $.Plan.free.ID }}'
      code: webhooks
`
	next := replace(t, base, "      quota: 5000\n      enabled: true\n", "      quota: 6000\n      enabled: false\n")
	next += `    - plan_id: '{{ $.Plan.free.ID }}'
      code: sso
      enabled: true
`
	return old, next
}

func kinds(res *Result, model string) map[fixturechange.Kind]int {
	out := map[fixturechange.Kind]int{}
	for _, c := range res.Changes {
		if c.Model == model {
			out[c.Kind]++
		}
	}
	return out
}

func TestModeSyncDeletesWhatLeftTheFile(t *testing.T) {
	old, next := featurePair(t)
	res := compute(t, old, next)
	if k := kinds(res, "Feature"); k[fixturechange.Insert] != 1 || k[fixturechange.Update] != 1 || k[fixturechange.Delete] != 1 {
		t.Fatalf("sync inserts, updates and deletes: %+v", res.Changes)
	}
	if len(res.LeftAlone) != 0 || len(res.LeftAloneLines()) != 0 {
		t.Fatalf("sync leaves nothing alone: %+v", res.LeftAlone)
	}
}

func TestModeUpsertNeverDeletes(t *testing.T) {
	cfg := ownedConfig(t, func(cfg *Config) { cfg.Models["Feature"].Mode = OwnUpsert })
	old, next := featurePair(t)
	res := computeWith(t, cfg, old, next)
	if k := kinds(res, "Feature"); k[fixturechange.Insert] != 1 || k[fixturechange.Update] != 1 || k[fixturechange.Delete] != 0 {
		t.Fatalf("upsert inserts and updates and never deletes: %+v", res.Changes)
	}
	if len(res.Refusals) != 0 {
		t.Fatalf("nothing to refuse: %+v", res.Refusals)
	}
	if len(res.LeftAlone) != 1 || res.LeftAlone[0].Model != "Feature" || res.LeftAlone[0].Mode != OwnUpsert ||
		res.LeftAlone[0].Rows != 1 || res.LeftAlone[0].Changed != 0 || len(res.LeftAlone[0].Columns) != 0 {
		t.Fatalf("the row left alone is counted: %+v", res.LeftAlone)
	}
	lines := res.LeftAloneLines()
	if len(lines) != 1 || lines[0] != "Feature: 1 row only in base, which mode upsert never deletes" {
		t.Fatalf("%q", lines)
	}
}

// The policy's mode is every model's that does not set one, and deletes is
// only refused where a model sets it next to a mode that never deletes.
func TestModeComesFromThePolicyAndRefusesDeletes(t *testing.T) {
	cfg := ownedConfig(t, func(cfg *Config) {
		cfg.Policy.Mode = OwnUpsert
		cfg.Policy.Deletes = DeleteCascade
		for _, m := range cfg.Models {
			m.Mode = ""
		}
		cfg.Models["Plan"].Mode = OwnSync
	})
	if cfg.Models["Feature"].Mode != OwnUpsert || cfg.Models["Currency"].Mode != OwnUpsert || cfg.Models["Plan"].Mode != OwnSync {
		t.Fatalf("modes: %s %s %s", cfg.Models["Feature"].Mode, cfg.Models["Currency"].Mode, cfg.Models["Plan"].Mode)
	}
	if err := cfg.Prepare(); err != nil {
		t.Fatalf("a second Prepare has to agree with the first: %v", err)
	}
	old, next := featurePair(t)
	res := computeWith(t, cfg, old, next)
	if k := kinds(res, "Feature"); k[fixturechange.Delete] != 0 {
		t.Fatalf("%+v", res.Changes)
	}

	cfg = testConfig(t)
	cfg.Models["Feature"].Mode = OwnInsert
	cfg.Models["Feature"].Deletes = DeleteCascade
	cfg.Models["Feature"].deletesInherited = false
	if err := cfg.Prepare(); err == nil || !strings.Contains(err.Error(), "deletes only applies under mode sync") {
		t.Fatalf("deletes next to mode insert has to be refused, got %v", err)
	}
	cfg = testConfig(t)
	cfg.Policy.Mode = "merge"
	if err := cfg.Prepare(); err == nil || !strings.Contains(err.Error(), "policy mode") {
		t.Fatalf("got %v", err)
	}
	cfg = testConfig(t)
	cfg.Models["Feature"].Mode = "merge"
	if err := cfg.Prepare(); err == nil || !strings.Contains(err.Error(), `model "Feature": mode`) {
		t.Fatalf("got %v", err)
	}
}

func TestModeInsertOnlySeeds(t *testing.T) {
	cfg := ownedConfig(t, func(cfg *Config) { cfg.Models["Feature"].Mode = OwnInsert })
	old, next := featurePair(t)
	res := computeWith(t, cfg, old, next)
	c := only(t, res, "Feature", fixturechange.Insert)
	if c.Key["code"].Lit != "sso" || c.New["enabled"].Lit != "true" {
		t.Fatalf("the new row is inserted whole: %+v", c)
	}
	if len(res.Changes) != 1 || len(res.Refusals) != 0 {
		t.Fatalf("insert writes nothing for a row the database holds: %+v %+v", res.Changes, res.Refusals)
	}
	if len(res.LeftAlone) != 1 || res.LeftAlone[0].Rows != 1 || res.LeftAlone[0].Changed != 1 {
		t.Fatalf("%+v", res.LeftAlone)
	}
	lines := res.LeftAloneLines()
	if len(lines) != 2 || !strings.Contains(lines[1], "1 row with other values in base, which mode insert never updates") {
		t.Fatalf("%q", lines)
	}
}

// A rename is an update of the key, which mode insert never writes; under
// upsert it is what it is under sync.
func TestARenameUnderEachMode(t *testing.T) {
	next := replace(t, base, "      name: team\n", "      name: crew\n")
	for _, mode := range []Ownership{OwnSync, OwnUpsert, OwnInsert} {
		cfg := ownedConfig(t, func(cfg *Config) {
			cfg.Policy.Renames = RenameUpdate
			cfg.Models["Plan"].Mode = mode
			cfg.Models["Plan"].Deletes = ""
		})
		res := computeWith(t, cfg, base, next)
		if mode == OwnInsert {
			// The rows pointing at the plan wait for it, as they do for a
			// rename refused under sync.
			last := res.Refusals[len(res.Refusals)-1]
			if len(res.Changes) != 0 || len(res.Refusals) != 3 || last.Model != "Plan" ||
				!strings.Contains(last.Reason, "mode insert never changes a row a database holds") {
				t.Fatalf("insert: %+v %+v", res.Changes, res.Refusals)
			}
			continue
		}
		c := only(t, res, "Plan", fixturechange.Update)
		if c.ID != "2" || c.New["name"].Lit != "crew" || len(res.Refusals) != 0 {
			t.Fatalf("%s: %+v %+v", mode, c, res.Refusals)
		}
	}
}

// A key respelled into another spelling of one value to its type, Go and GO
// in a citext column, finds the same row, whose spelling is the database's
// under mode insert.
func TestModeInsertLeavesARespelledKeyAlone(t *testing.T) {
	cfg := ownedConfig(t, func(cfg *Config) { cfg.Models["Currency"].Mode = OwnInsert })
	old := snap(t, cfg, base, "base")
	next := snap(t, cfg, replace(t, base, "code: EUR", "code: eur"), "head")
	for _, s := range []*Snapshot{old, next} {
		for _, e := range s.Entries["Currency"] {
			e.ID = ""
			e.folded = map[string]string{"code": "eur"}
		}
	}
	res, err := Compute(cfg, old, next)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Changes {
		if c.Model == "Currency" {
			t.Fatalf("%+v", res.Changes)
		}
	}
	for _, r := range res.Refusals {
		if r.Model == "Currency" {
			t.Fatalf("%+v", r)
		}
	}
	if len(res.LeftAlone) != 1 || res.LeftAlone[0].Changed != 1 {
		t.Fatalf("%+v", res.LeftAlone)
	}
}

// A row of a mode insert model keeps the ref value it has wherever it is, so
// a change naming it by the one the file gives it would find nothing there.
func TestModeInsertRefusesAReferenceByARefValueOnlyTheFileHas(t *testing.T) {
	cfg := &Config{Fixture: "f.yml", Policy: Policy{Renames: RenameUpdate}, Models: map[string]*Model{
		"Country": {Table: "countries", Key: []string{"code"}, Ref: "name", Mode: OwnInsert},
		"City":    {Table: "cities", Key: []string{"name"}, References: map[string]string{"country_id": "Country"}},
	}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	const old = `- model: Country
  rows:
    - {_id: de, id: 1, code: DE, name: Germany}
- model: City
  rows:
    - {name: Berlin, country_id: '{{ $.Country.de.ID }}'}
`
	next := replace(t, old, "name: Germany", "name: Deutschland") +
		"    - {name: Hamburg, country_id: '{{ $.Country.de.ID }}'}\n"
	res := computeWith(t, cfg, old, next)
	if len(res.Changes) != 0 {
		t.Fatalf("nothing can be written: %+v", res.Changes)
	}
	if len(res.Refusals) != 1 || !strings.Contains(res.Refusals[0].Reason, `points at Country "Deutschland", whose name was "Germany"`) {
		t.Fatalf("%+v", res.Refusals)
	}
	// Berlin points at the same row as before, and is no change.
	if res.Refusals[0].Key != "City/name=Hamburg" {
		t.Fatalf("%+v", res.Refusals)
	}
}

func TestInsertOnlyColumnsAreWrittenOnInsertAndNeverAfter(t *testing.T) {
	cfg := ownedConfig(t, func(cfg *Config) { cfg.Models["Feature"].InsertOnly = []string{"enabled"} })
	old, next := featurePair(t)
	res := computeWith(t, cfg, old, next)
	if c := only(t, res, "Feature", fixturechange.Insert); c.New["enabled"].Lit != "true" {
		t.Fatalf("an insert writes an insert_only column: %+v", c)
	}
	upd := only(t, res, "Feature", fixturechange.Update)
	if _, ok := upd.New["enabled"]; ok || upd.New["quota"].Lit != "6000" {
		t.Fatalf("an update never writes an insert_only column: %+v", upd)
	}
	del := only(t, res, "Feature", fixturechange.Delete)
	if _, ok := del.Old["enabled"]; ok || del.Old["quota"].Lit != "0" {
		t.Fatalf("a delete is not guarded by an insert_only column: %+v", del)
	}
	if len(res.LeftAlone) != 1 || res.LeftAlone[0].Columns["enabled"] != 1 {
		t.Fatalf("%+v", res.LeftAlone)
	}
	if lines := res.LeftAloneLines(); len(lines) != 1 ||
		lines[0] != "Feature: 1 row with another enabled in base, which insert_only leaves to the database" {
		t.Fatalf("%q", lines)
	}

	// Only enabled changed: nothing to write at all.
	only := replace(t, base, "      enabled: true\n", "      enabled: false\n")
	if res := computeWith(t, cfg, base, only); len(res.Changes) != 0 || len(res.Refusals) != 0 {
		t.Fatalf("%+v %+v", res.Changes, res.Refusals)
	}
}

// insert_only takes the columns a row's identity is not made of, and none
// another key already says nothing is written into.
func TestInsertOnlyRefusesWhatCannotBeOne(t *testing.T) {
	for _, tc := range []struct {
		col, why string
	}{
		{"code", "natural key"},
		{"plan_id", "natural key"},
		{"id", "is the id"},
		{"", "is empty"},
	} {
		cfg := testConfig(t)
		cfg.Models["Feature"].InsertOnly = []string{tc.col}
		if err := cfg.Prepare(); err == nil || !strings.Contains(err.Error(), tc.why) {
			t.Fatalf("%q: got %v", tc.col, err)
		}
	}
	cfg := testConfig(t)
	cfg.Models["Plan"].InsertOnly = []string{"price_per_seat_cents"}
	if err := cfg.Prepare(); err == nil || !strings.Contains(err.Error(), "derived") {
		t.Fatalf("got %v", err)
	}
	cfg = testConfig(t)
	cfg.Models["Currency"].Ref = "symbol"
	cfg.Models["Currency"].InsertOnly = []string{"symbol"}
	if err := cfg.Prepare(); err == nil || !strings.Contains(err.Error(), "ref column") {
		t.Fatalf("got %v", err)
	}
	cfg = testConfig(t)
	cfg.Models["Feature"].InsertOnly = []string{"quota", "quota"}
	if err := cfg.Prepare(); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("got %v", err)
	}
}

func TestIDsFromTheDatabaseAreNeverWrittenNorCompared(t *testing.T) {
	cfg := ownedConfig(t, func(cfg *Config) {
		cfg.Policy.Renames = RenameUpdate
		cfg.Models["Plan"].IDs = IDsDatabase
	})
	// A new plan, and a feature naming it by its id in the file.
	next := strings.Replace(base, "- model: Feature\n", `    - _id: pro
      id: 3
      name: pro
      currency_id: '{{ $.Currency.eur.ID }}'
      price_cents: 9000
      seats: 20
- model: Feature
`, 1) + `    - plan_id: 3
      code: api
`
	res := computeWith(t, cfg, base, next)
	ins := only(t, res, "Plan", fixturechange.Insert)
	if _, ok := ins.New["id"]; ok {
		t.Fatalf("an insert leaves the id to the database: %+v", ins.New)
	}
	var feature fixturechange.Change
	for _, c := range res.Changes {
		if c.Model == "Feature" {
			feature = c
		}
	}
	if ref := feature.New["plan_id"].Ref; ref == nil || ref.Model != "Plan" || ref.Key != "pro" {
		t.Fatalf("the file's id resolves a reference in the file: %+v", feature)
	}

	// Another id for the same plan is no id drift.
	renumbered := replace(t, base, "      id: 2\n      name: team\n", "      id: 7\n      name: team\n")
	if res := computeWith(t, cfg, base, renumbered); len(res.Changes) != 0 || len(res.Refusals) != 0 || len(res.Warnings) != 0 {
		t.Fatalf("%+v %+v %+v", res.Changes, res.Refusals, res.Warnings)
	}

	// The file's id still says which row a new key belongs to, and the
	// rename finds the row by its old key alone.
	renamed := replace(t, base, "      name: team\n", "      name: crew\n")
	res = computeWith(t, cfg, base, renamed)
	c := only(t, res, "Plan", fixturechange.Update)
	if c.ID != "" || c.Key["name"].Lit != "team" || c.New["name"].Lit != "crew" {
		t.Fatalf("%+v", c)
	}

	// A delete is not guarded by an id.
	gone := computeWith(t, ownedConfig(t, func(cfg *Config) {
		cfg.Models["Feature"].IDs = IDsDatabase
	}), base+"    - {id: 9, plan_id: '{{ $.Plan.free.ID }}', code: x}\n", base)
	if del := only(t, gone, "Feature", fixturechange.Delete); len(del.Old) == 0 || del.Old["id"] != (fixturechange.Value{}) {
		t.Fatalf("%+v", del)
	}
}

func TestIDsFromTheDatabaseCannotNameARow(t *testing.T) {
	cfg := testConfig(t)
	cfg.Models["Currency"].IDs = IDsDatabase
	cfg.Models["Currency"].Ref = "id"
	if err := cfg.Prepare(); err == nil || !strings.Contains(err.Error(), "set ref to a column of its own") {
		t.Fatalf("got %v", err)
	}
	cfg = testConfig(t)
	cfg.Models["Currency"].IDs = IDsDatabase
	cfg.Models["Currency"].Key = []string{"id"}
	if err := cfg.Prepare(); err == nil || !strings.Contains(err.Error(), "natural key") {
		t.Fatalf("got %v", err)
	}
	cfg = testConfig(t)
	cfg.Models["Currency"].IDs = "sequence"
	if err := cfg.Prepare(); err == nil || !strings.Contains(err.Error(), "ids is") {
		t.Fatalf("got %v", err)
	}
}
