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
				Table:    "currencies",
				Ref:      "code",
				Key:      []string{"code"},
				StableID: "id",
			},
			"Plan": {
				Table:      "plans",
				Key:        []string{"name"},
				StableID:   "id",
				References: map[string]string{"currency_id": "Currency"},
				Derived:    []string{"price_per_seat_cents"},
				NoDelete:   true,
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

func compute(t *testing.T, oldText, newText string) *Result {
	t.Helper()
	res, err := Compute(testConfig(t), doc(t, oldText), doc(t, newText))
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	return res
}

func computeErr(t *testing.T, oldText, newText string) error {
	t.Helper()
	_, err := Compute(testConfig(t), doc(t, oldText), doc(t, newText))
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

func TestInsertsComeBeforeDeletesAndFollowTheFileOrder(t *testing.T) {
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
	want := []string{"insert Plan", "insert Feature", "delete Feature"}
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
	res, err := Compute(cfg, doc(t, limits), doc(t, strings.Replace(limits, "      value: 5\n", "      value: 9\n", 1)))
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
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
