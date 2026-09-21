package fixturemigrate

import (
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"
)

// The database side of a comparison is built out of these four: the column
// list, the SELECT, the natural key and the anchor. None of them needs a
// connection, and all of them decide what the diff sees.

func TestReadColumnsTakesTheWholeTableWhenNobodyAsksForLess(t *testing.T) {
	cfg := testConfig(t)
	m := cfg.Models["Plan"]
	cols, err := readColumns(m, testTables()["public.plans"], nil)
	if err != nil {
		t.Fatal(err)
	}
	// Not the id, which is never an ordinary column, and not the derived one,
	// which the application recalculates.
	want := []string{"currency_id", "name", "note", "price_cents", "seats"}
	if strings.Join(cols, ",") != strings.Join(want, ",") {
		t.Fatalf("readColumns = %v, want %v", cols, want)
	}
}

// A comparison reads only the columns the fixture file writes, because a
// column no fixture row mentions is not master data. The key columns come
// along whether the file spells them out or not: without them the row cannot
// be matched at all.
func TestReadColumnsKeepsTheKeyAndDropsTheRest(t *testing.T) {
	cfg := testConfig(t)
	cols, err := readColumns(cfg.Models["Plan"], testTables()["public.plans"], []string{"price_cents"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cols, ",") != "name,price_cents" {
		t.Fatalf("readColumns = %v", cols)
	}
}

func TestReadColumnsReportsAColumnTheTableDoesNotHave(t *testing.T) {
	cfg := testConfig(t)
	_, err := readColumns(cfg.Models["Plan"], testTables()["public.plans"], []string{"trial_days"})
	if err == nil || !strings.Contains(err.Error(), `column "trial_days" is not in public.plans`) {
		t.Fatalf("expected the column and the table to be named, got %v", err)
	}

	cfg.Models["Plan"].Key = []string{"slug"}
	_, err = readColumns(cfg.Models["Plan"], testTables()["public.plans"], []string{"price_cents"})
	if err == nil || !strings.Contains(err.Error(), `key column "slug"`) {
		t.Fatalf("a key column that is not there is worse, not better: %v", err)
	}
}

func TestSelectQuery(t *testing.T) {
	cfg := testConfig(t)
	cfg.Models["Plan"].Where = "archived_at IS NULL"
	query, hasID, err := selectQuery(cfg, cfg.Models["Plan"], testTables()["public.plans"], []string{"name", "price_cents"})
	if err != nil {
		t.Fatal(err)
	}
	const want = `SELECT ("id")::text, ("name")::text, ("price_cents")::text ` +
		`FROM "public"."plans" WHERE (archived_at IS NULL) ORDER BY "id", "name"`
	if query != want {
		t.Fatalf("selectQuery =\n%s\nwant\n%s", query, want)
	}
	if !hasID {
		t.Fatal("the first column is the id")
	}
}

// Everything that reaches a query as an identifier is checked, wherever it
// came from. A key column is the one that used to slip through, because it is
// added to the ORDER BY rather than to the projection.
func TestSelectQueryRefusesAnIdentifierItCannotQuote(t *testing.T) {
	cfg := testConfig(t)
	table := testTables()["public.plans"]
	cfg.Models["Plan"].Key = []string{`name" , (SELECT 1)--`}
	if _, _, err := selectQuery(cfg, cfg.Models["Plan"], table, []string{"name"}); err == nil ||
		!strings.Contains(err.Error(), "key column") {
		t.Fatalf("expected the key column to be refused, got %v", err)
	}

	cfg = testConfig(t)
	cfg.Models["Plan"].Table = "plans; DROP TABLE plans"
	if _, _, err := selectQuery(cfg, cfg.Models["Plan"], table, []string{"name"}); err == nil {
		t.Fatal("expected the table to be refused")
	}
}

// A table without the configured id column is read without one: the model has
// no primary key this tool can use, and its rows are identified by their
// natural key alone.
func TestSelectQueryWithoutAnIDColumn(t *testing.T) {
	cfg := testConfig(t)
	cfg.Models["Currency"].ID = "currency_id"
	query, hasID, err := selectQuery(cfg, cfg.Models["Currency"], testTables()["public.currencies"], []string{"code"})
	if err != nil {
		t.Fatal(err)
	}
	if hasID {
		t.Fatal("the table has no such column")
	}
	if strings.Contains(query, "currency_id") {
		t.Fatalf("nothing may select a column that is not there: %s", query)
	}
}

func TestKeyOfPicksTheFirstSetColumnOfAGroup(t *testing.T) {
	cfg := testConfig(t)
	m := &Model{Table: "targets", ID: "id", Ref: "name", Key: []string{"name"},
		KeyAnyOf: [][]string{{"item_id", "unit_id", "building_id"}}}
	cfg.Models["Target"] = m
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}

	values := fixturechange.Values{
		"name":        fixturechange.Lit("reward"),
		"item_id":     fixturechange.Lit("0"),
		"unit_id":     fixturechange.RefTo("Plan", "team"),
		"building_id": fixturechange.Null(),
	}
	key, err := keyOf(cfg, m, "Target", values)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 2 || key["name"].Lit != "reward" || key["unit_id"].Ref == nil {
		t.Fatalf("expected name and unit_id, got %+v", key)
	}

	// A group where nothing is set still contributes its first column, so two
	// such rows compare equal instead of looking like different keys.
	empty, err := keyOf(cfg, m, "Target", fixturechange.Values{
		"name": fixturechange.Lit("reward"), "item_id": fixturechange.Lit("0")})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := empty["item_id"]; !ok || len(empty) != 2 {
		t.Fatalf("expected the group's first column, got %+v", empty)
	}
}

func TestKeyOfReportsAColumnThatWasNotRead(t *testing.T) {
	cfg := testConfig(t)
	_, err := keyOf(cfg, cfg.Models["Plan"], "Plan", fixturechange.Values{"price_cents": fixturechange.Lit("1")})
	if err == nil || !strings.Contains(err.Error(), `key column "name" was not read`) {
		t.Fatalf("expected the column to be named, got %v", err)
	}
}

// The anchor is what a reference in an exported file names, so it has to be
// readable, stable and unique inside its model.
func TestAnchorOf(t *testing.T) {
	for _, tc := range []struct {
		key  fixturechange.Values
		want string
	}{
		{fixturechange.Values{"code": fixturechange.Lit("EUR")}, "eur"},
		{fixturechange.Values{"name": fixturechange.Lit("Pro Plan (2026)")}, "pro_plan_2026"},
		{fixturechange.Values{"code": fixturechange.Lit("api"), "plan_id": fixturechange.RefTo("Plan", "team")},
			"api_team"},
		{fixturechange.Values{"note": fixturechange.Null()}, "null"},
		{fixturechange.Values{"code": fixturechange.Lit("...")}, "row"},
	} {
		if got := anchorOf(tc.key); got != tc.want {
			t.Errorf("anchorOf(%+v) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

func TestUniqueAnchorFallsBackToTheIDAndThenToACounter(t *testing.T) {
	taken := map[string]bool{}
	if got := uniqueAnchor("api", "7", taken); got != "api" {
		t.Fatalf("got %q", got)
	}
	// The second row with that key is told apart by its id, which is at least
	// something a person can look up.
	if got := uniqueAnchor("api", "8", taken); got != "api_8" {
		t.Fatalf("got %q", got)
	}
	// A row with no id, or one whose id-suffixed anchor is somehow taken too,
	// falls back to counting. Readability is the first of the three properties
	// to give up, uniqueness the last.
	if got := uniqueAnchor("api", "", taken); got != "api_2" {
		t.Fatalf("got %q", got)
	}
	if got := uniqueAnchor("api", "8", taken); got != "api_3" {
		t.Fatalf("got %q", got)
	}
}

func TestIsZero(t *testing.T) {
	for _, tc := range []struct {
		value fixturechange.Value
		want  bool
	}{
		{fixturechange.Lit("0"), true},
		{fixturechange.Lit("0.0"), true},
		{fixturechange.Lit(""), true},
		{fixturechange.Null(), true},
		{fixturechange.Lit("1"), false},
		{fixturechange.Lit("false"), false},
		{fixturechange.RefTo("Plan", "team"), false},
	} {
		if got := isZero(tc.value); got != tc.want {
			t.Errorf("isZero(%s) = %v", tc.value, got)
		}
	}
}

func TestNormalizeComparesNumbersByValueAndLeavesTextAlone(t *testing.T) {
	for in, want := range map[string]string{
		"1.0":    "1",
		" 2 ":    "2",
		"2.50":   "2.5",
		"1e3":    "1000",
		"01":     "1",
		"EUR":    "EUR",
		"":       "",
		"1 plan": "1 plan",
	} {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}
