package fixturemigrate

import (
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// The example schema used throughout the tests and the README: currencies,
// subscription plans, and the features a plan grants.
func testConfig(t *testing.T) *Config {
	t.Helper()
	cfg := &Config{
		Fixture:        "fixtures/fixture.yml",
		Out:            "migrations",
		SeedGuardTable: "plans",
		Models: map[string]*Model{
			"Currency": {
				Table: "currencies",
				Ref:   "code",
				Key:   []string{"code"},
			},
			"Plan": {
				Table:      "plans",
				Key:        []string{"name"},
				References: map[string]string{"currency_id": "Currency"},
				Derived:    []string{"price_per_seat_cents"},
				Deletes:    DeleteRefuse,
			},
			"Feature": {
				Table:      "features",
				Serial:     true,
				Key:        []string{"plan_id", "code"},
				References: map[string]string{"plan_id": "Plan"},
				Defaults:   map[string]string{"quota": "0", "enabled": "false"},
			},
		},
	}
	if err := cfg.Prepare(); err != nil {
		t.Fatalf("prepare config: %v", err)
	}
	return cfg
}

const base = `- model: Currency
  rows:
    - _id: eur
      id: 1
      code: EUR
      symbol: "E"
- model: Plan
  rows:
    - _id: free
      id: 1
      name: free
      currency_id: '{{ $.Currency.eur.ID }}'
      price_cents: 0
      seats: 1
    - _id: team
      id: 2
      name: team
      currency_id: '{{ $.Currency.eur.ID }}'
      price_cents: 2000
      seats: 10
- model: Feature
  rows:
    - plan_id: '{{ $.Plan.free.ID }}'
      code: api
      quota: 100
    - plan_id: '{{ $.Plan.team.ID }}'
      code: api
      quota: 5000
      enabled: true
`

func doc(t *testing.T, text string) Doc {
	t.Helper()
	d, err := ParseDoc([]byte(text))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return d
}

// snap resolves a fixture document with the test configuration.
func snap(t *testing.T, cfg *Config, text, source string) *Snapshot {
	t.Helper()
	s, err := FixtureSnapshot(cfg, doc(t, text), source)
	if err != nil {
		t.Fatalf("snapshot %s: %v", source, err)
	}
	return s
}

func computeWith(t *testing.T, cfg *Config, oldText, newText string) *Result {
	t.Helper()
	res, err := Compute(cfg, snap(t, cfg, oldText, "base"), snap(t, cfg, newText, "head"))
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	return res
}

func compute(t *testing.T, oldText, newText string) *Result {
	t.Helper()
	return computeWith(t, testConfig(t), oldText, newText)
}

func computeErr(t *testing.T, oldText, newText string) error {
	t.Helper()
	cfg := testConfig(t)
	old, err := FixtureSnapshot(cfg, doc(t, oldText), "base")
	if err == nil {
		var next *Snapshot
		if next, err = FixtureSnapshot(cfg, doc(t, newText), "head"); err == nil {
			_, err = Compute(cfg, old, next)
		}
	}
	if err == nil {
		t.Fatal("expected an error")
	}
	return err
}

func only(t *testing.T, res *Result, model string, kind fixturechange.Kind) fixturechange.Change {
	t.Helper()
	var found []fixturechange.Change
	for _, c := range res.Changes {
		if c.Model == model && c.Kind == kind {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one %s %s, got %d of them in %+v", model, kind, len(found), res.Changes)
	}
	return found[0]
}

func replace(t *testing.T, text, old, new string) string {
	t.Helper()
	if !strings.Contains(text, old) {
		t.Fatalf("the test document does not contain %q", old)
	}
	return strings.Replace(text, old, new, 1)
}

func TestNoChange(t *testing.T) {
	res := compute(t, base, base)
	if len(res.Changes) != 0 || len(res.Refusals) != 0 {
		t.Fatalf("a file against itself must not change anything: %+v / %+v", res.Changes, res.Refusals)
	}
}

// A column a row leaves out is the configured default, so writing that default
// out explicitly is no change.
func TestOmittedColumnIsItsDefault(t *testing.T) {
	res := compute(t, base, replace(t, base, "      code: api\n      quota: 100\n",
		"      code: api\n      quota: 100\n      enabled: false\n"))
	if len(res.Changes) != 0 {
		t.Fatalf("spelling out the default is not a change: %+v", res.Changes)
	}
}

// A column with no default that only one revision spells out cannot be
// compared, and the generator says so instead of picking a meaning.
func TestColumnOnOneSideOnlyIsRefused(t *testing.T) {
	res := compute(t, base, replace(t, base, "      seats: 1\n", "      seats: 1\n      trial_days: 14\n"))
	if len(res.Changes) != 0 {
		t.Fatalf("expected no change, got %+v", res.Changes)
	}
	if len(res.Refusals) != 1 || !strings.Contains(res.Refusals[0].Reason, "trial_days") {
		t.Fatalf("expected a refusal naming trial_days, got %+v", res.Refusals)
	}
}

func TestDerivedColumnsAreNeverCompared(t *testing.T) {
	old := replace(t, base, "      seats: 1\n", "      seats: 1\n      price_per_seat_cents: 0\n")
	next := replace(t, base, "      seats: 1\n", "      seats: 1\n      price_per_seat_cents: 999\n")
	res := compute(t, old, next)
	if len(res.Changes) != 0 || len(res.Refusals) != 0 {
		t.Fatalf("a derived column must not produce anything: %+v / %+v", res.Changes, res.Refusals)
	}
}

func TestUpdateCarriesOnlyTheChangedColumns(t *testing.T) {
	res := compute(t, base, replace(t, base, "      price_cents: 2000\n", "      price_cents: 2500\n"))
	c := only(t, res, "Plan", fixturechange.Update)
	if got := c.Key["name"].Lit; got != "team" {
		t.Fatalf("key should be the natural key, got %+v", c.Key)
	}
	if _, ok := c.Key["id"]; ok {
		t.Fatalf("the id must not be part of the key: %+v", c.Key)
	}
	if len(c.New) != 1 || c.New["price_cents"].Lit != "2500" || c.Old["price_cents"].Lit != "2000" {
		t.Fatalf("expected only price_cents 2000 -> 2500, got old %+v new %+v", c.Old, c.New)
	}
}

// "1.0" and "1" are the same number.
func TestNumbersCompareByValue(t *testing.T) {
	res := compute(t, base, replace(t, base, "      price_cents: 2000\n", "      price_cents: 2000.0\n"))
	if len(res.Changes) != 0 {
		t.Fatalf("expected no change, got %+v", res.Changes)
	}
}

func TestReferenceBecomesAReferenceByKey(t *testing.T) {
	old := replace(t, base, "      code: EUR\n", "      code: EUR\n    - _id: usd\n      id: 2\n      code: USD\n")
	next := replace(t, old, "      name: team\n      currency_id: '{{ $.Currency.eur.ID }}'\n",
		"      name: team\n      currency_id: '{{ $.Currency.usd.ID }}'\n")
	res := compute(t, old, next)
	c := only(t, res, "Plan", fixturechange.Update)
	ref := c.New["currency_id"].Ref
	if ref == nil || ref.Model != "Currency" || ref.Key != "USD" {
		t.Fatalf("expected a reference to Currency USD, got %+v", c.New["currency_id"])
	}
	if old := c.Old["currency_id"].Ref; old == nil || old.Key != "EUR" {
		t.Fatalf("expected the old value to be Currency EUR, got %+v", c.Old["currency_id"])
	}
	if _, ok := res.Tables["Currency"]; !ok {
		t.Fatalf("the table of a referenced model belongs in the set: %+v", res.Tables)
	}
}

// A reference column may also hold the target id as a plain number; the row it
// names is looked up in the same file.
func TestPlainIDInAReferenceColumnResolves(t *testing.T) {
	old := replace(t, base, "      currency_id: '{{ $.Currency.eur.ID }}'\n      price_cents: 0\n",
		"      currency_id: 1\n      price_cents: 0\n")
	res := compute(t, old, old)
	if len(res.Changes) != 0 {
		t.Fatalf("the two spellings name the same row: %+v", res.Changes)
	}
}

// A template that names a field other than the id is the value of that field,
// not a reference.
func TestTemplateOnAPlainColumnResolvesToTheValue(t *testing.T) {
	old := replace(t, base, "      seats: 1\n", "      seats: 1\n      note: '{{ $.Currency.eur.Code }}'\n")
	next := replace(t, base, "      seats: 1\n", "      seats: 1\n      note: EUR\n")
	res := compute(t, old, next)
	if len(res.Changes) != 0 || len(res.Refusals) != 0 {
		t.Fatalf("expected nothing, got %+v / %+v", res.Changes, res.Refusals)
	}
}

// A reference the file cannot resolve is an error. It used to degrade into a
// placeholder, which made two rows that both failed compare equal.
func TestUnresolvableReferenceIsAnError(t *testing.T) {
	next := replace(t, base, "      currency_id: '{{ $.Currency.eur.ID }}'\n      price_cents: 2000\n",
		"      currency_id: '{{ $.Currency.gbp.ID }}'\n      price_cents: 2000\n")
	err := computeErr(t, base, next)
	if !strings.Contains(err.Error(), "names no row of Currency") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInsertWritesTheIDButKeysOnTheNaturalKey(t *testing.T) {
	next := replace(t, base, "- model: Feature\n", `    - _id: pro
      id: 3
      name: pro
      currency_id: '{{ $.Currency.eur.ID }}'
      price_cents: 9000
      seats: 100
- model: Feature
`)
	res := compute(t, base, next)
	c := only(t, res, "Plan", fixturechange.Insert)
	// The insert must not be guarded by the id as well: a database that holds
	// the row under another id already has it, and a second insert either
	// trips a unique index or duplicates the row.
	if len(c.Key) != 1 || c.Key["name"].Lit != "pro" {
		t.Fatalf("the insert key must be the natural key alone, got %+v", c.Key)
	}
	if c.New["id"].Lit != "3" {
		t.Fatalf("the row itself still carries its id, got %+v", c.New)
	}
	if c.New["currency_id"].Ref == nil {
		t.Fatalf("references belong in the inserted row, got %+v", c.New["currency_id"])
	}
}

// An inserted row gets the columns it spells out plus whatever the defaults
// say about the ones it leaves out, so it matches what the fixture loader
// would have written.
func TestInsertFillsInTheDefaults(t *testing.T) {
	next := base + `    - plan_id: '{{ $.Plan.free.ID }}'
      code: webhooks
`
	res := compute(t, base, next)
	c := only(t, res, "Feature", fixturechange.Insert)
	if c.New["quota"].Lit != "0" || c.New["enabled"].Lit != "false" {
		t.Fatalf("expected the defaults to be written, got %+v", c.New)
	}
	if _, ok := c.New["id"]; ok {
		t.Fatalf("a serial row without an id in the file must not get one: %+v", c.New)
	}
}

func TestDelete(t *testing.T) {
	old := base + `    - plan_id: '{{ $.Plan.free.ID }}'
      code: webhooks
`
	res := compute(t, old, base)
	c := only(t, res, "Feature", fixturechange.Delete)
	if c.Key["code"].Lit != "webhooks" {
		t.Fatalf("unexpected key %+v", c.Key)
	}
	if c.Old["quota"].Lit != "0" {
		t.Fatalf("a delete is guarded by the row it expects to find, got %+v", c.Old)
	}
}

func TestDeleteOfAProtectedModelIsRefused(t *testing.T) {
	next := strings.Replace(base, `    - _id: team
      id: 2
      name: team
      currency_id: '{{ $.Currency.eur.ID }}'
      price_cents: 2000
      seats: 10
`, "", 1)
	// The features of that plan go too, otherwise they lose their reference.
	next = strings.Replace(next, `    - plan_id: '{{ $.Plan.team.ID }}'
      code: api
      quota: 5000
      enabled: true
`, "", 1)
	res := compute(t, base, next)
	if len(res.Refusals) != 1 || !strings.Contains(res.Refusals[0].Reason, "deletes of this model are refused") {
		t.Fatalf("expected one delete refusal, got %+v", res.Refusals)
	}
	for _, c := range res.Changes {
		if c.Model == "Plan" {
			t.Fatalf("a refused delete must not produce a change: %+v", c)
		}
	}
}

// A rename is an insert and a delete that happen to be the same row. Writing
// half of it leaves the database with both rows, so neither half is written,
// and the rows that point at the renamed one are refused with it.
func TestRenameIsRefusedAndWritesNothing(t *testing.T) {
	next := replace(t, base, "      name: team\n", "      name: crew\n")
	res := compute(t, base, next)
	if len(res.Changes) != 0 {
		t.Fatalf("a refused rename must write nothing at all, got %+v", res.Changes)
	}
	var renameSeen, cascadeSeen int
	for _, r := range res.Refusals {
		switch {
		case strings.Contains(r.Reason, "renamed from"):
			renameSeen++
		case strings.Contains(r.Reason, "whose rename was refused"):
			cascadeSeen++
		default:
			t.Fatalf("unexpected refusal %v", r)
		}
	}
	if renameSeen != 1 || cascadeSeen != 2 {
		t.Fatalf("expected the rename and the two rows pointing at it, got %+v", res.Refusals)
	}
}

// The other half of the same problem: the row kept its name and got a new id.
func TestRenumberedIDIsRefused(t *testing.T) {
	next := replace(t, base, "      id: 2\n      name: team\n", "      id: 7\n      name: team\n")
	res := compute(t, base, next)
	if len(res.Refusals) != 1 || !strings.Contains(res.Refusals[0].Reason, "changed from 2 to 7") {
		t.Fatalf("expected a refusal about the id, got %+v", res.Refusals)
	}
	if len(res.Changes) != 0 {
		t.Fatalf("expected no change, got %+v", res.Changes)
	}
}

func TestDuplicateKeyIsFineWhileNothingChanges(t *testing.T) {
	dup := base + `    - plan_id: '{{ $.Plan.free.ID }}'
      code: api
      quota: 100
`
	res := compute(t, dup, dup)
	if len(res.Changes) != 0 || len(res.Refusals) != 0 {
		t.Fatalf("expected nothing, got %+v / %+v", res.Changes, res.Refusals)
	}
}

func TestDuplicateKeyThatChangesIsRefused(t *testing.T) {
	old := base + `    - plan_id: '{{ $.Plan.free.ID }}'
      code: api
      quota: 100
`
	next := replace(t, old, "      code: api\n      quota: 100\n", "      code: api\n      quota: 150\n")
	res := compute(t, old, next)
	if len(res.Refusals) != 1 || !strings.Contains(res.Refusals[0].Reason, "not unique") {
		t.Fatalf("expected a refusal about the key, got %+v", res.Refusals)
	}
	if len(res.Changes) != 0 {
		t.Fatalf("expected no change, got %+v", res.Changes)
	}
}

// A delete can free what an insert takes, so deletes go first, children
// before parents; inserts follow the file's order, parents before children.
func TestDeletesComeFirstAndInsertsFollowTheFileOrder(t *testing.T) {
	old := base + `    - plan_id: '{{ $.Plan.team.ID }}'
      code: sso
      quota: 1
`
	next := replace(t, base, "- model: Feature\n", `    - _id: pro
      id: 3
      name: pro
      currency_id: '{{ $.Currency.eur.ID }}'
      price_cents: 9000
      seats: 100
- model: Feature
`) + `    - plan_id: '{{ $.Plan.pro.ID }}'
      code: sso
      quota: 1
`
	res := compute(t, old, next)
	var order []string
	for _, c := range res.Changes {
		order = append(order, string(c.Kind)+" "+c.Model)
	}
	want := []string{"delete Feature", "insert Plan", "insert Feature"}
	if len(order) != len(want) {
		t.Fatalf("expected %v, got %v", want, order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, order)
		}
	}
}

func TestModelMissingFromTheConfigurationIsAnError(t *testing.T) {
	err := computeErr(t, base, base+`- model: Coupon
  rows:
    - code: welcome
`)
	if !strings.Contains(err.Error(), `"Coupon"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExplicitNullIsNotAnEmptyString(t *testing.T) {
	old := replace(t, base, "      seats: 1\n", "      seats: 1\n      note: ~\n")
	next := replace(t, base, "      seats: 1\n", "      seats: 1\n      note: \"\"\n")
	res := compute(t, old, next)
	c := only(t, res, "Plan", fixturechange.Update)
	if !c.Old["note"].IsNull || c.New["note"].IsNull || c.New["note"].Lit != "" {
		t.Fatalf("expected NULL -> \"\", got old %+v new %+v", c.Old["note"], c.New["note"])
	}
}

// key_any_of covers a table whose identity includes which of a few mutually
// exclusive columns is set.
func TestKeyAnyOfPicksTheColumnThatIsSet(t *testing.T) {
	cfg := &Config{
		Models: map[string]*Model{
			"Plan":  {Table: "plans", Key: []string{"name"}},
			"Addon": {Table: "addons", Key: []string{"name"}},
			"Limit": {
				Table:      "limits",
				Serial:     true,
				Key:        []string{"code"},
				KeyAnyOf:   [][]string{{"plan_id", "addon_id"}},
				References: map[string]string{"plan_id": "Plan", "addon_id": "Addon"},
				Defaults:   map[string]string{"plan_id": "0", "addon_id": "0"},
			},
		},
	}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	const limits = `- model: Plan
  rows:
    - _id: free
      id: 1
      name: free
- model: Addon
  rows:
    - _id: extra
      id: 1
      name: extra
- model: Limit
  rows:
    - plan_id: '{{ $.Plan.free.ID }}'
      code: seats
      value: 1
    - addon_id: '{{ $.Addon.extra.ID }}'
      code: seats
      value: 5
`
	res := computeWith(t, cfg, limits, strings.Replace(limits, "      value: 5\n", "      value: 9\n", 1))
	if len(res.Changes) != 1 {
		t.Fatalf("the two rows must not collapse into one key: %+v", res.Changes)
	}
	c := res.Changes[0]
	if c.Key["addon_id"].Ref == nil || c.Key["addon_id"].Ref.Key != "extra" {
		t.Fatalf("expected the addon row, got key %+v", c.Key)
	}
	if _, ok := c.Key["plan_id"]; ok {
		t.Fatalf("the column that is not set does not belong in the key: %+v", c.Key)
	}
}

func TestSummary(t *testing.T) {
	res := compute(t, base, replace(t, base, "      price_cents: 2000\n", "      price_cents: 2500\n"))
	got := strings.Join(res.Summary(), "; ")
	if got != "Plan: 1 update" {
		t.Fatalf("unexpected summary %q", got)
	}
}

// A rename is refused by default because an insert plus a delete is not a
// rename. With policy.renames set to update it is written as what it is: an
// update of the key columns, guarded by the id as well, so it cannot land on a
// row that merely happens to carry the old name.
func TestRenameAsAnUpdate(t *testing.T) {
	cfg := testConfig(t)
	cfg.Policy.Renames = RenameUpdate
	res := computeWith(t, cfg, base, replace(t, base, "      name: team\n", "      name: crew\n"))
	if len(res.Refusals) != 0 {
		t.Fatalf("expected no refusal, got %+v", res.Refusals)
	}
	c := only(t, res, "Plan", fixturechange.Update)
	if c.ID != "2" {
		t.Fatalf("a rename has to be guarded by the id, got %q", c.ID)
	}
	if c.Key["name"].Lit != "team" || c.New["name"].Lit != "crew" || c.Old["name"].Lit != "team" {
		t.Fatalf("expected team -> crew, got key %+v old %+v new %+v", c.Key, c.Old, c.New)
	}
	// The rename runs before everything else, so a row that points at the new
	// name finds it.
	if res.Changes[0].ID != "2" {
		t.Fatalf("the rename belongs first: %+v", res.Changes)
	}
}

// The case a cascade has to cover: one row is renamed away from a name and
// another row takes it. Writing only half of that leaves the database holding
// the old name twice, or not at all.
func TestRenameRefusedWhileANewRowTakesTheOldName(t *testing.T) {
	next := replace(t, base, "      name: team\n", "      name: crew\n")
	next = replace(t, next, "- model: Feature\n", `    - _id: team2
      id: 9
      name: team
      currency_id: '{{ $.Currency.eur.ID }}'
      price_cents: 3000
      seats: 20
- model: Feature
`)
	res := compute(t, base, next)
	for _, c := range res.Changes {
		if c.Model == "Plan" {
			t.Fatalf("nothing may be written for Plan while the rename stands: %+v", c)
		}
	}
	if len(res.Refusals) == 0 {
		t.Fatal("expected the rename to be refused")
	}
}

// With renames set to update the same case works, because the rename runs
// first and the name is free by the time the new row is inserted.
func TestRenameAsAnUpdateFreesTheOldName(t *testing.T) {
	cfg := testConfig(t)
	cfg.Policy.Renames = RenameUpdate
	next := replace(t, base, "      name: team\n", "      name: crew\n")
	next = replace(t, next, "- model: Feature\n", `    - _id: team2
      id: 9
      name: team
      currency_id: '{{ $.Currency.eur.ID }}'
      price_cents: 3000
      seats: 20
- model: Feature
`)
	res := computeWith(t, cfg, base, next)
	if len(res.Refusals) != 0 {
		t.Fatalf("expected no refusal, got %+v", res.Refusals)
	}
	var kinds []string
	for _, c := range res.Changes {
		kinds = append(kinds, string(c.Kind)+" "+c.Model)
	}
	if len(kinds) != 2 || kinds[0] != "update Plan" || kinds[1] != "insert Plan" {
		t.Fatalf("the rename has to come before the insert that takes the name: %v", kinds)
	}
}

// An accepted rename rewrites the base state, and that has to happen on a copy:
// check reads the database once, and a snapshot that came back changed would
// make a second comparison against it answer something else.
func TestComputeLeavesItsSnapshotsAlone(t *testing.T) {
	cfg := testConfig(t)
	cfg.Policy.Renames = RenameUpdate
	next := replace(t, base, "      name: team\n", "      name: crew\n")
	old, head := snap(t, cfg, base, "base"), snap(t, cfg, next, "head")

	first, err := Compute(cfg, old, head)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	second, err := Compute(cfg, old, head)
	if err != nil {
		t.Fatalf("second Compute: %v", err)
	}
	if len(first.Changes) != len(second.Changes) {
		t.Fatalf("the second run saw %d changes, the first %d", len(second.Changes), len(first.Changes))
	}
	for _, e := range old.Entries["Plan"] {
		if e.ID == "2" && e.Cells["name"].Lit != "team" {
			t.Fatalf("the base snapshot was rewritten in place: %+v", e.Cells)
		}
	}
}

// Renumbering a primary key is refused by default: live data points at the old
// id. A project whose ids are internal can say so.
func TestIDDriftPolicy(t *testing.T) {
	next := replace(t, base, "      id: 2\n      name: team\n", "      id: 7\n      name: team\n")

	// warn reports and carries on: a warning, not a refusal, and the rest of
	// the row is still migrated.
	warn := testConfig(t)
	warn.Policy.IDDrift = ModeWarn
	res := computeWith(t, warn, base, replace(t, next, "      price_cents: 2000\n", "      price_cents: 2500\n"))
	if len(res.Refusals) != 0 || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0].Reason, "changed from 2 to 7") {
		t.Fatalf("expected one warning and no refusal, got %+v / %+v", res.Warnings, res.Refusals)
	}
	if c := only(t, res, "Plan", fixturechange.Update); c.New["price_cents"].Lit != "2500" {
		t.Fatalf("the rest of the row is migrated: %+v", c)
	}

	ignore := testConfig(t)
	ignore.Policy.IDDrift = ModeIgnore
	res = computeWith(t, ignore, base, next)
	if len(res.Refusals) != 0 || len(res.Changes) != 0 {
		t.Fatalf("ignore means the id is nobody's business: %+v / %+v", res.Refusals, res.Changes)
	}
}

func TestDeletePolicyAppliesToModelsThatDoNotOverrideIt(t *testing.T) {
	cfg := testConfig(t)
	cfg.Models["Feature"].Deletes = ""
	cfg.Policy.Deletes = DeleteRefuse
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	old := base + `    - plan_id: '{{ $.Plan.free.ID }}'
      code: webhooks
`
	res := computeWith(t, cfg, old, base)
	if len(res.Changes) != 0 || len(res.Refusals) != 1 {
		t.Fatalf("expected the delete to be refused, got %+v / %+v", res.Changes, res.Refusals)
	}
}

// The models have to be written out in an order that lets dbfixture resolve
// every reference as it goes.
func TestDependencyOrder(t *testing.T) {
	order, err := testConfig(t).DependencyOrder()
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, m := range order {
		pos[m] = i
	}
	if pos["Currency"] > pos["Plan"] || pos["Plan"] > pos["Feature"] {
		t.Fatalf("unexpected order %v", order)
	}
}

func TestDependencyOrderReportsACircle(t *testing.T) {
	cfg := &Config{Models: map[string]*Model{
		"A": {Table: "a", References: map[string]string{"b_id": "B"}},
		"B": {Table: "b", References: map[string]string{"a_id": "A"}},
	}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.DependencyOrder(); err == nil || !strings.Contains(err.Error(), "circle") {
		t.Fatalf("expected a circle to be reported, got %v", err)
	}
}

// Two rows holding one natural key make every lookup by it ambiguous. It is
// reported with the ids, so the list can be worked through.
func TestSnapshotReportsDuplicateKeys(t *testing.T) {
	cfg := testConfig(t)
	dup := replace(t, base, "      code: EUR\n", "      code: EUR\n    - _id: eur2\n      id: 2\n      code: EUR\n")
	s := snap(t, cfg, dup, "fixture.yml")
	if len(s.Findings) != 1 || s.Findings[0].Kind != FindingDuplicateKey {
		t.Fatalf("expected one duplicate finding, got %+v", s.Findings)
	}
	if !strings.Contains(s.Findings[0].Detail, "(1, 2)") {
		t.Fatalf("the finding has to name the colliding ids: %q", s.Findings[0].Detail)
	}
}

// Two rows swapping names cannot be written in either order without the
// intermediate step breaking a unique index, so it is refused even where
// renames are allowed.
func TestSwappedNamesAreRefused(t *testing.T) {
	cfg := testConfig(t)
	cfg.Policy.Renames = RenameUpdate
	next := replace(t, base, "      name: free\n", "      name: TEMP\n")
	next = replace(t, next, "      name: team\n", "      name: free\n")
	next = replace(t, next, "      name: TEMP\n", "      name: team\n")
	res := computeWith(t, cfg, base, next)
	if len(res.Refusals) == 0 {
		t.Fatalf("expected a refusal, got changes %+v", res.Changes)
	}
	var swap int
	for _, r := range res.Refusals {
		switch {
		case strings.Contains(r.Reason, "cannot swap"):
			swap++
		case strings.Contains(r.Reason, "whose rename was refused"):
		default:
			t.Fatalf("unexpected refusal %v", r)
		}
	}
	if swap != 2 {
		t.Fatalf("both halves of the swap have to be named: %+v", res.Refusals)
	}
	if len(res.Changes) != 0 {
		t.Fatalf("a refused swap writes nothing: %+v", res.Changes)
	}
}

// A natural key is compared as one string, which has to be equal exactly when
// the keys are: values holding the separators, a NULL and the text "NULL", a
// reference and a text that reads like one are all different keys.
func TestNaturalKeysThatOnlyReadAlikeAreDifferentKeys(t *testing.T) {
	cfg := &Config{Models: map[string]*Model{
		"Currency": {Table: "currencies", Ref: "code", Key: []string{"code"}},
		"Pair":     {Table: "pairs", Key: []string{"a", "b"}},
		"Code":     {Table: "codes", Key: []string{"code"}, References: map[string]string{"currency": "Currency"}},
	}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	s := snap(t, cfg, `- model: Currency
  rows:
    - {_id: eur, id: 1, code: EUR}
- model: Pair
  rows:
    - {id: 1, a: "x/b=y", b: "z"}
    - {id: 2, a: "x", b: "y/b=z"}
- model: Code
  rows:
    - {id: 1, code: ~}
    - {id: 2, code: "NULL"}
- model: Code
  rows:
    - {id: 3, code: x, currency: '{{ $.Currency.eur.ID }}'}
    - {id: 4, code: y, currency: 0}
`, "fixture.yml")
	if len(s.Findings) != 0 {
		t.Fatalf("no two of these rows share a key: %+v", s.Findings)
	}
	codes := s.Entries["Code"]
	if codes[0].KeyStr == codes[1].KeyStr {
		t.Fatalf("NULL and the text NULL are one key: %q", codes[0].KeyStr)
	}
	ref := keyString("Code", fixturechange.Values{"code": fixturechange.RefTo("Currency", "EUR")})
	lit := keyString("Code", fixturechange.Values{"code": fixturechange.Lit("Currency(EUR)")})
	if ref == lit {
		t.Fatalf("a reference and a text that reads like one are one key: %q", ref)
	}
	// The label stays the readable one the reports have always shown.
	if got := codes[1].label("Code"); got != "Code/code=NULL" {
		t.Fatalf("label %q", got)
	}

	// Without an id to tie them together, a row keyed by NULL and one keyed
	// by the text "NULL" are two rows, not one row that did not change.
	cfgNoID := &Config{Models: map[string]*Model{"Code": {Table: "codes", Key: []string{"code"}}}}
	if err := cfgNoID.Prepare(); err != nil {
		t.Fatal(err)
	}
	res := computeWith(t, cfgNoID, "- model: Code\n  rows:\n    - {code: \"NULL\", v: 1}\n",
		"- model: Code\n  rows:\n    - {code: ~, v: 1}\n")
	if ins, _, del := res.Totals(); ins != 1 || del != 1 {
		t.Fatalf("expected an insert and a delete, got %+v / %+v", res.Changes, res.Refusals)
	}
}

// Two groups of rows sharing a key compare as multisets of whole rows, which
// tell a NULL from the text "NULL" as well.
func TestSameRowSetTellsANullFromItsSpelling(t *testing.T) {
	a := []*Entry{{Cells: fixturechange.Values{"note": fixturechange.Null()}}}
	b := []*Entry{{Cells: fixturechange.Values{"note": fixturechange.Lit("NULL")}}}
	if sameRowSet(a, b) {
		t.Fatal("NULL and the text NULL are the same row")
	}
}

// kindsOf is the changes of a result as "kind Model key", in order.
func kindsOf(res *Result) string {
	var out []string
	for _, c := range res.Changes {
		out = append(out, string(c.Kind)+" "+keyLabel(c.Model, c.Key))
	}
	return strings.Join(out, "; ")
}

// Closing a price and opening the next one in one release: an exclusion
// constraint, or a partial unique index on the open price, accepts the new
// open row only once the old one is closed. Updates come before inserts.
func TestAnUpdateThatFreesAValueComesBeforeTheInsertTakingIt(t *testing.T) {
	cfg := &Config{Models: map[string]*Model{
		"Plan":  {Table: "plans", Ref: "code", Key: []string{"code"}},
		"Price": {Table: "prices", Serial: true, Key: []string{"plan_id", "valid_from"}, References: map[string]string{"plan_id": "Plan"}},
	}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	head := "- model: Plan\n  rows:\n    - {_id: basic, id: 1, code: basic}\n- model: Price\n  rows:\n"
	old := head + "    - {id: 1, plan_id: '{{ $.Plan.basic.ID }}', valid_from: '2026-01-01', valid_to: ~, cents: 900}\n"
	next := head + "    - {id: 1, plan_id: '{{ $.Plan.basic.ID }}', valid_from: '2026-01-01', valid_to: '2026-11-01', cents: 900}\n" +
		"    - {id: 2, plan_id: '{{ $.Plan.basic.ID }}', valid_from: '2026-11-01', valid_to: ~, cents: 1200}\n"
	got := kindsOf(computeWith(t, cfg, old, next))
	if got != "update Price/plan_id=Plan(basic)/valid_from=2026-01-01; insert Price/plan_id=Plan(basic)/valid_from=2026-11-01" {
		t.Fatalf("the old price has to be closed before the new one opens: %s", got)
	}
}

// A row moved under a parent inserted in the same set waits for the insert;
// the parent it leaves is deleted once nothing points at it any more.
func TestAMoveToANewParentComesBetweenItsInsertAndTheOldParentsDelete(t *testing.T) {
	old := replace(t, base, "- model: Feature\n", `    - _id: old
      id: 3
      name: old
      currency_id: '{{ $.Currency.eur.ID }}'
      price_cents: 1
      seats: 1
- model: Feature
`) + "    - plan_id: '{{ $.Plan.old.ID }}'\n      code: sso\n      quota: 1\n"
	next := replace(t, base, "- model: Feature\n", `    - _id: pro
      id: 4
      name: pro
      currency_id: '{{ $.Currency.eur.ID }}'
      price_cents: 1
      seats: 1
- model: Feature
`) + "    - plan_id: '{{ $.Plan.pro.ID }}'\n      code: sso\n      quota: 1\n"
	cfg := testConfig(t)
	cfg.Models["Plan"].Deletes = DeleteAllow
	cfg.Models["Feature"].Key = []string{"code", "quota"}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	got := kindsOf(computeWith(t, cfg, old, next))
	want := "insert Plan/name=pro; update Feature/code=sso/quota=1; delete Plan/name=old"
	if got != want {
		t.Fatalf("expected\n%s\ngot\n%s", want, got)
	}
}

// A model that leaves the file entirely still comes before the models
// pointing at it, so its rows are deleted after theirs.
func TestAModelThatLeftTheFileIsDeletedAfterTheRowsPointingAtIt(t *testing.T) {
	cfg := &Config{Models: map[string]*Model{
		"Cur":  {Table: "curs", Ref: "code", Key: []string{"code"}},
		"Plan": {Table: "plans", References: map[string]string{"cur_id": "Cur"}},
	}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	res := computeWith(t, cfg, `- model: Cur
  rows:
    - {_id: usd, id: 1, code: USD}
- model: Plan
  rows:
    - {id: 1, name: a, cur_id: '{{ $.Cur.usd.ID }}'}
    - {id: 2, name: keep, cur_id: ~}
`, `- model: Plan
  rows:
    - {id: 2, name: keep, cur_id: ~}
`)
	if got := kindsOf(res); got != "delete Plan/name=a; delete Cur/code=USD" {
		t.Fatalf("the plan pointing at the currency has to go first: %s", got)
	}
	if strings.Join(res.Order, ",") != "Cur,Plan" {
		t.Fatalf("model order %v", res.Order)
	}
}

// A model split over several blocks keeps its rows in file order, after the
// models it points at, wherever their blocks are.
func TestAModelSplitOverBlocksKeepsItsRowOrder(t *testing.T) {
	cfg := &Config{Models: map[string]*Model{
		"Cur":  {Table: "curs", Ref: "code", Key: []string{"code"}},
		"Plan": {Table: "plans", References: map[string]string{"cur_id": "Cur"}},
	}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	res := computeWith(t, cfg, "[]\n", `- model: Plan
  rows:
    - {id: 1, name: b, cur_id: ~}
- model: Cur
  rows:
    - {_id: usd, id: 1, code: USD}
- model: Plan
  rows:
    - {id: 2, name: a, cur_id: '{{ $.Cur.usd.ID }}'}
    - {id: 3, name: c, cur_id: ~}
`)
	if got := kindsOf(res); got != "insert Cur/code=USD; insert Plan/name=b; insert Plan/name=a; insert Plan/name=c" {
		t.Fatalf("got %s", got)
	}
}

// Rows of a tree as a database returns them, in id order, where a parent can
// come after its child: inserts still go parents first, deletes children first.
func TestATreeIsInsertedParentsFirstAndDeletedChildrenFirst(t *testing.T) {
	cfg := treeConfig(t)
	tree := treeState([3]string{"1", "leaf", "root"}, [3]string{"2", "root", ""}, [3]string{"3", "other", ""})
	empty := &Snapshot{Source: "empty", Order: []string{"Node"}, Entries: map[string][]*Entry{}}
	res, err := Compute(cfg, empty, tree)
	if err != nil {
		t.Fatal(err)
	}
	if got := kindsOf(res); got != "insert Node/name=root; insert Node/name=leaf; insert Node/name=other" {
		t.Fatalf("inserts: %s", got)
	}
	reversed := treeState([3]string{"1", "root", ""}, [3]string{"2", "leaf", "root"})
	if res, err = Compute(cfg, reversed, empty); err != nil {
		t.Fatal(err)
	}
	if got := kindsOf(res); got != "delete Node/name=leaf; delete Node/name=root" {
		t.Fatalf("deletes: %s", got)
	}
}

// A unique value can move from one row to another in one set: the row giving
// it up goes first. Two rows trading values cannot both go first, which means
// the column is not unique, and they keep their order.
func TestARowTakingAValueAnotherGivesUpWaitsForIt(t *testing.T) {
	cfg := &Config{Models: map[string]*Model{"Seat": {Table: "seats", Ref: "code", Key: []string{"code"}}}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	old := "- model: Seat\n  rows:\n    - {id: 1, code: b, slot: 3}\n    - {id: 2, code: a, slot: 1}\n"
	next := "- model: Seat\n  rows:\n    - {id: 1, code: b, slot: 1}\n    - {id: 2, code: a, slot: 2}\n"
	if got := kindsOf(computeWith(t, cfg, old, next)); got != "update Seat/code=a; update Seat/code=b" {
		t.Fatalf("a gives slot 1 up, so it goes first: %s", got)
	}
	swap := "- model: Seat\n  rows:\n    - {id: 1, code: b, slot: 1}\n    - {id: 2, code: a, slot: 3}\n"
	if got := kindsOf(computeWith(t, cfg, old, swap)); got != "update Seat/code=b; update Seat/code=a" {
		t.Fatalf("a trade keeps the file's order: %s", got)
	}
}
