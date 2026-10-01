package dbtest_test

// What a guard holds a value to, type by type: a hand edit has to show up as
// a changed row, and the value the change wrote, or the database already held,
// as the same value.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/fixtureapply"
	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
)

// guardOne applies one change to model "Row" of table and returns its outcome.
func guardOne(t *testing.T, db bun.IDB, table string, policy fixturechange.Policy, c fixturechange.Change) (fixtureapply.Outcome, error) {
	t.Helper()
	c.Model = "Row"
	set := fixturechange.Set{Name: "guard", Policy: policy,
		Tables:  fixturechange.Tables{"Row": {Name: table, ID: "id", Key: "name"}},
		Changes: []fixturechange.Change{c}}
	outcomes, err := applyReporting(t, db, set)
	if len(outcomes) != 1 {
		t.Fatalf("outcomes %+v, %v", outcomes, err)
	}
	return outcomes[0], err
}

func lit(col, v string) fixturechange.Values { return fixturechange.Values{col: fixturechange.Lit(v)} }

var strict = fixturechange.Policy{ChangedRow: fixturechange.ModeError}

// interval's = says '1 mon' is '30 days' and '1 day' is '24:00:00'. A hand
// edit from one month to 30 days -- '2026-01-31' plus the one is 2026-02-28,
// plus the other 2026-03-02 -- was overwritten under changed_row error, and a
// row holding '24:00:00' was taken for one already holding '1 day'.
func TestAnIntervalGuardTellsAMonthFromThirtyDays(t *testing.T) {
	db := connect(t)
	run(t, db, "DROP TABLE IF EXISTS guard_intervals",
		"CREATE TABLE guard_intervals (id bigint PRIMARY KEY, name text UNIQUE NOT NULL, trial interval, steps interval[])",
		"INSERT INTO guard_intervals VALUES (1, 'team', '30 days', '{30 days}'), (2, 'solo', '24 hours', NULL), "+
			"(3, 'free', '-1 days +02:03:04', NULL)")
	trial := func(name string) string {
		return scan[string](t, db, "SELECT trial::text FROM guard_intervals WHERE name = ?", name)
	}
	out, err := guardOne(t, db, "guard_intervals", strict, fixturechange.Change{Kind: fixturechange.Update,
		Key: lit("name", "team"), Old: lit("trial", "1 mon"), New: lit("trial", "14 days")})
	if err == nil || out.Problem != fixtureapply.ProblemChangedRow || trial("team") != "30 days" {
		t.Fatalf("the hand edit to 30 days has to be a changed row: %v %+v, trial %s", err, out, trial("team"))
	}
	// An array of them compares its elements alike.
	out, err = guardOne(t, db, "guard_intervals", strict, fixturechange.Change{Kind: fixturechange.Update,
		Key: lit("name", "team"), Old: lit("steps", `["1 mon"]`), New: lit("steps", `["14 days"]`)})
	if err == nil || out.Problem != fixtureapply.ProblemChangedRow {
		t.Fatalf("{30 days} is not {1 mon}: %v %+v", err, out)
	}
	out, err = guardOne(t, db, "guard_intervals", strict, fixturechange.Change{Kind: fixturechange.Update,
		Key: lit("name", "solo"), Old: lit("trial", "2 days"), New: lit("trial", "1 day")})
	if err == nil || out.Problem != fixtureapply.ProblemChangedRow || trial("solo") != "24:00:00" {
		t.Fatalf("24:00:00 is not 1 day already held: %v %+v, trial %s", err, out, trial("solo"))
	}
	// The change made, and made again.
	change := fixturechange.Change{Kind: fixturechange.Update,
		Key: lit("name", "solo"), Old: lit("trial", "24:00:00"), New: lit("trial", "1 day")}
	for _, want := range []fixtureapply.Status{fixtureapply.StatusApplied, fixtureapply.StatusUnchanged} {
		if out, err := guardOne(t, db, "guard_intervals", strict, change); err != nil || out.Status != want {
			t.Fatalf("want %s: %v %+v", want, err, out)
		}
	}
	if trial("solo") != "1 day" {
		t.Fatalf("trial %s", trial("solo"))
	}

	// IntervalStyle decides how '-1 2:03:04' is read: under sql_standard the
	// sign covers the time as well. The change set reads it as the
	// generator did, under postgres, whatever the connection has.
	sqlStandard := openDB(t, os.Getenv("BUN_FIXTURE_MIGRATE_POSTGRES"), map[string]string{"IntervalStyle": "sql_standard"})
	t.Cleanup(func() { sqlStandard.Close() })
	if out, err := guardOne(t, sqlStandard, "guard_intervals", strict, fixturechange.Change{Kind: fixturechange.Update,
		Key: lit("name", "free"), Old: lit("trial", "-1 2:03:04"), New: lit("trial", "7 days")}); err != nil ||
		out.Status != fixtureapply.StatusApplied {
		t.Fatalf("the old value is the one the row holds: %v %+v", err, out)
	}
	if trial("free") != "7 days" {
		t.Fatalf("trial %s", trial("free"))
	}
}

// citext's = ignores case, as does a nondeterministic collation's. A hand
// edit that only changed the case of a value was taken for the value the
// change was generated against, and overwritten under changed_row error. A
// natural key keeps the column's own equality, which the unique index holds
// it to.
func TestACaseInsensitiveColumnGuardsItsCase(t *testing.T) {
	db := connect(t)
	run(t, db, "CREATE EXTENSION IF NOT EXISTS citext")
	for _, c := range []struct{ name, typ, table string }{{"citext", "citext", "guard_citext"},
		{"a nondeterministic collation", `text COLLATE guard_ci`, "guard_collation"}} {
		t.Run(c.name, func(t *testing.T) {
			if c.typ != "citext" {
				if _, err := db.ExecContext(context.Background(), "CREATE COLLATION IF NOT EXISTS guard_ci "+
					"(provider = icu, locale = 'und-u-ks-level2', deterministic = false)"); err != nil {
					t.Skipf("no ICU collation in this server: %v", err)
				}
			}
			run(t, db, "DROP TABLE IF EXISTS "+c.table,
				"CREATE TABLE "+c.table+" (id bigint PRIMARY KEY, name "+c.typ+" UNIQUE NOT NULL, label "+c.typ+", price int)",
				"INSERT INTO "+c.table+" VALUES (1, 'Team', 'TEAM', 10)")
			label := func() string { return scan[string](t, db, "SELECT label::text FROM "+c.table) }
			// An admin wrote the label in capitals.
			out, err := guardOne(t, db, c.table, strict, fixturechange.Change{Kind: fixturechange.Update,
				Key: lit("name", "Team"), Old: lit("label", "Team"), New: lit("label", "Team plan")})
			if err == nil || out.Problem != fixtureapply.ProblemChangedRow || label() != "TEAM" {
				t.Fatalf("a case-only edit has to be a changed row: %v %+v, label %s", err, out, label())
			}
			// A delete guarded by the old row leaves it alone just the same.
			out, err = guardOne(t, db, c.table, strict, fixturechange.Change{Kind: fixturechange.Delete,
				Key: lit("name", "Team"), Old: fixturechange.Values{"name": fixturechange.Lit("Team"),
					"label": fixturechange.Lit("Team"), "price": fixturechange.Lit("10")}})
			if err == nil || out.Problem != fixtureapply.ProblemChangedRow {
				t.Fatalf("the delete has to be refused: %v %+v", err, out)
			}
			// The natural key finds the row the unique index says it names.
			out, err = guardOne(t, db, c.table, strict, fixturechange.Change{Kind: fixturechange.Update,
				Key: lit("name", "team"), Old: lit("price", "10"), New: lit("price", "12")})
			if err != nil || out.Status != fixtureapply.StatusApplied {
				t.Fatalf("the key team names the row Team: %v %+v", err, out)
			}
			// An insert of a key the index holds equal to one there finds that
			// row, which is spelled otherwise: a changed row, not a unique
			// violation.
			out, err = guardOne(t, db, c.table, fixturechange.Policy{}, fixturechange.Change{Kind: fixturechange.Insert,
				Key: lit("name", "TEAM"), New: fixturechange.Values{"id": fixturechange.Lit("1"),
					"name": fixturechange.Lit("TEAM"), "label": fixturechange.Lit("TEAM"), "price": fixturechange.Lit("12")}})
			if err != nil || out.Status != fixtureapply.StatusSkipped || out.Problem != fixtureapply.ProblemChangedRow {
				t.Fatalf("the insert has to find the row there: %v %+v", err, out)
			}
			// And the label's change, made and made again.
			change := fixturechange.Change{Kind: fixturechange.Update,
				Key: lit("name", "Team"), Old: lit("label", "TEAM"), New: lit("label", "Team plan")}
			for _, want := range []fixtureapply.Status{fixtureapply.StatusApplied, fixtureapply.StatusUnchanged} {
				if out, err := guardOne(t, db, c.table, strict, change); err != nil || out.Status != want {
					t.Fatalf("want %s: %v %+v", want, err, out)
				}
			}
		})
	}
}

// dbfixture seeds []string{"EUR", "US"} into a char(3)[] column as
// {EUR,"US "}: each element padded to the length. Compared through its text
// with the fixture's {EUR,US}, the row never matched its guard, and under the
// default policy every change to it was skipped as a changed row and the
// migration recorded as applied.
func TestACharArrayMatchesItsGuard(t *testing.T) {
	db := connect(t)
	reset := func() {
		run(t, db, "DROP TABLE IF EXISTS guard_regions",
			"CREATE TABLE guard_regions (id bigint PRIMARY KEY, name text UNIQUE NOT NULL, currencies char(3)[] NOT NULL, label text)",
			"INSERT INTO guard_regions VALUES (1, 'emea', '{EUR,US}', 'EMEA')")
	}
	currencies := func() string {
		return scan[string](t, db, "SELECT coalesce((SELECT currencies::text FROM guard_regions), 'gone')")
	}
	reset()
	update := fixturechange.Change{Kind: fixturechange.Update, Key: lit("name", "emea"),
		Old: lit("currencies", `["EUR","US"]`), New: lit("currencies", `["EUR","GBP"]`)}
	for _, want := range []fixtureapply.Status{fixtureapply.StatusApplied, fixtureapply.StatusUnchanged} {
		if out, err := guardOne(t, db, "guard_regions", strict, update); err != nil || out.Status != want {
			t.Fatalf("update, want %s: %v %+v", want, err, out)
		}
	}
	if got := currencies(); got != "{EUR,GBP}" {
		t.Fatalf("currencies %s", got)
	}

	// A real difference is still one.
	out, err := guardOne(t, db, "guard_regions", strict, fixturechange.Change{Kind: fixturechange.Update,
		Key: lit("name", "emea"), Old: lit("currencies", `["EUR","US"]`), New: lit("currencies", `["EUR"]`)})
	if err == nil || out.Problem != fixtureapply.ProblemChangedRow {
		t.Fatalf("{EUR,GBP} is not the old value: %v %+v", err, out)
	}

	reset()
	insert := fixturechange.Change{Kind: fixturechange.Insert, Key: lit("name", "emea"),
		New: fixturechange.Values{"id": fixturechange.Lit("1"), "name": fixturechange.Lit("emea"),
			"currencies": fixturechange.Lit(`["EUR","US"]`), "label": fixturechange.Lit("EMEA")}}
	if out, err := guardOne(t, db, "guard_regions", strict, insert); err != nil || out.Status != fixtureapply.StatusUnchanged {
		t.Fatalf("the row is there already: %v %+v", err, out)
	}
	remove := fixturechange.Change{Kind: fixturechange.Delete, Key: lit("name", "emea"),
		Old: fixturechange.Values{"name": fixturechange.Lit("emea"), "currencies": fixturechange.Lit(`["EUR","US"]`),
			"label": fixturechange.Lit("EMEA")}}
	for _, want := range []fixtureapply.Status{fixtureapply.StatusApplied, fixtureapply.StatusUnchanged} {
		if out, err := guardOne(t, db, "guard_regions", strict, remove); err != nil || out.Status != want {
			t.Fatalf("delete, want %s: %v %+v", want, err, out)
		}
	}
	if got := currencies(); got != "gone" {
		t.Fatalf("currencies %s", got)
	}
	// And inserted again, padded as dbfixture would have it.
	for _, want := range []fixtureapply.Status{fixtureapply.StatusApplied, fixtureapply.StatusUnchanged} {
		if out, err := guardOne(t, db, "guard_regions", strict, insert); err != nil || out.Status != want {
			t.Fatalf("insert, want %s: %v %+v", want, err, out)
		}
	}
	if got := currencies(); got != `{EUR,"US "}` {
		t.Fatalf("currencies %s", got)
	}
}

// A list an array column cannot hold. A NUL in an element was dropped without
// a word, so ["a\u0000b"] was stored as {ab}: Validate refuses it before any
// statement runs. An empty list inside a list, or lists nested deeper than
// PostgreSQL's six dimensions, failed with PostgreSQL's words at deploy time;
// they are refused with a sentence, and the set rolls back.
func TestAListAnArrayCannotHoldIsRefused(t *testing.T) {
	db := connect(t)
	run(t, db, "DROP TABLE IF EXISTS guard_lists",
		"CREATE TABLE guard_lists (id bigint PRIMARY KEY, name text UNIQUE NOT NULL, t text[], i int[])")
	for _, c := range []struct{ col, list, want string }{
		{"t", `["a\u0000b"]`, "the value holds a NUL character, which PostgreSQL cannot store"},
		{"i", `[[]]`, "empty list inside a list"},
		{"i", `[[[[[[[1]]]]]]]`, "PostgreSQL stores arrays of at most 6 dimensions"},
	} {
		set := fixturechange.Set{Name: "lists", Tables: fixturechange.Tables{"Row": {Name: "guard_lists", ID: "id", Key: "name"}},
			Changes: []fixturechange.Change{
				{Model: "Row", Kind: fixturechange.Insert, Key: lit("name", "first"),
					New: fixturechange.Values{"id": fixturechange.Lit("1"), "name": fixturechange.Lit("first")}},
				{Model: "Row", Kind: fixturechange.Insert, Key: lit("name", "second"),
					New: fixturechange.Values{"id": fixturechange.Lit("2"), "name": fixturechange.Lit("second"),
						c.col: fixturechange.Lit(c.list)}},
			}}
		err := fixtureapply.Apply(context.Background(), db, set, quiet())
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: want %q, got %v", c.list, c.want, err)
		}
		if n := scan[int64](t, db, "SELECT count(*) FROM guard_lists"); n != 0 {
			t.Fatalf("%s: %d rows written", c.list, n)
		}
	}
	// Six dimensions are fine.
	set := fixturechange.Set{Name: "lists", Tables: fixturechange.Tables{"Row": {Name: "guard_lists", ID: "id", Key: "name"}},
		Changes: []fixturechange.Change{{Model: "Row", Kind: fixturechange.Insert, Key: lit("name", "deep"),
			New: fixturechange.Values{"id": fixturechange.Lit("1"), "name": fixturechange.Lit("deep"),
				"i": fixturechange.Lit(`[[[[[[1]]]]]]`)}}}}
	if err := fixtureapply.Apply(context.Background(), db, set, quiet()); err != nil {
		t.Fatal(err)
	}
	if got := scan[string](t, db, "SELECT i::text FROM guard_lists"); got != "{{{{{{1}}}}}}" {
		t.Fatalf("stored %s", got)
	}
}
