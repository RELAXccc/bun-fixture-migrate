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

Every command that connects, `export`, `check`, `generate`, `baseline`, `status`, `plan` and `sync`,
takes `-dsn`: the database to use instead of the configuration's `database`, as a URL or as `env:NAME` to
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
the triggers are named. Every guess is marked.

| Flag | |
| --- | --- |
| `-dsn` | the database, as a URL or `env:NAME`; default `$DATABASE_URL` |
| `-o` | write to this file, which must not exist; default standard output |
| `-schema` | the schema to read, default `public` |
| `-tables` | comma-separated tables; default all of them |

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

| Flag | |
| --- | --- |
| `-o` | write here instead, with one fixture file only |
| `-stdout` | write to standard output |
| `-all-columns` | write every column of every model, not only those the fixture files use |

### check

Compares the database with the fixture files and reports every difference, every finding and every
difference `generate` would refuse. Exit 3 for a difference, a refusal or a finding the policy makes
an error; a finding the policy makes a warning is reported and leaves the exit code 0.

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
| `-no-lint` | skip the checks against the database's columns and defaults |

The migration is named one second after the newest migration in the directory, never earlier than
now and never the name of another one. A migration whose name sorts after it is warned about.

With a database configured, both sides are respelled by it first, so a value written two ways (`1.10`
and `1.1` in a numeric column) is no change. When the fixture files differ from the state file but
change no value, nothing is generated and the state file takes the new text, so `status -offline`
agrees.

With `-allow-partial`, the refused changes are recorded in the state file as left out: the next
`generate` does not see them again, and `status` fails on them until `baseline -force`.

Refused (exit 2), whatever else it finds: a finding in the fixture files that the policy makes an
error, such as two rows sharing a natural key, as `sync` refuses one; and a fixture migration in the
directory that the state file's history does not include, which is one generated on another branch
against an older state, or one written by hand and not recorded with `baseline -force`. See
[the runbook](production.md#the-state-file-conflicts-in-a-merge).

The changes run in this order: renames, deletes, updates, inserts, and last the updates that point
at a row inserted in the same migration. A delete or an update can free what an insert takes: a
value of a unique column, or the open end of a price that a partial unique index or an exclusion
constraint allows once. On top of that order every change waits for the ones it depends on: a row
pointing at a new row waits for its insert, parents before children in one table too; a row is
deleted once nothing in the migration still names it, children before parents; and a row taking a
value another row of the table gives up waits for it, unless two rows trade values, which no unique
column allows anyway. Models follow their references, in file order otherwise, whether or not the
new file still mentions them.

### baseline

Records the fixture files, as they are, as what the migrations leave a database holding. A state
that differs in content is only replaced with `-force` (exit 2 otherwise), because recording a change
nobody migrated is how a change gets lost. A difference only the column types can settle, such as
`1.10` against `1.1`, is asked of the database when one is configured, as `generate` asks it: when it
is no change, the state is replaced without `-force`.

Also refused without `-force`: a state that records changes `generate -allow-partial` left out, and a
fixture migration in the directory that is not in the state's history and was written by hand.
Refused even with `-force`: a fixture migration generated against another state, on another branch,
which has to be generated again (see [the runbook](production.md#the-state-file-conflicts-in-a-merge)),
and a finding in the fixture files that the policy makes an error.

| Flag | |
| --- | --- |
| `-from <rev>` | record the fixture files as of a git revision; refused (exit 2) when they are missing or empty there |
| `-old <file>` | record this file as the fixture file, under the fixture file's path; with one fixture file only |
| `-force` | replace a differing state, and clear what was left out: a migration you wrote covers the difference |
| `-offline` | do not ask the database whether a difference is only in how values are written |
| `-dsn` | the database to ask, instead of the configuration's |

### status

Lists the fixture files, the state file and what the fixture files change that no migration makes,
then the migrations directory with, when a database is asked, what it applied. With a database, both
sides are respelled by it first, as `generate` does; a difference that is only spelling is a note.

Exit 3 when:

- the state file is there and does not read: it holds git's conflict markers, or its checksum does
  not match;
- the fixture files change something no migration makes, or the state file records a change
  `generate -allow-partial` left out;
- the fixture files have a finding the policy makes an error;
- the directory holds two migrations bun would record under one name, or a fixture migration the state
  file's history does not include;
- with a database, bun's locks table holds the lock on the migrations table;
- with `-require-applied`, a migration is not applied;
- with `-strict-order`, a pending migration sorts before one the database applied.

Exit 1 when there is no state file and git cannot read the fixture files as of `HEAD` (it is not
installed, or this is not a repository): nothing then says what the files change.

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

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | done; for `check`, `status` and `plan`: nothing found. A finding the policy makes a warning is reported and is not a failure |
| 1 | the command could not do its job: a bad flag, no connection, an unreadable file, output that could not be written, a plan that could not finish, nothing for `status` to compare the fixture files with |
| 2 | refused: a difference that needs a hand-written migration, a finding the policy makes an error, a state `baseline` will not replace, a fixture migration the state file does not include (`generate`, `baseline`), a file `export` will not write |
| 3 | found something: drift (`check`), a change no migration makes, a change left out, a state file that does not read, a migration not applied or out of order, a leftover lock (`status`), a migration that would fail or skip (`plan`), a problem in the migrations directory or the state file's history of it (`status`, `plan`) |

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
| `seed_guard_table` | | a table never empty in a seeded database; while it is empty a fixture migration does nothing. Written into the migration in `schema` when it names none and `schema` is not `public` |
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
| `id` | `id` | the primary key column: written on insert when the row has one, never compared or updated, what a reference resolves to |
| `serial` | `false` | the id comes from a sequence; migrations move it past explicit ids |
| `ref` | `name` | the column a reference to this model names a row by |
| `key` | `[ref]` | the natural key: the columns that identify a row without its id |
| `key_any_of` | | groups of mutually exclusive columns; the first set one of each group joins the key |
| `references` | | column to model, for columns holding another row's id |
| `defaults` | | what a column means when a row leaves it out; `~` is NULL |
| `derived` | | columns the application recalculates: never compared, written or exported |
| `ignore` | | columns that take no part |
| `deletes` | `policy.deletes` | `allow`, `refuse` or `cascade` |
| `where` | | an SQL predicate limiting which rows are master data; your SQL, used as written. A generated migration carries it: every statement, natural-key lookup and reference for the model sees only those rows, and a row it writes must hold it. A `;` or a parenthesis it does not open is refused |

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

`id_drift`, `missing_row` and `changed_row` are copied into every generated migration, so changing
them later does not change what an existing migration does.

## JSON output

`check`, `status`, `plan` and `sync` take `-json`. Values are strings as the database spells them,
NULL is `null`, and a reference is `{"model": "Currency", "key": "USD"}`, so neither can be mistaken
for a string. Lists are `[]` rather than `null` when empty. Fields may be added; none will change
meaning.

### check output

```json
{
  "agree": false,
  "findings": [{"kind": "zero against a default", "level": "warn", "model": "Plan", "row": "Plan/name=free",
                "detail": "..."}],
  "refusals": [{"model": "Plan", "key": "Plan/name=old", "reason": "..."}],
  "changes": [
    {"model": "Plan", "kind": "update", "key": {"name": "team"},
     "database": {"price_cents": "2200"}, "file": {"price_cents": "2500"}}
  ]
}
```

`agree` is what the exit code says: `true` for 0. A finding's `level` is what the policy makes of its
kind, `error` or `warn`; a `warn` finding is listed and leaves `agree` true. A change is what a
migration from the database to the file would do: an `insert` is a row only the file has, a `delete`
one only the database has.

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
  "left_out": [],
  "findings": [],
  "directory": "migrations",
  "migrations": [
    {"id": "20260930165255_fixture_plan_prices", "name": "20260930165255", "fixture": true, "changes": 3,
     "applied": {"group": 2, "at": "2026-09-30T17:00:00Z"}, "out_of_order": false}
  ],
  "not_in_state": [],
  "database": {"table": "bun_migrations", "table_exists": true, "not_in_directory": [],
               "newest_applied": "20260930165255", "locks_table": "bun_migration_locks", "locked": false},
  "problems": [],
  "notes": []
}
```

| Field | |
| --- | --- |
| `state` | `null` when no state file is configured. `exists` is whether the file is there, and `error` why one that is there does not read, such as a merge that stopped in it; nothing is compared then. `format` is 1 for a file written before the format was numbered. `covers` is the newest fixture migration whose changes the state includes, `base` what that one was generated against |
| `base` | what `uncovered` was worked out against: `the state file`, or `HEAD` while there is none |
| `uncovered`, `refused` | what the fixture files change that no migration makes, one line per model, and what of it `generate` would refuse |
| `left_out` | changes `generate -allow-partial` refused and recorded in the state file, until `baseline -force` |
| `findings` | as in [check](#check-output), the ones the policy does not ignore |
| `migrations` | `applied` is `null` for a pending migration and for all of them without a database; `out_of_order` is a pending one that sorts before `newest_applied` |
| `not_in_state` | fixture migrations of the directory the state's history does not include; `problems` says why |
| `database` | `null` when none was asked. `locked` is a row in `locks_table` naming `table`: a migrator running now, or one that died and left it |

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
ran. `after` names pending migrations that were not simulated and run before this one. A
migration's `notes`, when there are any, say what its `result` and `error` do not: why the plan
could not tell, or where the deploy can differ from the plan. The top-level `notes` say why a
migration `-with-sql` would have run is in `not_simulated`. `problems` are those `status` reports in
the migrations directory, each of which fails the plan: two migrations under one name, a generated
file that does not read, a fixture migration the state file's history does not include, and the
one it includes last gone from the directory. `rows_locked` is how many rows the fixture
migrations wrote and held locked until the rollback, and `locked_seconds` how long the plan's
transaction was open.

### sync output

```json
{"applied": true, "dry_run": false, "findings": [], "refusals": [], "changes": [ ... ]}
```

`findings` are as in [check](#check-output), with a `level` each.

### Outcomes

`plan` and `sync` report each change as `fixtureapply.Outcome`:

| Field | |
| --- | --- |
| `set` | the change set's name |
| `index` | the change's position in the set; `-1` for an outcome about the whole set |
| `model`, `kind`, `key` | which change: `insert`, `update` or `delete`, and the natural key as `col=value,...` |
| `status` | `applied`, `unchanged` (the database held it already), `skipped` (the policy passed it over), `failed`, `unseeded` (the seed guard table is empty), `sequence` (a sequence moved past explicit ids) |
| `rows` | rows an applied change touched |
| `problem` | why it could not be made: `missing row`, `changed row`, `id drift`, `referenced` (a delete other rows point at), `duplicate key` (more than one row holds the natural key), `lock timeout`, `error` |
| `message` | the sentence a person reads |

## The Go packages

| Package | For |
| --- | --- |
| `fixtureapply` | the run time generated migrations call, in the application's process |
| `fixturechange` | the change-set types a generated migration is written in |
| `fixturemigrate` (the module root) | everything the command does, as a library: configuration, snapshots, diff, render, `Sync` |
| `dbschema` | the catalog reader |

`fixtureapply` needs bun and nothing else; it works under any `database/sql` PostgreSQL driver bun
does, and is tested under `pgdriver` and `pgx`.

| Function | |
| --- | --- |
| `Up(set, opts...)`, `Down(set, opts...)` | the up and down functions a generated file registers with `MustRegister`; they run `Apply` and `Revert`, and `Up` knows the migration's name from the file it is called in |
| `Apply(ctx, db, set, opts...)` | run a change set in one transaction; see [what a migration does](../README.md#what-a-generated-migration-does) |
| `Revert(ctx, db, set, opts...)` | the same set backwards, every change inverted; it assumes `Apply` made every change on this database ([rolling back](production.md#rolling-back)) |
| `Validate(set)` | check a set without a database |
| `SyncSequences(ctx, db, tables...)` | move the sequences of serial and identity columns past the values present, forward only; after a `dbfixture` seed |
| `WithLogger(fn)` | where the per-row lines go; default `log.Printf` |
| `WithSlog(logger)` | write the per-row report to a `*slog.Logger` instead, one record per change with the outcome's fields as attributes; a skipped change is a warning |
| `WithReport(fn)` | receive every `Outcome` as it happens |
| `WithMigrationName(name)` | the migration name, when `Apply` is called by hand from outside the file bun registered |
| `WithDryRun()` | for a caller that rolls back: sequences are reported, not moved |

A change that fails the set comes back as a `*fixtureapply.ChangeError`, which `errors.As` finds in
the error bun's migrator returns: its `Outcome` says which change and why, and it unwraps to the
statement's own error, such as PostgreSQL's, or to nothing when the policy made a problem with the
row fatal. When `Apply` took back bun's record of the failed migration, `errors.Is(err,
fixtureapply.ErrRecordRemoved)` holds: the migration is pending again.

`Apply` takes a `bun.IDB`. Given a `*bun.DB` it opens its own transaction; given a `bun.Tx` it runs
in a savepoint inside it, which is how a migration that also does other work keeps it all in one
transaction. Only on the migrator's `*bun.DB` does a failure take back bun's record of the
migration.

`fixturemigrate.Compute(cfg, old, next)` diffs two snapshots into a `*Result`: `Changes` in the order
they apply, `Refusals` that need a hand-written migration, and `Warnings` the policy lets a migration
carry on past (a renumbered row under `id_drift: warn`). Only refusals stop a migration from being
written.

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

`fixturemigrate.Sync(ctx, db, cfg, files, SyncOptions{DryRun, Logf})` is the `sync` command,
returning a `*SyncResult` with the diff, the findings and the outcomes; `ErrSyncRefused` wraps a
refusal. See [tests and development servers](usage.md#tests-and-development-servers).
