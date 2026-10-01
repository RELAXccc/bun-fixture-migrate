# Using it with bun

bun projects run their migrations and seed their data in more than one way. This page takes each in
turn and says what to configure and what to watch for. [`examples/basic`](../examples/basic) is a
complete project for the most common one.

- [The shape of a project](#the-shape-of-a-project)
- [Deploying: migrate, then seed](#deploying-migrate-then-seed)
- [Which migrator settings](#which-migrator-settings)
- [Where the migrations run](#where-the-migrations-run)
- [SQL migrations next to fixture migrations](#sql-migrations-next-to-fixture-migrations)
- [Several fixture files](#several-fixture-files)
- [When production data is edited in production](#when-production-data-is-edited-in-production)
- [Tests and development servers](#tests-and-development-servers)
- [Use from Go](#use-from-go)
- [Drivers](#drivers)
- [How model fields meet the fixture file](#how-model-fields-meet-the-fixture-file)

## The shape of a project

```
fixture-migrate.yml
fixtures/fixture.yml                       master data, the file dbfixture loads
internal/migrations/migrations.go          var Migrations = migrate.NewMigrations()
internal/migrations/20260901000000_create_plans.up.sql
internal/migrations/20260930165255_fixture_plan_prices.go
internal/migrations/fixture_state.yml
```

Fixture migrations live in the same bun migrations package as every other migration, register with
the same `*migrate.Migrations`, and run in the same name order. A fixture migration that changes a
column is therefore always after the schema migration that created it, as long as it was generated
after it.

The day-to-day loop is:

1. edit the fixture file;
2. `bun-fixture-migrate generate -name "what changed"`;
3. read the migration, commit both files and the state file together;
4. before the deploy, `bun-fixture-migrate plan` against a copy of production.

CI runs `status -offline` on every change, which fails a fixture edit that came without its
migration. See [CI](ci.md).

## Deploying: migrate, then seed

`dbfixture` only fills an empty database, and a fixture migration only changes a filled one. A
deploy step is both, in this order:

```go
migrator := migrate.NewMigrator(db, migrations.Migrations, migrate.WithMarkAppliedOnSuccess(true))
if err := migrator.Init(ctx); err != nil { ... }
if err := lock(ctx, migrator, time.Minute); err != nil { ... }
defer migrator.Unlock(context.WithoutCancel(ctx))

if _, err := migrator.Migrate(ctx); err != nil { ... }

// A new database: its fixture migrations found the seed guard table empty and
// did nothing. Load the fixture file as it is now, which already holds every
// change they would have made.
err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
	seeded, err := tx.NewSelect().Model((*Plan)(nil)).Exists(ctx)
	if err != nil || seeded {
		return err
	}
	if err := dbfixture.New(tx).Load(ctx, fixtures, "fixtures/fixture.yml"); err != nil {
		return err
	}
	_, err = fixtureapply.SyncSequences(ctx, tx, "currencies", "plans", "features")
	return err
})
```

Four details matter:

- **The lock.** bun's `Migrator.Lock` does not wait: it inserts a row into `bun_migration_locks` and
  fails at once, `migrations table is already locked`, while another process's row is there. Retry
  it for a bounded time:

  ```go
  func lock(ctx context.Context, m *migrate.Migrator, wait time.Duration) error {
  	deadline := time.Now().Add(wait)
  	for {
  		err := m.Lock(ctx)
  		if err == nil || !strings.Contains(err.Error(), "already locked") || time.Now().After(deadline) {
  			return err
  		}
  		select {
  		case <-ctx.Done():
  			return ctx.Err()
  		case <-time.After(250 * time.Millisecond):
  		}
  	}
  }
  ```

  A process that dies between `Lock` and `Unlock` leaves its row, and no wait ends that: every later
  deploy fails until somebody deletes it. `status` reports such a row, and
  [the runbook](production.md#every-migrate-fails-the-migrations-table-is-already-locked) says how to
  remove it.

- **The seed guard.** `seed_guard_table` names a table that is empty exactly when the database was
  never seeded. While it is, a fixture migration logs `plans is empty, nothing to do` and succeeds.
  Without it, a new database would run every fixture migration ever written against nothing and
  fail on the first update.
- **The seed runs in a transaction.** A load that fails halfway would otherwise leave a database the
  next deploy takes for seeded.
- **Sequences.** A fixture file names its ids, and `dbfixture` writes them explicitly; a sequence
  does not see an explicit id go by, so the application's first insert collides with id 1.
  `fixtureapply.SyncSequences` moves each serial and identity column's sequence past the largest
  value, and never backwards: a sequence restarted at 1000 and not called since stays at 1000 when
  the ids are below it. Generated migrations do the same for every explicit id they write. Moving a
  sequence takes `UPDATE` on it, which `setval` needs, and seeing where it stands takes `SELECT`, or
  `USAGE`; the tables' owner has them all. A role with `USAGE` and `UPDATE` and no `SELECT` reads
  where the sequence stands through `pg_sequence_last_value` and its start value, which cannot see
  where a `RESTART WITH` left a sequence not called since: such a sequence, restarted past the ids,
  is taken to be at its start value and moved back to just past them. Grant `SELECT` where sequences
  are restarted ahead of the master data's ids. A role lacking what it needs is told the `GRANT`,
  and nothing is changed.

## Which migrator settings

| Setting | What to do |
| --- | --- |
| `migrate.NewMigrator(db, m)` | works. bun records a migration *before* running it and keeps the record when it fails; a failing fixture migration deletes that record itself, so it runs again once the database is fixed |
| `WithMarkAppliedOnSuccess(true)` | works, and has no window between the record and its removal. Prefer it |
| `WithTableName("schema_migrations")` | set `migrations_table` to the same name; `status`, `plan` and the record removal read it |
| `WithLocksTableName` | set `migration_locks_table` to the same name; `status` reads it to report a lock left behind |
| `WithUpsert(true)` and `RunMigration` | re-running an applied fixture migration finds its changes made and reports them `unchanged` |
| `BeforeMigration` / `AfterMigration` | run around the migration as usual |
| `Rollback` | runs the generated down function, which reverts the change set with the same guards: with an `audit_table`, only the changes the migration made in that database |

The record removal only happens when `Apply` runs under bun's migrator, on the migrator's own
`*bun.DB`, and only to the rows that, before the change set ran, carried the migration's name, had
been written in the last minute and were newer than every other migration's record: the record bun's
default mode makes just before calling the migration, or one per replica when replicas start it at
the same moment without bun's `Lock`. A failing replica deletes them all, so the migration is not
left recorded by the record of another replica that failed too; when that other replica succeeded
instead, the next migrate runs the migration again and finds every change made. bun's `Lock`, or
`WithUpsert(true)`, which keeps one record per name, keeps replicas from writing more than one record
of a run in the first place. A generated file registers
`fixtureapply.Up(set)` and `fixtureapply.Down(set)`; `Up` reads the migration's name from the file
that calls it, as bun's `Register` reads it from the same file, so both are called in the
migration's own file. Files from earlier versions register functions that call `Apply` and `Revert`
themselves, and `Apply` finds the name on the call stack instead: the innermost file named like a
migration, so a helper in another file is fine. An up function that calls `Apply` from outside any
migration-named file needs `fixtureapply.WithMigrationName`; without it nothing is removed, and a
failure leaves bun's record in place.

## Where the migrations run

**A deploy step or job** (a CI/CD stage, a Kubernetes Job, an init container): the recommended
shape. Run `plan -strict` against a copy of production before it and `status -require-applied`
after. The `examples/basic` binary is such a step.

**At application start, from every replica.** bun's `Lock` does not make the others wait: of
replicas starting together, all but the one that took it fail their `Lock` at once. Retry it for a
bounded time, as above, so they go through one after another; the ones after the first find nothing
to run. Every change set also takes a transaction-scoped advisory lock of its own, so two processes
applying the same set, one of them outside the migrator's lock, cannot both insert a row: the second
finds every change made and reports `unchanged`. The seed step needs the migrator's lock too: hold
it around the seed, as above.

**By hand, in an emergency.** `apply -file` shows what one generated migration does against any
database, applied or not, and with `-yes -record` runs it and records it as bun's migrator would, in
one transaction; see [the runbook](production.md#running-a-migration-by-hand). Do not edit the rows
by hand instead: the next fixture migration's guards compare against the values
the state file says the database holds, and a hand edit shows up there as a changed row.

## SQL migrations next to fixture migrations

`Migrations.Discover(fs)` with `.up.sql` and `.down.sql` files and `Migrations.MustRegister` with
generated Go files mix freely. `status` lists both, and reports two migrations bun would record under
one name, which bun's `Discover` only catches between two SQL files.

`plan` cannot run arbitrary Go, so it names pending migrations it did not write and says which
fixture migration they would run before. A fixture migration after one of them that finds a table or
a column missing could be waiting for that migration to create it, so the plan is inconclusive there
(exit 1) rather than a failure. `plan -with-sql` runs pending SQL migrations in the same
rolled-back transaction, so a fixture migration that writes a column a pending SQL migration adds is
planned against that column. It reads each file exactly as bun v1.2.18 does:

- split at `--bun:split` lines, any other `--bun:` line an error;
- a line longer than 64 KiB fails the migration before any of it runs, as bun's line scanner does.
  Under bun's default migrator that failed migration stays recorded as applied and never runs, so
  plan reports it as a failure;
- blank lines are kept. bun after v1.2.18 drops them, which changes a quoted string or a function
  body that holds one; plan notes where a file has such a line;
- a file holding `{{` is not run. bun renders SQL files as Go templates when the migrator is built
  `WithTemplateData`, and plan does not have the data, so the migration is listed as not simulated.

The plan's one transaction differs from the deploy, which commits each migration, and each statement
of a SQL migration whose name has no `.tx.`. Constraints declared `DEFERRABLE INITIALLY DEFERRED` are
checked at those same points, so a migration that breaks one fails in the plan as in the deploy. What
cannot be reproduced makes the plan inconclusive (exit 1) rather than wrong: a SQL migration that
cannot run in a transaction (`CREATE INDEX CONCURRENTLY`), and an enum value one migration adds and a
later one uses, which no transaction can do in PostgreSQL. Plan again once those are applied.

Two things a rollback does not take back. A sequence a SQL migration moves, with `setval`, `nextval`
or an insert, stays moved in the database plan ran against, and plan notes a migration that calls
`setval` or `nextval`: a `setval` that winds a sequence back, run against production, leaves the
application's next insert colliding with an existing id. And while it runs, plan holds the locks a
SQL migration takes, for most `ALTER TABLE` on the whole table. Run `plan -with-sql` against a copy.

## Several fixture files

An application that loads its master data with
`fixture.Load(ctx, fsys, "currencies.yml", "plans.yml")` lists them in the same order:

```yaml
fixtures: [fixtures/currencies.yml, fixtures/plans.yml]
```

They are read as `dbfixture` reads them: in order, one scope of anchors across them, so a row in
`plans.yml` can name `{{ $.Currency.eur.ID }}` and a row in `currencies.yml` cannot name a plan. The
state file holds all of them. `export` writes each model back into the file that holds it and a new
model into the last file. Splitting a file, or moving rows between files, changes no row and
generates nothing.

## When production data is edited in production

An admin UI or a support script that edits master data in production makes production, not the
file, the newer truth. First say what the database owns, so that it is no drift at all; see
[who owns what](reference.md#who-owns-what):

- A column an admin or an operator edits after the row exists, a price in the admin UI, a feature
  flag's `enabled`: list it in the model's `insert_only`. A new row gets the file's value, and from
  then on nothing compares, updates or guards on it, and an export keeps the file's value.

  ```yaml
  Flag:
    table: flags
    key: [code]
    ref: code
    insert_only: [enabled]
  ```

- A table whose rows the database owns once they exist, values and all, such as countries an admin
  maintains after the first seed: `mode: insert`. The file only seeds the rows a database lacks.
- A table tenants or the application add rows to, next to the master rows: `mode: upsert`, so a row
  the file does not hold is not drift and is never deleted, and an export writes only the file's
  rows. Where the database numbers those rows from one sequence, `ids: database` too: a migration
  then inserts a master row without an id, so it takes the sequence's next one instead of an id a
  tenant's row may already hold, and the file's rows point at it by name as before. Where a column
  tells the two kinds of rows apart, `where: tenant_id IS NULL` keeps the tenants' rows out of
  everything instead.

For what the file does own, three settings and one loop keep the edits safe:

- `policy.changed_row: warn` (the default): a migration that finds a row changed since the file was
  written leaves it alone and says so, instead of overwriting the edit.
- `check` on a schedule reports the difference, with exit code 3, and counts what the configuration
  leaves to the database, which is no drift.
- An admin UI that saves a row through bun, `db.NewUpdate().Model(row)`, writes `DEFAULT` rather than
  NULL for a nil pointer or a zero in a `nullzero` field, since bun v1.2.17, as an insert does.
  Clearing such a field leaves the column's default, so `check` reports the row against a file that
  says `~`, with a `hint:` line saying why, and an export writes the default into the file.
- To take the edits into the file, export from production and generate:

  ```
  bun-fixture-migrate export -dsn env:PRODUCTION_READONLY_DSN
  bun-fixture-migrate generate -name "admin edits"
  ```

  The migration brings every other database to the file; on production itself every change is
  already made and reports `unchanged`. Review the export's diff like any other change. It holds
  the columns and ids the file held, but it is written anew from the database: models in
  dependency order, values quoted the export's way, and no comments, which it says it dropped. So
  against a file export wrote, the diff shows the edits and nothing else; against one edited by
  hand, it shows that layout too, once. The read-only role has to see every row of the master
  data: one a row-level security policy limits is refused, because an export without the rows it
  hides would delete them everywhere else.

`generate -from-db` is the other direction: it diffs a database against the file and writes the
migration that makes that database match the file. It is the tool for "production is out of step and
the file is right".

## Tests and development servers

A test database, a developer's database or a preview environment does not need migrations for its
master data; it needs the data. Two ways:

**Seed from scratch** with `dbfixture`, then move the sequences:

```go
fixture := dbfixture.New(db, dbfixture.WithRecreateTables())
if err := fixture.Load(ctx, os.DirFS("testdata"), "fixture.yml"); err != nil { ... }
if _, err := fixtureapply.SyncSequences(ctx, db, "currencies", "plans"); err != nil { ... }
```

**Bring an existing database to the file**, keeping everything else in it: the `sync` command, or
from Go:

```go
project, err := fixturemigrate.LoadProject("fixture-migrate.yml")
report, err := project.Sync(ctx, db, fixturemigrate.SyncOptions{})
if errors.Is(err, fixturemigrate.ErrRefused) {
	// report.Findings and report.Diff.Refusals say why
}
```

`Sync` compares and applies in one `REPEATABLE READ` transaction with the guards and the policy of a
generated migration, refuses what `generate` would refuse, seeds an empty database, and records
nothing in the migrations table. `SyncOptions{DryRun: true}` rolls back and reports. A test suite
that syncs its database once and checks it is [below](#a-test-database).

## Use from Go

`check`, `export`, `generate`, `baseline`, `status` and `sync` are methods of a
`fixturemigrate.Project`, for a program that would otherwise run the binary: a test helper, a
development server that syncs on start, an admin tool that exports production, a CI program, a job.
A method does what its command does, with the same checks and the same refusals, and its result,
encoded as JSON, is what the command prints with `-json`.

```go
project, err := fixturemigrate.LoadProject("fixture-migrate.yml")
```

reads the configuration and the fixture files, the paths in it relative to it, as the command does.
`project.Config` may be changed before a method is called, as `-dsn` changes `Database`.

| Method | Command | The database |
| --- | --- | --- |
| `Check(ctx, db)` | `check` | needed |
| `Export(ctx, db, ExportOptions{})` | `export` | needed |
| `Generate(ctx, db, GenerateOptions{Name: "plan prices"})` | `generate` | `nil` works offline; given one, it respells and lints as `generate` does, and is the base with `FromDB` |
| `Baseline(ctx, db, BaselineOptions{})` | `baseline` | `nil` works offline; given one, it is asked whether a difference is only spelling |
| `Status(ctx, db, StatusOptions{})` | `status` | `nil` works offline |
| `Sync(ctx, db, SyncOptions{})` | `sync` | needed, a `*bun.DB` |

The library never connects by itself: it uses the database the program hands it. Given a `*bun.DB`
or a `bun.Conn`, a method reads in a `REPEATABLE READ, READ ONLY` transaction of its own, as the
command does. Given a `bun.Tx`, it reads in that transaction, under a savepoint it rolls back: it
sees what the transaction wrote, and leaves the transaction and its settings as they were.

`Generate`, `Baseline` and `Export` write nothing. Their result says what would be written, and its
`Write` writes it:

```go
g, err := project.Generate(ctx, nil, fixturemigrate.GenerateOptions{Name: "plan prices"})
var refused *fixturemigrate.RefusedError
switch {
case errors.As(err, &refused):
	// What the command exits 2 on. refused.Message says what to do; its
	// Refusals, Findings and Problems say what was refused.
case err != nil:
	// What the command exits 1 on: it could not run.
default:
	fmt.Println(string(g.Source))
	_, err = g.Write() // the migration and the state file
}
```

`errors.Is` tells a refusal's kind: `ErrFindings` for a finding the policy makes an error,
`ErrLineage` for a migration the state file's history does not include, `ErrUnmigrated` for
changes `baseline` would record that no migration makes. A refusal comes with the method's result
all the same. The examples on [pkg.go.dev](https://pkg.go.dev/github.com/RELAXccc/bun-fixture-migrate)
show each method, and [the reference](reference.md#the-project) lists them.

### A test database

A test suite brings its database to the fixture files once, in `TestMain`, after the schema is in
place, and a test fails when the two disagree, which a test that writes master data and does not
clean up shows:

```go
var (
	testDB  *bun.DB
	project *fixturemigrate.Project
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	testDB = bun.NewDB(sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN(os.Getenv("TEST_DATABASE_URL")))), pgdialect.New())
	var err error
	// go test runs in the package's directory.
	if project, err = fixturemigrate.LoadProject("../../fixture-migrate.yml"); err != nil {
		log.Fatal(err)
	}
	// The schema is in place: the migrator ran, or the tables were created.
	// Sync seeds an empty database and puts back what an earlier run changed.
	if _, err := project.Sync(ctx, testDB, fixturemigrate.SyncOptions{}); err != nil {
		log.Fatal(err)
	}
	code := m.Run()
	testDB.Close()
	os.Exit(code)
}

func TestTheDatabaseHoldsTheFixtureFiles(t *testing.T) {
	report, err := project.Check(context.Background(), testDB)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Agree {
		t.Errorf("the test database and the fixture files disagree:\n%s",
			strings.Join(report.Lines(), "\n"))
	}
}
```

A test that works in a transaction it rolls back passes that `bun.Tx` to `Check`, which then sees
the test's own writes. `go test ./...` runs packages at once: packages that share one test database
each sync it, and two syncs that change the same rows at once fail one of them, so give each package
a database of its own, or run them one after another with `go test -p 1`.

### A development server, an admin tool, a job

A development server calls `project.Sync(ctx, db, fixturemigrate.SyncOptions{Logf: log.Printf})`
before it serves: a developer's database follows the fixture files without migrations. Sync is not
for a database that is deployed to; that one gets the migrations, which it records.

An admin tool that takes production's edits into the fixture files calls `Export` with a read-only
role's `*bun.DB` and `Write`s the result, then `Generate`s the migration, as
[above](#when-production-data-is-edited-in-production). A Kubernetes job that checks production on a
schedule calls `Check` and exits non-zero unless the report's `Agree`. A CI program calls
`Status(ctx, nil, fixturemigrate.StatusOptions{})` as `status -offline` and fails on the report's
`Failures`.

## Drivers

The command connects with `pgdriver`. The run time, `fixtureapply`, runs in your application with
whatever driver it uses: it is tested under bun's `pgdriver` and under `pgx/v5/stdlib` with
`pgdialect`. It sets `TimeZone`, `DateStyle` and `IntervalStyle` for its own transaction and restores
them, so values compare the same whatever the connection's settings.

What else runs in that transaction sees them too: `TimeZone` is `UTC`, `DateStyle` `ISO, YMD` and
`IntervalStyle` `postgres` for every trigger a change fires and every column default an insert
fills. `now()` is the same instant either way, but `current_date`, `localtimestamp`, `localtime`,
`now()::date`, `date_trunc('day', now())` and `to_char(now(), ...)` are UTC's: an insert into a
table whose `created_on date DEFAULT current_date` the fixture leaves out gets the date in UTC, and a
trigger stamping the local date writes UTC's. A trigger turning a date or an interval into text
gets the ISO and `postgres` spellings. The setting is not avoidable by binding the values
differently: a change set written by an earlier version, or by hand, may hold a timestamp without
an offset, which is a UTC time, and only the session's `TimeZone` reads it as one.

The DSN is a URL (`postgres://user:password@host:5432/db?sslmode=require`). The command refuses a
keyword DSN (`host=... user=...`) with a sentence, never repeats a password in a message, and sets
`application_name=bun-fixture-migrate` unless the DSN sets one.

## How model fields meet the fixture file

The tool never reads your Go types; the catalog tells it the columns. What bun's `Insert` writes
for a field decides what a seeded database holds, so these are the idioms that matter. bun writes
`DEFAULT` for a nil pointer, and for a zero in a field tagged `nullzero` or `default:`; the tool
sees the column's default in the catalog instead of the tag, which agrees whenever the tables were
created from the models or the tags mirror the schema.

| Model field | Fixture value | bun writes | The tool |
| --- | --- | --- | --- |
| ``Seats int64 `bun:",default:1"` `` | `seats: 0` | `DEFAULT`, so 1 | reports it (`zero_default`), and `export` refuses to write such a file |
| `Note *string` on a column with a default | `note: ~` or left out | `DEFAULT` | reports an explicit null (`null_default`) |
| `Note *string` on a column without a default | `note: ~` | `DEFAULT`, which is NULL | fine; tell it with `defaults: {note: ~}` |
| `Settings map[string]any` with `type:jsonb` | a YAML mapping | the JSON | compared as `jsonb`, exported as a YAML mapping |
| `Tags []string` with `array` | a YAML sequence | a PostgreSQL array | compared as the array type |
| `ID int64` with `pk,autoincrement` on `bigserial` | `id: 3` | 3 | `serial: true`; sequences moved past it |
| `ID uuid.UUID` with `default:gen_random_uuid()` | usually left out | `DEFAULT` | natural keys identify rows; ids never compared |
| `GENERATED ALWAYS AS IDENTITY` | no id | `DEFAULT` | `export` leaves the id out; an explicit id is reported |
| a generated column (`GENERATED ALWAYS AS (...) STORED`) | not in the file | nothing | skipped by `export` and reported if the file writes it |
| `CreatedAt time.Time` with `default:current_timestamp` | left out | `DEFAULT` | `ignore` it |

Dates and timestamps are written unquoted, as YAML timestamps, because a quoted one decodes into a
`time.Time` only in RFC 3339.
