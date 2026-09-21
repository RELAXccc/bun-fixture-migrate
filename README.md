# bun-fixture-migrate

Keeps the master data of a [bun](https://github.com/uptrace/bun) application and its
[dbfixture](https://pkg.go.dev/github.com/uptrace/bun/dbfixture) YAML file in step, in both
directions.

`dbfixture` loads a fixture file into an empty database. That is fine for a fresh checkout and
useless for a deployed one: after the first seed, editing the YAML changes nothing, because nobody
re-seeds a production database. So every change to seed data needs a data migration. And if anything
else writes that data — an admin UI, a support script, a hand-run `UPDATE` — the file and the
database drift apart with nothing to say so.

Four commands:

| | |
| --- | --- |
| `scaffold` | write a starter configuration from a live database |
| `export` | write the fixture file from a database |
| `check` | report what the database and the fixture file disagree about, changing nothing |
| `generate` | write the bun migration that closes the gap, from git or from the database |

It never looks at your Go model types. PostgreSQL's catalog knows the tables, the columns and their
types, the column defaults, the primary keys, the unique indexes, the foreign keys and the
sequences; the configuration file says which model lives in which table and which column is a
reference. That is enough for all four.

## Install

```
go install github.com/RELAXccc/bun-fixture-migrate/cmd/bun-fixture-migrate@latest
```

The library your migrations import needs bun and `gopkg.in/yaml.v3`. The command additionally needs
`pgdialect` and `pgdriver` to connect, which any bun PostgreSQL application already has.

## Two things about bun you may not know

Both were found in a production codebase, both had been silently wrong for a long time, and both are
why this tool exists in the shape it does.

### A zero is not written into a column that has a default

`InsertQuery.appendStructValues` writes `DEFAULT` rather than the value whenever the field holds its
zero value and carries a `default:` tag (`query_insert.go`, `marshalsToDefault`). So this model:

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

Your tool cannot read the Go tag, and it does not need to: `pg_attrdef` says the column has a
default other than the type's zero, which is the same condition. Every command checks it.

- `check` and `generate` report a fixture row that writes such a zero.
- `export` marks the value in the file it writes, on the line it happened, and by default refuses to
  write the file at all:

```yaml
      production_max: 0  # ROUND-TRIP HAZARD: the column defaults to 1 and bun writes DEFAULT for a
                         # zero, so loading this file stores 1 here, not 0
```

An export that does not reproduce the database it was taken from is worse than no export.

`policy.zero_default` turns this into a warning or off. It is on by default because the failure is
silent and permanent.

### A migration that did nothing is recorded as done

`migrate.Migrator` marks a migration applied as soon as its function returns nil. So a guarded
`UPDATE` that matched no row — because the row is missing, or because somebody edited it here — is
not merely skipped. It is *lost*: fix the database, deploy again, and the migration is already in
`bun_migrations` and will never run.

So every zero row count is diagnosed rather than logged and forgotten, and by default it fails, which
rolls the transaction back and leaves the migration unrecorded. There are four answers and they are
kept apart, because an operator does something different about each:

| | |
| --- | --- |
| the row already holds these values | nothing to do, not a problem (a second run) |
| no row has that natural key | `policy.missing_row`, default **error** |
| the row is there but was changed here | `policy.changed_row`, default **warn**: their edit is kept |
| the row exists under a different id | `policy.id_drift`, default **error** |

If you use `WithMarkAppliedOnSuccess`, note that it changes *when* a migration is recorded, not
*whether*: a function returning nil is still a success. The check has to be inside the function.

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
Every guess is marked, and so is every column carrying the zero-default hazard above.

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
so a `text` column holding `01` stays a string and a `bool` stays a boolean.

### 3. Check

```
$ bun-fixture-migrate check
In the database, not in the fixture file:
  Plan name=pro

Different in the database and the fixture file:
  Plan name=team
    price_cents: database 2500, file 2000
```

Nothing is written. Exit code 3 when anything was found, so it fits in CI or a deploy gate. It is the
same comparison `generate` makes, read the other way round — a check that answers a different
question from the generator is a check that passes while the generator is about to do something else.

### 4. Generate

Against the previous git revision of the fixture file:

```
$ bun-fixture-migrate generate -name "plan prices"
Plan: 1 insert, 1 update
wrote internal/migrations/20260921120000_fixture_plan_prices.go
read it, then run your migrations
```

Or against the live database, which is the one you want when an admin UI edits master data in
production:

```
$ bun-fixture-migrate generate -from-db -name "master data"
```

`-base <ref>` picks another revision, `-old <file>` skips git, `-dry-run` prints instead of writing,
`-allow-partial` writes what was accepted when something else was refused.

The output is a plain Go file meant to be read:

```go
var fixtureChanges20260921120000PlanPrices = fixturechange.Set{
	Name:           "20260921120000_fixture_plan_prices",
	SeedGuardTable: "plans",
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
- diagnoses every zero row count, as above;
- moves the sequence past any explicit id it wrote into a `serial` table;
- runs everything in one transaction and logs one line per row through `log.Printf` — pass
  `fixtureapply.WithLogger` to send that elsewhere.

`Revert` undoes the set in reverse: the insert becomes a guarded delete, the update swaps its values,
the delete puts the row back.

## Configuration

[`fixture-migrate.example.yml`](fixture-migrate.example.yml) is the whole thing with a comment on
every setting saying what changing it does. `scaffold` writes most of it for you. The short version:

Top level: `fixture`, `out`, `package`, `migrator`, `seed_guard_table`, `database` (a DSN, or
`env:NAME`), `schema`.

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

And the policy block: `id_drift`, `missing_row`, `changed_row`, `zero_default`, `duplicate_key`,
`renames`, `deletes`. Defaults are the strict reading of each. `missing_row` is the one that can lose
a change if you set it wrong, and its comment says so.

Everything else is not configurable, on purpose:

- a reference is always resolved and checked, never left as a subselect;
- an insert is always keyed on the natural key alone;
- a sequence is always moved past an explicit id;
- a model in the fixture file that the configuration does not list is always an error;
- two rows sharing one natural key are always reported, never guessed between;
- a check that could not run is never reported as a check that found nothing.

A few details worth knowing:

- References are resolved through the fixture file itself. A `{{ $.Plan.team.ID }}` template, a plain
  id some row in the file declares, and an explicit `~` all work; anything the file cannot resolve is
  an error rather than a guess.
- A reference column holding `0`, `""` or null points at nothing and stays a literal.
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

In every case the answer is the same: write that one migration by hand, then run the tool again.

## Limitations

- PostgreSQL only, plainly. The catalog queries, the sequence handling and `IS NOT DISTINCT FROM` are
  written for it, and there is no abstraction pretending otherwise.
- Scalar columns only. A mapping or a sequence in the fixture file is an error unless you `ignore` it.
- Only `{{ $.Model.row.Field }}` templates are understood. Any other template is compared as text, so
  a fixture full of `{{ now }}` will produce noise; `ignore` those columns.
- Values travel as text and are bound as parameters, so the database coerces them. Numbers, booleans,
  text and null are covered; exotic column types are untested.
- The zero-default check knows the zero of the numeric, boolean and text types. A column of another
  type with a default is not checked, because this tool will not guess what its zero is.
- `where` is your SQL, inserted as written. Everything else that reaches a query is a plain
  identifier from the catalog or the configuration, checked and quoted.
- A delete is guarded by the whole old row including its id, so on a database whose ids drifted the
  delete is reported as drift rather than deleting the wrong row.
- `export` reads whole tables into memory. It is built for master data — hundreds or thousands of
  rows — not for a data warehouse.

## Tests

`go test ./...` runs the diff, export, scaffold, rendering and validation tests without a database.

The tests that need a real PostgreSQL live in the `dbtest` module and are skipped unless a DSN is set.
They seed with the actual `dbfixture`, export the result and check the export is the file again;
apply a generated change set and compare against a database seeded from the new file; and exercise
every run-time policy, including the one that proves bun stores `1` where the fixture said `0`.

```
podman run --rm -d -p 55461:5432 -e POSTGRES_PASSWORD=pg --name bfm-test postgres:18
cd dbtest
BUN_FIXTURE_MIGRATE_POSTGRES='postgres://postgres:pg@127.0.0.1:55461/postgres?sslmode=disable' go test ./...
podman rm -f bfm-test
```

## Licence

BSD 2-Clause, like bun.
