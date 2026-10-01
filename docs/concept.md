# Concept: from migration generator to master-data migration manager

This document is the plan for growing bun-fixture-migrate into a tool a team can run its master
data on in production, across the different ways people use bun. It is PostgreSQL only and stays
that way: everything below leans on PostgreSQL's catalog, its type system and its transactions, and
a second database would halve what each feature can promise.

Status markers: **done** is on this branch and tested; **later** is designed but not scheduled.

## 1. Principles

These decide every trade-off further down.

1. **Never guess.** Anything ambiguous is refused with the model, the row and a reason. A tool that
   writes production data migrations is only useful if its silence means something.
2. **Every premise about bun is a test.** What `marshalsToDefault` does, what the migrator records,
   how dbfixture reads a file: each is pinned by a test that runs bun itself, in CI, against the
   pinned release and against bun's master branch.
3. **The database decides what a value is.** Equality of two values is PostgreSQL's equality for
   the column's type, not a string comparison and not a Go parse. Where a value has to be compared
   without a database, the YAML type of the scalar decides, never a guess from its spelling.
4. **What is generated is read.** A migration is a Go literal a reviewer can follow row by row; the
   state file is the fixture file verbatim; every report exists as text for people and as JSON for
   programs.
5. **A failed migration changes nothing and is not recorded.** One transaction per change set;
   bun's record of a failed migration is taken back.
6. **Reading never writes.** Every command that only reads runs in a transaction PostgreSQL itself
   holds to READ ONLY.

## 2. How bun is used, and what each use needs

### 2.1 Migrations

| Use | What the tool does | Status |
| --- | --- | --- |
| Go migrations, `Migrations.MustRegister(up, down)` | generates them; `status` and `plan` read them back | done |
| SQL migrations, `Migrations.Discover(fsys)` with `.up.sql` / `.tx.up.sql` | `status` lists them; `plan -with-sql` runs pending ones in the plan transaction, honouring `--bun:split` | done |
| Go and SQL migrations mixed in one directory | ordered as bun orders them; a name used twice is reported | done |
| SQL-only projects, or another migrator (goose, golang-migrate, dbmate) | generate the change set as a guarded PL/pgSQL migration | later |
| `migrate.NewMigrator` default: records **before** running | a failing change set deletes that record | done |
| `WithMarkAppliedOnSuccess(true)` | nothing to take back; tested in both modes | done |
| `WithTableName` / `WithLocksTableName` | `migrations_table` in the configuration | done |
| `WithUpsert` + `RunMigration` (re-running an applied migration) | the record is only taken back when it is the migrator's fresh one | done |
| `Lock` / `Unlock` | change sets additionally serialise on a transaction-scoped advisory lock | done |
| `BeforeMigration` / `AfterMigration` hooks | untouched: they run around `Apply` | done |
| Migrations run at application start by every replica | advisory lock; a second replica finds everything `unchanged` | done |
| Migrations run as a separate job (Kubernetes Job, init container, CD step) | `plan` before, `status -require-applied` after | done |
| No migration files at all: bring a dev, CI or staging database in line | `sync` command and `Sync` library call | done |
| A new database: migrate, then seed with dbfixture | `fixtureapply.SyncSequences` after the seed; the example project's deploy step | done |

### 2.2 dbfixture

| Use | What the tool does | Status |
| --- | --- | --- |
| One fixture file | the whole pipeline | done |
| Several files, `fixture.Load(ctx, fsys, "a.yml", "b.yml")`, one anchor scope | `fixtures:` list read in that order with one scope; state file holds all of them | done |
| Templates `{{ $.Model.row.Field }}`, `pk<id>` anchors, latest anchor wins, file order | read exactly as dbfixture reads them, checked against it | done |
| `{{ now }}`, `WithTemplateFuncs` | refused unless the column is ignored: the database never holds that text | done |
| `WithTruncateTables` / `WithRecreateTables` for test databases | seed guard makes migrations no-ops on an empty database | done |
| `WithBeforeInsert` altering rows | invisible to the tool; documented as a limit | documented |

### 2.3 Model idioms

| Idiom | Hazard | What the tool does | Status |
| --- | --- | --- | --- |
| `default:` tag / column default, zero in the file | bun writes DEFAULT | finding, export marker, policy | done |
| pointer or `nullzero` field, `~` in the file | bun writes DEFAULT | finding, export marker, policy | done |
| `type:jsonb` map or struct | YAML mapping in the file | compared as JSON, written as JSON, exported as YAML | done |
| `json` column | no equality operator in PostgreSQL | compared through `jsonb` | done |
| `array` tag, `text[]`, `int[]` | YAML sequence in the file | compared and written as a PostgreSQL array | done |
| `string` field, `1.10` or `01234` unquoted | yaml.v3 hands a string field the text as written | the column's type decides; refused without a database | done |
| uuid primary key, `gen_random_uuid()` | no sequence, ids differ everywhere | natural keys; zero uuid known | done |
| `GENERATED ALWAYS AS IDENTITY` | an explicit id cannot be inserted, by anyone | export leaves the id out; lint | done |
| composite primary key, m2m join table | no single id | keyed on the natural key only | done |
| self-referencing model (`parent_id`) | order inside one model | parents inserted first, children deleted first, exported parents first; read whatever the id order | done |
| enum types, domains, `citext` | text in, typed out; a domain's own name hides its base type; `Go` and `GO` are one `citext` | PostgreSQL compares; a domain is its base type, its default and `NOT NULL` the column's; keys equal under their type are a duplicate | done |
| `soft_delete` | a delete is an UPDATE of `deleted_at` | soft-delete aware snapshot and delete | later |
| schema-qualified table, mixed-case or reserved-word names | quoting | quoted everywhere | done |

### 2.4 Drivers and topologies

| | Status |
| --- | --- |
| `pgdriver` | done |
| `pgx/v5/stdlib` under `pgdialect` (the runtime in the application) | done: the database suite runs under both |
| checks against a read replica | done: read-only transactions work on a hot standby |
| `plan` pointed at a standby by mistake | refused with a sentence instead of a misleading failure | done |
| PostgreSQL 12 to 18 | done: CI matrix |

## 3. Edge cases, verified

Each was reproduced before it went into this table.

| Case | What happened | Severity | Fix | Status |
| --- | --- | --- | --- | --- |
| failing migration under bun's default migrator | recorded before running, never retried | data change lost | take the record back | done |
| `pgdriver.WithDSN` on a keyword DSN or bad URL | panic with stack trace; `url.Parse` errors repeat the password | outage noise, secret leak | validate first, redact | done |
| two migrations with one timestamp | bun records them as one, one never runs | data change lost | `status` reports it; `generate` never reuses a name | done |
| fixture edit committed before generating | nothing to generate against HEAD | data change lost | state file | done |
| quoted code `"01234"`, e.g. a postcode | written as `1234` into migrations and exports | **silent corruption** | keep the text of a string exactly | done |
| string with leading or trailing spaces | trimmed | **silent corruption** | never trim a string | done |
| integer above 2^53, long `numeric` | rounded through float64 | **silent corruption** | exact decimal arithmetic | done |
| `017`, `0x1F`, `0o17`, `1_000` in YAML | dbfixture stores 15, 31, 15, 1000; the tool wrote 17 or failed | wrong value | resolve integers the way YAML does | done |
| text `"1.0"` in the database, `"1"` in the file | compared equal | missed drift | only numbers compare as numbers | done |
| `json`, `point`, `xml` column in a guard | `operator does not exist` at deploy time | failed deploy | typed comparison at run time | done |
| timestamps | text depends on the session `TimeZone` and `DateStyle` | phantom drift, wrong instant | fixed session settings; PostgreSQL compares | done |
| delete of a row other tables reference with `ON DELETE CASCADE` or `SET NULL` | user data deleted or detached | **data loss** | refused unless the model says `deletes: cascade` | done |
| two replicas applying the same change set at once | both see "not there yet", one fails on a unique index or inserts twice | failed deploy / duplicate | advisory lock | done |
| `GENERATED ALWAYS` identity exported with ids | the export cannot be loaded | broken export | leave the id out | done |
| `plan` against a hot standby | "would FAIL" for the wrong reason | misleading | detect `pg_is_in_recovery()` | done |
| `1.10`, `01234`, `True` unquoted in a text column | dbfixture stores the text as written; the tool resolved it to 1.1, 668, true and saw drift, and a migration would have written those | **silent corruption** | keep both readings, let the column's type decide, refuse without one | done |
| a reference to a row whose ref value is written `0012` in a `bigint` column | the reference carried `0012` and the row held 10; `sync` on a freshly seeded database repointed the reference at the row whose code is 12 | **silent corruption** | the ref column's type decides what a reference carries; refused without one | done |
| a tree whose root has a higher id than its leaves | `check`, `export`, `sync` and `generate -from-db` failed with a dangling reference; an export in id order did not load | failed command, broken export | read every row's name before resolving any; export parents first | done |
| a parent model that leaves the file entirely | its rows were deleted before the rows pointing at them | failed deploy | models ordered by their references across both states | done |
| closing an effective-dated price and opening the next in one release | the insert ran before the update and hit the one-open-price index | failed deploy | deletes and updates before inserts, each change after what it depends on | done |
| natural keys `{a: "x/b=y", b: z}` and `{a: x, b: "y/b=z"}`, or a NULL and the text `NULL` | compared as one key | false duplicate, false id drift | an encoding no two keys share | done |
| `id_drift: warn` | stopped `generate` and `sync` like `error` | blocked deploy | warnings apart from refusals | done |
| a YAML alias `*name` of a scalar | written as its JSON, quotes and all, or the text `null` | **silent corruption** | an alias is the value it names | done |
| `~` inside a sequence | `[]string` drops it, `[]*string` keeps it; the tool wrote it | wrong value | finding, and the change refused | done |
| `~` in a NOT NULL column without a default | no finding; the deploy failed | failed deploy | `invalid value` finding | done |
| the primary key as the natural key (a currency keyed by its ISO code) | `export`, `check`, `sync` failed: the key was not read | failed command | the key may be the id | done |
| a primary key `"0"` | read as no id: permanent drift on every reference to it | false drift | a zero is "no id" only in a `serial` model | done |
| ids 9 and 10 | read in the order of their text, 10 before 9 | broken export of a tree, unstable files | ordered by the column, not its text | done |
| two rows sharing an id after a merge | taken for a rename; the deploy and a new seed failed | failed deploy | `duplicate id` finding, the insert refused | done |
| a reference by a name two rows hold (per-parent category names) | written, then failed at deploy | failed deploy | refused | done |
| `schema: app` with unqualified tables | the migration named `roles`, found in `public` or nowhere | failed deploy or **wrong table** | the change set names the schema | done |
| `'{{ "Hello {{ name }}" }}'` | refused as a template | refused file | a template of string constants is its text | done |
| explicit ids from a dbfixture seed | the sequence stays behind; the application's first insert fails | failed insert | `fixtureapply.SyncSequences` after the seed | done |
| `char(n)` array elements shorter than n | padded on the database side, not on the file side | phantom drift | both read without the padding | done |
| a domain over integer or `jsonb` | the domain's name was asked instead of its base type: exported as `"5"` and as a quoted string, a zero against the domain's default unreported | broken export, wrong value | the base type everywhere; the domain's default is the column's | done |
| a value too long for `varchar(n)`, `char(n)`, `bit(n)` | an explicit cast cut it without a word, so it was no finding | failed deploy, **silent truncation** | an `INSERT`'s length rules: an `invalid value`, trailing spaces dropped | done |
| a domain `CHECK`, an `hstore`, `ltree` or `tsquery` syntax error | not of class 22, so the command stopped with a raw error | outage noise | any error casting one value is an `invalid value` | done |
| an offset into `timestamp` or `date`, more than six fractional digits, a zone-less string into `timestamptz`, `01/02/2026` | a `time.Time` and a string field store different values, or the seeding session decides | **silent corruption** | an `invalid value` naming both, unless every reading agrees | done |
| nested timestamps and long numbers in `jsonb` through `map[string]any` | `encoding/json` writes a `time.Time` and a `float64` its own way | phantom drift, wrong value | written as `encoding/json` writes them | done |
| a timestamp, a date or a long float at the top of a `jsonb` column, or as an element of a sequence there | read as an array column's elements: in UTC, a date as a date, a float exactly; dbfixture's `any` field keeps the offset, makes a date midnight UTC and a float a `float64` | phantom drift, wrong value | the cell keeps a JSON reading, which a `json` or `jsonb` column takes | done |
| a configured model the files hold no block of | `check` and `sync` read only the models in the files and agreed with its rows, while `generate` deletes a block that leaves the files | missed drift, two answers to one file | every configured model is read; one without a block has no rows | done |
| a `key_any_of` group a row leaves out, or a group column no row writes | the row keyed by `""` where the database holds NULL, read as a rename; every row a column on one side only | false refusal | a group column left out is NULL | done |
| a primary key that is also a reference, `id: plan_id, references: {plan_id: Plan}` | the id read as the template text naming the plan: every row an invalid value, an insert and a delete | false drift, failed command | refused in the configuration: leave `id` out | done |
| a template copying a null, a float or a timestamp field, `{{ $.Src.s.Note }}` | dbfixture stores it as `fmt` prints it, `<nil>`, `1e+08`, `2026-01-01 10:00:00 +0000 UTC`; the tool read NULL or the value | phantom drift, wrong value | a copy of a string or integer column is the value; any other, a null or a structure is refused, and undecided without the database | done |
| a YAML merge key `<<` inside a `jsonb` mapping | the whole file refused as holding a key JSON cannot hold; dbfixture merges it | refused file | yaml.v3's merge rules | done |
| an unquoted date `2026-01-01` in a `timestamptz` column | refused as session-dependent; a `time.Time` field stores midnight UTC, as for `2026-01-01 00:00:00` | false refusal | the `time.Time` reading, midnight UTC | done |
| `~` in a `json` or `jsonb` column | the JSON null through a map field, NULL through a pointer | phantom drift | a `null against a default` finding | done |
| `bytea` through `[]byte` | the sequence's JSON text was cast to `bytea`; the export wrote `"\x48..."` | **silent corruption**, broken export | the sequence's bytes; exported as a sequence | done |
| `!!binary` | the base64 text was kept | wrong value | the text it encodes | done, but in a string column it needs the reader to drop the as-written base64 |
| NEL, DEL, the C1 range, U+FFFE in exported text | NEL folded into a space, the rest unparseable | broken export | escaped; every export is parsed back before it is written | done |
| `NaN`, `Infinity`, timestamp `infinity`, `jsonb` strings and null, `Hello {{ name }}` in an export | written so dbfixture could not load them, or refused needlessly | broken export | YAML's spellings, a string-literal template, or a refusal with the reason | done |
| `{"a": 1.0}` in `jsonb`, written by SQL | compared as text with the file's `1` | phantom drift | numbers canonical on both sides | done |
| a 2-D array; an array numbered from 0 | an invalid value after export; `[0:1]={7,8}` exported as `[7, 8]` | broken export, **silent corruption** | the array literal built from nested JSON; another lower bound refused | done, and see below |
| a 2-D array in a file or an export | bun cannot write a nested slice into an array column, so `dbfixture` fails to load the sequence of sequences the tool read and exported | broken export, unloadable file | an `invalid value` in a file, an export refused with the reason | done |
| an array holding a NULL, exported under `array_nulls: refuse` | written as `[1, null, 3]`, which the tool then refused to read and a `[]int64` field loads as `{1,3}` | broken export | the export refused unless `array_nulls: keep` | done |
| a table `CHECK` the new value violates | generate wrote it, the deploy failed | failed deploy | a single-column `CHECK` is an `invalid value`; others `plan` reports | done |
| `1.5` in an integer column | dbfixture stores 1 | phantom drift | an `invalid value` saying so | done |
| a view named as a model's table | "not a table" | misleading | says it is a view, and that only tables hold master data | done |

## 4. Features

### Done: the last iteration

- **Value fidelity.** A cell keeps its YAML type. Strings are exact; integers are resolved as YAML
  resolves them and written in decimal; decimals are canonicalised exactly, never through float64.
  The database side reads numbers as numbers and everything else as its exact text, in a session
  with fixed `TimeZone`, `DateStyle`, `IntervalStyle`, `extra_float_digits` and `bytea_output`.
  When a database is at hand, fixture values are canonicalised by casting them to the column's type
  in PostgreSQL, so equality is PostgreSQL's; a value the column cannot hold becomes a finding
  before anything is generated.
- **Typed run time.** `fixtureapply` reads the column types of every table it touches and compares
  through them: `json` through `jsonb`, types without equality through their text.
- **JSON and arrays.** YAML mappings and sequences in `json`/`jsonb` columns, and sequences in array
  columns, are compared, written and exported.
- **Safety at run time.** A transaction-scoped advisory lock serialises change sets. A delete that
  would cascade into, or null out, rows elsewhere is refused unless the model says
  `deletes: cascade`.
- **Several fixture files.** `fixtures: [a.yml, b.yml]`, one anchor scope, in load order.
- **`sync`.** Brings a database to the fixture file directly, with the plan printed first and
  `-yes` to go through; `fixturemigrate.Sync` does the same from Go, for development servers and
  test setups.
- **`plan -with-sql`.** Runs pending bun SQL migrations in the plan transaction too, so a fixture
  migration that needs a column a schema migration adds is simulated against that column.
- **CI.** PostgreSQL 12 to 18, two Go versions, both drivers, bun master; GitHub Actions and an
  equivalent GitLab CI pipeline; a composite GitHub Action and a GitLab CI template for projects that
  use the tool.
- **Documentation.** A production runbook — failed migration, drift, state-file conflict,
  migrating by hand — a troubleshooting guide, and a reference for the configuration, exit codes and
  JSON output.
- **The value as a string field gets it.** A plain scalar keeps its written text next to its
  resolved value, and the column's type picks one; without the database a change that depends on
  it is refused.
- **Seeding.** `fixtureapply.SyncSequences` moves sequences past the ids a `dbfixture` seed wrote,
  and `examples/basic` is a runnable bun project that migrates, then seeds a new database.

### Later

- **SQL output.** A change set rendered as guarded PL/pgSQL (`DO` blocks, `GET DIAGNOSTICS`,
  `RAISE EXCEPTION` per policy) for projects whose migrations are SQL only and for other migrators.
  It cannot take back bun's record of a failure from inside the failed transaction, so with bun it
  needs `WithMarkAppliedOnSuccess(true)`, and says so in the file.
- **Soft deletes.** A model with `soft_delete: deleted_at` reads only live rows, deletes by setting
  the column, and undeletes instead of inserting a second copy.
- **Scoped inserts.** An insert into a model with a `where` clause is checked against it, so a row
  the export would not see again cannot be written.
- **Batching** for change sets of thousands of rows: one statement per model and kind instead of
  one per row, with the same guards.
- **Drift monitoring.** `check -json` on a schedule with a documented alerting recipe (the CI
  guide has the job), and a bot that exports production and opens a pull request with the fixture
  change and its generated migration when an admin UI changed master data.
- **Column types next to the state file.** The types of the columns the fixture files use, recorded
  when a database is at hand, so a value such as `1.10` is settled offline too and `status -offline`
  never has to refuse one.
- **Squash.** Replace a chain of applied fixture migrations with one, for projects with hundreds.
- **Key lint.** A natural key without a unique index behind it is reported, because no guard is
  reliable without one.

## 5. Tests

Four layers, each with a job the others cannot do:

1. **Unit** — the diff, reading and rendering, the state file, value canonicalisation, the reader
   of generated files. Fuzzed where the input is text from outside: number canonicalisation, the
   state file, the Go literal reader.
2. **Database** — the change-set runtime against PostgreSQL: every policy, every edge case in §3, both
   drivers.
3. **bun itself** — every premise in §1.2, pinned against the real `dbfixture`, the real
   `InsertQuery` and the real `migrate.Migrator` in both of its modes.
4. **End to end** — the command built and driven through a fixture change from baseline to deploy,
   including a generated migration compiled into a program that runs bun's migrator.

Variants, as a CI matrix: PostgreSQL 12 to 18; the pinned bun release and bun master; `pgdriver`
and `pgx`; Go's oldest supported version and the current one. The same pipeline runs on GitHub
Actions and GitLab CI.

## 6. Not planned

- Databases other than PostgreSQL.
- Schema migrations. bun has them; this tool moves data and only reads the schema.
- Evaluating arbitrary templates or reading Go model types: both need the application's code, and a
  standalone tool that ran it would be a different, riskier tool.
