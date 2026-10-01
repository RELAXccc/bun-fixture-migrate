package main

import (
	"bufio"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/migrate"
)

// recorder is a database/sql driver that keeps every statement it is sent and
// runs none, so bun's own reading of a SQL migration can be watched without a
// database.
type recorder struct{ queries []string }

func (r *recorder) Connect(context.Context) (driver.Conn, error) { return recorderConn{r}, nil }
func (r *recorder) Driver() driver.Driver                        { return recorderDriver{r} }

type recorderDriver struct{ r *recorder }

func (d recorderDriver) Open(string) (driver.Conn, error) { return recorderConn{d.r}, nil }

type recorderConn struct{ r *recorder }

func (recorderConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (recorderConn) Close() error                        { return nil }
func (recorderConn) Begin() (driver.Tx, error)           { return recorderTx{}, nil }
func (c recorderConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.r.queries = append(c.r.queries, query)
	return driver.RowsAffected(0), nil
}

type recorderTx struct{}

func (recorderTx) Commit() error   { return nil }
func (recorderTx) Rollback() error { return nil }

// bunRuns is what bun's migrator sends to the database for a SQL migration
// file, through bun's own Discover and up function.
func bunRuns(t testing.TB, name string, data []byte) ([]string, error) {
	t.Helper()
	rec := &recorder{}
	db := bun.NewDB(sql.OpenDB(rec), pgdialect.New())
	defer db.Close()
	ms := migrate.NewMigrations()
	if err := ms.Discover(fstest.MapFS{name: {Data: data}}); err != nil {
		t.Fatal(err)
	}
	mig := ms.Sorted()[0]
	err := mig.Up(context.Background(), migrate.NewMigrator(db, ms), &mig)
	return rec.queries, err
}

// planRuns is what plan sends for the same file: the statements readSQL finds,
// less those of nothing but white space, which bun skips too.
func planRuns(data []byte, dropBlankLines bool) ([]string, error) {
	queries, err := readSQL(data)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, q := range queries {
		if dropBlankLines {
			var kept []string
			for _, line := range strings.SplitAfter(q, "\n") {
				if strings.TrimSpace(line) != "" {
					kept = append(kept, line)
				}
			}
			q = strings.Join(kept, "")
		}
		if strings.TrimSpace(q) != "" {
			out = append(out, q)
		}
	}
	return out, nil
}

// bunVersion is the version of bun this test is built with.
func bunVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, d := range info.Deps {
		if d.Path == "github.com/uptrace/bun" {
			if d.Replace != nil {
				return d.Replace.Version
			}
			return d.Version
		}
	}
	return ""
}

// sameAsBun fails unless plan sends what bun sends. Against a bun other than
// v1.2.18, whose master branch drops blank lines, plan may differ by the
// blank lines alone, which is what its note about them says.
func sameAsBun(t *testing.T, label string, data []byte) {
	t.Helper()
	pinned := bunVersion() == "v1.2.18"
	want, wantErr := bunRuns(t, "20260101000000_x.tx.up.sql", data)
	got, gotErr := planRuns(data, !pinned)
	if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
		t.Fatalf("%s: plan reads it with error %v, bun %s with %v", label, gotErr, bunVersion(), wantErr)
	}
	if fmt.Sprintf("%q", got) != fmt.Sprintf("%q", want) {
		t.Fatalf("%s: plan runs\n%q\nbun %s runs\n%q", label, got, bunVersion(), want)
	}
}

func TestReadSQLReadsLikeBun(t *testing.T) {
	long := func(n int) string { return "SELECT '" + strings.Repeat("x", n-len("SELECT '';")) + "';" }
	for label, text := range map[string]string{
		"split":                     "ALTER TABLE a ADD b int;\n\n--bun:split\r\nUPDATE a SET b = 1;\n  \n--bun:split\n",
		"split first":               "--bun:split\nSELECT 1;\n",
		"no newline at the end":     "SELECT 1;\n--bun:split\nSELECT 2;",
		"blank line in a literal":   "INSERT INTO t VALUES ('para one\n\npara two');\n",
		"blank lines around":        "\n\nSELECT 1;\n\n\n",
		"carriage returns":          "SELECT 1;\r\n\r\nSELECT 'a\rb';\r\n",
		"unknown directive":         "SELECT 1;\n--bun:nope\nSELECT 2;\n",
		"directive with a space":    "--bun:split \nSELECT 1;\n",
		"indented directive":        "  --bun:split\nSELECT 1;\n",
		"empty":                     "",
		"line of 64 KiB less one":   long(64*1024-1) + "\n",
		"line of 64 KiB":            long(64*1024) + "\n",
		"line of 64 KiB at the end": long(64 * 1024),
		"line longer than 64 KiB":   "SELECT 1;\n" + long(70*1024) + "\nSELECT 2;\n",
		"directive before the long": "--bun:what\n" + long(70*1024) + "\n",
	} {
		sameAsBun(t, label, []byte(text))
	}
	if _, err := readSQL([]byte(long(70*1024) + "\n")); !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("a line over 64 KiB: %v", err)
	}
}

func FuzzReadSQLLikeBun(f *testing.F) {
	for _, seed := range []string{
		"SELECT 1;\n--bun:split\nSELECT 2;\n",
		"INSERT INTO t VALUES ('a\n\nb');\r\n--bun:split\n\n",
		"--bun:split\n--bun:split\n",
		"--bun:x\n",
		"\r\n\r\n",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		sameAsBun(t, "fuzz", data)
	})
}

// Under bun v1.2.18 a blank line inside a literal is part of the value, and
// bun after it drops blank lines; plan notes where that changes a literal and
// nowhere else.
func TestBlankLineInLiteral(t *testing.T) {
	for query, want := range map[string]bool{
		"INSERT INTO t VALUES ('para one\n\npara two');\n":                                    true,
		"INSERT INTO t VALUES ('para one\n  \t\npara two');\n":                                true,
		"SELECT 1;\n\nSELECT 2;\n":                                                            false,
		"/* a\n\nb */ SELECT 1;\n":                                                            false,
		"/* a /* nested */\n\n*/ SELECT 1;\n":                                                 false,
		"CREATE FUNCTION f() RETURNS int AS $$\nBEGIN\n\n  RETURN 1;\nEND $$ LANGUAGE sql;\n": true,
		"CREATE FUNCTION f() RETURNS int AS $body$\nSELECT '$$';\n\n$body$ LANGUAGE sql;\n":   true,
		"SELECT $body$ x $body$;\n\nSELECT 2;\n":                                              false,
		"PREPARE p AS SELECT $1;\n\nSELECT 2;\n":                                              false,
		"SELECT 'it''s';\n\nSELECT 2;\n":                                                      false,
		"SELECT E'a\\'b';\n\nSELECT 2;\n":                                                     false,
		"SELECT 'a\\';\n\nSELECT 2;\n":                                                        false,
		"-- it's\n\nSELECT 1;\n":                                                              false,
		"SELECT \"a\n\nb\" FROM t;\n":                                                         true,
		"SELECT a$b$c FROM t;\n\nSELECT 2;\n":                                                 false,
		"SELECT 'a'\n\n'b';\n":                                                                false,
		"SELECT 'unterminated;\n":                                                             false,
	} {
		if got := blankLineInLiteral(query); got != want {
			t.Errorf("%q: %v, want %v", query, got, want)
		}
	}
}

// sqlState is a PostgreSQL error as a driver returns one.
type sqlState string

func (s sqlState) Error() string    { return "ERROR: something (SQLSTATE=" + string(s) + ")" }
func (s sqlState) SQLState() string { return string(s) }

// What an error means for the deploy: some come from the plan's one rolled
// back transaction and say nothing about it.
func TestJudge(t *testing.T) {
	for _, tc := range []struct {
		err    error
		result string
		note   string
	}{
		{fmt.Errorf("x: %w", sqlState("23503")), "fails", ""},
		{errors.New("no row of items has name=anvil"), "fails", ""},
		{bufio.ErrTooLong, "fails", "64 KiB"},
		{fmt.Errorf("x: %w", sqlState("55P04")), "inconclusive", "enum value"},
		{sqlState("25001"), "inconclusive", "inside a transaction"},
		{sqlState("55P03"), "inconclusive", ""},
		{sqlState("08006"), "inconclusive", ""},
		{context.Canceled, "inconclusive", ""},
	} {
		result, note := judge(tc.err)
		if result != tc.result || (tc.note == "") != (note == "") || !strings.Contains(note, tc.note) {
			t.Errorf("%v: %s %q, want %s with %q", tc.err, result, note, tc.result, tc.note)
		}
	}
}
