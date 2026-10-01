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
if err := migrator.Lock(ctx); err != nil { ... }
defer migrator.Unlock(ctx)

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

Three details matter:

- **The seed guard.** `seed_guard_table` names a table that is empty exactly when the database was
  never seeded. While it is, a fixture migration logs `plans is empty, nothing to do` and succeeds.
  Without it, a new database would run every fixture migration ever written against nothing and
  fail on the first update.
- **The seed runs in a transaction.** A load that fails halfway would otherwise leave a database the
  next deploy takes for seeded.
- **Sequences.** A fixture file names its ids, and `dbfixture` writes them explicitly; a sequence
  does not see an explicit id go by, so the application's first insert collides with id 1.
  `fixtureapply.SyncSequences` moves each serial and identity column's sequence past the largest
  value, and never backwards. Generated migrations do the same for every explicit id they write.

## Which migrator settings

| Setting | What to do |
| --- | --- |
| `migrate.NewMigrator(db, m)` | works. bun records a migration *before* running it and keeps the record when it fails; a failing fixture migration deletes that record itself, so it runs again once the database is fixed |
| `WithMarkAppliedOnSuccess(true)` | works, and has no window between the record and its removal. Prefer it |
| `WithTableName("schema_migrations")` | set `migrations_table` to the same name; `status`, `plan` and the record removal read it |
| `WithLocksTableName` | nothing to do |
| `WithUpsert(true)` and `RunMigration` | re-running an applied fixture migration finds its changes made and reports them `unchanged` |
| `BeforeMigration` / `AfterMigration` | run around the migration as usual |
| `Rollback` | runs the generated down function, which reverts the change set with the same guards |

The record removal only happens when `Apply` runs under bun's migrator, on the migrator's own
`*bun.DB`, and the newest row of the migrations table carries the migration's name and was written
within the hour. The name is read the way bun's `Register` reads it, from the innermost file on the
call stack named like a migration, so a helper in another file is fine. An up function that is
defined outside any migration-named file needs `fixtureapply.WithMigrationName`; without it nothing
is removed, and a failure leaves bun's record in place.

## Where the migrations run

**A deploy step or job** (a CI/CD stage, a Kubernetes Job, an init container): the recommended
shape. Run `plan -strict` against a copy of production before it and `status -require-applied`
after. The `examples/basic` binary is such a step.

**At application start, from every replica.** bun's `Lock` serialises the migrator, and every
change set additionally takes a transaction-scoped advisory lock, so two processes applying the
same set cannot both insert a row. The second one finds every change made and reports `unchanged`.
The seed step needs the same protection: hold the migrator's lock around it, as above.

**By hand, in an emergency.** Every generated migration is a plain Go value; `plan -file` shows
what one does against any database, applied or not, and `Apply` runs it from a small program. Do
not edit the rows by hand instead: the next fixture migration's guards compare against the values
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
file, the newer truth. Three settings and one loop keep that safe:

- `policy.changed_row: warn` (the default): a migration that finds a row changed since the file was
  written leaves it alone and says so, instead of overwriting the edit.
- `check` on a schedule reports the difference, with exit code 3.
- To take the edits into the file, export from production and generate:

  ```
  bun-fixture-migrate export -dsn env:PRODUCTION_READONLY_DSN
  bun-fixture-migrate generate -name "admin edits"
  ```

  The migration brings every other database to the file; on production itself every change is
  already made and reports `unchanged`. Review the export's diff like any other change: it holds
  the columns and ids the file held, so it shows the edits and nothing else. The
  read-only role has to see every row of the master data: one a row-level security policy limits
  is refused, because an export without the rows it hides would delete them everywhere else.

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
cfg, err := fixturemigrate.LoadConfig("fixture-migrate.yml")
data, err := os.ReadFile("fixtures/fixture.yml")
res, err := fixturemigrate.Sync(ctx, db, cfg,
	[]fixturemigrate.FixtureFile{{Path: "fixtures/fixture.yml", Data: data}},
	fixturemigrate.SyncOptions{})
if errors.Is(err, fixturemigrate.ErrSyncRefused) {
	// res.Findings and res.Diff.Refusals say why
}
```

`Sync` compares and applies in one `REPEATABLE READ` transaction with the guards and the policy of a
generated migration, refuses what `generate` would refuse, seeds an empty database, and records
nothing in the migrations table. `SyncOptions{DryRun: true}` rolls back and reports.

## Drivers

The command connects with `pgdriver`. The run time, `fixtureapply`, runs in your application with
whatever driver it uses: it is tested under bun's `pgdriver` and under `pgx/v5/stdlib` with
`pgdialect`. It sets `TimeZone` and `DateStyle` for its own transaction and restores them, so values
compare the same whatever the connection's settings.

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
