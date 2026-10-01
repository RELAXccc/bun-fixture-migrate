package fixturemigrate

import (
	"strings"
	"testing"

	"github.com/RELAXccc/bun-fixture-migrate/dbschema"
)

// What PostgreSQL 16 makes of interval text under IntervalStyle postgres, as
// SELECT '...'::interval::text says: intervalText has to say the same, or
// that it cannot tell ("?"), never something else. "" is text PostgreSQL
// refuses. dbtest holds the reading against the server it runs on.
func TestIntervalTextIsPostgreSQLs(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"1 day", "1 day"},
		{"1 days", "1 day"},
		{"24 hours", "24:00:00"},
		{"86400 seconds", "24:00:00"},
		{"24:00:00", "24:00:00"},
		{"PT24H", "24:00:00"},
		{"P1D", "1 day"},
		{"1 day 2 hours", "1 day 02:00:00"},
		{"1 day 02:00:00", "1 day 02:00:00"},
		{"1 2:03:04", "1 day 02:03:04"},
		{"-1 day 2 hours", "-1 days +02:00:00"},
		{"-1 day -2 hours", "-1 days -02:00:00"},
		{"1 day ago", "-1 days"},
		{"1 day 2 hours ago", "-1 days -02:00:00"},
		{"-1 day ago", "1 day"},
		{"@ 1 day", "1 day"},
		{"1 year 2 months", "1 year 2 mons"},
		{"1-2", "1 year 2 mons"},
		{"-1-2", "-1 years -2 mons"},
		{"+1-2", "1 year 2 mons"},
		{"14 mons", "1 year 2 mons"},
		{"1 mon", "1 mon"},
		{"1 month", "1 mon"},
		{"2 months", "2 mons"},
		{"1.5 days", "1 day 12:00:00"},
		{"1.5 hours", "01:30:00"},
		{"90 minutes", "01:30:00"},
		{"1:30", "01:30:00"},
		{"1:30:00", "01:30:00"},
		{"1:30.5", "00:01:30.5"},
		{"00:00:01.5", "00:00:01.5"},
		{"1.5 seconds", "00:00:01.5"},
		{"1500 ms", "00:00:01.5"},
		{"1500000 us", "00:00:01.5"},
		{"0.5 ms", "00:00:00.0005"},
		{"0.0000005 s", "?"},
		{"1.5 weeks", "?"},
		{"1 week", "7 days"},
		{"7 days", "7 days"},
		{"2 weeks 1 day", "15 days"},
		{"1 decade", "10 years"},
		{"10 years", "10 years"},
		{"1 century", "100 years"},
		{"1 millennium", "1000 years"},
		{"1 y 2 mon 3 d 4 h 5 m 6 s", "1 year 2 mons 3 days 04:05:06"},
		{"1 yr 2 mons 3 days", "1 year 2 mons 3 days"},
		{"1 day 1 day", ""},
		{"1 hour 01:00", ""},
		{"1 2", ""},
		{"1 day hour", "?"},
		{"day", ""},
		{"ago", ""},
		{"1 ago", ""},
		{"01:00 ago", "-01:00:00"},
		{"-01:30", "-01:30:00"},
		{"+01:30", "01:30:00"},
		{"1 day -01:30", "1 day -01:30:00"},
		{"-1 day +01:30", "-1 days +01:30:00"},
		{"1 day, 2 hours", "1 day 02:00:00"},
		{"1day", "1 day"},
		{"5min", "00:05:00"},
		{"3 microseconds", "00:00:00.000003"},
		{"3 milliseconds", "00:00:00.003"},
		{"3 microsecondsx", "00:00:00.000003"},
		{"P1Y2M3DT4H5M6S", "1 year 2 mons 3 days 04:05:06"},
		{"P1W", "7 days"},
		{"PT1M", "00:01:00"},
		{"P1M", "1 mon"},
		{"PT1.5H", "01:30:00"},
		{"P0.5D", "12:00:00"},
		{"P-1D", "-1 days"},
		{"PT-1H", "-01:00:00"},
		{"p1d", ""},
		{"P1DT", "1 day"},
		{"P1Y1Y", "2 years"},
		{"PT", "?"},
		{"P", ""},
		{"P1.5Y", "?"},
		{"1 day 25:00:00", "1 day 25:00:00"},
		{"25:00:00", "25:00:00"},
		{"100:00:00", "100:00:00"},
		{"0", "00:00:00"},
		{"0 days", "00:00:00"},
		{"00:00:00", "00:00:00"},
		{"-0 days", "00:00:00"},
		{"1.0 week", "7 days"},
		{"1.0 days", "1 day"},
		{"1 day 60 seconds", "1 day 00:01:00"},
		{"00:00:60", "00:01:00"},
		{"00:60:00", ""},
		{"1.", "?"},
		{".5 days", "12:00:00"},
		{".5", "00:00:00.5"},
		{"1:2", "01:02:00"},
		{"1:02:03.1234567", "?"},
		{"1:02:03.1234560", "01:02:03.123456"},
		{"1 hour 30 minutes 10 seconds", "01:30:10"},
		{"2 hours 3 minutes", "02:03:00"},
		{"1 week 1 day 1 hour", "8 days 01:00:00"},
		{"3 days 1 hour ago", "-3 days -01:00:00"},
		{"1 year -1 mons", "11 mons"},
		{"-1 year 1 mon", "-11 mons"},
		{"-1 years -2 mons", "-1 years -2 mons"},
		{"-3 days +04:05:06", "-3 days +04:05:06"},
		{"1 mon -1 day", "1 mon -1 days"},
		{"1 Day", "1 day"},
		{"1 DAYS", "1 day"},
		{"1 HOURS", "01:00:00"},
	} {
		got, ok := intervalText(tc.in)
		switch {
		case tc.want == "?" && ok:
			t.Errorf("%q: %q, where PostgreSQL works in floating point or a version decides", tc.in, got)
		case tc.want == "" && ok:
			t.Errorf("%q: %q, which PostgreSQL refuses", tc.in, got)
		case tc.want != "?" && tc.want != "" && (!ok || got != tc.want):
			t.Errorf("%q: %q %v, want %q", tc.in, got, ok, tc.want)
		}
	}
}

// Without the catalog, a value respelled as another spelling of one interval
// is refused, as 1.10 respelled as 1.1 is: an interval column holds one
// value, and the database run finds no change, while a text column holds
// two. Before, it was an update offline. Two intervals are a change, two
// plain numbers are text as before, and with the catalog read the values
// are the database's and nothing is refused.
func TestARespelledIntervalWithoutTheCatalog(t *testing.T) {
	cfg := &Config{Models: map[string]*Model{"Plan": {Table: "plans"}}}
	if err := cfg.Prepare(); err != nil {
		t.Fatal(err)
	}
	file := func(trial string) string {
		return "- model: Plan\n  rows:\n    - {id: 1, name: team, trial: '" + trial + "', seats: 3}\n"
	}
	res := computeWith(t, cfg, file("86400 seconds"), file("24:00:00"))
	if len(res.Changes) != 0 || len(res.Refusals) != 1 || !strings.Contains(res.Refusals[0].String(),
		"Plan/name=team: trial is written 86400 seconds before and 24:00:00 after, which an interval column holds "+
			"as one value, 24:00:00, and any other column as two: only the column's type says") {
		t.Fatalf("got %+v / %+v", res.Changes, res.Refusals)
	}
	for _, pair := range [][2]string{{"1 day", "24 hours"}, {"01", "001"}, {"1 day", "2 days"}} {
		if res := computeWith(t, cfg, file(pair[0]), file(pair[1])); len(res.Changes) != 1 || len(res.Refusals) != 0 {
			t.Errorf("%q to %q: %+v / %+v", pair[0], pair[1], res.Changes, res.Refusals)
		}
	}
	// With the catalog read, the column's type has decided: a text column
	// holding the two spellings changes.
	old, next := snap(t, cfg, file("86400 seconds"), "old"), snap(t, cfg, file("24:00:00"), "new")
	table := &dbschema.Table{Columns: []dbschema.Column{{Name: "id", Type: "int8"}, {Name: "name", Type: "text"},
		{Name: "trial", Type: "text"}, {Name: "seats", Type: "int8"}}}
	old.noteUniques("Plan", table)
	next.noteUniques("Plan", table)
	if res, err := Compute(cfg, old, next); err != nil || len(res.Changes) != 1 || len(res.Refusals) != 0 {
		t.Fatalf("with the catalog: %v %+v / %+v", err, res.Changes, res.Refusals)
	}
}
