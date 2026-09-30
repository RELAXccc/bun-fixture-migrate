package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// resolveDSN reads a DSN written as "env:NAME" from the environment, so the
// password stays out of the repository.
func resolveDSN(dsn string) (string, error) {
	if name, ok := strings.CutPrefix(dsn, "env:"); ok {
		dsn = os.Getenv(name)
		if dsn == "" {
			return "", fmt.Errorf("the configuration reads the database DSN from %s, which is not set", name)
		}
	}
	if dsn == "" {
		return "", fmt.Errorf("no database in the configuration file; this command needs one")
	}
	return dsn, nil
}

// openDB connects to PostgreSQL.
//
// pgdriver.WithDSN panics on a DSN it cannot read, and url.Parse puts the
// whole DSN, password and all, into its error. So the DSN is checked here
// first, and no message this function returns contains it: a typo in
// DATABASE_URL must end up as a sentence in a deploy log, not as a stack
// trace or a leaked password.
func openDB(ctx context.Context, dsn string) (*bun.DB, error) {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql" && u.Scheme != "unix") {
		return nil, errors.New("the database DSN is not a URL pgdriver can read; write it as " +
			"postgres://user:password@host:5432/dbname?sslmode=verify-full (a libpq \"host=... dbname=...\" " +
			"string is not supported)")
	}
	connector, err := newConnector(dsn)
	if err != nil {
		return nil, fmt.Errorf("the database DSN %s cannot be used: %v", u.Redacted(), err)
	}
	db := bun.NewDB(sql.OpenDB(connector), pgdialect.New())
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to %s: %w", u.Redacted(), err)
	}
	return db, nil
}

func newConnector(dsn string) (c *pgdriver.Connector, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	// The application name goes first so that one in the DSN wins. It is what
	// a DBA sees in pg_stat_activity next to the locks a plan holds.
	return pgdriver.NewConnector(
		pgdriver.WithApplicationName("bun-fixture-migrate"),
		pgdriver.WithDSN(dsn),
	), nil
}

// readOnly runs fn inside one REPEATABLE READ, READ ONLY transaction, and
// rolls it back.
//
// READ ONLY is PostgreSQL's promise, not this tool's: export, check, status
// and scaffold write nothing even when a model's where clause calls a function
// that would. REPEATABLE READ makes every query in fn see the same snapshot,
// so a row inserted between reading two tables cannot turn up as a reference
// to a row that is not there.
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
	return fn(tx)
}
