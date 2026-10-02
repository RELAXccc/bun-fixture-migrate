// Package migrations is the example SaaS application's bun migrations package:
// the SQL migrations that shape the schema, and the fixture migrations
// bun-fixture-migrate generates next to them. Both register with Migrations
// and run in name order.
package migrations

import (
	"embed"

	"github.com/uptrace/bun/migrate"
)

// Migrations is what every migration in this directory registers with.
var Migrations = migrate.NewMigrations()

//go:embed *.sql
var sqlMigrations embed.FS

func init() {
	if err := Migrations.Discover(sqlMigrations); err != nil {
		panic(err)
	}
}
