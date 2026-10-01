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
- [At run time](#at-run-time)

## Configuration

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

**`the configuration reads the database DSN from DATABASE_URL, which is not set`.** `database:
env:DATABASE_URL` names an environment variable; set it.

**`the database DSN is not a URL pgdriver can read`.** Write it as
`postgres://user:password@host:5432/dbname?sslmode=disable`. A keyword DSN (`host=... user=...`) is
not accepted. Passwords are never repeated in a message.

**`the database DSN ... is a unix socket DSN without the socket's path`.** pgdriver's socket form
names the socket file: `unix://user:password@dbname/var/run/postgresql/.s.PGSQL.5432`.

**`the database did not start a read-only transaction; refusing to go on`.** A pooler in transaction
mode, or a proxy, dropped `SET TRANSACTION READ ONLY`. Connect directly, or through a session-mode
pool, for the reading commands.

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
| `its id changed from 3 to 4` | a row changed its id, under `policy.id_drift: error`; with `warn` it is a warning and the rest of the row is migrated |
| `column "x" is written on one side and left out on the other` | no `defaults` entry says what leaving it out means |
| `version is written 1.10, which a column written from a Go string holds as written ...` | a value whose meaning depends on the column type, in a change computed without a database |
| `currency_id points at the Currency whose code is written 0012, ...` | the same for a reference: the ref value of the row it names reads two ways, and only that ref column's type says which the database holds |
| `code is written 0012 before and 012 after` | a key or a value only respelled: no change in a numeric column, a rename in a text one |
| `parent_id points at Category "Accessories", which 2 rows hold` | a reference by a ref value more than one row holds; make the ref column unique |
| `its id, 4, is the id of Plan/name=max too` | two rows of the file share an id, usually after merging two branches that each added the next one |
| `it leaves out note, which other rows of Plan write` | an inserted row leaves out a column with no `defaults` entry; write it, or add one |
| `tags: it is a sequence holding a null` | a null inside a YAML sequence, which a `[]string` field drops and a `[]*string` keeps |

For the last one, configure `database` so `generate` can read the column types (and leave out
`-no-lint`), or write the value unambiguously: quoted for a text column, resolved for any other
(`1.1`, `15`, `true`, `2026-01-01T10:00:00Z`). [Fixture files](fixture-files.md#values) explains
why.

## The state file

**`the state file does not match its own checksum, so it was edited by hand`.** Or merged line by
line. See [the runbook](production.md#the-state-file-was-edited-or-lost).

**`this is not a state file bun-fixture-migrate wrote: the marker line is missing`.** `state:` points
at a fixture file, or the file was replaced. Check the path.

**`no state file at ... yet, so this diffs against git HEAD`.** Not an error: until `baseline` or the
first `generate` writes one, the base is the committed fixture file.

**`baseline would record N changes as migrated with no migration to make them`.** The fixture file
differs from the state, and no migration covers it. Run `generate`. Pass `-force` only when you
wrote the migration yourself.

## Generating

**`nothing changed in fixtures/fixture.yml since the state after ...`.** The file and the state agree.
If you expected a change, check the file was saved, and that `status -offline` agrees.

**`warning: migration 3_backfill sorts after 20260930165255, so bun runs it after this one`.** bun
orders migrations by name as strings. A migration named with a short number sorts after every
timestamp. Rename it with a timestamp, or check that running it later is harmless.

**`migrations/x.go is package foo and the configuration says package migrations`.** The generated
file would not compile there. Fix `package:` in the configuration.

**`warning: no file in migrations declares the variable Migrations ...`.** The generated file
registers with the variable named by `migrator:`. Declare it, or fix the name.

**`the migration is written, the state file is not`.** The file system refused the second write. The
migration is fine; run `baseline -force` once the cause is fixed, before generating again.

## Planning

**`could not be planned` / `inconclusive`.** The plan itself failed: a row lock held longer than
`-lock-timeout`, a statement timeout, a lost connection. Nothing is known about the migration. Try
again, or raise `-lock-timeout`.

**`pending before it and not simulated: ...`.** Migrations the tool did not write, such as schema
changes, run before this one in the deploy but not in the plan. If they change the tables the
fixture migration touches, run `plan -with-sql` so SQL migrations run too, or plan against a copy
that already has them.

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
