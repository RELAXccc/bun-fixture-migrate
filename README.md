# bun-fixture-migrate

Keeps the master data of a [bun](https://github.com/uptrace/bun) application and its
[dbfixture](https://pkg.go.dev/github.com/uptrace/bun/dbfixture) YAML files in step, in both
directions, and manages the data migrations that do it. PostgreSQL only.

`dbfixture` loads a fixture file into an empty database. That is fine for a fresh checkout and
useless for a deployed one: after the first seed, editing the YAML changes nothing, because nobody
re-seeds a production database. So every change to seed data needs a data migration. And if anything
else writes that data (an admin UI, a support script, a hand-run `UPDATE`), the file and the
database drift apart with nothing to say so.

| Command | |
| --- | --- |
| `scaffold` | write a starter configuration from a live database |
| `export` | write the fixture files from a database |
| `check` | report what the database and the fixture files disagree about, changing nothing |
| `generate` | write the bun migration for what changed, against the state the last migration left |
| `baseline` | record the fixture files as migrated without writing a migration |
| `status` | list the migrations, what a database applied, and what no migration covers yet |
| `plan` | run the pending migrations against a database and roll back, reporting every row |
| `sync` | bring a development, test or staging database to the fixture files directly |
| `apply` | run one generated migration by hand, and record it as bun's migrator would |

It never looks at your Go model types. PostgreSQL's catalog knows the tables, the columns and their
types, the defaults, the keys, the foreign keys and the sequences; the configuration says which
model lives in which table and which column is a reference. That is enough for all of it.

## Documentation

| | |
| --- | --- |
| [Using it with bun](docs/usage.md) | the deploy step, migrator settings, SQL migrations, several fixture files, admin UIs, tests, use from Go, drivers, model idioms |
| [Production runbook](docs/production.md) | the pipeline, and what to do when a migration fails, a change is skipped, drift is found, a state file conflicts |
| [CI](docs/ci.md) | the GitHub Action and the GitLab CI templates |
| [Fixture files](docs/fixture-files.md) | how a file is read, what each value means, what is refused, limits |
| [Reference](docs/reference.md) | commands and flags, exit codes, configuration, JSON output, the Go API |
| [Troubleshooting](docs/troubleshooting.md) | messages and what to do about them |
| [`examples/basic`](examples/basic) | a complete bun project: schema and fixture migrations, the deploy step, seeding |
| [Concept](docs/concept.md) | where this is going, and why |

## Install

```
go install github.com/RELAXccc/bun-fixture-migrate/cmd/bun-fixture-migrate@latest
```

Build it with a supported Go release. Use the same version of the command as of the
`fixtureapply` package your migrations import, which needs bun and nothing
else.

## In five minutes

```
$ export DATABASE_URL=postgres://localhost/myapp?sslmode=disable
$ bun-fixture-migrate scaffold -o fixture-migrate.yml   # then read it: every GUESS is yours to fix
$ bun-fixture-migrate export                             # or keep your fixture file and run check
$ bun-fixture-migrate baseline                           # the databases hold the file as it is
```

Before the export, read what scaffold wrote. It proposes every table but bun's own as master data,
and only you know which the application writes: delete the models of users, orders, sessions and
the like, or export writes their rows into the fixture file and every deploy is drift. Check
`seed_guard_table`, which it guesses: a table the fixture file fills, without which a new
environment runs its fixture migrations before the seed.

Then, for every change to master data, edit the fixture file and:

```
$ bun-fixture-migrate generate -name "plan prices"
Plan: 1 insert, 1 update
wrote internal/migrations/20260921120000_fixture_plan_prices.go
wrote internal/migrations/fixture_state.yml
read it, run plan against a copy of production, then deploy

$ bun-fixture-migrate plan
20260921120000_fixture_plan_prices: would succeed
  applied  Plan name=pro insert (1 row)
  skipped  Plan name=team update [changed row]: plans name=team no longer holds the values this change was generated against: it was changed in this database, or by a migration that ran before this one. It was left alone. Compare it with the fixture file and decide which one is right

rolled back: nothing was changed, except that an id an insert drew from a sequence stays drawn, which only leaves a gap
```

The migration registers with your bun migrator and deploys with everything else. In CI,
`status -offline` fails a fixture edit that came without its migration; after a deploy, `check`
reports drift. [`examples/basic`](examples/basic) is the whole loop in a runnable project.

## What a generated migration does

It is a plain Go file meant to be read:

```go
func init() {
	Migrations.MustRegister(
		fixtureapply.Up(fixtureChanges20260921120000PlanPrices),
		fixtureapply.Down(fixtureChanges20260921120000PlanPrices),
	)
}

var fixtureChanges20260921120000PlanPrices = fixturechange.Set{
	Name:            "20260921120000_fixture_plan_prices",
	SeedGuardTable:  "plans",
	MigrationsTable: "bun_migrations",
	Tables: fixturechange.Tables{
		"Currency": {Name: "currencies", ID: "id", Key: "code"},
		"Plan":     {Name: "plans", ID: "id", Key: "name", Serial: true},
	},
	Policy: fixturechange.Policy{
		MissingRow: "error", ChangedRow: "warn", IDDrift: "error", DuplicateKey: "error",
	},
	Changes: []fixturechange.Change{
		{Model: "Plan", Kind: fixturechange.Insert,
			Key: fixturechange.Values{"name": fixturechange.Lit("pro")},
			New: fixturechange.Values{
				"currency_id": fixturechange.RefTo("Currency", "USD"),
				"id":          fixturechange.Lit("3"),
				"name":        fixturechange.Lit("pro"),
				"price_cents": fixturechange.Lit("9000"),
				"settings":    fixturechange.Lit(`{"sso":true,"trial_days":30}`),
			},
		},
		{Model: "Plan", Kind: fixturechange.Update,
			Key: fixturechange.Values{"name": fixturechange.Lit("team")},
			Old: fixturechange.Values{"price_cents": fixturechange.Lit("2000")},
			New: fixturechange.Values{"price_cents": fixturechange.Lit("2500")},
		},
	},
}
```

Values are Go string literals, and whatever the file's comments quote from the data, such as the key
and reason of a refused row, has its control and invisible characters escaped: a value holding a line
break cannot end a comment and become code in your migrations package.

No ids in the `Key` maps, and `currency_id` is a name, not a number: rows are found by their
natural key and references are resolved against the database the migration runs on, because ids
drift between databases and names do not. The policy is written into the file, so changing the
configuration later does not change what an old migration does. At run time `fixtureapply`:

- does nothing while `seed_guard_table` is empty: that database has not been seeded, and
  `dbfixture` will load the new state by itself;
- runs the whole set in one transaction, under an advisory lock, so two replicas applying it at once
  cannot both insert a row, and checks `DEFERRABLE` constraints once the whole set is done, so a
  rename of a code a foreign key points at can be followed by the rows pointing at it;
- resolves every reference to a real id first. One it writes fails the migration if it matches no
  row or more than one; one a guard compares with, whose row was renamed or removed here, matches
  nothing, which is a missing or changed row under the policy like any other;
- inserts only when no row with that natural key exists, and updates or deletes only while the row
  still holds the values the change was generated against, so a hand edit survives and a second run
  is a no-op;
- compares through each column's type: `jsonb` as JSON, arrays as arrays, `numeric` as numbers,
  timestamps as instants whatever the session's time zone;
- refuses a delete that other rows point at, rather than letting `ON DELETE CASCADE` or `SET NULL`
  reach them, unless the model says `deletes: cascade`, and then says how many rows the delete reached;
  a rollback does not bring those back;
- diagnoses every row count of zero, as below, and takes back bun's record of a migration that
  failed;
- moves the sequence past an explicit id before it writes it, so an insert by the application
  meanwhile cannot draw that id;
- logs one line per row, or writes it to a `log/slog` logger with `fixtureapply.WithSlog`, or hands
  every row's outcome to `fixtureapply.WithReport`; a change that fails the migration is a
  `*fixtureapply.ChangeError` that `errors.As` finds.

`Revert` is the same set backwards, every change inverted and guarded the same way. A rename finds
its row under the name it gave it, so it reverts, and a second run finds it already made. With an
`audit_table`, every run records which changes it made, found made already or skipped, and `Revert`
undoes only the ones it made in that database, and nothing a second time; without one, it assumes
the migration made them all, including those it found already made. See [rolling back](docs/production.md#rolling-back).

## Three things about bun you may not know

All three are in bun's own source, all three are pinned by a test in `dbtest` that runs the real bun
against a real PostgreSQL, and all three are why this tool exists in the shape it does.

### A zero is not written into a column that has a default

`InsertQuery.appendStructValues` writes `DEFAULT` rather than the value whenever `marshalsToDefault`
holds (`query_insert.go`):

```go
return (f.IsPtr && f.HasNilValue(v)) ||
	(f.HasZeroValue(v) && (f.NullZero || f.SQLDefault != ""))
```

So this model:

```go
ProductionMax int64 `bun:"production_max,notnull,default:1"`
```

and this fixture row:

```yaml
- name: rope
  production_max: 0
```

produce a database holding `1`. The file says `0`, the database says `1`, from the very first seed,
and nothing anywhere reports it. Four rows of one table were wrong this way for months.

This tool cannot read the Go tag, and it does not need to: `pg_attrdef` says the column has a
default other than the type's zero, which is the same condition. Every command checks it.

- `check` and `generate` report a fixture row that writes such a zero.
- `export` marks the value in the file it writes, on the line it happened, and by default refuses to
  write the file at all:

```yaml
      production_max: 0  # ROUND-TRIP HAZARD: the column defaults to 1 and bun writes DEFAULT for a
                         # zero, so loading this file stores 1 here, not 0
```

An export that does not reproduce the database it was taken from is worse than no export.
`policy.zero_default` turns this into a warning or off.

### Nor is a null

Read the same line from its other end: a nil pointer is also written as `DEFAULT`, and so is a zero
in a `nullzero` field — which between them is how a bun model spells a nullable column. So
`note: ~` on a column with any default, a literal or `now()`, loads as that default and not as NULL.
Every command reports it the same way, under `policy.null_default`. The only field that does write
the NULL is one of a type such as `sql.NullString` with neither tag; if that is how your models spell
nullable columns, set the policy to `warn`.

Since bun v1.2.17 the same holds for an `UPDATE` of a model, `db.NewUpdate().Model(row)`: a nil
pointer and a zero in a `nullzero` field are written as `DEFAULT`. An admin UI that clears such a
field in production leaves the column's default there, not NULL, and `check` reports the row as
drift against a file that says `~`.

### A failed migration is recorded as applied

`migrate.Migrator.Migrate` records a migration in `bun_migrations` **before** it runs it, unless the
migrator was built `WithMarkAppliedOnSuccess(true)`, and leaves the record there when the migration
fails. The record is written in its own statement, outside anything the migration does, so it
survives the migration's rollback. Verified against bun's migrator:

```
markAppliedOnSuccess=false: migrate error=20260101000000: up: the change could not be made; rows recorded in bun_migrations=1
markAppliedOnSuccess=true:  migrate error=20260101000000: up: the change could not be made; rows recorded in bun_migrations=0
```

A recorded migration never runs again. So with bun's default, a data migration that fails because
production was not in the state it expected is lost for good: fix the data, deploy again, and
`migrate` has nothing to do.

A generated migration fails on purpose whenever it cannot do what it says (below), so it has to
handle this. Under bun's migrator, the generated migration first looks for the records the migrator
made of it a moment before: every row of `bun_migrations` with this migration's name, written in the
last minute and newer than every other migration's record (two replicas racing without bun's `Lock`
make one each). If the change set fails, it deletes those rows, and only those, through the
migrator's own `*bun.DB`. A record another replica writes after it looked is not one it found, and is
left alone. The error says so:

```
migrate: 20260921120000: up: …: no row of items has name=anvil. …

bun had recorded migration 20260921120000 as applied before running it, as its migrator
does unless built WithMarkAppliedOnSuccess(true); that record was removed, so the migration runs again
once this is fixed
```

The file registers `fixtureapply.Up(set)` and `fixtureapply.Down(set)`, and `Up` reads the name from
the file that registers it exactly as bun's `Register` does, from the migration's file name, so
renaming the file keeps working. Files written by earlier versions, which register functions calling
`Apply` and `Revert`, keep working: `Apply` finds the name on the call stack. With `WithMarkAppliedOnSuccess(true)` there is no such
record and nothing is deleted. Set `migrations_table` if your migrator uses `WithTableName`.

### Why a generated migration fails at all

A guarded `UPDATE` that matched no row — because the row is missing, or because somebody edited it
here — returns nil unless something looks at the row count. So every zero row count is diagnosed
rather than logged and forgotten, and the four answers are kept apart, because an operator does
something different about each:

| | |
| --- | --- |
| the row already holds these values | nothing to do, not a problem (a second run) |
| no row has that natural key | `policy.missing_row`, default **error** |
| the row is there but was changed here | `policy.changed_row`, default **warn**: their edit is kept |
| the row exists under a different id | `policy.id_drift`, default **error** |

One more, which is not bun's fault but bites in the same place: inside a PostgreSQL transaction, any
failed statement poisons it, and the eventual `COMMIT` then returns the ROLLBACK tag **with no
error**. A diagnostic query whose error is swallowed therefore throws the whole migration away while
reporting success. Verified:

```
diagnostic error (swallowed by the caller): ERROR: function no_such_function() does not exist
commit error: <nil>
probe_t exists after the 'successful' commit: 0
```

Every query this tool runs propagates its error.

## The state file

`generate` diffs against the state file, `<out>/fixture_state.yml`: the fixture files as the
generated migrations leave a database. It rewrites it with every migration, and `baseline` rewrites
it without one. Diffing against git instead loses or doubles a change in two ordinary sequences of
events: committing the fixture edit before generating (HEAD already has it, nothing is generated),
and generating twice before committing (the second migration repeats the first).

Commit it next to the migrations. It carries a checksum and is refused if edited by hand. Two
branches that each generate a migration conflict in it, on purpose;
[the runbook](docs/production.md#the-state-file-conflicts-in-a-merge) says how to merge them.

## Compatibility

- PostgreSQL 12 to 18, each in CI.
- bun v1.2.18, and bun's master branch in a CI job of its own: every claim above about bun is a test
  against bun itself, so a change upstream shows up as a failing test rather than a wrong migration.
- `pgdriver` and `pgx/v5/stdlib` for the run time in your application.
- Go 1.24 or later to build; a supported release for anything you ship.

## Tests

`go test ./...` runs everything that needs no database: the diff, value canonicalisation, the
export, the reports, the scaffold, the rendering and reading back of generated files, the state
file, and the commands against files. The parsers of text that people edit (state files, migration
files, fixture files, numbers) are fuzzed.

The `dbtest` module needs PostgreSQL. It seeds with the real `dbfixture` and checks that an export
loads back as the database it came from; runs every run-time policy and edge case; compiles
generated migrations and runs them under bun's `migrate.Migrator` in both of its modes; runs the
same pipeline over a series of schema variants (composite keys, uuids, trees, reserved words,
another schema); and builds the command and the example project and deploys them.

```
podman run --rm -d -p 55461:5432 -e POSTGRES_PASSWORD=pg --name bfm-test postgres:18
cd dbtest
BUN_FIXTURE_MIGRATE_POSTGRES='postgres://postgres:pg@127.0.0.1:55461/postgres?sslmode=disable' go test ./...
BUN_FIXTURE_MIGRATE_DRIVER=pgx BUN_FIXTURE_MIGRATE_POSTGRES=... go test ./...
podman rm -f bfm-test
```

[CI](docs/ci.md#this-repositorys-own-ci) runs it against every supported PostgreSQL.

## Licence

BSD 2-Clause, like bun.
