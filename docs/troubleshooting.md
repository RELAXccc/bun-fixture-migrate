# Troubleshooting

Messages the tool prints, what each means, and what to do. For what to do when a deploy goes wrong,
see the [production runbook](production.md); this page is about the tool itself.

- [Configuration](#configuration)
- [Connecting](#connecting)
- [Reading the fixture file](#reading-the-fixture-file)
- [Findings](#findings)
- [Refusals](#refusals)
- [The state file](#the-state-file)
- [Generating](#generating)
- [Planning](#planning)
- [Status](#status)
- [At run time](#at-run-time)

## Configuration

**`flag provided but not defined: -x; "bun-fixture-migrate plan -h" lists its flags`.** Flags come
after the command, and each command has its own; `-h` lists them. Only the commands that connect take
`-dsn`.

**`open fixture-migrate.yml: no such file or directory`.** The configuration is looked for in the
current directory unless `-config` or `$BUN_FIXTURE_MIGRATE_CONFIG` names it.

**`model "X" is in the fixture file but not in the configuration`.** Every model the fixture files
name needs an entry under `models:`. A model the tool silently skipped would be a change that never
happens. `scaffold` writes entries for every table.

**`model "X": the configuration says Y, which is not a table in this database`.** `table:` names a
table the database does not have in `schema` (default `public`). Qualify it, `billing.plans`, or set
`schema`.

**`policy missing_row is "warning", it has to be one of error, warn`.** Every policy value is checked;
a typo is not read as the nearest value, because the generated migration copies it and nobody reads
it again.

**`field X not found in type fixturemigrate.Config`.** An unknown key in the configuration. Keys are
listed in the [reference](reference.md#configuration).

**`the models A, B reference each other in a circle`.** The `references:` form a cycle, so there is
no order to insert them in. A self-reference is fine; a cycle across models needs one of the columns
left out of `references` (and set by hand).

## Connecting

**`no database in the configuration file and no -dsn; this command needs one`.** Set `database` in
the configuration, usually `env:DATABASE_URL`, or pass `-dsn`.

**`the database DSN is to be read from the environment variable DATABASE_URL, which is not set`.**
`database: env:DATABASE_URL`, or `-dsn env:DATABASE_URL`, names an environment variable; set it.

**`the database DSN is not a URL pgdriver can read`.** Write it as
`postgres://user:password@host:5432/dbname?sslmode=disable`. A keyword DSN (`host=... user=...`) is
not accepted. Passwords are never repeated in a message.

**`the database DSN ... is a unix socket DSN without the socket's path`.** pgdriver's socket form
names the socket file: `unix://user:password@dbname/var/run/postgresql/.s.PGSQL.5432`.

**`the database did not start a read-only transaction; refusing to go on`.** A pooler in transaction
mode, or a proxy, dropped `SET TRANSACTION READ ONLY`. Connect directly, or through a session-mode
pool, for the reading commands.

**`row-level security hides rows of X from this role`.** A policy on the table limits what the role
connecting sees, and master data read through it would lack the rows it hides. Connect as a role with
`BYPASSRLS`, or as the table's owner while the table is not `FORCE ROW LEVEL SECURITY`.

**`the database is a standby, which accepts no writes, so nothing can be planned`.** `plan` has to
write to simulate. Point it at the primary or a writable copy; `check` and `status` work on a
standby.

## Reading the fixture file

**`X.col is {{ $.Y.row.ID }}, but "row" is not a name text/template can follow`.** An anchor with a
dash or starting with a digit cannot be named in a template, so `dbfixture` cannot load the file
either. Rename the `_id`.

**`... names a row of Y that the file only defines further down`.** `dbfixture` resolves templates
top to bottom. Move the target row, or its whole model block, above the row naming it.

**`X.col is {{ now }}, a template dbfixture evaluates when it loads the file`.** The database never
holds this text, so it cannot be compared or migrated. Add the column to `ignore`.

**`X.col = 7: no row of Y in this file has that id`.** A reference column holding a plain id has to
name a row of the file. Write the reference as a template instead.

**`key column "code" is missing from a row and has no default`.** Every row needs its natural key.
Add it, or a `defaults` entry.

## Findings

Findings are problems in the fixture files that the configuration's `policy` makes errors (exit 2)
or warnings.

| Kind | Means | Fix |
| --- | --- | --- |
| `zero against a default` | a `0`, `false` or `""` into a column whose default is something else; bun writes `DEFAULT` for it | write the default's value, drop the column default, or `policy.zero_default` |
| `null against a default` | `~` into a column with a default; a nil pointer or `nullzero` field writes `DEFAULT` | leave the column out, write the value, or `policy.null_default` if your models use `sql.Null*` types |
| `invalid value` | PostgreSQL cannot cast the value to the column's type: `abc` into an integer, `2026-02-30` into a date, an enum label that does not exist | fix the value |
| `unknown column` | the table has no such column | fix the name, or `ignore` it |
| `duplicate key` | two rows share a natural key | fix the rows, and give the table a unique index on the key |

A generated column, or an explicit id in an `IDENTITY ALWAYS` column, is reported the same way:
PostgreSQL refuses to write either.

## Refusals

A refusal is a difference `generate` will not write as it stands (exit 2).

| Reason | Means |
| --- | --- |
| `the natural key is not unique` | two rows share a key and the group changed |
| `deletes of this model are refused by the configuration` | the model is `deletes: refuse` |
| `renamed from name=a to name=b` | a row kept its id and changed its key; `policy.renames: update` writes it |
| `its id changed from 3 to 4` | a row changed its id, under `policy.id_drift` |
| `column "x" is written on one side and left out on the other` | no `defaults` entry says what leaving it out means |
| `version is written 1.10, which a column written from a Go string holds as written ...` | a value whose meaning depends on the column type, in a change computed without a database |

For the last one, configure `database` so `generate` can read the column types (and leave out
`-no-lint`), or write the value unambiguously: quoted for a text column, resolved for any other
(`1.1`, `15`, `true`, `2026-01-01T10:00:00Z`). [Fixture files](fixture-files.md#values) explains
why.

## The state file

**`the state file does not match its own checksum, so it was edited by hand`.** Or merged line by
line. See [the runbook](production.md#the-state-file-was-edited-or-lost).

**`this is not a state file bun-fixture-migrate wrote: the marker line is missing`.** `state:` points
at a fixture file, or the file was replaced. Check the path.

**`the state file holds git's conflict markers`.** Two branches each generated a migration, and the
merge stopped in the state file, as it is meant to. See
[the runbook](production.md#the-state-file-conflicts-in-a-merge).

**`the state file is format 3, written by a newer bun-fixture-migrate than this one`.** Somebody
generated with a newer release. Use the release the project pins. A state file written before the
format was numbered still reads, and is rewritten in the current format the next time `generate` or
`baseline` writes it.

**`no state file at ... yet, so this diffs against git HEAD`.** Not an error: until `baseline` or the
first `generate` writes one, the base is the committed fixture file.

**`there is no state file at ..., and git cannot say what ... was at HEAD`** (`status`, exit 1).
Without a state file the base is git's `HEAD`, and git is not installed, or the project is not a
repository, or the fixture file was never committed. Nothing then says what the fixture file
changes, so status does not pass it. Run `baseline` once the databases hold the fixture file, or
run `status` in a checkout with git.

**`baseline would record N changes as migrated with no migration to make them`.** The fixture file
differs from the state, and no migration covers it. Run `generate`. Pass `-force` only when you
wrote the migration yourself. A value written another way, `1.10` for `1.1` in a numeric column, is
not counted when a database is configured: `baseline` asks it, unless `-offline`.

**`left out`** (`status`, exit 3) **/ `the state records N changes generate left out`** (`baseline`,
exit 2). `generate -allow-partial` wrote the rest of a change and recorded these in the state file.
Write their migration by hand, then `baseline -force`.

**`... is a generated fixture migration whose changes the state file does not include`** (`status`,
exit 3; `generate` and `baseline`, exit 2). Two branches each generated a migration from one state,
and the merge kept the state file of one of them. `baseline -force` cannot fix that: see
[the runbook](production.md#the-state-file-conflicts-in-a-merge).

**`... is a fixture migration whose changes the state file does not include. If you wrote it by
hand ...`.** A migration holding a `fixturechange.Set` that `generate` did not write. Once the
fixture file holds what it does, record it with `baseline -force`.

**`the fixture file differs from the state file only in how values are written`** (`status` note).
A value is spelled differently, `1.10` for `1.1` in a numeric column, which only the database can
tell from a change; `status -offline` fails on it until the state file has the new spelling. Run
`generate`: it writes no migration and records the new spelling.

## Generating

**`nothing changed in fixtures/fixture.yml since the state after ...`.** The file and the state agree.
If you expected a change, check the file was saved, and that `status -offline` agrees. When the file
differs from the state only in comments or in how values are written, `generate` says it wrote the
state file with the new text.

**`N fixture migrations the state file does not include, nothing written`.** See the state file
section above.

**`warning: migration 3_backfill sorts after 20260930165255, so bun runs it after this one`.** bun
orders migrations by name as strings. A migration named with a short number sorts after every
timestamp. Rename it with a timestamp, or check that running it later is harmless.

**`migrations/x.go is package foo and the configuration says package migrations`.** The generated
file would not compile there. Fix `package:` in the configuration.

**`warning: no file in migrations declares the variable Migrations ...`.** The generated file
registers with the variable named by `migrator:`. Declare it, or fix the name.

**`the migration is written, the state file is not`.** The file system refused the second write.
Delete the migration it names and generate again once the cause is fixed: recording it with
`baseline` instead is refused, because the state file's history does not include it.

## Planning

**`could not be planned` / `inconclusive`.** The plan itself failed: a row lock held longer than
`-lock-timeout`, a statement timeout, a lost connection, or something a single transaction cannot
do, which the note under it names. Nothing is known about the migration. Try again, or raise
`-lock-timeout`.

**`pending before it and not simulated: ...`.** Migrations the tool did not write, such as schema
changes, run before this one in the deploy but not in the plan. If they change the tables the
fixture migration touches, run `plan -with-sql` so SQL migrations run too, or plan against a copy
that already has them.

**`it needs a table or a column this database does not have`.** The same, when the fixture migration
failed on a missing table or column: one of those migrations may create it, so the plan cannot tell.
Plan with `-with-sql` if they are SQL migrations, or against a copy that has them applied.

**`when it commits, where PostgreSQL checks the constraints it defers: ...`.** The migration breaks
a constraint declared `DEFERRABLE INITIALLY DEFERRED`, which PostgreSQL checks at `COMMIT`. Its
statements succeed and the deploy fails when it commits, as in the plan. `sync` says the same as
`the changes would fail when committed`.

**`bufio.Scanner: token too long`.** A line of a SQL migration is longer than 64 KiB, and bun reads
SQL migrations a line at a time. The deploy fails before running any of the file, and unless the
migrator is built `WithMarkAppliedOnSuccess(true)` bun keeps it recorded as applied, so it never runs
again. Break the line up, for instance a long `VALUES` list into a row per line.

**`holds "{{", which bun renders as a Go template`.** `plan -with-sql` does not run such a file: what
bun runs depends on the migrator's `WithTemplateData`. Plan against a copy that has it applied.

**`it uses an enum value a migration before it in this plan added`.** One transaction cannot use an
enum value it added, and the plan is one transaction; the deploy commits the migration that adds it
first. Plan again once that migration is applied.

**`note: a quoted string or dollar-quoted body in it holds a blank line`.** Not an error under bun
v1.2.18, which runs the file as written. bun after it drops blank lines from SQL migrations, which
changes that string; write the line break as `E'\n'` or `chr(10)` before upgrading bun.

## Status

**`out of order: runs after 20261001110000`.** The migration is pending and sorts before one this
database applied. bun runs it on the next migrate all the same, after migrations it was not written
to follow: usually a branch merged after a later one was deployed. `plan` against a copy of the
database shows what it does there; `status -strict-order` fails on it.

**`locked: bun_migration_locks holds bun's lock on bun_migrations`** (exit 3). A migrator is running,
or one died between `Lock` and `Unlock`, and every migrate fails with
`migrations table is already locked` until the row is gone. See
[the runbook](production.md#every-migrate-fails-the-migrations-table-is-already-locked).

**`Plan.price_cents is written like 29.00 in 12 rows, ...`** (`status -offline`). Without a
database, a value whose meaning depends on the column's type is refused, once per model and column.
Configure `database` and run `status` without `-offline`, or write the value the way the column
reads it back (`29`, or `"29.00"` for a text column).

**`N findings in the fixture file that the policy makes errors`** (`status`, exit 3; `generate` and
`baseline`, exit 2). The fixture files turned up something the policy makes an error, such as two
rows sharing a natural key. Fix it, or set the policy to `warn`.

**Starting the history over.** When every database applied every migration in the directory and the
state file's history no longer matters, delete the state file and run `baseline`: a new state file
includes every fixture migration there is.

## At run time

Messages a generated migration returns through bun's migrator:

| Message contains | Problem | See |
| --- | --- | --- |
| `no row of plans has name=team` | missing row | [a migration failed](production.md#a-fixture-migration-failed-during-a-deploy) |
| `no longer holds the values this change was generated against` | changed row | [a change was skipped](production.md#a-change-was-skipped) |
| `exists, but under id 7 and not 3` | id drift | [a migration failed](production.md#a-fixture-migration-failed-during-a-deploy) |
| `2 rows of features point at plans name=pro through ...` | referenced | [a migration failed](production.md#a-fixture-migration-failed-during-a-deploy) |
| `plans is empty, nothing to do` | not a problem: the database is not seeded yet | [a new environment](production.md#a-new-environment) |
| `wait for another change set to finish` | the advisory lock wait was cancelled | another process was applying a change set; retry |

**`bun may have recorded X as applied before running it, and the record could not be removed`.** The
migration failed and rolled back, but its record in the migrations table is still there. Delete the
row named in the message before the next deploy, or the migration will not run again. With
`WithMarkAppliedOnSuccess(true)` this cannot happen.

**A migration succeeds on one database and fails on another.** Ids and hand edits differ between
databases; that is what the guards are for. `plan` against each shows why.
