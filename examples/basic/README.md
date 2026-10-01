# Example: a bun application with fixture migrations

A small bun project laid out the way the tool expects one: subscription plans, the currency each is
priced in, and the features a plan grants, kept in a dbfixture file and changed through migrations.

```
fixture-migrate.yml                          the tool's configuration
fixtures/fixture.yml                         the master data, loaded by dbfixture
migrations/migrations.go                     the bun migrations package
migrations/20260930160000_schema.up.sql      the schema, as a bun SQL migration
migrations/20260930160000_schema.down.sql
migrations/20260930165255_fixture_plan_prices.go
                                             a fixture migration written by generate
migrations/fixture_state.yml                 what the migrations leave a database holding
main.go, models.go                           the deploy step: migrate, then seed a new database
```

It is its own Go module, so the tool's module does not depend on dbfixture; `replace` points it at
the checkout it sits in.

## The deploy step

`main.go` is what a bun application runs on deploy:

```
export DATABASE_URL='postgres://postgres:secret@localhost:5432/example?sslmode=disable'
go run . migrate     # the pending migrations, then the seed of a new database
go run . status
go run . rollback    # the last group
```

`migrate` runs every pending migration, the schema's and the fixture's alike, in name order. A
fixture migration changes a database that holds the fixture data and leaves one that does not
alone: `plans` is its seed guard table, and on a new database it logs that the table is empty and
succeeds without a change. Then `migrate` seeds a database whose `plans` table is empty by loading
the fixture file as it is now, which already holds every change the fixture migrations would have
made, and moves the sequences past the ids the file names (`fixtureapply.SyncSequences`). So a
deploy is always both steps, in that order, and a new database and an old one end up the same.

The migrator is built `WithMarkAppliedOnSuccess(true)`: a migration that fails is not recorded,
and the next deploy runs it again. Without the option bun records it before running it; a fixture
migration that fails then takes the record back itself.

bun's `Migrator.Lock` does not wait for another deploy: it inserts a row into `bun_migration_locks`
and fails at once while one is there. So `main.go` retries it for `LOCK_WAIT` (a minute by default)
before giving up, and the error then says how to remove a lock a deploy that died left behind;
`bun-fixture-migrate status` reports such a lock too.

## How the fixture migration was made

The fixture file started with two plans. The databases held it, so it was recorded as migrated:

```
bun-fixture-migrate baseline
```

Then the file was edited: the team plan's price, settings and note changed, and a pro plan with an
API quota was added. `status -offline` in CI failed: the file had changes no migration makes. And

```
bun-fixture-migrate generate -name "plan prices"
```

wrote `20260930165255_fixture_plan_prices.go` and moved the state file on. Read it: every change
names the row by its natural key, holds the values it expects to find, and resolves references
(`RefTo("Currency", "USD")`) to ids at run time, because the ids differ between databases.

## Try the commands

With the database migrated and seeded:

```
bun-fixture-migrate status             # every migration applied, nothing not migrated
bun-fixture-migrate check              # the database and the fixture file agree
psql "$DATABASE_URL" -c "UPDATE plans SET price_cents = 2200 WHERE name = 'team'"
bun-fixture-migrate check              # exit 3: the team plan's price differs
bun-fixture-migrate sync               # what sync would change back
bun-fixture-migrate sync -yes          # and does
```

`dbtest/example_test.go` in the repository runs all of this against a real PostgreSQL: a new
database, one that holds the data from before the migration, one where somebody renamed a plan
by hand (the migration stops and stays pending), one where somebody changed a price (the migration
skips that row and says so, and `sync` repairs it), and a rollback.
