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
happens. `scaffold` writes entries for every table but bun's own; keep those of master data.

**`model "X": the configuration says Y, which is not a table in this database`.** `table:` names a
table the database does not have in `schema` (default `public`). Qualify it, `billing.plans`, or set
`schema`. When Y is a view, a materialized view or a foreign table the message says so: only a table
holds master data, so name the table whose rows it shows.

**`policy missing_row is "warning", it has to be one of error, warn`.** Every policy value is checked;
a typo is not read as the nearest value, because the generated migration copies it and nobody reads
it again.

**`field X not found in type fixturemigrate.Config`.** An unknown key in the configuration. Keys are
listed in the [reference](reference.md#configuration).

**`model "X": its key is [name], and public.events has no column name`** (`export`, `check`,
`generate -from-db`). The key, or `ref` when `key` is not set, names a column the table does not
have. Set `key` to the columns that tell two rows apart, those of a unique index; a table with none
is not master data this tool can migrate. **`model "X": references names C, and T has no such
column`** is the same for a reference.

**`-tables names plan, which is not a table of schema public`** (`scaffold`). A typo, a table of
another schema (`-schema`), a partition, or bun's own table. **`schema S has no table to propose as a
model`**: the schema does not exist, the role cannot see its tables, or it holds only bun's.

**`warning: no seed_guard_table`** (`generate`). The migration is written, and on a database that was
never seeded it runs before the seed and fails the deploy. Set `seed_guard_table` to a table the
fixture file fills. **`warning: seed_guard_table X is the table of no model`**: the fixture files do
not fill it, so a seeded database may hold it empty, and there every fixture migration does nothing
and is recorded as applied. Name the table of a model.

**`model "X": deletes is "cascade", but mode is upsert, under which no row is ever deleted`.**
`deletes` only applies under `mode: sync`; a model under `upsert` or `insert` deletes nothing. Take
`deletes` off the model, or its `mode`. A `deletes` the model inherits from the policy block is fine.

**`model "X": insert_only column "code" is part of the natural key`** (or the `ref` column, the id,
or a column in `ignore` or `derived`). An `insert_only` column is one the database owns once the row
exists, and every row is found by its key and named by its `ref` column, which have to be the
files'. **`model "X": ids is database, so its id differs from one database to the next`**: under
`ids: database` the key and the `ref` column cannot be the id; key and name the rows by columns the
files own.

**`model "X": soft_delete column "deleted_at" is named in where`** (or is the id, part of the key,
the `ref` column, a reference, or in `ignore`, `derived`, `insert_only` or `defaults`). The
[`soft_delete`](reference.md#soft-deleted-rows) column says whether a row is live and nothing else.
Drop `deleted_at IS NULL` from `where`: `soft_delete` limits the model to live rows already, and a
migration that restores a row has to see the soft-deleted ones. **`is set with deletes: cascade`**:
a soft delete reaches no row through a foreign key; take `deletes` out. **`is set with mode
insert, which this version does not support`**: under `mode: insert` a soft-deleted row is the
database's and must not come back, which a migration cannot tell; use `mode: upsert`, or leave
`soft_delete` out.

**`soft_delete names deleted_at, a bigint column`** (`check`, `sync`, `export`, `generate`).
bun's `int64` soft-delete field writes a time into the column and fails on PostgreSQL; only a nullable
`timestamptz` or `timestamp` column, NULL for a live row, works. **`which is NOT NULL, so no row could
be live`**: make the column nullable. **`which defaults to now()`**: bun writes the default for a
`nullzero` field's zero and for a nil pointer, so every row bun inserts is born soft-deleted; drop
the default. **`does not have`**: the table has no such column.

**`the models A, B reference each other in a circle`.** The `references:` form a cycle, so there is
no order to insert them in. A self-reference is fine; a cycle across models needs one of the columns
left out of `references` (and set by hand).

## Connecting

**`no database in the configuration file and no -dsn; this command needs one`.** Set `database` in
the configuration, usually `env:DATABASE_URL`, or pass `-dsn`.

**`the database DSN is to be read from the environment variable DATABASE_URL, which is not set`.**
`database: env:DATABASE_URL`, or `-dsn env:DATABASE_URL`, names an environment variable; set it.
`generate` connects only to check the fixture file against the columns and respell its values;
`generate -no-lint` writes the migration without it.

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

**`the database starts every transaction of this connection read only`.** The role `plan` connects
as, or its DSN, sets `default_transaction_read_only`: a read-only role. `plan` writes and rolls back,
so connect it as a role with the rights the migrations need, such as the one the deploy uses; keep
the read-only role for `check` and `status`.

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

**`X.col: {{ $.Y.row.ID }} points at a row of Y that is soft-deleted (deleted_at = …)`.** The row it
names sets the model's [`soft_delete`](reference.md#soft-deleted-rows) column, so `dbfixture` seeds it
soft-deleted and bun loads it through no relation. Take `deleted_at` off that row, or point this one
elsewhere. **`X: col = 3 points at public.y id 3, which is soft-deleted`** (`check`, `sync`,
`export`) is the same in the database: restore the row, `UPDATE … SET deleted_at = NULL`, or repoint
the live row. **`whose deleted_at holds the zero time`**: the row is live to a `time.Time` field
without `nullzero`, which wrote it; see the `soft delete` finding below.

## Findings

Findings are problems in the fixture files that the configuration's `policy` makes errors (exit 2)
or warnings.

| Kind | Means | Fix |
| --- | --- | --- |
| `zero against a default` | a `0`, `false` or `""` into a column whose default is something else; bun writes `DEFAULT` for it | write the default's value, drop the column default, or `policy.zero_default` |
| `null against a default` | `~` into a column with a default; a nil pointer or `nullzero` field writes `DEFAULT`. In a `json` or `jsonb` column, `~` is the JSON null to a map field and NULL to a pointer | leave the column out, write the value, or `policy.null_default` if your models use `sql.Null*` types or pointers |
| `invalid value` | the column cannot take the value as `dbfixture` writes it: PostgreSQL cannot cast it (`abc` into an integer, `2026-02-30` into a date, an enum label that does not exist, a domain's `CHECK`), it is too long for the column, a single-column `CHECK` refuses it, an integer column gets a fraction; or a `time.Time` and a string field, or two servers, would store two values. The message names them | fix the value; [dates and times](fixture-files.md#dates-and-times) |
| `unknown column` | the table has no such column | fix the name, or `ignore` it |
| `duplicate key` | two rows share a natural key, or two keys are one value to the key's type, `Go` and `GO` in `citext`, or to a unique index stricter than the key, `Ann@` and `ann@` under `lower(email)` | fix the rows, and give the table a unique index on the key; where it has one that let the second row in, the message names it and says what to change, below |
| `unbacked key` | no unique index or constraint makes the natural key unique among the model's rows; see [unbacked keys](#unbacked-keys) | add the index the message names, or set `policy.key_index` |
| `soft delete` | a [`soft_delete`](reference.md#soft-deleted-rows) column that is not a nullable timestamp without a default; or rows holding the zero time in it, `N rows hold the zero time in deleted_at`, which a `time.Time` field without `nullzero` writes for a live row and the tool reads as deleted. Always an error | give the field `nullzero` or make it a pointer, and `UPDATE … SET deleted_at = NULL WHERE deleted_at = '0001-01-01 00:00:00+00'` |
| `ambiguous value` | among others, `deleted_at is the zero time` in a fixture row: a `nullzero` field writes NULL, a live row, a pointer field the zero time, a deleted one | write `~` for a live row, or the time it was deleted |

A generated column, or an explicit id in an `IDENTITY ALWAYS` column, is reported the same way:
PostgreSQL refuses to write either.

### Unbacked keys

`check`, `generate`, `status` and `sync` read the table's unique indexes and constraints and say,
for every natural key and every `ref` column another model references, whether one makes it unique
among the model's rows. Without one, the application, an admin UI or a race with the migration's
own insert adds a second row with the key, and every change to it fails from then on. They are
warnings under the default `policy.key_index: warn`, errors under `error` (exit 3 for `check`, 2 for
`generate` and `sync`), and a model can set its own. The message says which; each ends with what to
do:

**`no unique index or constraint backs key [plan_id, code]: … CREATE UNIQUE INDEX ON features
(plan_id, code)`.** There is none. Create the one it names: with `NULLS NOT DISTINCT` (PostgreSQL 15
and later) or over `COALESCE` before 15 where a key column is nullable, and with the model's
`where` as its predicate when it has one. On a live table, `CREATE UNIQUE INDEX CONCURRENTLY`, in a
migration of its own that runs outside a transaction. For a `ref` column it says every reference
then fails to resolve: two rows hold the name a reference looks up.

**`UNIQUE (code, plan_id) is over more columns than key [code], so two rows may share code`.** The
index allows two rows with one `code` and two plans. Either the key is wrong, and `key: [code,
plan_id]` is what tells two rows apart, or the table needs the index on `code` the message names.

**`parent_id is nullable and UNIQUE (parent_id, code) holds NULLs distinct, so any number of rows may
hold the same code with parent_id NULL`.** A unique index never refuses a row with a NULL in it, and
the tool looks a NULL up as NULL, so two root categories called `root` are one key to the tool and
two rows to the index. Recreate it `NULLS NOT DISTINCT` (PostgreSQL 15 and later), index
`(COALESCE(parent_id, 0), code)` in its place before 15, or make the column `NOT NULL`. **`lower(email)
is NULL where email is`** is the same for an expression index.

**`UNIQUE (code) WHERE deleted_at IS NULL holds only where deleted_at IS NULL, and this model reads
every row`** (or `the rows this model reads, where …, are not all within it`). A partial index backs
the key only among the rows its predicate holds for. Over `deleted_at IS NULL` it is the live rows of
a table bun soft-deletes: set the model's [`soft_delete`](reference.md#soft-deleted-rows), and the
model reads them, which the index backs. Otherwise, if only those rows are master data, give the
model a `where` that says so, such as the predicate itself; if every row is, the table needs an index
over every row. A predicate the `where` repeats is taken as implied; otherwise PostgreSQL's planner
decides, so `status = 'active'` implies `status <> 'retired'`.

**`cannot tell whether UNIQUE … backs key […]: the planner chose … for the lookup instead`** (or
`PostgreSQL's planner could not be asked`, or `could not evaluate`). The lint could not decide, and
says so as a warning whatever `key_index` is. Repeat the index's predicate in the model's `where` if
it holds, or create the index the message names.

**`unique index k_invalid_code is invalid, left by a CREATE INDEX CONCURRENTLY that failed, and backs
nothing`.** The index build failed, usually on the very duplicates it was to keep out, and the index
stayed behind refusing nothing. Find and remove the duplicates (`check` lists them as `duplicate
key`), then `REINDEX INDEX CONCURRENTLY` it, or drop it and create it again.

**`UNIQUE (lower(email)) holds the natural keys email=ann@example.com and email=Ann@example.com
equal, so dbfixture cannot load these 2 rows`** (a `duplicate key`). An index stricter than the key
holds two keys of the fixture files equal: dbfixture fails loading them, and a migration fails on
the second insert. Make the rows differ as the index compares them, or drop one.

**The duplicate-key messages name an index that let the second row in**: `UNIQUE (parent_id, code)
holds NULLs distinct, and lets in a second row with parent_id NULL`, `UNIQUE (code) WHERE deleted_at
IS NULL holds only where deleted_at IS NULL, and lets in the rows outside it`, or an invalid index.
Remove the extra rows, then fix the index as above; only a table without one is asked to `give the
table a unique index`.

## Refusals

A refusal is a difference `generate` will not write as it stands (exit 2).

| Reason | Means |
| --- | --- |
| `the natural key is not unique` | two rows share a key and the group changed |
| `deletes of this model are refused by the configuration, policy.deletes: refuse` | the model is under `deletes: refuse`, the policy block's or, where the message says `the model's deletes`, its own |
| `renamed from name=a to name=b` | a row kept its id and changed its key; `policy.renames: update` writes it |
| `its id changed from 3 to 4` | a row changed its id, under `id_drift: error`, the policy block's or the model's own, as the message names it; with `warn` it is a warning and the rest of the row is migrated |
| `column "x" is written on one side and left out on the other` | no `defaults` entry says what leaving it out means |
| `version is written 1.10, which a column written from a Go string holds as written ...` | a value whose meaning depends on the column type, in a change computed without a database |
| `trial is written 86400 seconds before and 24:00:00 after, which an interval column holds as one value` | two spellings of one interval, in a change computed without a database: no change in an interval column, a change in a text one |
| `currency_id points at the Currency whose code is written 0012, ...` | the same for a reference: the ref value of the row it names reads two ways, and only that ref column's type says which the database holds |
| `code is written 0012 before and 012 after` | a key or a value only respelled: no change in a numeric column, a rename in a text one |
| `moves (grp, position) from (g, 3) to (g, 1) while Item/name=a and Item/name=b trade values with it in a circle` | rows swap or rotate the values of a unique index that is checked after every statement, so no order of the updates gets through: declare it a `UNIQUE` constraint `DEFERRABLE INITIALLY IMMEDIATE`, which a migration checks at its end, or move one row to a free value in a migration of its own first. Without the database it is a warning, since nothing says the column is unique |
| `parent_id points at Category "Accessories", which 2 rows hold` | a reference by a ref value more than one row holds; make the ref column unique |
| `its id, 4, is the id of Plan/name=max too` | two rows of the file share an id, usually after merging two branches that each added the next one |
| `it leaves out note, which other rows of Plan write` | an inserted row leaves out a column with no `defaults` entry; write it, or add one |
| `tags: it is a sequence holding a null` | a null inside a YAML sequence, which a `[]string` field drops and a `[]*string` keeps |
| `renamed from code=a to code=b, and mode insert never changes a row a database holds` | under `mode: insert` a rename would update a row the database owns: put the key back, or give the new row an id of its own to add it beside the old one |
| `points at Country "Deutschland", whose name was "Germany", and mode insert never updates a row a database holds` | the row a change points at is under `mode: insert`, and the files changed its `ref` value, which a database holding the row never takes: put it back, or hand-write the change |

For the last one, configure `database` so `generate` can read the column types (and leave out
`-no-lint`), or write the value unambiguously: quoted for a text column, resolved for any other
(`1.1`, `15`, `true`, `2026-01-01T10:00:00Z`). [Fixture files](fixture-files.md#values) explains
why.

## The state file

**`the state file does not match its own checksum, so it was edited by hand`.** Or merged line by
line. See [the runbook](production.md#the-state-file-was-edited-or-lost).

**`this is not a state file bun-fixture-migrate wrote: no "# format:" line follows the comment on top`.**
`state:` points at a fixture file, or the file was replaced. Check the path. A blank line in the
comment on top is not the cause: nothing reads the comment.

**`the state file holds git's conflict markers`.** Two branches each generated a migration, and the
merge stopped in the state file, as it is meant to. See
[the runbook](production.md#the-state-file-conflicts-in-a-merge). `baseline -force` does not
replace such a file either: take one side first, `git checkout --ours` or `--theirs`. `status` says
`state file ... does not read` and exits 3.

**`the state file includes the changes of X, which is not in internal/migrations`** (`status` and
`plan`, exit 3; `generate` and `baseline`, exit 2). The migration the state file says it includes last
was deleted or renamed, so the state says its changes are made and no migration makes them: they
would reach no database. Put the file back under its name, or take the state file back from git as
it was before that migration (`git checkout <rev> -- <state file>`) and generate again. In a merge,
it is the state file of the side whose migration was deleted: take the other side.

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

**`-old records one file, and the configuration has N fixture files`** (`baseline`). Export the files
in place, run `baseline -force`, and take them back with `git checkout`, as
[the runbook](production.md#the-state-file-was-edited-or-lost) says.

**`fixtures/fixture.yml is missing or empty as of <rev>`** (`baseline -from`, exit 2). The revision is
from before the fixture file existed. Recorded, it would say the databases hold no master data, and
the next `generate` would insert every row.

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

**`note: the export does not keep the comments of fixtures/fixture.yml`** (`export`). The file is
written anew from the database, so its comments are gone. Put back the ones to keep before you
commit; the diff shows where they were.

**`the migration is written, the state file is not`.** The file system refused the second write.
Delete the migration it names and generate again once the cause is fixed: recording it with
`baseline` instead is refused, because the state file's history does not include it.

**`holds a change set this tool cannot read back (line 12: unknown field X; the file may have been written
by a newer version ...)`.** `status` and `plan` read generated files without compiling them, and
this version does not know something the file uses. Upgrade the command to the version of
`fixtureapply` the application uses.

**`fixtureapply.Up is handed fixtureChangesB, and the change set this file declares is
fixtureChangesA`.** The file was copied from another migration and only the set was renamed: bun
would run the other migration's set under this file's name. Hand `Up` and `Down` the set the file
declares.

## Planning

**`note: Feature: no unique index or constraint backs key [code, plan_id]: …`.** A key a change of the
migration looks a row up by is not backed by a unique index, as the database stands when the
migration would run: a duplicate the application adds before the deploy makes the change fail. It
changes nothing about the plan's result; see [unbacked keys](#unbacked-keys).

**`could not be planned` / `inconclusive`.** The plan itself failed: a row lock held longer than
`-lock-timeout`, a statement timeout, a lost connection, or something a single transaction cannot
do, which the note under it names. Nothing is known about the migration. Try again, or raise
`-lock-timeout`.

**`the role plan connects as lacks a privilege here, or a row-level security policy limits it`.** A
change, or the record of the run in the audit table, failed with `permission denied`. plan cannot
tell whether it connects as the role the deploy migrates as, so the plan is inconclusive (exit 1).
If it is that role, the deploy fails here the same way: grant what the error names, such as `CREATE`
on the schema of an audit table that does not exist yet. If not, plan as that role. **`the role plan
connects as cannot write here`** is a read-only transaction: plan as the role the deploy uses.

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

**`plan -file takes a fixture migration generate wrote, a .go file`.** A SQL migration is not planned
by name: `plan -with-sql` runs the pending ones in bun's order with the fixture migrations. `apply
-file` says the same.

**`migration X is recorded in bun_migrations already`** (`apply -record`, exit 2). bun's migrator
does not run a recorded migration, and apply does not record it twice. Leave out `-record` to run
the change set again, which finds every change it made. **`... is not recorded in bun_migrations,
so there is no record to take back`** (`apply -revert -record`, exit 2) is the same the other way
round.

**`bun_migrations does not exist, so there is nothing to record the migration in`** (`apply
-record`, exit 1). The migrator's `Init` creates it; run it once, or leave out `-record`.

**`bun_migration_locks holds the lock on bun_migrations, which bun's Lock took`** (`apply -record`,
exit 2). A migrator that took bun's `Lock` is migrating now, and apply does not record a migration
beside it, which could record it twice. Run apply once the migrator is done. A lock that stays, of a
migrator that stopped without `Unlock`, is reported by `status` too; delete its row once no migrator
runs.

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

**`edited after it ran here`** (`status` with an `audit_table`). The migration file's change set is
not the one the audit table recorded when it ran in this database: somebody edited the file since,
for instance to set `MissingRow` for a database where it failed. Databases that have not run it yet
run the edited one. A revert here matches the file's changes to that run by model, key and kind.

**`the audit table X cannot be read as this role`** (`status`, exit 1). Grant the role status
connects as `SELECT` on the table, or run `status -offline`.

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
| `is to point at Currency "EUR", and no row of currencies has code = "EUR"` | error: the row a change writes a reference to is not there | put it back, or fix the fixture file and generate again |
| `Currency "EUR" is not in this database under that name` | added to a missing or changed row: a guard names a row that was renamed or removed | [a migration failed](production.md#a-fixture-migration-failed-during-a-deploy) |
| `cannot be found: the key refers to Plan(free), which no row holds any more` | changed row: a delete whose key names a row that was renamed or removed in this database, so whether the row to delete is still there cannot be told | look for the row under the new name, and delete it if it is to go |
| `no longer holds the values this change was generated against` | changed row | [a change was skipped](production.md#a-change-was-skipped) |
| `changed no row all the same: a BEFORE trigger that returned NULL, a rule, or a row-level security policy stopped it` | error | find the trigger, rule or policy on the table; the change set cannot be made past it |
| `row-level security is active on plans for the role running the migration` | error, before anything runs: a policy would hide rows of a table the set reads or writes, of the seed guard table, of a table pointing at one the set deletes from, or of the audit table, where hidden rows would have a revert undo every change | run migrations as the tables' owner while they are not `FORCE ROW LEVEL SECURITY`, or as a role with `BYPASSRLS`. A table only a trigger writes into is not checked: the policy applies to the trigger's rows as to any write |
| `the sequence public.plans_id_seq of plans has to be kept past the ids written into the table explicitly, and the role running this ... may not ...: GRANT UPDATE ON SEQUENCE ...` | error, before the id is written: nothing was changed | run the `GRANT` it names; `plan` says the same |
| `The role running the migration lacks a privilege, or a row-level security policy applies to it` | error: PostgreSQL refused a statement, or a row a trigger wrote did not pass a policy's check | grant what is missing, or run migrations as the tables' owner or a role with `BYPASSRLS` |
| `exists, but under id 7 and not 3` | id drift | [a migration failed](production.md#a-fixture-migration-failed-during-a-deploy) |
| `is soft-deleted (id 3, deleted at …) with other values than this change writes, and constraint "plans_name_key" refuses a second row` | changed row: a row coming back that a soft-deleted row holds the key of with other values, under a unique index over every row | [a returning row is soft-deleted with other values](production.md#a-returning-row-is-soft-deleted-with-other-values) |
| `cannot take the values this change writes, because constraint … refuses them: a soft-deleted row holds code=GBP` | changed row: a rename into a key, or a value, a soft-deleted row holds under a unique index over every row | the same |
| `It is soft-deleted, since …, and the fixture file still holds it` | missing row: an update of a row soft-deleted in this database | restore it, `UPDATE … SET deleted_at = NULL`, or take it out of the fixture file |
| `is soft-deleted, since …: restore it, or point the row elsewhere` | error: a change writes a reference to a row soft-deleted in this database | restore that row, or change the fixture file |
| `the row is already soft-deleted, since …, nothing to delete` | not a problem: a second run, or a replica that came second | nothing |
| `restored the row soft-deleted at …` / `inserted beside the soft-deleted row` | not a problem: what an insert of a model with a `soft_delete` did | nothing; see [soft-deleted rows](reference.md#soft-deleted-rows) |
| `still points at it …; bun loads a soft-deleted row through no relation` | not a problem: a soft delete of a row the application's rows point at, which they now load as nil | repoint them, if the application should not |
| `2 rows of features point at plans name=pro through ...` | referenced | [a migration failed](production.md#a-fixture-migration-failed-during-a-deploy) |
| `rows of plans hold name=team` | duplicate key: more than one row has the natural key, none is touched. The message names a unique index that let the second row in, one holding NULLs distinct, a partial or an invalid one, and what to change about it | remove the extra rows and fix the index, or add a unique index on the key where the table has none; see [unbacked keys](#unbacked-keys) |
| `held a lock on a row of plans for longer than the lock timeout` | lock timeout: nothing was changed | the next deploy runs it again; find the long transaction |
| `checking the constraints PostgreSQL defers waited for a lock another session held` | lock timeout, while the `DEFERRABLE` constraints were checked at the end of the set: a foreign key's check locks the row it points at | the next deploy runs it again; find the long transaction |
| `does not hold the model's where` | error: the row a change writes would not be master data | the fixture row and the model's `where` disagree; fix one |
| `once every change was made, a constraint did not hold` | error: a `DEFERRABLE` constraint, checked when the set is done | the set leaves a foreign key or unique key broken; [plan](production.md#a-fixture-migration-failed-during-a-deploy) shows the changes |
| `is a bytea column` | error: a list of numbers for binary data | write the bytes as `\x` and hex digits |
| `PostgreSQL only stores an array whose rows all have the same length` | error | make every row of the array as long as the others |
| `empty list inside a list` | error: an array cannot have an empty row | drop the empty list, or make the whole value an empty list |
| `PostgreSQL stores arrays of at most 6 dimensions` | error: lists nested more than six deep | nest them less deep |
| `the value holds a NUL character, which PostgreSQL cannot store` | error, before anything runs: a NUL, written `\u0000` in a list, which bun would drop without a word | remove it from the fixture file and generate again |
| `the change set is in format 2` | the file was generated by a newer version | upgrade `github.com/RELAXccc/bun-fixture-migrate` in the application |
| `plans is empty, nothing to do` | not a problem: the database is not seeded yet | [a new environment](production.md#a-new-environment) |
| `wait for another change set to finish` | the advisory lock wait was cancelled | another process was applying a change set; retry |
| `the audit table bun_fixture_audit does not exist, and the role running the migration may not create it` | error, after the changes: nothing was changed | grant the role `CREATE` on the schema, or have a role that may run a migration once and grant this one `SELECT` and `INSERT` on the table |
| `may not write into the audit table` / `may not read the audit table` / `may not use the schema of the audit table` | error: nothing was changed | grant the role `SELECT` and `INSERT` on the table, and `USAGE` on its schema |
| `holds no row of this change set: it never ran here with the audit table, so every change is reverted` | not an error, a revert's log line: the set never ran here with an audit table, or ran before it had one | look at the rows the migration reported `unchanged` before rolling back; see [rolling back](production.md#rolling-back) |
| `the change set is reverted here already` / `not reverted: already reverted here, audit row N` | not an error: the newest row of the set in the audit table is a revert's, and no migration ran since, so this revert changes nothing; it is a second rollback, or a rollback after `apply -revert` | nothing; `apply -revert -yes -record` takes the migration's record out without reverting again |
| `not reverted: the run here was unseeded` | not an error: the migration ran here before the database was seeded and changed nothing, so the revert changes nothing either | nothing; seed again from the old fixture file if the old values are wanted |
| `not reverted: the migration did not make it in this database` | not an error: the audit table says the migration found the change made, or skipped it, so the revert leaves the row alone | nothing |

**`bun had recorded X as applied before running it, and the record could not be removed`.** The
migration failed and rolled back, but its record in the migrations table is still there. Delete the
row with the id the message names before the next deploy, or the migration will not run again. With
`WithMarkAppliedOnSuccess(true)` this cannot happen.

**A migration succeeds on one database and fails on another.** Ids and hand edits differ between
databases; that is what the guards are for. `plan` against each shows why.
