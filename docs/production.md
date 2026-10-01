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
connects as a role with the rights the migrations need, the deploy's own or one granted the same:
as a read-only role it proves nothing, and says so. It
does hold the rows its changes touch locked until it rolls back, and says how many and for how long;
a change set of thousands of rows can hold application writes to those rows for seconds, so plan a
large one against a copy, or off-peak.

## What touches production, and how

| | |
| --- | --- |
| `export`, `check`, `status`, `scaffold`, `generate -from-db` | read, in one `REPEATABLE READ, READ ONLY` transaction |
| `generate`, `baseline` | read the same way whenever a database is configured or named with `-dsn`: `generate` to check the fixture file against the columns and respell its values (not with `-no-lint`), `baseline` to ask whether a difference is only in how values are written (not with `-offline`) |
| `plan` | writes in one transaction and always rolls it back. A sequence an insert drew from stays advanced, which only leaves a gap in the ids; a sequence the migration would move is reported, not moved. Under `-with-sql`, a SQL migration's own `setval` or `nextval` is not rolled back either, because PostgreSQL's sequences are not transactional; plan notes such a migration |
| `sync` | writes, with `-yes`. Meant for databases that are not deployed to |
| `apply` | without `-yes`, as `plan -file`; with it, runs one migration as the migrator would, and with `-record` writes or deletes its record in the migrations table, in the same transaction |
| a fixture migration | writes, in one transaction, under a transaction-scoped advisory lock; with an `audit_table`, one row there per run, in the same transaction |

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

With an `audit_table`, `status` against the database lists, per migration, the changes its last
run here skipped and why, long after the deploy log is gone; without one, only that log says so.

**Steps.** Run `check`. It lists the row with the database's and the file's values side by side.
Then decide which one is right:

- **the database is right**: take its values into the file (`export`, or edit the file), then
  `generate`, which writes a migration that brings the other databases to them;
- **the file is right**: `generate -from-db -name "..."` against this database writes a migration
  that makes it match the file, or on a database that is not deployed to, `sync -yes`.

### A returning row is soft-deleted with other values

**Symptom.** On a model with a [`soft_delete`](reference.md#soft-deleted-rows):
`skipped Plan name=legacy insert [changed row]: plans name=legacy is soft-deleted (id 3, deleted at
2026-02-01 00:00:00+00) with other values than this change writes, and constraint "plans_name_key"
refuses a second row`.

**What happened.** The file brings back a row that an admin, or an earlier migration, soft-deleted,
and the soft-deleted row holds other values. Under a unique index over live rows only, `UNIQUE (name)
WHERE deleted_at IS NULL`, the migration inserts a new row beside it, and says so. Under one over every
row, `UNIQUE (name)`, the soft-deleted row still holds the key, and nothing can be inserted beside it:
the change is a changed row, under `changed_row`.

**Steps.** Decide which row the file means:

- **the old row**: restore it by hand, `UPDATE plans SET deleted_at = NULL WHERE id = 3`, and the
  migration's next run, or a new one, updates it to the file's values; the rows pointing at it point
  at it again.
- **a new row**: delete the soft-deleted one for good, if nothing points at it, and run the migration
  again; or make the index partial, `WHERE deleted_at IS NULL`, so a name can be reused.

A rename into a key a soft-deleted row holds under a unique index over every row is the same
changed row, and the same steps apply.

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

When the same drift comes back after every deploy because the database owns it, a price finance
edits, a flag operators toggle, roles tenants add, say so instead of folding it into the file each
time: `insert_only`, `mode: insert` or `mode: upsert` on the model, and check no longer reports it
([who owns what](reference.md#who-owns-what)). A difference with a `hint:` line is a null or a zero
in the file where bun wrote the column's default: write the default's value in the file.

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
   branch's migration file with `git rm -f <file>`: during a merge, a plain `git rm` of a file the
   merge staged refuses. It must not have run anywhere that matters (`status` against a database
   lists what it applied).
3. Take the state file as the kept migration left it: when merging the other branch into yours,
   `git checkout --ours -- internal/migrations/fixture_state.yml`. Not the side the deleted
   migration left: that state includes changes no migration in the directory makes any more, and
   `status`, `generate` and `baseline` refuse it (`the state file includes the changes of ..., which
   is not in ...`).
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

**Symptom.** A command refuses the state file: its checksum does not match, or it is not a state
file at all.
(For conflict markers, see [above](#the-state-file-conflicts-in-a-merge).)

**What happened.** It was edited by hand or merged line by line. Line-ending conversion by git is not
an edit and is accepted. A state nobody can vouch for would let `generate` write a migration against
the wrong base, so it is refused.

**Steps.** When git has the state file as it was, take it back: `git checkout <rev> --
internal/migrations/fixture_state.yml`. Otherwise find the fixture file as the last migration left
it. When the last commit that touched the state file also added that migration, it is the fixture
file at that commit: `bun-fixture-migrate baseline -from <commit> -force`.

If you cannot tell, export from a database that applied every migration (`status -require-applied`
against it says so) into a file of its own, and record that. The fixture file keeps the edits no
migration makes yet, which `status` then lists and the next `generate` writes:

```
bun-fixture-migrate export -dsn env:APPLIED_DSN -o /tmp/applied.yml
bun-fixture-migrate baseline -old /tmp/applied.yml -force
```

The state records the export under the fixture file's own path. `-o` and `-old` take one file, so
with several fixture files, commit the pending edits first, then export in place, run
`bun-fixture-migrate baseline -force`, and take the files back with
`git checkout -- <the fixture files>`.

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

A model with a [`soft_delete`](reference.md#soft-deleted-rows) loses nothing: the rollback restores
the rows the migration soft-deleted, with their ids, and soft-deletes the rows it inserted or
restored, so the rows the application pointed at them since are not refused or lost. The row a
rollback soft-deletes carries the rollback's time, not the time it was soft-deleted before the
migration restored it.

Set `audit_table` before you need to roll back. With it, every run of a generated migration records,
in the transaction that made its changes, which of them it applied, found made already and skipped,
and a rollback undoes only the ones it applied in that database. A change the migration found made
-- the row already held the new value through some other path, or was already there -- is left as it
is, and so is one it skipped: a delete it skipped because somebody had removed the row does not put
the row back. The log says why for each. `status` against the database shows what the last run did.
A second rollback with no deploy between them -- `apply -revert` by hand and then the migrator's
`Rollback`, or two replicas rolling back -- finds the migration reverted already and changes nothing.

With it, a rollback in an environment that was seeded after its migrations ran -- a development
database, CI, a preview -- reverts nothing. The migrations ran there against the empty database,
recorded that they did nothing, and the seed then loaded the fixture file with their changes in it;
the rollback says "the run here was unseeded" for every change and leaves the seeded rows alone.
Without an audit table, the same rollback reverts every change, as below. To get the old values back
there, seed again from the fixture file as it was.

Without it, or for a migration that ran before it was set, a rollback assumes the migration made
every one of its changes on this database, and says so in the log. A change the migration found
already made is rolled back all the same: the update writes the old value, which this database may
never have held, and the inserted row is deleted. A soft delete the migration found made already,
a plan an admin had soft-deleted before the deploy, is restored by such a rollback; with an audit
table it is left soft-deleted. Before rolling back such a migration where it
reported `unchanged` changes, look at those rows. Where a row does not hold what the migration
writes, the change is not rolled back and the log says so.

To roll back one migration rather than the migrator's last group, use `apply -revert` (below).

A rolled-back migration is pending again, and the next deploy runs it again. To undo the change for
good, revert the commit that brought the migration, the fixture edit and the state file, together,
before that deploy.

Rolling back is the exception. A fixture change that turned out wrong is usually better fixed
forwards: correct the file and generate a new migration.

## Running a migration by hand

To see what any fixture migration does against any database, applied or not:

```
bun-fixture-migrate apply -file internal/migrations/20260930165255_fixture_plan_prices.go
```

Without `-yes` that is `plan -file`: everything runs, nothing stays. To apply it outside the
migrator, when a deploy cannot run it or one database needs it before the others:

```
bun-fixture-migrate apply -file internal/migrations/20260930165255_fixture_plan_prices.go -yes -record
```

It runs the change set as the migrator would, with the policy, lock timeout and audit table the
file carries, and `-record` writes bun's record of the migration into the migrations table in the
same transaction: the changes and the record are committed together or not at all, and the migrator
then treats the migration as applied and does not run it. Without `-record` the migrator runs it again
on the next deploy, harmlessly: every change reports `unchanged`. `-record` refuses a migration that
is recorded already.

To take one migration back, whatever group the migrator ran it in:

```
bun-fixture-migrate apply -file internal/migrations/20260930165255_fixture_plan_prices.go -revert
bun-fixture-migrate apply -file internal/migrations/20260930165255_fixture_plan_prices.go -revert -yes -record
```

The first says which changes the revert undoes (with an `audit_table`, those the migration made
here) and what it finds; the second makes them and deletes the record, so the migrator runs the
migration again on the next deploy, unless you remove it first, as [above](#rolling-back) says.
Reverted without `-record`, the migration stays recorded; `-revert -yes -record` afterwards deletes
the record, and with an `audit_table` it does not revert the change set a second time.

Run it as the role the deploy migrates as, and not while a deploy is migrating: the change set's
advisory lock keeps two change sets apart, but not bun's migrator from recording the same migration.
`-record` takes bun's own lock, the row `Migrator.Lock` writes into `migration_locks_table`, for its
transaction: it refuses while a deploy holds it, and a deploy that calls `Lock` meanwhile waits and
then finds the migration recorded. A migrator that does not call `Lock` is not kept out.
`apply` exits 3 when the change set fails, as it would in the deploy, and 2 when the record refuses
it; either way nothing was changed.

## Adopting the tool on an existing project

1. `bun-fixture-migrate scaffold -o fixture-migrate.yml` against a database that holds the master
   data, then read every guess it marks `# GUESS:`. Above all, delete the model of every table the
   application writes, users, orders, sessions: scaffold proposes every table but bun's own and the
   audit table, and a table left in is exported into the fixture file and is drift after every
   deploy.
2. Set `seed_guard_table` to a table the fixture file fills and the application never empties.
   scaffold guesses one; without one, `generate` warns, and a new environment fails its first
   deploy (see [below](#a-new-environment)).
3. `bun-fixture-migrate export` from production, or keep your existing fixture file and run `check`
   against production until they agree. `check` also names every natural key no unique index backs,
   an error under the `key_index: error` scaffold writes: create the index it names, `CONCURRENTLY`
   on a live table, before the first fixture migration, or the application's next duplicate fails
   the deploy that touches it. A table holding duplicates already cannot get one until they are
   gone, and `check` lists those too.
4. `bun-fixture-migrate baseline`: the databases hold the file, record that.
5. Add `status -offline` to CI.

From then on every fixture edit comes with a generated migration. Databases that were never seeded
from the file still need to agree with it before the first migration: `check` each one.

## A new environment

A new database gets the schema from the migrations and the data from the fixture file:

1. run the migrator; the fixture migrations see an empty `seed_guard_table` and do nothing. Without
   a seed guard they run here, before the seed, against empty tables, and the first that changes a
   row fails the deploy: `generate` warns about a migration written without one;
2. load the fixture file with `dbfixture`, in a transaction;
3. `fixtureapply.SyncSequences` on the seeded tables.

[Using it with bun](usage.md#deploying-migrate-then-seed) has the code. The result is the same
database an old environment reaches by running every fixture migration, which `check` confirms.
