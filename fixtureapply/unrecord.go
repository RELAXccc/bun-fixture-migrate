package fixtureapply

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/RELAXccc/bun-fixture-migrate/fixturechange"

	"github.com/uptrace/bun"
)

// bunMigrationFile is bun's own pattern for a migration file name
// (migrate/migrations.go, fnameRE): the digits are the name bun records.
var bunMigrationFile = regexp.MustCompile(`^(\d{1,14})_([0-9a-z_\-]+)\.`)

// migrationFromStack finds the migration file Apply was called from, the way
// bun's Register finds the file it was called from.
func migrationFromStack() string {
	var pcs [64]uintptr
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	var list []runtime.Frame
	for {
		f, more := frames.Next()
		list = append(list, f)
		if !more {
			return migrationFromFrames(list)
		}
	}
}

// migrationFromFrames is the first frame outside this package whose file is
// named like a migration, innermost first. It answers only when bun's migrator
// is further out, so Apply called from a test or a tool never goes looking for
// a record to take back, whatever the calling file is named.
func migrationFromFrames(frames []runtime.Frame) string {
	name := ""
	for _, f := range frames {
		if strings.Contains(f.Function, "/bun/migrate.") {
			return name
		}
		if name == "" && !strings.Contains(f.Function, "/fixtureapply.") {
			if m := bunMigrationFile.FindStringSubmatch(filepath.Base(f.File)); m != nil {
				name = m[1]
			}
		}
	}
	return ""
}

// unrecord deletes the record bun's migrator made of this migration before
// running it, and returns the failure with a note saying what it did.
func unrecord(ctx context.Context, db bun.IDB, set fixturechange.Set, o options, failure error) error {
	// Only the migrator's own connection pool. Inside somebody else's
	// transaction a failing statement would poison it, and the migrator never
	// hands a migration anything but the *bun.DB.
	bdb, ok := db.(*bun.DB)
	if !ok || o.migration == "" {
		return failure
	}
	table := set.MigrationsTable
	if table == "" {
		table = fixturechange.DefaultMigrationsTable
	}
	// bun puts the table name into its SQL as written, unquoted, so
	// PostgreSQL folds it to lower case; this has to find the same table.
	// Validate has made sure it is a plain identifier.
	if _, err := quoteIdent(table); err != nil {
		return failure
	}
	quoted := table
	// The migration may have failed because the context ended; the record
	// still has to go, or the next deploy skips this migration.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	var exists bool
	if err := bdb.QueryRowContext(ctx, "SELECT to_regclass(?) IS NOT NULL", quoted).Scan(&exists); err != nil {
		return fmt.Errorf("%w\n\nwhether bun recorded %s as applied could not be checked (%v): if %s holds a row "+
			"named %s, delete it, or the migration will not run again", failure, o.migration, err, table, o.migration)
	}
	if !exists {
		return failure
	}
	res, err := bdb.ExecContext(ctx, fmt.Sprintf(
		"DELETE FROM %s WHERE name = ? AND id = (SELECT max(id) FROM %s) "+
			"AND migrated_at > clock_timestamp() - interval '1 hour'", quoted, quoted), o.migration)
	if err != nil {
		return fmt.Errorf("%w\n\nbun may have recorded %s as applied before running it, and the record could not "+
			"be removed (%v): delete the row named %s from %s, or the migration will not run again",
			failure, o.migration, err, o.migration, table)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return failure
	}
	o.logf("%s: bun had recorded migration %s as applied before running it; the record was removed, "+
		"so it runs again once this is fixed", set.Name, o.migration)
	return fmt.Errorf("%w\n\nbun had recorded migration %s as applied before running it (the migrator was not "+
		"built WithMarkAppliedOnSuccess(true)); that record was removed, so the migration runs again once "+
		"this is fixed", failure, o.migration)
}
