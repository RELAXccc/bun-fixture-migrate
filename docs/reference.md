# Reference

Everything the command and the library take and give back. For what to do with it, see
[using it with bun](usage.md) and [the production runbook](production.md).

- [Commands](#commands)
- [Exit codes](#exit-codes)
- [Configuration](#configuration)
- [JSON output](#json-output)
- [The Go packages](#the-go-packages)

## Commands

`bun-fixture-migrate <command> [flags]`. Every command but `scaffold` and `version` reads a
configuration file: the one `-config` names, else the one `$BUN_FIXTURE_MIGRATE_CONFIG` names, else
`fixture-migrate.yml` in the current directory. Paths in it are relative to it. Every command takes
`-h`.

Every command that connects, `export`, `check`, `generate`, `baseline`, `status`, `plan`, `sync` and
`apply`, takes `-dsn`: the database to use instead of the configuration's `database`, as a URL or as `env:NAME` to
read one from the environment, which keeps the password out of the process list. Neither the
command nor its errors repeat a password.

| Command | Database | Writes |
| --- | --- | --- |
| `scaffold` | reads | a configuration file |
| `export` | reads | the fixture files |
| `check` | reads | nothing |
| `generate` | reads with `-from-db`, and to lint when one is configured | a migration, the state file |
| `baseline` | reads, unless `-offline`, when one is configured and the files differ from the state | the state file |
| `status` | reads unless `-offline` or none is configured | nothing |
| `plan` | writes, and rolls back | nothing |
| `sync` | writes with `-yes` | the database |
| `apply` | writes, and rolls back without `-yes` | the database, with `-yes` |
| `version` | none | nothing |

"Reads" is a `REPEATABLE READ, READ ONLY` transaction: PostgreSQL refuses any write in it, and every
table is read from one snapshot. Such a command works against a hot standby. It runs with
`row_security` off, so a role that a row-level security policy limits gets an error instead of the
rows the policy lets through.

### scaffold

Writes a commented starting configuration from a database's catalog: a model per table, the primary
key, whether it is serial, the foreign keys to a table's id as `references`, the column defaults as
`defaults`, and a natural key guessed from the narrowest unique index besides the primary key. A
partition is part of its partitioned table, not a model of its own; a foreign key to another column,
a code say, is an ordinary column. Timestamps the database writes with a row, from a default such as
`now()` or, on a table with a `BEFORE` row trigger, an `updated_at`, are proposed for `ignore`, and
the triggers are named. Every guess is marked `# GUESS:`.

Which tables are master data only you know. bun's migrations and locks tables are left out and named
as `migrations_table` and `migration_locks_table`; so is a table with the columns of the
[audit table](#the-audit-table), whatever its name, which the header then proposes, commented out,
as `audit_table`; every other table is proposed, each model with a
`# GUESS:` to delete it when the application writes the table, as it does users, orders or sessions.
Kept, such a table is exported into the fixture file and is drift after every deploy. A table with
neither a unique index besides its primary key nor a `name` column has nothing a key can be guessed
from and is written commented out, with a sentence saying why. `seed_guard_table` is guessed as the
first table, in dependency order, that another model points at, and marked; it has to stay the table
of a model the fixture files fill.

Refused (exit 1): a table in `-tables` that is not in the schema, a partition, the migrator's own or
the audit table;
and a schema with no table to propose.

| Flag | |
| --- | --- |
| `-dsn` | the database, as a URL or `env:NAME`; default `$DATABASE_URL` |
| `-o` | write to this file, which must not exist; default standard output |
| `-schema` | the schema to read, default `public` |
| `-tables` | comma-separated tables; default all of them |
| `-migrations-table` | the migrator's table, left out and written into the configuration; default `bun_migrations` |
| `-migration-locks-table` | the migrator's locks table, likewise; default `bun_migration_locks` |

### export

Writes the fixture files from the database: models in dependency order, rows in id order except
that a row pointing at a row of its own model comes after it (a tree's parents before their
children), references as templates naming the target row, anchors from the natural key, values in
the notation that loads back as the same value. Rows pointing at each other in a circle are refused:
`dbfixture` cannot load them in any order. With several fixture files, each model goes back into the file that holds it and a new
one into the last. Refuses (exit 2) to write a file `dbfixture` would not load back as the database,
such as a zero bun would replace with a column default. Two exports of one database are the same
bytes: the header holds no time. The file is written anew, not edited: comments in the file it
replaces are not kept, and a note says how many lines of them it dropped. Output that cannot all be
written, to a full disk or a closed pipe, is exit 1.

For a model the fixture files hold, it writes what they hold: the columns their rows use, with the
key and the `ref` column, and the ids only when the rows name them. A column the files never wrote
is not master data, and the ids of the database exported from mean nothing in another one; written
into the file, either would be a change of every row to `generate`. A model the files do not hold
yet is written whole, ids included, unless a default other than a sequence makes its ids up, as
`gen_random_uuid()` does. Fixture files that cannot be read are replaced whole, with a note.

It writes what the fixture files own and nothing the configuration gives to the database (see
[who owns what](#who-owns-what)): of a model under `mode: upsert` or `insert` the files hold, only
the rows they hold, so a tenant's rows never reach the file; in a row the files hold, the files' own
value of every `insert_only` column, and under `mode: insert` of every column but the key and the
`ref` column, so an operator's toggle is not written into the file new databases are seeded from;
and no id of a model under `ids: database`, whose rows the file names by their anchors. A model the
files do not hold yet is written whole, whatever its mode: that is how it is first taken into them.

Before it reads a row, it checks the configuration against the catalog: a key column or a reference
column the table does not have is a sentence naming the model and the column (exit 1), as it is for
`check` and `generate -from-db`.

| Flag | |
| --- | --- |
| `-o` | write here instead, with one fixture file only |
| `-stdout` | write to standard output; with several fixture files, each one after a line `# ==> <path> <==`, in load order, as `head` prints several files |
| `-all-columns` | write every column of every model, not only those the fixture files use |
| `-json` | say what was written as JSON, see [export](#export-output); not with `-stdout` |

### check

Compares the database with the fixture files and reports every difference, every finding and every
difference `generate` would refuse. Exit 3 for a difference, a refusal or a finding the policy makes
an error; a finding the policy makes a warning is reported and leaves the exit code 0. A column the
fixture files write and the table does not have is not read: it is the `unknown column` finding.

What the configuration gives to the database (see [who owns what](#who-owns-what)) is no difference
and leaves the exit code 0; a last block counts it per model, such as `Role: 3 rows only in the
database, which mode upsert never deletes`, so nobody wonders whether check saw it. A difference in
which the database holds the column's literal default and the file a null or the type's zero gets a
`hint:` line: bun writes `DEFAULT` for a nil pointer, a zero in a `nullzero` field and a zero in a
field with a `default:` tag, on an `INSERT` and, since bun v1.2.17, on an `UPDATE` of a model, so a
seed or an admin UI left the default there.

| Flag | |
| --- | --- |
| `-json` | the report as JSON, see [check](#check-output) |

### generate

Writes the migration that takes a database from the base state to the fixture files, and moves the
state file on. The base is the state file; while there is none, git's `HEAD`.

| Flag | |
| --- | --- |
| `-name` | a short name for the migration, required when there is something to write |
| `-from-db` | take the configured database as the base instead |
| `-base <rev>` | take the fixture files as of a git revision as the base |
| `-old <file>` | take a file as the base |
| `-out <dir>` | write the migration here instead of `out` |
| `-dry-run` | print the migration and write nothing |
| `-allow-partial` | write what was accepted when something else was refused |
| `-no-lint` | skip the checks against the database's columns and defaults, and do not connect for them |
| `-at <time>` | name the migration as of this time, `YYYYMMDDHHMMSS` or RFC 3339, instead of now: for output that is the same on every run |
| `-json` | say what was generated and written as JSON, see [generate](#generate-output); with `-dry-run` the migration is in it |

The migration is named with the current time, or one second after the newest migration when that is
not earlier, and never with the name of another one. A migration whose name sorts after it is warned
about. So is a migration without a `seed_guard_table`, which on a database that was never seeded
runs before the seed and fails, and one whose seed guard is the table of no model.

With a database configured, both sides are respelled by it first, so a value written two ways (`1.10`
and `1.1` in a numeric column) is no change. When the fixture files differ from the state file but
change no value, nothing is generated and the state file takes the new text, so `status -offline`
agrees.

With `-allow-partial`, the refused changes are recorded in the state file as left out: the next
`generate` does not see them again, and `status` fails on them until `baseline -force`.

Refused (exit 2), whatever else it finds: a finding in the fixture files that the policy makes an
error, such as two rows sharing a natural key, as `sync` refuses one; a fixture migration in the
directory that the state file's history does not include, which is one generated on another branch
against an older state, or one written by hand and not recorded with `baseline -force`; and a state
file whose newest migration is no longer in the directory, deleted or renamed, whose changes the
state says are made and no migration makes. See
[the runbook](production.md#the-state-file-conflicts-in-a-merge). With `-dry-run` these two are
warnings, and the migration is printed. A state file that does not read is an error (exit 1)
whichever base is asked for, because this run would overwrite it.

The changes run in this order: renames, deletes, updates, inserts, and last the updates that point
at a row inserted in the same migration. A delete or an update can free what an insert takes: a
value of a unique column, or the open end of a price that a partial unique index or an exclusion
constraint allows once. On top of that order every change waits for the ones it depends on: a row
pointing at a new row waits for its insert, parents before children in one table too; a row is
deleted once nothing in the migration still names it, children before parents; and a row taking a
value another row of the table gives up waits for it. A unique index over several columns orders the
changes by the tuple it holds, an item moved down a list to make room at the top included. With a
database configured the catalog's unique indexes decide; without one, the columns whose values are
distinct on both sides are taken for unique, and two such guesses that contradict each other both
give way, with a warning to run with the database.

Rows trading the values of a unique index among themselves, two swapping them or a list rotated
(positions 1, 2, 3 becoming 2, 3, 1), cannot be updated in any order while the index is checked after
every statement: whichever row moves first finds its new value still held. Nor can rows whose changes
two such indexes order in opposite ways. With the database configured, `generate`, `check` and `sync`
refuse those changes, naming the rows and the index: declare it a `UNIQUE` constraint `DEFERRABLE
INITIALLY IMMEDIATE`, which a migration checks at its end, and generate again; or move one of the
rows to a value no row holds in a migration of its own first, and the others in the next. A
`DEFERRABLE` constraint orders nothing, and any trade gets through it. Where the files do not write
every column of the index, or without the database, where the index is a guess, such a circle is a
warning instead. A row whose ref value changes, a country renamed from Germany to
Deutschland, is no change to the rows pointing at it: they point at its id. Models follow their
references, in file order otherwise, whether or not the new file still mentions them.

### baseline

Records the fixture files, as they are, as what the migrations leave a database holding. A state
that differs in content is only replaced with `-force` (exit 2 otherwise), because recording a change
nobody migrated is how a change gets lost. A difference only the column types can settle, such as
`1.10` against `1.1`, is asked of the database when one is configured, as `generate` asks it: when it
is no change, the state is replaced without `-force`.

Also refused without `-force`: a state that records changes `generate -allow-partial` left out, a
fixture migration in the directory that is not in the state's history and was written by hand, and
a state file that does not read. Refused even with `-force`: a fixture migration generated against
another state, on another branch, which has to be generated again (see
[the runbook](production.md#the-state-file-conflicts-in-a-merge)); a state whose newest migration is
gone from the directory; a state file holding git's conflict markers, of which one side is taken
first; and a finding in the fixture files that the policy makes an error.

| Flag | |
| --- | --- |
| `-from <rev>` | record the fixture files as of a git revision; refused (exit 2) when they are missing or empty there |
| `-old <file>` | record this file as the fixture file, under the fixture file's path; with one fixture file only |
| `-force` | replace a differing state, and clear what was left out: a migration you wrote covers the difference |
| `-offline` | do not ask the database whether a difference is only in how values are written |
| `-dsn` | the database to ask, instead of the configuration's |
| `-json` | say what was recorded as JSON, see [baseline](#baseline-output) |

### status

Lists the fixture files, the state file and what the fixture files change that no migration makes,
then the migrations directory with, when a database is asked, what it applied. With a database, both
sides are respelled by it first and the fixture files are checked against the columns, as `generate`
does; a difference that is only spelling is a note, and what the check finds is a finding.

Exit 3 when:

- the state file is there and does not read: it holds git's conflict markers, or its checksum does
  not match;
- the fixture files change something no migration makes, or the state file records a change
  `generate -allow-partial` left out;
- the fixture files have a finding the policy makes an error;
- the directory holds two migrations bun would record under one name, or a fixture migration the state
  file's history does not include, or no longer holds the one the state file includes last;
- with a database, bun's locks table holds the lock on the migrations table;
- with `-require-applied`, a migration is not applied;
- with `-strict-order`, a pending migration sorts before one the database applied.

Exit 1 when there is no state file and git cannot read the fixture files as of `HEAD` (it is not
installed, or this is not a repository): nothing then says what the files change.

With a database and an `audit_table`, status also reads the newest row of each fixture migration in
the audit table and says what that run did in this database: applied or reverted, when and as which
role, how many changes it applied, found unchanged and skipped, each skipped change with its
problem, and whether the migration file was edited after it ran here, which it tells by the change
set's SHA-256. A table that does not exist yet is said to be so, and is no failure; one the role may
not read is an error (exit 1). A migration edited after it ran is a note, not a failure.

| Flag | |
| --- | --- |
| `-offline` | do not connect, even with a database configured |
| `-require-applied` | fail unless the database applied every migration in the directory |
| `-strict-order` | fail when a pending migration sorts before one the database applied, which bun runs after it all the same |
| `-json` | the report as JSON, see [status](#status-output) |

### plan

Runs the pending fixture migrations against the database, in bun's order, in one transaction that is
always rolled back, and reports every change. Constraints PostgreSQL defers to `COMMIT` are checked
where the deploy commits: after each migration, and after each statement of a SQL migration bun runs
without a transaction. Pending migrations it did not write are named, and so is where they would
run. Refused against a standby, and as a role whose transactions start read only
(`default_transaction_read_only`): it writes, so it connects as a role with the rights the
migrations need, such as the one the deploy migrates as.

| Flag | |
| --- | --- |
| `-file <path>` | plan this migration file, applied or not; repeat for several |
| `-with-sql` | also run the pending `.up.sql` migrations, read as bun v1.2.18 reads them; one holding `{{` is not run, because bun renders it as a template under `WithTemplateData` |
| `-strict` | fail when a change would be skipped, too |
| `-lock-timeout <d>` | give up on a row lock after this long, default `5s` |
| `-json` | the report as JSON, see [plan](#plan-output) |

What plan writes it holds locked until it rolls back: every row a fixture migration writes, and
whatever a SQL migration locks, which for most `ALTER TABLE` is the whole table, reads included.
Against a live database, another session writing those rows waits for as long as the plan runs, and
with a change set of thousands of rows that is seconds; the report says how many rows and how long.
Plan against a copy of production, or off-peak. `-lock-timeout` limits how long plan waits for
others' locks, not how long it holds its own. Sequences are outside every transaction: an id an
insert draws stays drawn, and a sequence a SQL migration moves with `setval` or `nextval` stays
moved, in the database plan ran against; plan notes such a migration.

Exit 3 when a migration would fail, or with `-strict` be skipped; exit 1 when the plan could not
finish (a lock waited for too long, a lost connection, a SQL migration that cannot run in a
transaction, an enum value a migration in the same plan added, a table or column missing after a
migration plan did not run, a write the role plan connects as may not make), which says nothing
about the migration.

### sync

Brings the database to the fixture files directly, without a migration: a development database, a
test's, a staging copy. It compares as `check` does and applies the difference with the guards and
the policy of a generated migration, in one `REPEATABLE READ` transaction. Records nothing in the
migrations table. Without `-yes` it rolls back and shows what it would change, after checking the
constraints PostgreSQL defers to `COMMIT`.

| Flag | |
| --- | --- |
| `-yes` | make the changes |
| `-json` | the report as JSON, see [sync](#sync-output) |

Exit 2 when a finding the policy makes an error, or a difference `generate` would refuse, stops it.

### apply

Runs one generated fixture migration against the database outside bun's migrator: the escape hatch of
[migrating by hand](production.md#running-a-migration-by-hand). Without `-yes` it is `plan -file`
for that file: the same simulation in a transaction that is rolled back, the same report and the same
exit codes, and with `-revert` the change set's `Revert` in place of its `Apply`. With `-yes` it runs
the change set and commits, in one transaction under the change set's advisory lock, with the
change set's own policy, lock timeout and audit table, as the migrator would.

`-record` writes bun's record of the migration into `migrations_table` in the same transaction, so
the changes and the record are committed together or not at all: the digits of the file name as
`name`, the newest `group_id` plus one as `group_id`, and the time, which is what bun v1.2.18's
`Migrate` writes. bun's migrator then reports the migration applied and does not run it. With
`-revert`, `-record` deletes the record instead, and the migrator runs the migration again on the
next migrate; when the audit table says the change set is reverted here already, by an earlier
`apply -revert -yes`, it deletes the record and does not run the revert again. Refused (exit 2), with nothing changed: `-record` of a migration recorded already, and
`-revert -record` of one that is not; both are looked at again once the change set holds its lock;
and `-record` while bun's lock is held.
`-record` needs the migrations table, which the migrator's `Init` creates (exit 1 without it). It
takes bun's lock as `Migrator.Lock` does, a row of `migration_locks_table` naming the migrations
table, in the same transaction, and deletes it before committing: while a migrator holds that lock
it refuses (exit 2), and a migrator calling `Lock` while apply runs waits for it and then finds the
migration recorded.

Without `-record`, the migrations table is left as it is, and apply says what that means: a
migration it applied is still pending for the migrator, which runs it and finds every change made;
one it reverted is still recorded, and the migrator does not run it again.

| Flag | |
| --- | --- |
| `-file <path>` | the generated migration, a `.go` file; required |
| `-yes` | make the changes and commit; without it, roll back and report |
| `-revert` | run the change set's `Revert` instead of its `Apply` |
| `-record` | record the migration in the migrations table, or with `-revert` delete its record, in the same transaction |
| `-lock-timeout <d>` | without `-yes`: give up on a row lock after this long, default `5s`, as `plan` does |
| `-json` | the report as JSON: without `-yes` as [plan](#plan-output)'s, with it as [apply](#apply-output) |

Without `-yes`, the exit codes are `plan`'s. With it: 0 when the change set was committed, skipped
changes included; 3 when a change could not be made, as the migration would fail in the deploy;
1 when apply could not do its job, such as a lock waited for too long, a lost connection, or a
privilege the role lacks; 2 when the record refuses it. Whenever it is not 0, nothing was changed.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | done; for `check`, `status` and `plan`: nothing found. A finding the policy makes a warning is reported and is not a failure |
| 1 | the command could not do its job: a bad flag, no connection, an unreadable file, output that could not be written, a plan that could not finish, nothing for `status` to compare the fixture files with |
| 2 | refused: a difference that needs a hand-written migration, a finding the policy makes an error, a state `baseline` will not replace, a fixture migration the state file does not include (`generate`, `baseline`), a file `export` will not write, a migration `apply -record` finds recorded already, or not recorded for `-revert`, or bun's lock held |
| 3 | found something: drift (`check`), a change no migration makes, a change left out, a state file that does not read, a migration not applied or out of order, a leftover lock (`status`), a migration that would fail or skip (`plan`), a problem in the migrations directory or the state file's history of it (`status`, `plan`), a migration that would fail or failed (`apply`) |

A pipeline can tell "the database drifted" (3) from "the check could not run" (1). Whatever the
code, unless it is 0, the last line on standard error says why, starting with `bun-fixture-migrate:`.

## Configuration

[`fixture-migrate.example.yml`](../fixture-migrate.example.yml) has a comment on every setting saying
what changing it does. Unknown keys are an error.

### Top level

| Key | Default | |
| --- | --- | --- |
| `fixture` | | the fixture file, relative to the configuration |
| `fixtures` | `[fixture]` | several fixture files, in the order `fixture.Load` loads them; one anchor scope across them. Set this or `fixture` |
| `out` | | the migrations directory |
| `package` | `migrations` | its Go package |
| `migrator` | `Migrations` | the `*migrate.Migrations` variable generated files register with |
| `migrations_table` | `bun_migrations` | the migrator's table, when it is built `WithTableName`; may be schema-qualified |
| `migration_locks_table` | `bun_migration_locks` | the migrator's locks table, when it is built `WithLocksTableName`; `status` reports a lock left in it |
| `state` | `<out>/fixture_state.yml` | the state file |
| `seed_guard_table` | | a table never empty in a seeded database; while it is empty a fixture migration does nothing. Written into the migration in `schema` when it names none and `schema` is not `public`. Without one, `generate` warns: on a database that was never seeded, the migration runs before the seed and fails |
| `lock_timeout` | | how long a generated migration waits for a row lock another session holds before it fails and rolls back, in PostgreSQL's spelling (`500ms`, `10s`, `1min`); written into the migration. Empty waits as long as the other session holds it |
| `audit_table` | | a table every generated migration records each of its runs in, in the transaction that made its changes; see [the audit table](#the-audit-table). Optionally schema-qualified, else in `schema` as a model's table is; written into the migration. Empty records nothing |
| `database` | | a DSN, or `env:NAME` to read one from the environment |
| `schema` | `public` | the schema of tables named without one. A generated migration names such a table with this schema unless it is `public`, because the application's `search_path` may not include it |
| `policy` | | see below |
| `models` | | see below; required |

### models

The key is the model name as the fixture files spell it. A model in a fixture file that is not
listed stops every command.

| Key | Default | |
| --- | --- | --- |
| `table` | | the SQL table, optionally schema-qualified; required |
| `id` | `id` | the primary key column: written on insert when the row has one, never compared or updated, what a reference resolves to. A primary key that is itself a reference, a plan's limits keyed by the plan, is refused as `id`: leave `id` out and keep the column in `key` and `references` |
| `serial` | `false` | the id comes from a sequence; migrations move it past explicit ids |
| `ref` | `name` | the column a reference to this model names a row by |
| `key` | `[ref]` | the natural key: the columns that identify a row without its id |
| `key_any_of` | | groups of mutually exclusive columns; the first set one of each group joins the key |
| `references` | | column to model, for columns holding another row's id |
| `defaults` | | what a column means when a row leaves it out; `~` is NULL |
| `derived` | | columns the application recalculates: never compared, written or exported |
| `ignore` | | columns that take no part |
| `deletes` | `policy.deletes` | `allow`, `refuse` or `cascade`; only under `mode: sync`, and refused next to `mode: upsert` or `insert`, under which nothing is deleted |
| `array_nulls` | `policy.array_nulls` | `refuse` or `keep`, for the model's array columns |
| `changed_row`, `missing_row`, `id_drift`, `duplicate_key` | the policy block's | the [policy](#policy) for this model's rows alone: translations an admin UI edits at `changed_row: warn`, prices at `error`. Written into the migration, in the model's table, only when set |
| `where` | | an SQL predicate limiting which rows are master data; your SQL, used as written. A generated migration carries it: every statement, natural-key lookup and reference for the model sees only those rows, and a row it writes must hold it. A `;` or a parenthesis it does not open is refused |
| `mode` | `policy.mode` | `sync`, `upsert` or `insert`: which of the model's rows the fixture files own; see [who owns what](#who-owns-what) |
| `insert_only` | | columns an insert writes and the database owns afterwards, an operator's `enabled` on a feature flag: never compared, updated or guarded on. Not a column of the key, the `ref` column, the id, nor one in `ignore` or `derived` |
| `ids` | `file` | `file`, or `database` for a table the application inserts into too: the database gives every row its id, which a migration never writes, nothing compares and export never writes. The key and the `ref` column cannot be the id |

### policy

| Key | Values | Default | Decides |
| --- | --- | --- | --- |
| `id_drift` | error, warn, ignore | error | a row under another id than the file's, or the file's id held by another row. `warn` reports it as a warning, not a refusal, and migrates the rest of the row; under warn and ignore a rename finds its row by the old natural key alone, and warn says when its id is not the file's |
| `missing_row` | error, warn | error | an update or delete whose row is not there. `warn` loses the change for good under bun's migrator |
| `changed_row` | error, warn | warn | a row somebody changed in this database; `warn` keeps their change |
| `zero_default` | error, warn, ignore | error | a zero in a column whose default is not that zero, which bun replaces with the default |
| `null_default` | error, warn, ignore | error | a null in a column with a default, which a nil pointer or `nullzero` field replaces with the default |
| `duplicate_key` | error, warn | error | two rows sharing a natural key |
| `renames` | refuse, update | refuse | a row that kept its id and changed its natural key |
| `deletes` | allow, refuse, cascade | allow | a row that left the file. `allow` fails while other rows point at it; `cascade` lets the foreign keys' ON DELETE act |
| `array_nulls` | refuse, keep | refuse | a null inside a sequence in an array column: `refuse` reports it and refuses a change carrying it, because a `[]string` field drops it and a `[]*string` one keeps it; `keep` says the models' array fields keep it. A model can override it |
| `mode` | sync, upsert, insert | sync | which rows of a model the fixture files own, for every model that does not say; see [who owns what](#who-owns-what) |

`id_drift`, `missing_row`, `changed_row` and `duplicate_key` are copied into every generated
migration, so changing them later does not change what an existing migration does. A model overrides
them, and `deletes` and `array_nulls`, with keys of the same names; a model's own `id_drift` and
`duplicate_key` also decide what `generate`, `check` and the other commands make of that model's
renumbered rows and shared keys, and its run-time policies are written into the migration as the
`Policy` of its table, holding only what the model sets:

```go
"Translation": {Name: "translations", ID: "id", Key: "key", Policy: &fixturechange.Policy{ChangedRow: "warn"}},
```

### Who owns what

By default the fixture files own every row and every column of a model: a row the database has and
the files do not is drift, and `generate` deletes it. Teams split that ownership: finance edits
prices in an admin UI, operators toggle feature flags in production, tenants add rows to shared
tables, a sequence numbers the rows. Three keys of a model say so, and every command reads them the
same way: `generate` from the state file and with `-from-db`, `check`, `status`, `sync`, `export`,
`baseline`. `plan` and `apply` run the generated sets, which hold fewer changes; nothing at run time
changes.

| `mode` | a row only the files hold | a row both hold | a row only the database holds |
| --- | --- | --- | --- |
| `sync` (default) | inserted | updated where they differ | deleted, under `deletes` |
| `upsert` | inserted | updated where they differ | left alone: no drift, no delete |
| `insert` | inserted | left alone: its values are the database's | left alone |

Rows are matched by their natural key. Under `upsert` a row that leaves the files stays in every
database that has it, and a row the application or a tenant added is not drift; `generate`,
`check` and `sync` count such rows in a note. Under `insert` the files only seed: a row is written
once, by the insert, and what it holds afterwards is the database's, the admin's edit included.
A row a mode leaves alone can be pointed at by the rows the files add, which find it by its `ref`
value as they find any other.

- **Renames.** `policy.renames` decides a rename under `sync` and `upsert` alike. Under `insert` a
  rename, a row that kept its id and changed its key, is refused: it would update a row the database
  owns. Put the key back, or give the row of the new key an id of its own to add it beside the old.
  A key that only changed its spelling to its type, `Go` to `GO` in a `citext` column, finds the same
  row and is left alone.
- **References by a ref value only the files have.** Under `insert`, a row whose `ref` value the
  files change keeps the old one wherever it exists, so a change naming it by the new one, such as a
  new row pointing at it, is refused: it would find nothing there.
- **Deletes.** `deletes` only means something under `sync`. A model that sets it next to `mode:
  upsert` or `insert` is refused; one that inherits `policy.deletes` is fine.
- **Models pointing at each other.** A `sync` model whose row a kept row points at cannot delete that
  row: the delete fails at run time, as any delete of a row others point at does without `deletes:
  cascade`. Give the model the kept rows point at a mode that keeps its rows too.

`insert_only: [columns]` is a column the insert writes, and the database owns from then on: the
`enabled` of a feature flag, set when the flag is created and toggled in production afterwards.
`check` and `status` do not compare it, an update never writes it, and a delete's guard leaves it
out, or the guard would miss a row an operator changed. `generate` notes how many rows hold another
value in it. Unlike `ignore`, which is never written, and `derived`, which the application
recalculates and is never written either, it is the files' until the row exists. An export writes
the files' value of it in a row the files hold, so the file new databases are seeded from keeps the
value a new row starts with; in a row new to the files, the database's.

`ids: database` is a table the application inserts into too, whose sequence or identity numbers
every row: a fixture file's ids would collide with it. The files may still carry ids, for their
references to resolve against and for a rename between two revisions of them to be told from an
insert and a delete; a migration never writes one, nothing compares one with a database's, and id
drift is never reported. An insert leaves the id to the database, a rename finds its row by the old
natural key alone, and an export writes no id and names the rows by their anchors. Against a
database, where the files' ids say nothing, a changed key is a row only the database holds and one
only the files hold, as for a file without ids. The id column needs a sequence, an identity or a
default, or an insert without the id fails. A fresh seed with `dbfixture` writes the files' ids, as
it always does: move the sequence after it with `fixtureapply.SyncSequences`.

What a revert cannot do: a delete is put back from its guard, which an `insert_only` column is not
in, so `Revert` inserts the row with the column's default there, and fails where the column is
`NOT NULL` without one. An insert is taken back by a delete guarded by everything it wrote, so a
row whose `insert_only` column an operator changed since is skipped as changed.

## JSON output

`check`, `export`, `generate`, `baseline`, `status`, `plan`, `sync` and `apply` take `-json`. Values
are strings as the database spells them, NULL is `null`, and a reference is
`{"model": "Currency", "key": "USD"}`, so neither can be mistaken for a string. Lists are `[]` rather
than `null` when empty, except the fields this page marks as left out when empty. Fields may be
added; none will change meaning.

With `-json`, standard output is the report and nothing else. A command that refuses (exit 2) or
finds something (exit 3) prints its report all the same, then says why on standard error. One that
could not do its job (exit 1) prints none, unless it got far enough to have one: a plan that could
not finish, a sync or an apply that failed halfway. The reports of `check`, `export`, `generate`,
`baseline`, `status` and `sync` are the encoding of what the [Go API](#the-project) returns for the
same call, so a program reading the command and one calling the library see the same thing.

### check output

```json
{
  "agree": false,
  "findings": [{"kind": "zero against a default", "level": "warn", "model": "Plan", "row": "Plan/name=free",
                "detail": "..."}],
  "refusals": [{"model": "Plan", "key": "Plan/name=old", "reason": "..."}],
  "warnings": [{"model": "Plan", "key": "Plan/name=team", "reason": "..."}],
  "changes": [
    {"model": "Plan", "kind": "update", "key": {"name": "team"},
     "database": {"price_cents": "2200", "seats": "1"}, "file": {"price_cents": "2500", "seats": "0"},
     "hints": {"seats": "the column defaults to 1, and bun writes DEFAULT for ..."}}
  ],
  "left_alone": [{"model": "Role", "mode": "upsert", "rows": 3, "changed": 0},
                 {"model": "Flag", "mode": "sync", "rows": 0, "changed": 0, "columns": {"enabled": 2}}]
}
```

`agree` is what the exit code says: `true` for 0. A finding's `level` is what the policy makes of its
kind, `error` or `warn`; a `warn` finding is listed and leaves `agree` true. A change is what a
migration from the database to the file would do: an `insert` is a row only the file has, a `delete`
one only the database has. A change's `database` and `file` are left out when empty: an insert has
no `database`, a delete no `file`. `warnings` are differences the policy lets a migration carry on
past, such as a renumbered row under `id_drift: warn`; they leave `agree` as it is. A change's
`hints` say, per column, why it differs where check can tell, and are left out when there are none.
`left_alone` counts, per model, what the configuration gives to the database and is no drift: `rows`
only the database holds under `mode: upsert` or `insert`, rows `changed` that `mode: insert` never
updates, and per `insert_only` column the rows holding another value; `columns` is left out when
there are none.

The `row` of a finding and the `key` of a refusal name the row for a person, as
`Model/column=value/…`, with a NULL as `NULL` and a reference as `Model(key)`. Two rows can read
alike there, a NULL and the text `NULL` for instance; the tool never compares rows by it, so they are
still two rows.

Finding kinds: `duplicate key`, `duplicate id` (two rows of the file sharing one id), `zero against a default`, `null against a default`,
`invalid value` (a value the column's type cannot hold), `ambiguous value` (a value only the Go
field's type could settle, such as a null inside a sequence), `unknown column`.

### status output

```json
{
  "fixture": "fixtures/fixture.yml",
  "state": {"path": "migrations/fixture_state.yml", "exists": true, "migration": "20260930165255_fixture_plan_prices",
            "format": 2, "covers": "20260930165255_fixture_plan_prices", "base": "20260921120000_fixture_seats"},
  "base": "the state file",
  "uncovered": [],
  "refused": [],
  "warnings": [],
  "left_out": [],
  "findings": [],
  "directory": "migrations",
  "migrations": [
    {"id": "20260930165255_fixture_plan_prices", "name": "20260930165255", "fixture": true, "changes": 3,
     "applied": {"group": 2, "at": "2026-09-30T17:00:00Z"}, "out_of_order": false,
     "audit": {"direction": "up", "at": "2026-09-30T17:00:00Z", "by": "deploy", "applied": 2, "unchanged": 0,
               "skipped": 1, "unseeded": false, "edited": false,
               "skipped_changes": [{"index": 1, "model": "Plan", "key": "name=team", "kind": "update",
                                    "problem": "changed row"}]}}
  ],
  "not_in_state": [],
  "database": {"table": "bun_migrations", "table_exists": true, "not_in_directory": [],
               "newest_applied": "20260930165255", "locks_table": "bun_migration_locks", "locked": false,
               "audit": {"table": "bun_fixture_audit", "exists": true}},
  "problems": [],
  "notes": [],
  "failures": []
}
```

| Field | |
| --- | --- |
| `state` | `null` when no state file is configured. `exists` is whether the file is there, and `error` why one that is there does not read, such as a merge that stopped in it; nothing is compared then. `format` is 1 for a file written before the format was numbered. `covers` is the newest fixture migration whose changes the state includes, `base` what that one was generated against |
| `base` | what `uncovered` was worked out against: `the state file`, or `HEAD` while there is none |
| `uncovered`, `refused` | what the fixture files change that no migration makes, one line per model, and what of it `generate` would refuse |
| `warnings` | what `generate` would report about those changes and carry on past |
| `left_out` | changes `generate -allow-partial` refused and recorded in the state file, until `baseline -force` |
| `findings` | as in [check](#check-output), the ones the policy does not ignore |
| `migrations` | `applied` is `null` for a pending migration and for all of them without a database; `out_of_order` is a pending one that sorts before `newest_applied` |
| `not_in_state` | fixture migrations of the directory the state's history does not include; `problems` says why, and says when the one the state includes last is gone |
| `database` | `null` when none was asked. `locked` is a row in `locks_table` naming `table`: a migrator running now, or one that died and left it. `audit` is the `audit_table` read and whether it `exists`, left out when none is configured |
| `migrations[].audit` | what the newest row of the audit table says the migration's last run did here, left out when there is none: `direction` `up` for an Apply, `down` for a Revert; `at` and `by` (the role); `applied`, `unchanged` and `skipped` count its changes, a revert's `applied` being those it reverted, and `skipped_changes` lists the skipped ones with their `problem`; `unseeded` is a run the empty seed guard table made a no-op; `edited` is a migration file whose change set is not the one that ran |
| `failures` | why status fails, one sentence each: the exit code is 3 while there is one, and the last line on standard error joins them |

### generate output

```json
{
  "migration": "20260930165255_fixture_plan_prices",
  "dry_run": false,
  "written": ["migrations/20260930165255_fixture_plan_prices.go", "migrations/fixture_state.yml"],
  "base": "the state after 20260921120000_fixture_seats",
  "summary": ["Plan: 1 insert, 1 update"],
  "changes": [
    {"model": "Plan", "kind": "update", "key": {"name": "team"},
     "old": {"price_cents": "2000"}, "new": {"price_cents": "2500"}}
  ],
  "findings": [],
  "refusals": [],
  "warnings": [],
  "not_in_state": [],
  "problems": [],
  "left_out": [],
  "left_alone": [],
  "notes": []
}
```

| Field | |
| --- | --- |
| `migration` | the migration as bun prints it; `""` when there is nothing to write |
| `written` | the files written, the migration first; `[]` with `-dry-run`, on a refusal, and when nothing changed. When the files differ from the state only in how values are written, the state file alone |
| `base` | what the fixture files were diffed against, as the generated file's comment names it; `""` when generate stopped before it compared |
| `summary`, `changes` | what the migration does, one line per model, and each change from the base (`old`) to the files (`new`), in the order it runs; `old` is left out of an insert and `new` of a delete |
| `findings` | as in [check](#check-output): what the fixture files turn up on their own, then what the lint against the database's columns does |
| `refusals`, `warnings` | as in [check](#check-output): what needs a hand-written migration, and what the policy lets the migration carry on past |
| `not_in_state`, `problems` | as in [status](#status-output): fixture migrations the state file's history does not include, and what is wrong, which refuses unless `-dry-run` |
| `left_out` | the changes the state file records as left out once this run wrote it, `-allow-partial`'s refusals included |
| `left_alone` | as in [check](#check-output), from the base to the files: a row that left the files of an `upsert` model, a row `mode: insert` does not update, an `insert_only` column; no change, and nothing the migration does |
| `notes` | what generate says and carries on past: no state file yet, so the diff is against `HEAD`; a migration that sorts after this one; a change set written in parts; a missing seed guard; the package it goes into |
| `source` | with `-dry-run` only: the migration's Go source |

### baseline output

```json
{
  "state": "migrations/fixture_state.yml",
  "recorded": "fixtures/fixture.yml",
  "written": ["migrations/fixture_state.yml"],
  "unchanged": false,
  "respelled": false,
  "summary": [],
  "refusals": [],
  "findings": [],
  "problems": [],
  "not_in_state": [],
  "left_out": []
}
```

| Field | |
| --- | --- |
| `recorded` | what was recorded: the fixture files, `<rev>:` them with `-from`, or the file `-old` names |
| `written` | `[]` when the state file records them already (`unchanged`), and on a refusal |
| `respelled` | the database said the files differ from the state only in how values are written |
| `summary`, `refusals` | what the files change against the state baseline would replace, when it refuses to record that without `-force` |
| `findings` | as in [check](#check-output); one the policy makes an error refuses the baseline |
| `problems` | what refuses the baseline even with `-force`: fixture migrations generated against another state, or the one the state includes last gone from the directory |
| `not_in_state` | fixture migrations written by hand that the state's history does not include: refused without `-force`, recorded with it |
| `left_out` | the changes the state records as left out: refused without `-force`, cleared with it |

### export output

```json
{
  "written": ["fixtures/fixture.yml"],
  "files": [{"path": "fixtures/fixture.yml", "dropped_comments": 0}],
  "findings": [],
  "notes": []
}
```

`written` are the files written, `-o`'s included, and `[]` when the export is refused. `files` are the
fixture files in load order, each with how many comment lines of the file it replaces it does not
have. `findings` are as in [check](#check-output): what the database holds that the files would not
load back as, which the policy may make a refusal. `notes` say that every column and id was
exported, because the fixture files there do not read.

### plan output

```json
{
  "migrations": [
    {"id": "20260930165255_fixture_plan_prices", "kind": "fixture", "result": "succeeds",
     "after": ["20260930160000_schema"],
     "changes": [{"set": "20260930165255_fixture_plan_prices", "index": 0, "model": "Plan",
                  "kind": "insert", "key": "name=pro", "status": "applied", "rows": 1}]}
  ],
  "not_simulated": ["20260930160000_schema"],
  "notes": [],
  "problems": [],
  "rows_locked": 1,
  "locked_seconds": 0.042
}
```

`result` is `succeeds`, `fails`, `unseeded`, `not reached` (after one that fails), or `inconclusive`
when the plan itself could not finish. `kind` is `fixture`, or `sql` for a migration `-with-sql`
ran. `after` names pending migrations that were not simulated and run before this one, and is left
out when there are none; `error` is left out when there is none. A migration's `notes`, left out
when there are none, say what its `result` and `error` do not: why the plan
could not tell, or where the deploy can differ from the plan. The top-level `notes` say why a
migration `-with-sql` would have run is in `not_simulated`. `problems` are those `status` reports in
the migrations directory, each of which fails the plan: two migrations under one name, a generated
file that does not read, a fixture migration the state file's history does not include, the
one it includes last gone from the directory, and a state file that does not read, such as one
holding git's conflict markers. `rows_locked` is how many rows the fixture
migrations wrote and held locked until the rollback, and `locked_seconds` how long the plan's
transaction was open.

### sync output

```json
{"applied": true, "dry_run": false, "findings": [], "refusals": [], "warnings": [], "changes": [ ... ]}
```

`findings` are as in [check](#check-output), with a `level` each, and so are `refusals` and
`warnings`: what stopped the sync, and what the policy let it carry on past.

### apply output

`apply -json` without `-yes` writes [plan](#plan-output)'s report. With `-yes`:

```json
{"id": "20260930165255_fixture_plan_prices", "direction": "up", "committed": true, "record": "recorded",
 "group_id": 3, "changes": [ ... ], "notes": []}
```

`direction` is `up`, or `down` with `-revert`; `committed` is whether the changes are in the
database; `record` is `recorded` or `unrecorded` for what `-record` did, left out without it, and so
is `group_id` unless a record was written; `already_reverted` is `true` when `-revert -record`
found the change set reverted here already and only deleted the record, left out otherwise; `error`
says why it failed, left out when it did not.
`changes` are [outcomes](#outcomes); `notes` what the run means for bun's record of the migration and,
for a revert, which changes it undoes.

### Outcomes

`plan` and `sync` report each change as `fixtureapply.Outcome`:

| Field | |
| --- | --- |
| `set` | the change set's name |
| `index` | the change's position in the set; `-1` for an outcome about the whole set |
| `model`, `kind`, `key` | which change: `insert`, `update` or `delete`, and the natural key as `col=value,...` |
| `status` | `applied`, `unchanged` (the database held it already, or, in a revert with an audit table, the migration did not make it here), `skipped` (the policy passed it over), `failed`, `unseeded` (the seed guard table is empty), `sequence` (a sequence moved past explicit ids) |
| `rows` | rows an applied change touched |
| `problem` | why it could not be made: `missing row`, `changed row`, `id drift`, `referenced` (a delete other rows point at), `duplicate key` (more than one row holds the natural key), `lock timeout`, `error` |
| `message` | the sentence a person reads |

## The Go packages

| Package | For |
| --- | --- |
| `fixtureapply` | the run time generated migrations call, in the application's process |
| `fixturechange` | the change-set types a generated migration is written in |
| `fixturemigrate` (the module root) | everything the command does, as a library: a [project](#the-project) with a method per command, and the configuration, snapshots, diff and render under it |
| `dbschema` | the catalog reader |

`fixtureapply` needs bun and nothing else; it works under any `database/sql` PostgreSQL driver bun
does, and is tested under `pgdriver` and `pgx`.

| Function | |
| --- | --- |
| `Up(set, opts...)`, `Down(set, opts...)` | the up and down functions a generated file registers with `MustRegister`; they run `Apply` and `Revert`, and `Up` knows the migration's name from the file it is called in |
| `Apply(ctx, db, set, opts...)` | run a change set in one transaction; see [what a migration does](../README.md#what-a-generated-migration-does) |
| `Revert(ctx, db, set, opts...)` | the same set backwards, every change inverted: with an `AuditTable`, only the changes `Apply` made in this database, and otherwise every one ([rolling back](production.md#rolling-back)) |
| `Validate(set)` | check a set without a database |
| `SyncSequences(ctx, db, tables...)` | move the sequences of serial and identity columns past the values present, forward only; after a `dbfixture` seed |
| `WithLogger(fn)` | where the per-row lines go; default `log.Printf` |
| `WithSlog(logger)` | write the per-row report to a `*slog.Logger` instead, one record per change with the outcome's fields as attributes; a skipped change is a warning |
| `WithReport(fn)` | receive every `Outcome` as it happens |
| `WithMigrationName(name)` | the migration name, when `Apply` is called by hand from outside the file bun registered |
| `WithDryRun()` | for a caller that rolls back: sequences are reported, not moved |
| `SetSHA256(set)` | the SHA-256 of a canonical encoding of the set, as the audit table records it |
| `ReadAudit(ctx, db, table)` | the newest row of the audit table for every set it holds, by name; false when the table does not exist |
| `WaitForChangeSets(ctx, tx)` | take, in a transaction, the advisory lock every change set runs under, and hold it until the transaction ends: for a program that reads the audit table and acts on it |
| `ApplyRecords(ctx, db, set)` | the rows a `Revert` of the set follows: its `up` rows after its last `down` row, newest first; and that `down` row when no `up` row follows it, which is a set reverted here already |

A change that fails the set comes back as a `*fixtureapply.ChangeError`, which `errors.As` finds in
the error bun's migrator returns: its `Outcome` says which change and why, and it unwraps to the
statement's own error, such as PostgreSQL's, or to nothing when the policy made a problem with the
row fatal. When `Apply` took back bun's record of the failed migration, `errors.Is(err,
fixtureapply.ErrRecordRemoved)` holds: the migration is pending again.

`Apply` takes a `bun.IDB`. Given a `*bun.DB` it opens its own transaction; given a `bun.Tx` it runs
in a savepoint inside it, which is how a migration that also does other work keeps it all in one
transaction. Only on the migrator's `*bun.DB` does a failure take back bun's record of the
migration.

### The project

`fixturemigrate.LoadProject(path)` reads a configuration and the fixture files it names, the paths in
it relative to it, as every command does; `NewProject(cfg, dir)` takes a configuration built in code.
A `*Project` has a method per command, which does what the command does, with the same checks and
the same refusals, and returns a result that, encoded as JSON, is what the command prints with
`-json`. [Use from Go](usage.md#use-from-go) shows them at work.

| Method | Returns |
| --- | --- |
| `Check(ctx, db)` | `*CheckReport`: `Agree`, the `Diff` with the database on the left, the `Findings`, the `Hints` of the differences by change and column; `Lines()` is the report as `check` prints it |
| `Export(ctx, db, ExportOptions{AllColumns})` | `*Exported`: the `Files` export would write, the `Findings`, the comment lines each drops; `Write()` writes them over the fixture files |
| `Generate(ctx, db, GenerateOptions{...})` | `*Generated`: the `Diff`, the migration's `ID`, `Path` and `Source`, the `State` that goes with it, the findings, the warnings, the state's history; `Write()` writes the migration and the state file |
| `Baseline(ctx, db, BaselineOptions{From, Old, Force})` | `*Baselined`: the `State` to record, and why it is refused when it is; `Write()` writes it |
| `Status(ctx, db, StatusOptions{RequireApplied, StrictOrder})` | `*StatusReport`, plain data; `Failures` is its verdict |
| `Sync(ctx, db, SyncOptions{DryRun, Logf})` | `*SyncReport`: `Sync` below, on the project's files |

`GenerateOptions` has a field for each flag of `generate`: `Name`, `At`, `Base`, `Old`, `FromDB`,
`Out`, `AllowPartial`, `NoLint`, `DryRun`. `Files()`, `FixturePaths()`, `OutDir()` and
`StatePath()` say what the project read and where it writes; `ReadFiles()` reads the fixture files
again.

The library connects to nothing on its own: `db` is the `bun.IDB` the program hands it. `nil` is
offline where the command can be: `Generate` without the lint, `Baseline` without asking about
spelling, `Status` without what was applied. Given a `*bun.DB` or a `bun.Conn`, a method reads in a
`REPEATABLE READ, READ ONLY` transaction of its own, as the command does; given a `bun.Tx`, in that
transaction, under a savepoint it rolls back, which leaves the transaction and its settings as they
were. `fixturemigrate.ReadOnly(ctx, db, fn)` is that read, for a program that builds its own
pipeline from the functions below.

A refusal, what the command exits 2 on, is a `*fixturemigrate.RefusedError`: its `Message` says
what to do, and its `Refusals`, `Findings` and `Problems` what was refused. The method returns its
result with it, which says what was refused; with any other error the result is what was found
before it, or nil.

| `errors.Is(err, …)` | |
| --- | --- |
| `ErrRefused` | every refusal |
| `ErrFindings` | a finding the policy makes an error |
| `ErrLineage` | a fixture migration the state file's history does not include, or the one it includes last gone from the directory |
| `ErrUnmigrated` | `Baseline` without `Force`: changes no migration makes |
| `ErrSyncRefused` | a refusal of `Sync`, the package function's too |
| `ErrStateConflict` | a state file holding git's conflict markers |
| `ErrNameRequired` | `Generate` with a migration to write and no `Name`; not a refusal |

Under the project, `fixturemigrate.Compute(cfg, old, next)` diffs two snapshots into a `*Result`:
`Changes` in the order they apply, `Refusals` that need a hand-written migration, and `Warnings` the
policy lets a migration carry on past (a renumbered row under `id_drift: warn`). Only refusals stop
a migration from being written. Its `LeftAlone` counts what the configuration gives to the database,
per model, and `LeftAloneLines()` says it as `check` and `generate` do. `KeepOwned(ctx, db, cfg,
tables, files, snap)` takes out of a database snapshot what an export leaves to the database, as
`Export` does before it writes.

`fixturemigrate.Sync(ctx, db, cfg, files, SyncOptions{DryRun, Logf})` is the `sync` command on
files the program read, returning a `*SyncResult` with the diff, the findings and the outcomes;
`ErrSyncRefused` wraps a refusal. See [tests and development servers](usage.md#tests-and-development-servers).

### The audit table

With `audit_table` set, a generated migration carries it as `fixturechange.Set.AuditTable`, and every
`Apply` and `Revert` of the set that succeeds writes one row into it, last in the transaction that
made its changes and under the advisory lock the set holds, and under its `lock_timeout`: a lock
another session holds on the table, an `ALTER TABLE` or a `VACUUM FULL`, fails the run as a lock on
a row does, rather than holding the migration and the rows it changed for as long as it lasts. A run that fails rolls back and writes
nothing. The table is created the first time, with comments saying what it is; that takes `CREATE`
on its schema, and a role without it gets a sentence saying so, and nothing is changed. A table
created by another role needs `SELECT` and `INSERT` granted to the role that migrates. A row-level
security policy that applies to that role on the audit table stops the run before it changes
anything, as one on the set's tables does: hiding rows there would hide the runs a `Revert` follows.

| Column | Type | |
| --- | --- | --- |
| `id` | `bigint`, an identity | the order the runs committed in: the advisory lock lets one set write at a time |
| `set_name` | `text` | the set's `Name`, the migration's file name |
| `direction` | `text` | `up` for `Apply`, `down` for `Revert` |
| `set_sha256` | `text` | `SetSHA256` of the set as it ran, 64 hex digits |
| `applied_at` | `timestamptz` | when the run wrote its row, just before it committed |
| `applied_by` | `text` | the role it ran as, `current_user` |
| `outcomes` | `jsonb` | an array, one object per outcome: `index` (`-1` for one about the whole set), `model`, `key`, `kind`, `status`, `problem` |

`Revert` reads the `up` rows of the set after its last `down` row, and inverts only the changes one
of them records as `applied`. A change the migration found made already, or skipped, is left alone,
reported `unchanged` with a message saying why. Not only the newest row: a revert that failed under
bun's default migrator leaves the migration pending with its changes made, and the `Apply` of the next
migrate finds them all `unchanged`; the run before it made them. A set edited since it ran is matched
to the run by each change's model, key and kind, and a change the run did not have is not reverted.
When the set's newest row is a `down` row, the set is reverted here already and no `Apply` ran
since: `apply -revert -yes` by hand followed by bun's `Rollback`, or two replicas rolling back. That
`Revert` changes nothing, reports every change `unchanged`, "already reverted here, audit row N",
and writes its own `down` row. When the `up` rows all record a run that found the seed guard table
empty, it changes nothing either: "the run here was unseeded". Without a row of the set, because
the table is new or the set ran before it had one, `Revert` inverts every change, as it does without
an audit table, and logs that it does.

The hash is of every field of the set, with every map's keys in order and every field at its zero
value left out, so a field a later version adds does not change the hash of a set that does not use
it. `status` compares it with the migration file.

### Generated files over time

A generated migration stays in your repository for good, and is compiled against whichever version
of `fixtureapply` the application uses later. Every version reads, compiles and runs the files the
earlier ones wrote: the repository keeps one of each shape, unchanged, under `testdata/generated`,
and its tests run them all under bun's migrator.

A change set of more than a thousand changes is written as one function per hundred changes,
joined by `fixturechange.Concat`: the Go compiler took 77 seconds and 1.3 GB over one literal of
4,800 changes, and takes 8 seconds over the same changes in parts. `fixturemigrate.RenderWarnings`
says so about such a set, and that it still runs in one transaction.

`fixturechange.Set` has a `Format`, which a file leaves out while it is 1. A new field needs no new
format: a file that uses one does not compile against an older `fixtureapply`, which is refusal
enough. Only a change to what an existing field means raises it, and then an older `fixtureapply`
refuses the file, and an older `status` or `plan` cannot read it, with a sentence saying to upgrade
`github.com/RELAXccc/bun-fixture-migrate`.
