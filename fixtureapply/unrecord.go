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

// record is what Apply found of bun's record of the migration before running
// it.
type record struct {
	// id is the row bun's migrator inserted just before calling the
	// migration, 0 when there is none.
	id int64
	// err is why it could not be looked for.
	err error
}

// findRecord looks, before the change set runs, for the record bun's migrator
// made of this run of the migration: the newest row of the migrations table,
// if it carries this migration's name and was written in the last minute. bun
// inserts it a moment before calling the migration unless it was built
// WithMarkAppliedOnSuccess(true).
//
// Reading it first is what makes taking it back safe. Read after a failure,
// the newest row could be one another replica wrote while this one waited for
// the advisory lock: its record of the same migration, made after it applied
// the change set. Deleting that would leave the migration pending with its
// changes made, and every later start running it again.
func findRecord(ctx context.Context, db bun.IDB, set fixturechange.Set, o options) record {
	// Only the migrator's own connection pool. Inside somebody else's
	// transaction a failing statement would poison it, and the migrator never
	// hands a migration anything but the *bun.DB.
	bdb, ok := db.(*bun.DB)
	if !ok || o.migration == "" {
		return record{}
	}
	table, ok := migrationsTable(set)
	if !ok {
		return record{}
	}
	var exists bool
	if err := bdb.QueryRowContext(ctx, "SELECT to_regclass(?) IS NOT NULL", table).Scan(&exists); err != nil {
		return record{err: err}
	}
	if !exists {
		return record{}
	}
	var id int64
	err := bdb.QueryRowContext(ctx, fmt.Sprintf("SELECT id FROM %s WHERE id = (SELECT max(id) FROM %s) "+
		"AND name = ? AND migrated_at > clock_timestamp() - interval '1 minute'", table, table), o.migration).Scan(&id)
	if err != nil && !isNoRows(err) {
		return record{err: err}
	}
	return record{id: id}
}

// migrationsTable is the table bun's migrator records in, as bun writes it
// into its SQL: unquoted, so PostgreSQL folds it to lower case, and this has
// to find the same table. Validate has made sure it is a plain identifier.
func migrationsTable(set fixturechange.Set) (string, bool) {
	table := set.MigrationsTable
	if table == "" {
		table = fixturechange.DefaultMigrationsTable
	}
	_, err := quoteIdent(table)
	return table, err == nil
}

// unrecord deletes the record bun's migrator made of this migration before
// running it, the one findRecord found, and returns the failure with a note
// saying what it did.
func unrecord(ctx context.Context, db bun.IDB, set fixturechange.Set, o options, rec record, failure error) error {
	bdb, ok := db.(*bun.DB)
	if !ok || o.migration == "" {
		return failure
	}
	table, _ := migrationsTable(set)
	if rec.err != nil {
		return fmt.Errorf("%w\n\nwhether bun recorded %s as applied could not be checked (%v): if %s holds a row "+
			"named %s written just before this run, delete it, or the migration will not run again",
			failure, o.migration, rec.err, table, o.migration)
	}
	if rec.id == 0 {
		return failure
	}
	// The migration may have failed because the context ended; the record
	// still has to go, or the next deploy skips this migration.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	res, err := bdb.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE id = ? AND name = ?", table), rec.id, o.migration)
	if err != nil {
		return fmt.Errorf("%w\n\nbun had recorded %s as applied before running it, and the record could not "+
			"be removed (%v): delete the row of %s with id %d, or the migration will not run again",
			failure, o.migration, err, table, rec.id)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return failure
	}
	o.logf("%s: bun had recorded migration %s as applied before running it; the record was removed, "+
		"so it runs again once this is fixed", set.Name, o.migration)
	return fmt.Errorf("%w\n\nbun had recorded migration %s as applied before running it, as its migrator does "+
		"unless built WithMarkAppliedOnSuccess(true); that record was removed, so the migration runs again once "+
		"this is fixed", failure, o.migration)
}
