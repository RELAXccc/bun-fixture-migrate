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
- [Every migrate fails: the migrations table is already locked](#every-migrate-fails-the-migrations-table-is-already-locked)
- [Rolling back](#rolling-back)
- [Running a migration by hand](#running-a-migration-by-hand)
- [Adopting the tool on an existing project](#adopting-the-tool-on-an-existing-project)
- [A new environment](#a-new-environment)

## The pipeline

| When | Command | Fails on |
| --- | --- | --- |
| every change | `status -offline` | a fixture edit without its migration; a change `generate -allow-partial` left out and nobody migrated; a fixture migration from another branch the state file does not include; two migrations bun would record under one name |
| before a deploy | `plan -strict` against a recent copy of production | a migration that would fail or skip a change |
| the deploy | your migrator, then the seed of an empty database | a fixture migration that cannot do what it says |
| after it | `status -require-applied` | a migration the database did not apply; the lock of a migrator that died, which fails the next deploy |
| after it, and on a schedule | `check` | drift between the database and the fixture file |

[CI](ci.md) has ready-made jobs for all of them. `plan` against production itself is safe too:
it rolls back, gives up on a lock after `-lock-timeout` (5s), and is refused against a standby. It
does hold the rows its changes touch locked until it rolls back, and says how many and for how long;
a change set of thousands of rows can hold application writes to those rows for seconds, so plan a
large one against a copy, or off-peak.

## What touches production, and how

| | |
| --- | --- |
| `export`, `check`, `status`, `scaffold`, `generate -from-db` | read, in one `REPEATABLE READ, READ ONLY` transaction |
| `plan` | writes in one transaction and always rolls it back. A sequence an insert drew from stays advanced, which only leaves a gap in the ids; a sequence the migration would move is reported, not moved. Under `-with-sql`, a SQL migration's own `setval` or `nextval` is not rolled back either, because PostgreSQL's sequences are not transactional; plan notes such a migration |
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
row back; or, if it is meant to be gone in this database, set MissingRow to "warn" in this migration's
Policy, and the change is recorded as done here without being made
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
     back, and deploy again. If its absence is right in this database only, set
     `MissingRow: "warn"` in the `Policy` of that one migration file and run `plan` again: here the
     change is then skipped and the migration recorded, every other database still gets it, and
     `check` here reports the row the file has and this database does not, which is what you
     decided. Do not remove the change from the migration: the databases that have not run it yet
     would never get it, and `status -offline` cannot see that, because the fixture file still has
     it.
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
2. Otherwise `generate -allow-partial` writes the rest, and records what it refused in the state
   file as left out. The next `generate` does not see those changes again; `status` lists them and
   fails until step 4.
3. Write the migration for what was left out by hand, in the same package, dated after the last one.
4. Record that your migration covers it: `bun-fixture-migrate baseline -force`.
5. `plan` against a copy of production, as always.

## The state file conflicts in a merge

**Symptom.** Two branches each generated a fixture migration; merging them conflicts in
`fixture_state.yml`. Or, when the conflict was resolved by taking one side, `status -offline` fails
with `... is a generated fixture migration whose changes the state file does not include`, and
`generate` and `baseline -force` refuse to go on.

**What happened.** Both migrations were generated against the same state, and each one's guards
expect the rows as that state has them. Whichever runs second finds the rows the other changed. When
both change one row, a database that applied the newer migration before the older one arrived skips
the older one's change as somebody's edit, and a database that runs both from the start ends with the
other value: production and a new environment disagree, and neither says so. Keeping both migrations
and recording the merge with `baseline -force` therefore does not work, and is refused.

**Steps.** One of the two migrations is generated again, on top of the other:

1. Resolve the fixture files as for any file: they say what the master data is after the merge.
2. Keep the migration a database already applied, usually the one merged first. Delete the other
   branch's migration file; it must not have run anywhere that matters (`status` against a database
   lists what it applied).
3. Take the state file as the kept migration left it: when merging the other branch into yours,
   `git checkout --ours -- internal/migrations/fixture_state.yml`.
4. `bun-fixture-migrate generate -name "..."`. It writes the deleted migration's changes, and anything
   the merge resolved differently, as a migration from what the kept one leaves, named after it.
5. Check the result where it can go wrong. `status -offline` must report nothing. Then
   `plan -strict` against a copy of a database that applied the kept migration, usually production:
   it must succeed, every change of the new migration `applied` and none `skipped`. When both
   branches added rows, check before step 4 that they do not share an explicit id, and give one of
   them another id in the fixture file if they do.

If a database did apply the deleted migration, it is listed as recorded but not in the directory,
and the new migration finds its changes made there (`unchanged`). If both migrations were deployed
to different databases before the merge, those databases have already diverged: after the next
deploy, run `check` against each and bring it to the fixture file with `generate -from-db`, or with
`sync` where nothing deploys to it.

A migration merged after a later-named one was deployed runs after it, whatever its name: bun runs
every migration it has no record of. `status` against such a database marks it `out of order`;
`status -strict-order` fails on it.

## The state file was edited or lost

**Symptom.** A command refuses the state file: its checksum does not match, or its marker is gone.
(For conflict markers, see [above](#the-state-file-conflicts-in-a-merge).)

**What happened.** It was edited by hand or merged line by line. Line-ending conversion by git is not
an edit and is accepted. A state nobody can vouch for would let `generate` write a migration against
the wrong base, so it is refused.

**Steps.** Find the fixture file as the last migration left it. When the last commit that touched
the state file also added that migration, it is the fixture file at that commit:
`bun-fixture-migrate baseline -from <commit> -force`. If you cannot tell, `export` from a database
that applied every migration (`status -require-applied`) and baseline that.

## Every migrate fails: the migrations table is already locked

**Symptom.** The migrator returns `migrate: migrations table is already locked`, on every deploy.
`status` against the database says `locked: bun_migration_locks holds bun's lock on bun_migrations`
and exits 3.

**What happened.** bun's `Migrator.Lock` inserts a row into its locks table and `Unlock` deletes it. A
deploy that died in between, killed or out of memory, left the row, and every `Lock` after it fails.

**Steps.** Make sure no migration is running: no deploy in progress, no replica starting. Then delete
the row, as `status` prints it:

```
DELETE FROM bun_migration_locks WHERE table_name = 'bun_migrations';
```

Then run `status -require-applied`: a migration the dead deploy did not finish is pending, and the
next deploy runs it. Set `migration_locks_table` if the migrator is built `WithLocksTableName`.

## Rolling back

`migrator.Rollback` runs each migration's down function. For a fixture migration that is `Revert`:
the changes backwards, every one inverted and guarded like the original, so a rollback that finds a
row changed since the migration ran reports it rather than overwriting it. A rename is looked up
under the name it gave the row, and named back.

A rollback puts back the rows the migration deleted, and nothing a delete reached through a foreign
key under `deletes: cascade`: the subscriptions that went with a plan stay gone, and the log of the
rollback says so for every such row. The log of the migration said how many there were.

A rollback assumes the migration made every one of its changes on this database: nothing records
which ones it made. A change the migration found already made -- the row already held the new value
through some other path, or was already there -- is rolled back all the same: the update writes the
old value, which this database may never have held, and the inserted row is deleted. Before rolling
back a database where the migration reported `unchanged` changes, look at those rows. Where a row
does not hold what the migration writes, the change is not rolled back and the log says so.

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
