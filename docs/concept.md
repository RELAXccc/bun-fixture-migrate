# Concept: from migration generator to master-data migration manager

This document is the plan for growing bun-fixture-migrate into a tool a team can run its master
data on in production, across the different ways people use bun. It is PostgreSQL only and stays
that way: everything below leans on PostgreSQL's catalog, its type system and its transactions, and
a second database would halve what each feature can promise.

Status markers: **done** is on this branch and tested; **next** is the following iteration;
**later** is designed but not scheduled.

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
| SQL migrations, `Migrations.Discover(fsys)` with `.up.sql` / `.tx.up.sql` | `status` lists them; `plan -with-sql` runs pending ones in the plan transaction, honouring `--bun:split` | status done, plan next |
| Go and SQL migrations mixed in one directory | ordered as bun orders them; a name used twice is reported | done |
| SQL-only projects, or another migrator (goose, golang-migrate, dbmate) | generate the change set as a guarded PL/pgSQL migration | later |
| `migrate.NewMigrator` default: records **before** running | a failing change set deletes that record | done |
| `WithMarkAppliedOnSuccess(true)` | nothing to take back; tested in both modes | done |
| `WithTableName` / `WithLocksTableName` | `migrations_table` in the configuration | done |
| `WithUpsert` + `RunMigration` (re-running an applied migration) | the record is only taken back when it is the migrator's fresh one | done |
| `Lock` / `Unlock` | change sets additionally serialise on a transaction-scoped advisory lock | next |
| `BeforeMigration` / `AfterMigration` hooks | untouched: they run around `Apply` | done |
| Migrations run at application start by every replica | advisory lock; a second replica finds everything `unchanged` | next |
| Migrations run as a separate job (Kubernetes Job, init container, CD step) | `plan` before, `status -require-applied` after | done |
| No migration files at all: bring a dev, CI or staging database in line | `sync` command and `Sync` library call | next |

### 2.2 dbfixture

| Use | What the tool does | Status |
| --- | --- | --- |
| One fixture file | the whole pipeline | done |
| Several files, `fixture.Load(ctx, fsys, "a.yml", "b.yml")`, one anchor scope | `fixtures:` list read in that order with one scope; state file holds all of them | next |
| Templates `{{ $.Model.row.Field }}`, `pk<id>` anchors, latest anchor wins, file order | read exactly as dbfixture reads them, checked against it | done |
| `{{ now }}`, `WithTemplateFuncs` | refused unless the column is ignored: the database never holds that text | done |
| `WithTruncateTables` / `WithRecreateTables` for test databases | seed guard makes migrations no-ops on an empty database | done |
| `WithBeforeInsert` altering rows | invisible to the tool; documented as a limit | later |

### 2.3 Model idioms

| Idiom | Hazard | What the tool does | Status |
| --- | --- | --- | --- |
| `default:` tag / column default, zero in the file | bun writes DEFAULT | finding, export marker, policy | done |
| pointer or `nullzero` field, `~` in the file | bun writes DEFAULT | finding, export marker, policy | done |
| `type:jsonb` map or struct | YAML mapping in the file | compared as JSON, written as JSON, exported as YAML | next |
| `json` column | no equality operator in PostgreSQL | compared through `jsonb` | next |
| `array` tag, `text[]`, `int[]` | YAML sequence in the file | compared and written as a PostgreSQL array | next |
| uuid primary key, `gen_random_uuid()` | no sequence, ids differ everywhere | natural keys; zero uuid known | next |
| `GENERATED ALWAYS AS IDENTITY` | an explicit id cannot be inserted, by anyone | export leaves the id out; lint | next |
| composite primary key, m2m join table | no single id | keyed on the natural key only | next (tests) |
| self-referencing model (`parent_id`) | order inside one model | file order, reverse for deletes | next (tests) |
| enum types, domains, `citext` | text in, typed out | PostgreSQL compares | next (tests) |
| `soft_delete` | a delete is an UPDATE of `deleted_at` | soft-delete aware snapshot and delete | later |
| schema-qualified table, mixed-case or reserved-word names | quoting | quoted everywhere | next (tests) |

### 2.4 Drivers and topologies

| | Status |
| --- | --- |
| `pgdriver` | done |
| `pgx/v5/stdlib` under `pgdialect` (the runtime in the application) | next (test variant) |
| checks against a read replica | done: read-only transactions work on a hot standby |
| `plan` pointed at a standby by mistake | refused with a sentence instead of a misleading failure | next |
| PostgreSQL 12 to 18 | next (CI matrix) |

## 3. Edge cases, verified

Each was reproduced before it went into this table.

| Case | What happened | Severity | Fix | Status |
| --- | --- | --- | --- | --- |
| failing migration under bun's default migrator | recorded before running, never retried | data change lost | take the record back | done |
| `pgdriver.WithDSN` on a keyword DSN or bad URL | panic with stack trace; `url.Parse` errors repeat the password | outage noise, secret leak | validate first, redact | done |
| two migrations with one timestamp | bun records them as one, one never runs | data change lost | `status` reports it; `generate` never reuses a name | done |
| fixture edit committed before generating | nothing to generate against HEAD | data change lost | state file | done |
| quoted code `"01234"`, e.g. a postcode | written as `1234` into migrations and exports | **silent corruption** | keep the text of a string exactly | next |
| string with leading or trailing spaces | trimmed | **silent corruption** | never trim a string | next |
| integer above 2^53, long `numeric` | rounded through float64 | **silent corruption** | exact decimal arithmetic | next |
| `017`, `0x1F`, `0o17`, `1_000` in YAML | dbfixture stores 15, 31, 15, 1000; the tool wrote 17 or failed | wrong value | resolve integers the way YAML does | next |
| text `"1.0"` in the database, `"1"` in the file | compared equal | missed drift | only numbers compare as numbers | next |
| `json`, `point`, `xml` column in a guard | `operator does not exist` at deploy time | failed deploy | typed comparison at run time | next |
| timestamps | text depends on the session `TimeZone` and `DateStyle` | phantom drift, wrong instant | fixed session settings; PostgreSQL compares | next |
| delete of a row other tables reference with `ON DELETE CASCADE` or `SET NULL` | user data deleted or detached | **data loss** | refused unless the model says `deletes: cascade` | next |
| two replicas applying the same change set at once | both see "not there yet", one fails on a unique index or inserts twice | failed deploy / duplicate | advisory lock | next |
| `GENERATED ALWAYS` identity exported with ids | the export cannot be loaded | broken export | leave the id out | next |
| `plan` against a hot standby | "would FAIL" for the wrong reason | misleading | detect `pg_is_in_recovery()` | next |

## 4. Features

### Next: this iteration

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
- **Drift monitoring.** `check -json` on a schedule with a documented alerting recipe, and a
  `generate -from-db` bot that opens a pull request when an admin UI changed master data.
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
