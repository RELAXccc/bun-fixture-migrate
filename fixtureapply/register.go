package fixtureapply

import (
	"context"
	"path/filepath"
	"runtime"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

// Up is the up function a generated migration registers with bun's migrator:
//
//	Migrations.MustRegister(fixtureapply.Up(set), fixtureapply.Down(set))
//
// It runs Apply with opts. It has to be called in the migration's own file, in
// the call to MustRegister, as above: that is the file bun derives the
// migration's name from, and Up reads the same name from the same file while
// the package initialises. Knowing it from the start, Apply never has to find
// it on the call stack when the migration fails, which would stop working the
// day bun's migrator moved to another package path or took names from
// somewhere else than the file. The functions files of earlier versions
// register, calling Apply and Revert themselves, still find it there.
func Up(set fixturechange.Set, opts ...Option) migrate.MigrationFunc {
	name := registeringMigration()
	return func(ctx context.Context, db *bun.DB) error {
		return Apply(ctx, db, set, append([]Option{WithMigrationName(name)}, opts...)...)
	}
}

// Down is the down function a generated migration registers next to Up. It
// runs Revert with opts.
func Down(set fixturechange.Set, opts ...Option) migrate.MigrationFunc {
	return func(ctx context.Context, db *bun.DB) error {
		return Revert(ctx, db, set, opts...)
	}
}

// registeringMigration is the name bun records for the migration whose file
// called Up: the digits its file name starts with, as bun's Register reads
// them. "" when the caller's file is not named like a migration, which bun's
// Register refuses anyway.
func registeringMigration() string {
	_, file, _, ok := runtime.Caller(2)
	if !ok {
		return ""
	}
	if m := bunMigrationFile.FindStringSubmatch(filepath.Base(file)); m != nil {
		return m[1]
	}
	return ""
}
