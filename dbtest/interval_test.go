package dbtest_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/dbschema"

	"github.com/uptrace/bun"
)

type IvPlan struct {
	bun.BaseModel `bun:"table:iv_plans"`
	ID            int64  `bun:"id,pk"`
	Name          string `bun:"name,notnull"`
	Trial         string `bun:"trial,type:interval,notnull"`
	Label         string `bun:"label,notnull"`
}

// Spellings of intervals, each as PostgreSQL reads it the same in every
// version this tool runs against.
var ivSpellings = []string{"1 day", "1 days", "24 hours", "86400 seconds", "24:00:00", "PT24H", "P1D", "1 day 2 hours",
	"1 day 02:00:00", "1 2:00:00", "26 hours", "-1 day 2 hours", "-1 days +02:00:00", "1 day ago", "-1 days",
	"@ 1 day", "1 year 2 months", "1-2", "14 mons", "1 mon", "30 days", "1.5 days", "1 day 12:00:00", "36 hours",
	"1.5 hours", "90 minutes", "1:30", "01:30:00", "5400 seconds", "1500 ms", "00:00:01.5", "1 week", "7 days",
	"P1W", "2 weeks 1 day", "15 days", "1 decade", "10 years", "PT1.5H", "1 hour 30 minutes", "0", "0 days",
	"00:00:00", "1day", "1 DAY", "60 seconds", "00:01:00", "1 min"}

// The same change is the same verdict without the database and with it: a
// respelling of one interval, '86400 seconds' to '24:00:00', is no change to
// an interval column, which the database says, and is refused without it,
// where the column could be text; two intervals, '1 day' and '24 hours', are
// a change both ways. Before, every respelling was an update offline and no
// change with the database. Every pair of the spellings is tried.
func TestAnIntervalRespelledWithAndWithoutTheDatabase(t *testing.T) {
	l := newLab(t, map[string]*fixturemigrate.Model{"IvPlan": {Table: "iv_plans"}}, "iv_plans",
		[]string{"DROP TABLE IF EXISTS iv_plans",
			"CREATE TABLE iv_plans (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, trial interval NOT NULL, label text NOT NULL)"},
		`SELECT string_agg(concat_ws('|', name, trial, label), E'\n' ORDER BY name) FROM iv_plans`, (*IvPlan)(nil))
	file := func(trial, label string) string {
		return fmt.Sprintf("- model: IvPlan\n  rows:\n    - {id: 1, name: team, trial: '%s', label: '%s'}\n", trial, label)
	}
	// Each spelling read once without the database and once with it, and
	// as PostgreSQL writes it.
	ctx := context.Background()
	raw, canon, text := map[string]*fixturemigrate.Snapshot{}, map[string]*fixturemigrate.Snapshot{}, map[string]string{}
	readOnlyDo(t, l.db, func(tx bun.Tx, tables map[string]*dbschema.Table) {
		for _, a := range ivSpellings {
			raw[a] = fixtureSnapshot(t, l.cfg, file(a, "x"), "old")
			canon[a] = fixtureSnapshot(t, l.cfg, file(a, "x"), "new")
			if err := fixturemigrate.Canonicalize(ctx, tx, l.cfg, canon[a], tables); err != nil {
				t.Fatal(err)
			}
			var s string
			if err := tx.QueryRowContext(ctx, "SELECT ?::interval::text", a).Scan(&s); err != nil {
				t.Fatal(err)
			}
			text[a] = s
		}
	})
	var pairs, respelled int
	for _, a := range ivSpellings {
		for _, b := range ivSpellings {
			if a == b {
				continue
			}
			pairs++
			offline, err := fixturemigrate.Compute(l.cfg, raw[a], raw[b])
			if err != nil {
				t.Fatal(err)
			}
			online, err := fixturemigrate.Compute(l.cfg, canon[a], canon[b])
			if err != nil {
				t.Fatal(err)
			}
			if len(online.Refusals) != 0 {
				t.Fatalf("%q to %q with the database: %+v", a, b, online.Refusals)
			}
			if text[a] == text[b] {
				respelled++
				if len(online.Changes) != 0 || len(offline.Changes) != 0 || len(offline.Refusals) != 1 ||
					!strings.Contains(offline.Refusals[0].Reason, "which an interval column holds as one value") {
					t.Errorf("%q to %q is one interval: with the database %d changes, without %d and %+v",
						a, b, len(online.Changes), len(offline.Changes), offline.Refusals)
				}
				continue
			}
			if len(online.Changes) != 1 || len(offline.Changes) != 1 || len(offline.Refusals) != 0 {
				t.Errorf("%q to %q are two intervals: with the database %d changes, without %d and %+v",
					a, b, len(online.Changes), len(offline.Changes), offline.Refusals)
			}
		}
	}
	if respelled < 50 {
		t.Fatalf("only %d of %d pairs are one interval", respelled, pairs)
	}

	// In a text column a respelling is a change, which the database says;
	// without it, it is refused, never an update the database would not
	// write or no change where it would.
	res := ucDiff(t, l, file("1 day", "86400 seconds"), file("1 day", "24:00:00"))
	if len(res.Changes) != 1 || len(res.Refusals) != 0 {
		t.Fatalf("a text column: %+v %+v", res.Changes, res.Refusals)
	}

	// '1 day' to '24 hours' is a change, which a migration and a sync write
	// as a fresh seed of the new file stores it: the run time compares an
	// interval through its text, where = holds the two equal.
	l.fidelity(file("1 day", "x"), file("24 hours", "x"))
	if got := l.current(); got != "team|24:00:00|x" {
		t.Fatalf("got %s", got)
	}
}

// The command: status without the database refuses to call a respelled
// interval a change or no change and says why, status with it notes a
// respelling, generate records the new spelling without a migration, and
// status -offline agrees afterwards.
func TestTheCommandOnARespelledInterval(t *testing.T) {
	a := newAdoption(t)
	run(t, a.db, "CREATE TABLE trials (id bigint PRIMARY KEY, name text NOT NULL UNIQUE, length interval NOT NULL)",
		"INSERT INTO trials VALUES (1, 'team', '86400 seconds')")
	if err := os.MkdirAll(filepath.Join(a.dir, "fixtures"), 0o755); err != nil {
		t.Fatal(err)
	}
	a.write("fixture-migrate.yml", "fixture: fixtures/fixture.yml\nout: migrations\npackage: migrations\n"+
		"seed_guard_table: trials\ndatabase: env:DATABASE_URL\nmodels:\n  Trial:\n    table: trials\n")
	a.write("fixtures/fixture.yml", "- model: Trial\n  rows:\n    - {id: 1, name: team, length: '86400 seconds'}\n")
	a.run(0, "baseline")
	a.write("fixtures/fixture.yml", "- model: Trial\n  rows:\n    - {id: 1, name: team, length: '24:00:00'}\n")
	if out := a.run(3, "status", "-offline"); !strings.Contains(out, "length is written 86400 seconds before and "+
		"24:00:00 after, which an interval column holds as one value") {
		t.Fatal(out)
	}
	a.run(2, "generate", "-name", "trial", "-no-lint")
	if out := a.run(0, "status"); !strings.Contains(out, "only in how values are written") {
		t.Fatal(out)
	}
	a.run(0, "check")
	if out := a.run(0, "generate", "-name", "trial"); !strings.Contains(out, "nothing changed") {
		t.Fatal(out)
	}
	a.run(0, "status", "-offline")
}
