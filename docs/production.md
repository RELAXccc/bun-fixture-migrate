# Production runbook

What to run before, during and after a deploy, and what to do when something goes wrong. Each
situation below names the symptom, the cause and the steps. Every command that only reads works
against production itself: it runs in a transaction PostgreSQL holds to `READ ONLY`.

- [The pipeline](#the-pipeline)
- [What touches production, and how](#what-touches-production-and-how)
- [A fixture migration failed during a deploy](#a-fixture-migration-failed-during-a-deploy)
- [A change was skipped](#a-change-was-skipped)
- [check reports drift](#check-reports-drift)
- [generate refused a change](#generate-refused-a-change)
- [The state file conflicts in a merge](#the-state-file-conflicts-in-a-merge)
- [The state file was edited or lost](#the-state-file-was-edited-or-lost)
- [Rolling back](#rolling-back)
- [Running a migration by hand](#running-a-migration-by-hand)
- [Adopting the tool on an existing project](#adopting-the-tool-on-an-existing-project)
- [A new environment](#a-new-environment)

## The pipeline

| When | Command | Fails on |
| --- | --- | --- |
| every change | `status -offline` | a fixture edit without its migration; two migrations bun would record under one name |
| before a deploy | `plan -strict` against a recent copy of production | a migration that would fail or skip a change |
| the deploy | your migrator, then the seed of an empty database | a fixture migration that cannot do what it says |
| after it | `status -require-applied` | a migration the database did not apply |
| after it, and on a schedule | `check` | drift between the database and the fixture file |

[CI](ci.md) has ready-made jobs for all of them. `plan` against production itself is safe too:
it rolls back, holds row locks only for the moment it runs, gives up on a lock after
`-lock-timeout` (5s), and is refused against a standby.

## What touches production, and how

| | |
| --- | --- |
| `export`, `check`, `status`, `scaffold`, `generate -from-db` | read, in one `REPEATABLE READ, READ ONLY` transaction |
| `plan` | writes in one transaction and always rolls it back. A sequence an insert drew from stays advanced, which only leaves a gap in the ids; a sequence the migration would move is reported, not moved |
| `sync` | writes, with `-yes`. Meant for databases that are not deployed to |
| a fixture migration | writes, in one transaction, under a transaction-scoped advisory lock |

Every connection the command opens carries `application_name=bun-fixture-migrate` unless the DSN
sets one. `SIGINT` and `SIGTERM` cancel the running query and roll back.

## A fixture migration failed during a deploy

**Symptom.** The migrator returns an error naming the migration, the model, the natural key and a
problem, for example:

```
20260930165255: up: 20260930165255_fixture_plan_prices: Plan name=team update: no row of plans has
name=team. The row this change updates is not in the database, so the change cannot be made. Put the
row back, or drop this change from the migration
```

Under a migrator built without `WithMarkAppliedOnSuccess(true)` it goes on:

```
bun had recorded migration 20260930165255 as applied before running it, as its migrator
does unless built WithMarkAppliedOnSuccess(true); that record was removed, so the migration runs again
once this is fixed
```

**What happened.** The migration found the database in a state it was not generated against, and the
policy makes that an error. Its transaction rolled back: nothing it did is left. It is not recorded
as applied, whichever way the migrator is built, so the next deploy runs it again.

**Steps.**

1. Run `bun-fixture-migrate plan` against the database. It shows every change of the pending
   migrations and which ones cannot be made, without changing anything.
2. Decide per problem:
   - **missing row**: the row the change updates or deletes is not there. Somebody deleted or
     renamed it, or renamed a row its natural key points at, which the message then names. Put it
     back, or, if its absence is right, remove that change from the migration file (it is a plain Go
     literal) and run `plan` again.
   - **id drift**: the row is there under another id than the file says, or the file's id belongs
     to another row. Something outside the database may name these ids; find out before touching
     them.
   - **referenced**: a delete would reach rows of other tables through a foreign key. Decide what
     should happen to them, then either delete or repoint them in a migration of your own, or set
     the model's `deletes: cascade` and generate again.
   - **changed row** under `changed_row: error`: see [a change was skipped](#a-change-was-skipped).
3. Deploy again. The migration runs from the start, in a new transaction.

If the error instead says bun's record **could not** be removed, delete the row it names from the
migrations table by hand before the next deploy, or the migration will not run again.

## A change was skipped

**Symptom.** The migration succeeded and logged a line such as
`skipped Plan name=team update [changed row]: ... it was changed in this database, or by a migration
that ran before this one. It was left alone.`

**What happened.** The row no longer held the values the migration expected: somebody edited it in
this database, or a migration that ran before this one changed it, as when two branches each
generated a migration for the same row and the older one was merged last. Under `changed_row: warn`,
the default, the row as it is wins and the migration moves on. The migration is recorded as applied; that change will not be attempted again.

**Steps.** Run `check`. It lists the row with the database's and the file's values side by side.
Then decide which one is right:

- **the database is right**: take its values into the file (`export`, or edit the file), then
  `generate`, which writes a migration that brings the other databases to them;
- **the file is right**: `generate -from-db -name "..."` against this database writes a migration
  that makes it match the file, or on a database that is not deployed to, `sync -yes`.

## check reports drift

**Symptom.** `check` exits 3 and lists rows only the database has, rows only the file has, and rows
that differ, with both values.

**Causes**, most common first: an admin UI or a script edited master data here; a migration skipped
a change (see above); a migration was never deployed here (`status -require-applied` says so); the
file was edited without a migration (`status -offline` says so).

**Steps.** As for a skipped change: decide which side is right, then either `export` and `generate`,
or `generate -from-db`. Read the findings too: `invalid value` is a file value the column's type
cannot hold, `duplicate key` two rows no guard can tell apart; both need fixing before any
migration of that model can be trusted.

`check -json` is the form to alert on. `agree` is the one field to test; `changes` has the rows.

## generate refused a change

**Symptom.** `generate` exits 2 with lines beginning `refused:`, and writes nothing.

**What happened.** The difference cannot be written as a guarded, reviewable migration without a
decision only you can make: a rename, a renumbered id, a delete of a model marked `deletes: refuse`,
a natural key two rows share, a column one side sets and the other leaves out with no default.
[Fixture files](fixture-files.md#what-it-refuses) explains each.

**Steps.**

1. Often the configuration can say it: `renames: update`, a `defaults` entry, `deletes: cascade` on
   the model. Then generate again.
2. Otherwise write that one migration by hand, in the same package, dated after the last one, and
   `generate -allow-partial` for the rest if there is any.
3. Record that your migration covers it: `bun-fixture-migrate baseline -force`.
4. `plan` against a copy of production, as always.

## The state file conflicts in a merge

**Symptom.** Two branches each generated a fixture migration; merging them conflicts in
`fixture_state.yml`, on its `migration:` and `sha256:` lines.

**What happened.** On purpose: each state file records the fixture file after its own branch's
migration, and neither is right for the merge.

**Steps.**

1. Keep both migrations. Resolve the conflict in the fixture file itself as for any file.
2. Check that the two migrations do not change the same rows: `plan` against a copy of production
   runs both in bun's order.
3. Take either side of the state file, then record the merged fixture file:
   `bun-fixture-migrate baseline -force`. `-force` is right here because both migrations exist.
4. `status -offline` must now report nothing not migrated. If it does, the merge changed something
   neither migration makes; `generate` it.

The two migrations run in name order, which is not necessarily the order they were written in. When
both touch the same row, the later name's guards expect the values the state file of its own branch
had; `plan` says whether that holds.

## The state file was edited or lost

**Symptom.** A command refuses the state file: its checksum does not match, or its marker is gone.

**What happened.** It was edited by hand or merged line by line. Line-ending conversion by git is not
an edit and is accepted. A state nobody can vouch for would let `generate` write a migration against
the wrong base, so it is refused.

**Steps.** Find the fixture file as the last migration left it. When the last commit that touched
the state file also added that migration, it is the fixture file at that commit:
`bun-fixture-migrate baseline -from <commit> -force`. If you cannot tell, `export` from a database
that applied every migration (`status -require-applied`) and baseline that.

## Rolling back

`migrator.Rollback` runs each migration's down function. For a fixture migration that is `Revert`:
the changes backwards, every one inverted and guarded like the original, so a rollback that finds a
row changed since the migration ran reports it rather than overwriting it. A rename is looked up
under the name it gave the row, and named back.

A rolled-back migration is pending again, and the next deploy runs it again. To undo the change for
good, revert the commit that brought the migration, the fixture edit and the state file, together,
before that deploy.

Rolling back is the exception. A fixture change that turned out wrong is usually better fixed
forwards: correct the file and generate a new migration.

## Running a migration by hand

To see what any fixture migration does against any database, applied or not:

```
bun-fixture-migrate plan -file internal/migrations/20260930165255_fixture_plan_prices.go
```

To apply one outside the migrator, write a small program that calls
`fixtureapply.Apply(ctx, db, theSet)` with the set from the file, and insert its row into the
migrations table the way bun would, or the migrator will run it again (harmlessly: every change will
report `unchanged`).

## Adopting the tool on an existing project

1. `bun-fixture-migrate scaffold -o fixture-migrate.yml` against a database that holds the master
   data, then read every guess it marks.
2. `bun-fixture-migrate export` from production, or keep your existing fixture file and run `check`
   against production until they agree.
3. `bun-fixture-migrate baseline`: the databases hold the file, record that.
4. Add `status -offline` to CI.

From then on every fixture edit comes with a generated migration. Databases that were never seeded
from the file still need to agree with it before the first migration: `check` each one.

## A new environment

A new database gets the schema from the migrations and the data from the fixture file:

1. run the migrator; the fixture migrations see an empty `seed_guard_table` and do nothing;
2. load the fixture file with `dbfixture`, in a transaction;
3. `fixtureapply.SyncSequences` on the seeded tables.

[Using it with bun](usage.md#deploying-migrate-then-seed) has the code. The result is the same
database an old environment reaches by running every fixture migration, which `check` confirms.
