# bun-fixture-migrate

Keeps the master data of a [bun](https://github.com/uptrace/bun) application and its
[dbfixture](https://pkg.go.dev/github.com/uptrace/bun/dbfixture) YAML file in step, in both
directions, and manages the data migrations that do it.

`dbfixture` loads a fixture file into an empty database. That is fine for a fresh checkout and
useless for a deployed one: after the first seed, editing the YAML changes nothing, because nobody
re-seeds a production database. So every change to seed data needs a data migration. And if anything
else writes that data — an admin UI, a support script, a hand-run `UPDATE` — the file and the
database drift apart with nothing to say so.

| | |
| --- | --- |
| `scaffold` | write a starter configuration from a live database |
| `export` | write the fixture file from a database |
| `check` | report what the database and the fixture file disagree about, changing nothing |
| `generate` | write the bun migration that closes the gap, from the last migration's state or from the database |
| `baseline` | record the fixture file as migrated without writing a migration |
| `status` | list the migrations, which ones a database applied, and what no migration covers yet |
| `plan` | run the pending fixture migrations against a database and roll back, reporting every row |

`bun-fixture-migrate version` says which build you have; every command takes `-h`.

It never looks at your Go model types. PostgreSQL's catalog knows the tables, the columns and their
types, the column defaults, the primary keys, the unique indexes, the foreign keys and the
sequences; the configuration file says which model lives in which table and which column is a
reference. That is enough for all of it.

## Install

```
go install github.com/RELAXccc/bun-fixture-migrate/cmd/bun-fixture-migrate@latest
```

The library your migrations import needs bun and `gopkg.in/yaml.v3`. The command additionally needs
`pgdialect` and `pgdriver` to connect, which any bun PostgreSQL application already has.

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
handle this. When `fixtureapply.Apply` fails under bun's migrator, it deletes the record the migrator
made of it a moment before: the newest row of `bun_migrations`, only if it carries this migration's
name and was written within the hour, and only through the migrator's own `*bun.DB`. The error says
so:

```
migrate: 20260921120000: up: …: no row of items has name=anvil. …

bun had recorded migration 20260921120000 as applied before running it (the migrator was not built
WithMarkAppliedOnSuccess(true)); that record was removed, so the migration runs again once this is fixed
```

It reads the name off the call stack exactly as bun's `Register` does, from the migration's file
name, so renaming the file keeps working. With `WithMarkAppliedOnSuccess(true)` there is no such
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

## Getting started

### 1. Scaffold

The adoption cost of a tool like this is describing your schema a second time. You do not have to:

```
$ export DATABASE_URL=postgres://localhost/myapp?sslmode=disable
$ bun-fixture-migrate scaffold -o fixture-migrate.yml
wrote fixture-migrate.yml
read it: the natural keys and the model names are guesses
```

It reads the catalog and writes a commented configuration: the tables, the model names, the primary
keys, which ids come from a sequence, the foreign keys as `references`, the column defaults as
`defaults`, and the natural key guessed from the narrowest unique index that is not the primary key.
Every guess is marked, and so is every column carrying one of the hazards above. It does not
overwrite an existing file.

Then read it. The model names come from a crude singulariser (`status` becomes `Statu`), and a table
with no unique index besides its primary key gets a natural key the tool had to invent — which is
itself worth knowing, because without a unique index the database cannot stop a duplicate appearing
and no guard is reliable.

### 2. Export

```
$ bun-fixture-migrate export
wrote fixtures/fixture.yml
```

```yaml
# Exported by bun-fixture-migrate from a live database on 2026-09-21T02:29:23Z.
# Models are in dependency order; references name the row they point at, not its id.

- model: Currency
  rows:
    - _id: eur
      id: 1
      code: "EUR"
      symbol: "€"

- model: Plan
  rows:
    - _id: team
      id: 2
      name: "team"
      currency_id: '{{ $.Currency.eur.ID }}'
      price_cents: 2000
      seats: 10
      note: "popular"
```

Models come out in dependency order, so `dbfixture` resolves every reference as it reads the file top
to bottom. A foreign key comes out as the template naming the target's row, never as the raw id,
because the ids of the database it came from mean nothing in another one. `_id` anchors are made from
the natural key. Values are written in the notation their column type reads back as the same value,
so a `text` column holding `01` stays a string and a `bool` stays a boolean. A value `dbfixture`
would evaluate as a template is refused rather than written. The file is replaced atomically, so an
interrupted export never leaves half a fixture file for the next seed.

### 3. Baseline

```
$ bun-fixture-migrate baseline
wrote internal/migrations/fixture_state.yml: generate now diffs against fixtures/fixture.yml
```

This records that the databases hold the fixture file as it is. Run it once, when the deployed
databases match the file, or name the revision they match with `-from <git rev>`. See
[the state file](#the-state-file) for why.

### 4. Check

```
$ bun-fixture-migrate check
In the database, not in the fixture file:
  Plan name=pro

Different in the database and the fixture file:
  Plan name=team
    price_cents: database 2500, file 2000
```

Nothing is written. Exit code 3 when anything was found, so it fits in CI or a deploy gate; `-json`
writes the same report for a program, with NULL as `null` and a reference as an object, so neither can
be mistaken for a string. It is the same comparison `generate` makes, read the other way round — a
check that answers a different question from the generator is a check that passes while the
generator is about to do something else.

### 5. Generate

Edit the fixture file, then:

```
$ bun-fixture-migrate generate -name "plan prices"
Plan: 1 insert, 1 update
wrote internal/migrations/20260921120000_fixture_plan_prices.go
wrote internal/migrations/fixture_state.yml
read it, run plan against a copy of production, then deploy
```

It diffs against the state file — what the existing migrations leave a database in — and writes the
state file again. Or against the live database, which is the one you want when an admin UI edits
master data in production:

```
$ bun-fixture-migrate generate -from-db -name "master data"
```

`-base <rev>` diffs against a git revision instead, `-old <file>` against a file, `-dry-run` prints
instead of writing, `-allow-partial` writes what was accepted when something else was refused.

The migration is dated one second after the newest migration in the directory if the clock says
otherwise, because it was generated against the state all of them leave, and bun runs pending
migrations in name order; it never takes a name another migration has. It refuses a directory
whose Go package is not the configured one, since the file would not compile there.

The output is a plain Go file meant to be read:

```go
var fixtureChanges20260921120000PlanPrices = fixturechange.Set{
	Name:            "20260921120000_fixture_plan_prices",
	SeedGuardTable:  "plans",
	MigrationsTable: "bun_migrations",
	Tables: fixturechange.Tables{
		"Currency": {Name: "currencies", ID: "id", Key: "code"},
		"Plan":     {Name: "plans", ID: "id", Serial: true},
	},
	Policy: fixturechange.Policy{MissingRow: "error", ChangedRow: "warn", IDDrift: "error"},
	Changes: []fixturechange.Change{
		{Model: "Plan", Kind: fixturechange.Insert,
			Key: fixturechange.Values{"name": fixturechange.Lit("pro")},
			New: fixturechange.Values{
				"currency_id": fixturechange.RefTo("Currency", "USD"),
				"id":          fixturechange.Lit("3"),
				"name":        fixturechange.Lit("pro"),
				"price_cents": fixturechange.Lit("9000"),
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

Note what is not in there: no ids in the `Key` maps, and `currency_id` is a name, not a number. The
migration finds rows by the columns you called the natural key and resolves `RefTo("Currency", "USD")`
against the database it runs on. Ids drift between databases; names do not. The policy is written
into the file, so changing the configuration later does not quietly change what an old migration does.

It registers itself with your migrator and runs with the rest of the chain. At run time
`fixtureapply`:

- does nothing at all while `seed_guard_table` is empty, because that database has not been seeded
  yet and `dbfixture` will load the new state by itself;
- resolves every reference to a real id first, and fails if one matches no row or more than one — a
  bare subselect in the statement would write a silent `NULL` foreign key and report a row affected;
- inserts only when no row with that natural key exists, keyed on the natural key **alone**: a row
  already there under a different id is somebody else's row, and inserting a second copy either trips
  a unique index or leaves two rows no later lookup can tell apart;
- checks before an insert with an explicit id whether another row already holds it, so the message
  names that row instead of arriving as a primary-key violation;
- updates and deletes only while the row still holds the values the base state had, so a hand edit
  survives and a second run is a no-op;
- diagnoses every zero row count, as above, and takes back bun's record of a migration that failed;
- moves the sequence past any explicit id it wrote into a `serial` table;
- runs everything in one transaction and logs one line per row through `log.Printf` — pass
  `fixtureapply.WithLogger` to send that elsewhere, or `fixtureapply.WithReport` to receive every
  row's outcome as a value.

`Revert` undoes the set in reverse: the insert becomes a guarded delete, the update swaps its values,
the delete puts the row back.

### 6. Plan

Before a deploy, ask a copy of production — or production itself — what the migrations will do
there:

```
$ bun-fixture-migrate plan
20260921120000_fixture_plan_prices: would succeed
  applied  Plan name=pro insert (1 row)
  skipped  Plan name=team update [changed row]: plans name=team no longer holds the values this change was generated against, so somebody changed it in this database. It was left alone. Compare it with the fixture file and decide which one is right

rolled back: nothing was changed, except that a sequence an insert drew from stays where it was moved to, which only leaves a gap in the ids
```

It reads `bun_migrations`, takes every fixture migration in the directory that is not applied yet,
reads each file back — the change set as written, including your edits — and runs them in bun's
order inside one transaction that is always rolled back, stopping at the first failure as the
migrator would. A migration that would fail is exit code 3; `-strict` makes a skipped row one too.
`-file` plans named files whether applied or not, `-json` writes the report for a program, and
`-lock-timeout` (default 5s) keeps it from queueing behind a production lock. Pending migrations this
tool did not write — schema changes, backfills — cannot be simulated and are named, including
before which fixture migration they would run.

### 7. Status

```
$ bun-fixture-migrate status
fixture file  fixtures/fixture.yml
state file    internal/migrations/fixture_state.yml, written by 20260921120000_fixture_plan_prices
not migrated  Plan: 1 update
              run: bun-fixture-migrate generate -name <what changed>

migrations in internal/migrations, applied according to bun_migrations
  applied  20260920000000_add_plans              group 1, 2026-09-20 10:00:00
  pending  20260921120000_fixture_plan_prices  fixture, 2 changes
the fixture file has changes no migration makes
```

Exit code 3 when the fixture file changes something no migration makes, or when the directory holds
two migrations bun would record under one name — one of them would never run, and bun's `Discover`
only catches that between two SQL files. `-offline` skips the database, `-require-applied` fails
unless the database has applied every migration in the directory, `-json` writes it for a program.

## The state file

`generate` used to diff against git's HEAD. Two ordinary sequences of events make that lose or
double a change without a word:

- **Commit the fixture edit, then generate.** HEAD already holds the edit, there is nothing to
  generate, and the change never reaches a seeded database.
- **Generate twice before committing.** The second migration carries the first one's change again,
  guarded by the value the first one has since replaced, so on a database where the first ran it is
  skipped as somebody's edit.

The state file — `<out>/fixture_state.yml` unless `state` says otherwise — is the fixture file as the
generated migrations leave a database. `generate` diffs against it and rewrites it with every
migration it writes; `baseline` rewrites it without one. Commit it next to the migrations. It carries
a SHA-256 of its content and is refused if it was edited by hand; line-ending conversion by git is
not an edit. Without a state file, `generate` still diffs against HEAD and says so.

`baseline` refuses to record a state whose content differs from the current one unless you pass
`-force`, because recording a change nobody migrated is exactly how one gets lost. The legitimate
case is a change `generate` refused and you migrated by hand: write that migration, then
`baseline -force`.

Two branches that each generate a migration both rewrite the two header lines of the state file, so
their merge conflicts there, on purpose. Keep both migrations, run `plan` against a copy of
production to see that they do not touch the same rows, then run `baseline` on the merged fixture
file.

## In production

A pipeline that uses all of it:

1. **In CI, on every pull request:** `bun-fixture-migrate status -offline`. It fails a change to the
   fixture file that comes without its migration, and a migrations directory bun would run wrongly.
   Review the generated file like any other code.
2. **Before a deploy:** `bun-fixture-migrate plan -strict` against a recent copy of production. What
   fails or skips there fails or skips in the deploy.
3. **The deploy:** your own migrator, as always. A fixture migration that fails rolls back and is not
   left recorded, whichever way the migrator is built.
4. **After it:** `bun-fixture-migrate status -require-applied` and `bun-fixture-migrate check`. The
   second is also worth running on a schedule where an admin UI edits master data, followed by
   `generate -from-db` to bring the file back in line.

What the database side does to your database:

- `export`, `check`, `status` and `scaffold` read inside one `REPEATABLE READ, READ ONLY`
  transaction. PostgreSQL itself refuses a write there, even one hidden in a `where` clause, and every
  table is read from the same snapshot.
- `plan` writes inside one transaction and always rolls it back. It holds row locks on what it
  touches until then, which is a moment; a sequence it draws from stays advanced, which only leaves a
  gap in the ids.
- Every connection carries `application_name=bun-fixture-migrate` unless the DSN sets one, so it is
  recognisable in `pg_stat_activity`.
- A DSN that is not a URL pgdriver can read is refused with a sentence, never a panic, and no message
  repeats a password.
- `SIGINT` and `SIGTERM` cancel the running query and roll its transaction back.

## Configuration

[`fixture-migrate.example.yml`](fixture-migrate.example.yml) is the whole thing with a comment on
every setting saying what changing it does. `scaffold` writes most of it for you. The short version:

Top level: `fixture`, `out`, `package`, `migrator`, `migrations_table`, `state`,
`seed_guard_table`, `database` (a DSN, or `env:NAME`), `schema`.

Per model, under `models:` — the key is the model name as the fixture file spells it, and every model
in the file must be listed or the tool stops:

| Key | Meaning |
| --- | --- |
| `table` | the SQL table, optionally schema-qualified. Required |
| `id` | primary-key column, default `id`. Never compared, never updated, written on an insert when the row has one |
| `ref` | the column a reference to this model matches on, default `name` |
| `serial` | the id comes from a sequence |
| `key` | the columns that identify a row without its id, default `[ref]` |
| `key_any_of` | groups of mutually exclusive columns; the first one in a group that is set joins the key |
| `references` | column to model, for columns holding another row's id |
| `derived` | columns your application recalculates; never compared, written or exported |
| `ignore` | columns that take no part at all |
| `defaults` | what a column means when the row leaves it out |
| `deletes` | `allow` or `refuse`, overriding `policy.deletes` |
| `where` | an SQL predicate limiting which rows of the table are master data |

And the policy block: `id_drift`, `missing_row`, `changed_row`, `zero_default`, `null_default`,
`duplicate_key`, `renames`, `deletes`. Defaults are the strict reading of each. `missing_row` is the
one that can lose a change if you set it wrong, and its comment says so. A value none of them knows
is refused by name rather than read as the nearest one: the generated migration carries its own copy
of the three run-time settings, and a typo in a file nobody reads again would quietly decide what
that migration does when a database is not in the state it expected.

Everything else is not configurable, on purpose:

- a reference is always resolved and checked, never left as a subselect;
- an insert is always keyed on the natural key alone;
- a sequence is always moved past an explicit id;
- a model in the fixture file that the configuration does not list is always an error;
- two rows sharing one natural key are always reported, never guessed between;
- a check that could not run is never reported as a check that found nothing.

## How the fixture file is read

Exactly as `dbfixture` reads it, because a file this tool accepts has to be a file `dbfixture` loads,
and a reference has to name the row `dbfixture` binds it to. Each rule is checked against the real
loader in `dbtest`:

- A template is `{{ $.Model.row.Field }}` with the spaces: `dbfixture` evaluates only values holding
  `{{ ` and ` }}`, so `{{$.Model.row.ID}}` is text to both.
- A row is named by its `_id`, and a row without one by `pk` and its primary key — `{{ $.Plan.pk3.ID }}`.
- Templates resolve in file order, against the rows above: a reference to a row further down is
  refused, because `dbfixture` cannot load it, and when two rows share an anchor each template names
  the latest one above it.
- Any other template — `{{ now }}`, a function call — is evaluated by `dbfixture` when it loads the
  file, so the database never holds its text. It is refused rather than compared or written into a
  migration; `ignore` the column.
- A plain id in a reference column resolves through the row of the file that declares that id; an
  explicit `~` is NULL. A reference column holding `0`, `""` or null points at nothing and stays a
  literal.
- An omitted column with no entry in `defaults` is "not set". Two not-set columns are equal; a column
  set on one side and not on the other is refused, because the tool would have to invent what the
  missing one means.
- Comparison is always literal. It is tempting, once the tool knows the column defaults, to substitute
  a default for a value on the file side — that is exactly the bug in the first section, and it
  produces both phantom changes and false negatives. The file is the truth; a value bun would not
  write is a fault in the file, and the lint says so.
- When comparing against a database, only the columns the fixture file mentions are read. A column no
  fixture row writes is not master data and a difference in it is not drift.

## What it refuses

Each of these ends up in the output with the model, the row and a reason, and nothing is written
unless you pass `-allow-partial`. The exit code is 2.

- **Renames** — the same id under a different natural key. An insert plus a delete is not a rename:
  rows elsewhere point at the old one, and so does whatever knows the old name outside the database.
  Both halves are dropped, along with every change that points at the renamed row, including a new
  row taking the name being vacated. Set `policy.renames: update` and it becomes what it actually is,
  an `UPDATE` of the key columns guarded by the id, placed before everything else in the migration;
  rows whose own key is built from a reference to the renamed one follow along without a statement of
  their own. Two rows swapping names is still refused: one of them has to be parked under a third
  name first.
- **Renumbered primary keys** — the same natural key under a different id. `policy.id_drift`.
- **Deletes of a model marked `deletes: refuse`.** Whether the rows pointing at it should cascade, be
  repointed or block the delete is a decision about your data.
- **Rows whose natural key is not unique**, once the group changes. Two rows with the same key cannot
  be told apart by a `WHERE` clause. An unchanged duplicate group costs nothing and is left alone;
  `check` and `export` report every one of them with the colliding ids.
- **Columns that appear on one side only** with no configured default.
- **A model the configuration does not list.** Silence is never the right answer to an unknown model:
  a model in the file that the tool never visits is a change that quietly does not happen.

In every case the answer is the same: write that one migration by hand, run `baseline -force`, and
carry on.

## Limitations

- PostgreSQL 12 or later, plainly. The catalog queries, the sequence handling and
  `IS NOT DISTINCT FROM` are written for it, and there is no abstraction pretending otherwise.
- Scalar columns only. A mapping or a sequence in the fixture file is an error unless you `ignore` it.
- Only `{{ $.Model.row.Field }}` templates are understood, and the field is mapped to a column by
  bun's default naming. A template naming a field whose column is spelled otherwise is an error, not
  a guess.
- Values travel as text. They never become part of the SQL this tool writes: they are passed as
  arguments, which bun quotes and escapes with the dialect's rules, and the database coerces them to
  the column's type. Numbers, booleans, text and null are covered; exotic column types are untested.
- The zero-default check knows the zero of the numeric, boolean and text types. A column of another
  type with a default is not checked, because this tool will not guess what its zero is.
- `where` is your SQL, inserted as written. Everything else that reaches a query is a plain
  identifier from the catalog or the configuration, checked and quoted.
- A delete is guarded by the whole old row including its id, so on a database whose ids drifted the
  delete is reported as drift rather than deleting the wrong row.
- `status` and `plan` read the migrations directory's top level, which is where a bun migrations
  package keeps them, and read a generated file back from its syntax: a change set built by code
  rather than written out as a literal is reported, not guessed at.
- `export` reads whole tables into memory. It is built for master data — hundreds or thousands of
  rows — not for a data warehouse.

## Compatibility

Every claim above about bun is checked by a test against bun itself: the `DEFAULT` for a zero and for
a null, the migrator's record of a failed migration in both of its modes, and each rule of how
`dbfixture` reads a file. The suite runs in CI against the pinned bun release and, in a job of its
own, against bun's master branch, so a change upstream shows up as a failing test rather than as a
wrong migration. As of this writing both pass: bun v1.2.18, and master at `691d97d`
(2026-09-30).

## Tests

`go test ./...` runs everything that needs no database: the diff, the export, the report the `check`
command prints, the scaffold, the rendering and reading back of a generated file, the state file,
the validation, the SQL the database side builds, and the commands themselves — `generate`,
`baseline` and `status -offline` against files are the whole pipeline and never connect.

The tests that need a real PostgreSQL live in the `dbtest` module and are skipped unless a DSN is set.
They seed with the actual `dbfixture`, export the result and check the export is the file again;
apply a generated change set and compare against a database seeded from the new file; exercise every
run-time policy; compile a generated migration and run it with bun's own `migrate.Migrator`, in both
of its modes, against a database where it has to fail; and build the command and drive it through a
fixture change from baseline to deploy.

```
podman run --rm -d -p 55461:5432 -e POSTGRES_PASSWORD=pg --name bfm-test postgres:18
cd dbtest
BUN_FIXTURE_MIGRATE_POSTGRES='postgres://postgres:pg@127.0.0.1:55461/postgres?sslmode=disable' go test ./...
podman rm -f bfm-test
```

## Licence

BSD 2-Clause, like bun.
