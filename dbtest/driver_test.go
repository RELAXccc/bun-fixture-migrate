package dbtest_test

import (
	"database/sql"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// openDB opens the test database through the driver
// BUN_FIXTURE_MIGRATE_DRIVER names: pgdriver, bun's own, by default, or pgx,
// which many bun applications use instead. The run time lives in the
// application's process, so it has to work under both; CI runs the whole
// suite once with each. params are run-time parameters for every connection.
func openDB(t *testing.T, dsn string, params map[string]string) *bun.DB {
	t.Helper()
	switch driver := os.Getenv("BUN_FIXTURE_MIGRATE_DRIVER"); driver {
	case "", "pgdriver":
		opts := []pgdriver.Option{pgdriver.WithDSN(dsn)}
		if len(params) > 0 {
			conn := map[string]any{}
			for k, v := range params {
				conn[k] = v
			}
			opts = append(opts, pgdriver.WithConnParams(conn))
		}
		return bun.NewDB(sql.OpenDB(pgdriver.NewConnector(opts...)), pgdialect.New())
	case "pgx":
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range params {
			cfg.RuntimeParams[k] = v
		}
		return bun.NewDB(stdlib.OpenDB(*cfg), pgdialect.New())
	default:
		t.Fatalf("BUN_FIXTURE_MIGRATE_DRIVER is %q, which is neither pgdriver nor pgx", driver)
		return nil
	}
}
