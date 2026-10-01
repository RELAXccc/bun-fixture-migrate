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
configuration file, `fixture-migrate.yml` in the current directory unless `-config` names another;
paths in it are relative to it. Every command takes `-h`.

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
table is read from one snapshot. Such a command works against a hot standby.

### scaffold

Writes a commented starting configuration from a database's catalog: a model per table, the primary
key, whether it is serial, the foreign keys as `references`, the column defaults as `defaults`, and
a natural key guessed from the narrowest unique index besides the primary key. Every guess is marked.

| Flag | |
| --- | --- |
| `-dsn` | the database; default `$DATABASE_URL` |
| `-o` | write to this file, which must not exist; default standard output |
| `-schema` | the schema to read, default `public` |
| `-tables` | comma-separated tables; default all of them |

### export

Writes the fixture files from the database: models in dependency order, references as templates
naming the target row, anchors from the natural key, values in the notation that loads back as the
same value. With several fixture files, each model goes back into the file that holds it and a new
one into the last. Refuses (exit 2) to write a file `dbfixture` would not load back as the database,
such as a zero bun would replace with a column default.

| Flag | |
| --- | --- |
| `-o` | write here instead, with one fixture file only |
| `-stdout` | write to standard output |

### check

Compares the database with the fixture files and reports every difference, every finding and every
difference `generate` would refuse. Exit 3 when anything was found.

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

### baseline

Records the fixture files, as they are, as what the migrations leave a database holding. A state
that differs in content is only replaced with `-force` (exit 2 otherwise), because recording a change
nobody migrated is how a change gets lost. A difference only the column types can settle, such as
`1.10` against `1.1`, is asked of the database when one is configured, as `generate` asks it: when it
is no change, the state is replaced without `-force`. A state that records changes
`generate -allow-partial` left out is replaced only with `-force` too.

| Flag | |
| --- | --- |
| `-from <rev>` | record the fixture files as of a git revision |
| `-old <file>` | record this file |
| `-force` | replace a differing state, and clear what was left out: a migration you wrote covers the difference |
| `-offline` | do not ask the database whether a difference is only in how values are written |

### status

Lists the fixture files, the state file and what the fixture files change that no migration makes,
then the migrations directory with, when a database is asked, what it applied. With a database, both
sides are respelled by it first, as `generate` does; a difference that is only spelling is a note.

Exit 3 when:

- the fixture files change something no migration makes, or the state file records a change
  `generate -allow-partial` left out;
- the directory holds two migrations bun would record under one name;
- with `-require-applied`, a migration is not applied.

Exit 1 when there is no state file and git cannot read the fixture files as of `HEAD` (it is not
installed, or this is not a repository): nothing then says what the files change.

| Flag | |
| --- | --- |
| `-offline` | do not connect, even with a database configured |
| `-require-applied` | fail unless the database applied every migration in the directory |
| `-json` | the report as JSON, see [status](#status-output) |

### plan

Runs the pending fixture migrations against the database, in bun's order, in one transaction that is
always rolled back, and reports every change. Pending migrations it did not write are named, and so
is where they would run. Refused against a standby.

| Flag | |
| --- | --- |
| `-file <path>` | plan this migration file, applied or not; repeat for several |
| `-with-sql` | also run the pending `.up.sql` migrations, split as bun splits them |
| `-strict` | fail when a change would be skipped, too |
| `-lock-timeout <d>` | give up on a row lock after this long, default `5s` |
| `-json` | the report as JSON, see [plan](#plan-output) |

Exit 3 when a migration would fail, or with `-strict` be skipped; exit 1 when the plan could not
finish (a lock waited for too long, a lost connection), which says nothing about the migration.

### sync

Brings the database to the fixture files directly, without a migration: a development database, a
test's, a staging copy. It compares as `check` does and applies the difference with the guards and
the policy of a generated migration, in one `REPEATABLE READ` transaction. Records nothing in the
migrations table. Without `-yes` it rolls back and shows what it would change.

| Flag | |
| --- | --- |
| `-yes` | make the changes |
| `-json` | the report as JSON, see [sync](#sync-output) |

Exit 2 when a finding the policy makes an error, or a difference `generate` would refuse, stops it.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | done; for `check`, `status` and `plan`: nothing found |
| 1 | the command could not do its job: a bad flag, no connection, an unreadable file, a plan that could not finish, nothing for `status` to compare the fixture files with |
| 2 | refused: a difference that needs a hand-written migration, a finding the policy makes an error, a state `baseline` will not replace, a file `export` will not write |
| 3 | found something: drift (`check`), a change no migration makes, a change left out or a migration not applied (`status`), a migration that would fail or skip (`plan`) |

A pipeline can tell "the database drifted" (3) from "the check could not run" (1).

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
| `state` | `<out>/fixture_state.yml` | the state file |
| `seed_guard_table` | | a table never empty in a seeded database; while it is empty a fixture migration does nothing |
| `database` | | a DSN, or `env:NAME` to read one from the environment |
| `schema` | `public` | the schema of tables named without one |
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
| `where` | | an SQL predicate limiting which rows are master data; your SQL, used as written |

### policy

| Key | Values | Default | Decides |
| --- | --- | --- | --- |
| `id_drift` | error, warn, ignore | error | a row under another id than the file's, or the file's id held by another row |
| `missing_row` | error, warn | error | an update or delete whose row is not there. `warn` loses the change for good under bun's migrator |
| `changed_row` | error, warn | warn | a row somebody changed in this database; `warn` keeps their change |
| `zero_default` | error, warn, ignore | error | a zero in a column whose default is not that zero, which bun replaces with the default |
| `null_default` | error, warn, ignore | error | a null in a column with a default, which a nil pointer or `nullzero` field replaces with the default |
| `duplicate_key` | error, warn | error | two rows sharing a natural key |
| `renames` | refuse, update | refuse | a row that kept its id and changed its natural key |
| `deletes` | allow, refuse, cascade | allow | a row that left the file. `allow` fails while other rows point at it; `cascade` lets the foreign keys' ON DELETE act |

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
  "findings": [{"kind": "zero against a default", "model": "Feature", "row": "code=api", "detail": "..."}],
  "refusals": [{"model": "Plan", "key": "name=old", "reason": "..."}],
  "changes": [
    {"model": "Plan", "kind": "update", "key": {"name": "team"},
     "database": {"price_cents": "2200"}, "file": {"price_cents": "2500"}}
  ]
}
```

A change is what a migration from the database to the file would do: an `insert` is a row only the
file has, a `delete` one only the database has.

Finding kinds: `duplicate key`, `zero against a default`, `null against a default`,
`invalid value` (a value the column's type cannot hold), `unknown column`.

### status output

```json
{
  "fixture": "fixtures/fixture.yml",
  "state": {"path": "migrations/fixture_state.yml", "exists": true, "migration": "20260930165255_fixture_plan_prices"},
  "base": "the state after 20260930165255_fixture_plan_prices",
  "uncovered": [],
  "refused": [],
  "left_out": [],
  "directory": "migrations",
  "migrations": [
    {"id": "20260930165255_fixture_plan_prices", "name": "20260930165255", "fixture": true, "changes": 3,
     "applied": {"group": 2, "at": "2026-09-30T17:00:00Z"}}
  ],
  "database": {"table": "bun_migrations", "table_exists": true, "not_in_directory": []},
  "problems": [],
  "notes": []
}
```

`applied` is `null` for a pending migration and for all of them without a database; `database` is
`null` when none was asked. `left_out` is the changes `generate -allow-partial` refused and recorded
in the state file, until `baseline -force`.

### plan output

```json
{
  "migrations": [
    {"id": "20260930165255_fixture_plan_prices", "kind": "fixture", "result": "succeeds",
     "after": ["20260930160000_schema"],
     "changes": [{"set": "20260930165255_fixture_plan_prices", "index": 0, "model": "Plan",
                  "kind": "insert", "key": "name=pro", "status": "applied", "rows": 1}]}
  ],
  "not_simulated": ["20260930160000_schema"]
}
```

`result` is `succeeds`, `fails`, `unseeded`, `not reached` (after one that fails), or `inconclusive`
when the plan itself could not finish. `kind` is `fixture`, or `sql` for a migration `-with-sql`
ran. `after` names pending migrations that were not simulated and run before this one.

### sync output

```json
{"applied": true, "dry_run": false, "findings": [], "refusals": [], "changes": [ ... ]}
```

### Outcomes

`plan` and `sync` report each change as `fixtureapply.Outcome`:

| Field | |
| --- | --- |
| `set` | the change set's name |
| `index` | the change's position in the set; `-1` for an outcome about the whole set |
| `model`, `kind`, `key` | which change: `insert`, `update` or `delete`, and the natural key as `col=value,...` |
| `status` | `applied`, `unchanged` (the database held it already), `skipped` (the policy passed it over), `failed`, `unseeded` (the seed guard table is empty), `sequence` (a sequence moved past explicit ids) |
| `rows` | rows an applied change touched |
| `problem` | why it could not be made: `missing row`, `changed row`, `id drift`, `referenced` (a delete other rows point at), `error` |
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
| `Apply(ctx, db, set, opts...)` | run a change set in one transaction; see [what a migration does](../README.md#what-a-generated-migration-does) |
| `Revert(ctx, db, set, opts...)` | the same set backwards, every change inverted |
| `Validate(set)` | check a set without a database |
| `SyncSequences(ctx, db, tables...)` | move the sequences of serial and identity columns past the values present, forward only; after a `dbfixture` seed |
| `WithLogger(fn)` | where the per-row lines go; default `log.Printf` |
| `WithReport(fn)` | receive every `Outcome` as it happens |
| `WithMigrationName(name)` | the migration name, when `Apply` is not called from a file bun registered |
| `WithDryRun()` | for a caller that rolls back: sequences are reported, not moved |

`Apply` takes a `bun.IDB`. Given a `*bun.DB` it opens its own transaction; given a `bun.Tx` it runs
in a savepoint inside it, which is how a migration that also does other work keeps it all in one
transaction. Only on the migrator's `*bun.DB` does a failure take back bun's record of the
migration.

`fixturemigrate.Sync(ctx, db, cfg, files, SyncOptions{DryRun, Logf})` is the `sync` command,
returning a `*SyncResult` with the diff, the findings and the outcomes; `ErrSyncRefused` wraps a
refusal. See [tests and development servers](usage.md#tests-and-development-servers).
