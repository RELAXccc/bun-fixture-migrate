package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"

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

// openDB connects to PostgreSQL, and fails at once when it cannot.
func openDB(ctx context.Context, dsn string) (*bun.DB, error) {
	u, connector, err := connectorFor(dsn)
	if err != nil {
		return nil, err
	}
	db := bun.NewDB(sql.OpenDB(connector), pgdialect.New())
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to %s: %s", redact(u), scrub(err.Error(), u))
	}
	return db, nil
}

// connectorFor is the connector of a DSN.
//
// pgdriver.WithDSN panics on a DSN it cannot read, with the DSN in the panic,
// and url.Parse puts the whole DSN, password and all, into its error. So the
// DSN is checked here first, a panic's text is never shown, and every message
// this function returns has the password masked, wherever the DSN put it: a
// typo in DATABASE_URL must end up as a sentence in a deploy log, not as a
// stack trace or a leaked password.
func connectorFor(dsn string) (*url.URL, *pgdriver.Connector, error) {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql" && u.Scheme != "unix") {
		return nil, nil, errors.New("the database DSN is not a URL pgdriver can read; write it as " +
			"postgres://user:password@host:5432/dbname?sslmode=verify-full (a libpq \"host=... dbname=...\" " +
			"string is not supported)")
	}
	if u.Scheme == "unix" && u.Path == "" {
		return nil, nil, fmt.Errorf("the database DSN %s is a unix socket DSN without the socket's path", redact(u))
	}
	connector, err := newConnector(dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("pgdriver cannot use the database DSN %s", redact(u))
	}
	return u, connector, nil
}

// lazyDB is the configured database, connected the first time a query needs
// it. A command hands it to the library, which reads the fixture files and
// the state before it asks the database anything, so what is wrong with them
// is said before a database that cannot be reached; and baseline, which asks
// the database only about a difference in spelling, does not connect without
// one. Whatever stops the connection, an unset variable or an unreachable
// host, is the error of that first query, as a connectError.
func lazyDB(dsn string) *bun.DB {
	return bun.NewDB(sql.OpenDB(&lazyConnector{dsn: dsn}), pgdialect.New())
}

// connectError is a database the command could not connect to.
type connectError struct{ err error }

func (e connectError) Error() string { return e.err.Error() }
func (e connectError) Unwrap() error { return e.err }

type lazyConnector struct {
	dsn       string
	once      sync.Once
	u         *url.URL
	connector *pgdriver.Connector
	err       error
}

func (c *lazyConnector) Connect(ctx context.Context) (driver.Conn, error) {
	c.once.Do(func() {
		dsn, err := resolveDSN(c.dsn)
		if err == nil {
			c.u, c.connector, err = connectorFor(dsn)
		}
		c.err = err
	})
	if c.err != nil {
		return nil, connectError{c.err}
	}
	conn, err := c.connector.Connect(ctx)
	if err != nil {
		return nil, connectError{fmt.Errorf("connect to %s: %s", redact(c.u), scrub(err.Error(), c.u))}
	}
	return conn, nil
}

func (c *lazyConnector) Driver() driver.Driver { return pgdriver.NewDriver() }

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
