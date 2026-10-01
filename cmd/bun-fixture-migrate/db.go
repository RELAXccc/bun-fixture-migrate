package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	fixturemigrate "github.com/RELAXccc/bun-fixture-migrate"
	"github.com/RELAXccc/bun-fixture-migrate/internal/pgerr"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// resolveDSN reads a DSN written as "env:NAME" from the environment, so the
// password stays out of the repository and out of the command line.
func resolveDSN(dsn string) (string, error) {
	if name, ok := strings.CutPrefix(dsn, "env:"); ok {
		if name == "" {
			return "", fmt.Errorf("the database DSN is \"env:\" with no variable after it; write env:NAME, " +
				"such as env:DATABASE_URL")
		}
		dsn = os.Getenv(name)
		if dsn == "" {
			return "", fmt.Errorf("the database DSN is to be read from the environment variable %s, which is not set", name)
		}
	}
	if dsn == "" {
		return "", fmt.Errorf("no database in the configuration file and no -dsn; this command needs one")
	}
	return dsn, nil
}

// openDB connects to PostgreSQL.
//
// pgdriver.WithDSN panics on a DSN it cannot read, with the DSN in the panic,
// and url.Parse puts the whole DSN, password and all, into its error. So the
// DSN is checked here first, a panic's text is never shown, and every message
// this function returns has the password masked, wherever the DSN put it: a
// typo in DATABASE_URL must end up as a sentence in a deploy log, not as a
// stack trace or a leaked password.
func openDB(ctx context.Context, dsn string) (*bun.DB, error) {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql" && u.Scheme != "unix") {
		return nil, errors.New("the database DSN is not a URL pgdriver can read; write it as " +
			"postgres://user:password@host:5432/dbname?sslmode=verify-full (a libpq \"host=... dbname=...\" " +
			"string is not supported)")
	}
	if u.Scheme == "unix" && u.Path == "" {
		return nil, fmt.Errorf("the database DSN %s is a unix socket DSN without the socket's path", redact(u))
	}
	connector, err := newConnector(dsn)
	if err != nil {
		return nil, fmt.Errorf("pgdriver cannot use the database DSN %s", redact(u))
	}
	db := bun.NewDB(sql.OpenDB(connector), pgdialect.New())
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to %s: %s", redact(u), scrub(err.Error(), u))
	}
	return db, nil
}

func newConnector(dsn string) (c *pgdriver.Connector, err error) {
	defer func() {
		if r := recover(); r != nil {
			// The panic's text repeats the DSN; it is dropped, not shown.
			err = errors.New("pgdriver refused the DSN")
		}
	}()
	// The application name goes first so that one in the DSN wins. It is what
	// a DBA sees in pg_stat_activity next to the locks a plan holds.
	return pgdriver.NewConnector(
		pgdriver.WithApplicationName("bun-fixture-migrate"),
		pgdriver.WithDSN(dsn),
	), nil
}

// secretParams are the query parameters of a DSN that hold a secret.
var secretParams = map[string]bool{"password": true, "sslpassword": true}

// redact is the DSN with every password masked: the one in the user info and
// any passed as a query parameter, which url.URL.Redacted leaves alone.
func redact(u *url.URL) string {
	c := *u
	if c.User != nil {
		if _, ok := c.User.Password(); ok {
			c.User = url.UserPassword(c.User.Username(), "xxxxx")
		}
	}
	q := c.Query()
	for k := range q {
		if secretParams[strings.ToLower(k)] {
			q.Set(k, "xxxxx")
		}
	}
	c.RawQuery = q.Encode()
	return c.String()
}

// scrub masks every password of the DSN in a message from the driver, in the
// spellings a message could carry it in.
func scrub(msg string, u *url.URL) string {
	var secrets []string
	if u.User != nil {
		if p, ok := u.User.Password(); ok {
			secrets = append(secrets, p)
		}
	}
	for k, vs := range u.Query() {
		if secretParams[strings.ToLower(k)] {
			secrets = append(secrets, vs...)
		}
	}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		for _, spelling := range []string{secret, url.QueryEscape(secret), url.PathEscape(secret)} {
			msg = strings.ReplaceAll(msg, spelling, "xxxxx")
		}
	}
	return msg
}

// readOnly runs fn inside one REPEATABLE READ, READ ONLY transaction, and
// rolls it back.
//
// READ ONLY is PostgreSQL's promise, not this tool's: export, check, status
// and scaffold write nothing even when a model's where clause calls a function
// that would. REPEATABLE READ makes every query in fn see the same snapshot,
// so a row inserted between reading two tables cannot turn up as a reference
// to a row that is not there.
//
// row_security is off, so a role a row-level security policy limits gets an
// error rather than the rows the policy lets through. Read through such a
// policy, master data is missing rows nothing says are missing: an export
// writes a file without them, and a migration generated from it deletes them
// everywhere else.
func readOnly(ctx context.Context, db *bun.DB, fn func(tx bun.Tx) error) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var readOnly string
	if err := tx.QueryRowContext(ctx, "SHOW transaction_read_only").Scan(&readOnly); err != nil {
		return err
	}
	if readOnly != "on" {
		return errors.New("the database did not start a read-only transaction; refusing to go on")
	}
	if err := fixturemigrate.PrepareSession(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "SELECT set_config('row_security', 'off', true)"); err != nil {
		return err
	}
	return rowSecurity(fn(tx))
}

// rlsTable is the table PostgreSQL names when row_security is off and a
// policy would filter a query.
var rlsTable = regexp.MustCompile(`row-level security policy for table "(.*)"`)

// rowSecurity turns PostgreSQL's refusal to read past a row-level security
// policy into what to do about it.
func rowSecurity(err error) error {
	if pgerr.State(err) != pgerr.InsufficientPrivilege {
		return err
	}
	m := rlsTable.FindStringSubmatch(pgerr.Message(err))
	if m == nil {
		return err
	}
	return fmt.Errorf("row-level security hides rows of %s from this role, so what it reads is not all the "+
		"master data; connect as a role that bypasses row-level security (BYPASSRLS) or owns the table "+
		"without FORCE ROW LEVEL SECURITY", m[1])
}
