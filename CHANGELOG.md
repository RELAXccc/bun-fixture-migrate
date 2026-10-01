# Changelog

Organised by what the tool can do, not by commit. The [reference](docs/reference.md) has every
detail, and the [concept](docs/concept.md#known-limitations) lists what is known not to work yet.

## Unreleased

Everything below is new: this is the first body of work on the tool. It was built in waves of
research, implementation, adversarial review and fixes, and validated against bun v1.2.18, bun
master and PostgreSQL 12 to 18.

### Commands

- **`scaffold`** writes a starter configuration from a live database. It guesses:
  - natural keys, from unique, partial, expression and exclusion indexes;
  - references, from foreign keys, including a primary key that is the parent's id (`id: none`);
  - the seed guard table;
  - `soft_delete` columns, and timestamps to ignore.

  Every guess is marked `GUESS`. It leaves out bun's own tables and the audit table.
- **`export`** writes fixture files from a database, in dependency order, parsed back before
  writing. It writes only what the files own (`mode`, `insert_only`, `ids: database`), and never
  writes soft-deleted rows.
- **`check`** reports what the database and the fixture files disagree about, in a read-only
  transaction. It explains drift where it can: bun wrote `DEFAULT`, a row is soft-deleted and a
  migration restores it, or the configuration leaves a value to the database.
- **`generate`** writes the bun migration for what changed since the state file. It can also diff
  against a database (`-from-db`) or a git revision. `-dry-run` prints the migration, and `-at`
  gives reproducible output.
- **`baseline`** records the fixture files as migrated without writing a migration.
- **`status`** lists the migrations: what a database applied, what the audit table says each run
  did, what no migration covers yet, out-of-order migrations, stale lock rows, and state-file
  lineage problems.
- **`plan`** runs the pending migrations, fixture and bun SQL migrations alike (`-with-sql`), in a
  transaction it rolls back, and reports every row. It also notes natural keys no index backs, and
  says when a role or a setting makes the result inconclusive.
- **`sync`** brings a development, test or staging database to the files directly, printing the
  plan first.
- **`apply`** runs one generated migration by hand:
  - `-record` records it as bun's migrator would, under bun's lock;
  - `-revert` takes it back, and `-revert -record` only removes the record when the audit table
    says it was reverted already.
- **`-json`** on every command, with a documented shape, and exit codes that separate drift (3)
  from refusal (2) and errors (1).

### Generated migrations and the run time (`fixtureapply`)

- **Each migration is a typed `fixturechange.Set` literal,** registered with
  `fixtureapply.Up` / `Down`. It needs bun and nothing else.
- **Guards:**
  - every update and delete names the values its row must still hold;
  - a count guard stops any statement from touching more than one row;
  - references are resolved by natural key at run time.
- **Policies, written into each migration:**
  - `missing_row`, `changed_row`, `id_drift` and `duplicate_key`, each set to error, warn or skip;
  - `deletes: refuse|cascade`;
  - per-model overrides.
- **Safety:**
  - a transaction-scoped advisory lock serialises change sets across replicas;
  - `lock_timeout` is set after that lock is taken;
  - a set refuses to run under row-level security;
  - deferrable constraints are deferred, then checked before commit;
  - identity `ALWAYS` columns, sequences moved past explicit ids (never backwards), and NUL bytes
    are all handled.
- **Failed migrations:** bun records a migration as applied before running it. A failed change set
  removes exactly its own record, so the next deploy retries it.
- **Seed guard:** on a database not yet seeded, the migration does nothing and leaves the seeding to
  `dbfixture`.
- **Audit table** (opt-in, `audit_table`). Each run's outcome per change is recorded with the set's
  SHA-256, so `Revert` undoes only what that database's `Apply` did. A second revert changes
  nothing, and `status` shows what each run skipped.
- **Soft deletes** (`soft_delete: deleted_at`). A delete sets the column, an insert restores the
  soft-deleted row holding its values or inserts beside it, and lookups see live rows only.
- **Typed comparisons** through the column's type: json through jsonb, arrays, intervals, and
  citext.
- **Format checks:** `Set.Format` and `Validate` refuse a migration newer than the run time.
  Generated files from every earlier version are kept in `testdata/generated`, and each still
  compiles and runs.

### Reading fixture files faithfully

- **dbfixture is emulated:**
  - `_id` anchors and whole-value templates, evaluated in load order, latest wins;
  - aliases, merge keys and `!!binary`;
  - several files sharing one anchor scope.
- **A value keeps its YAML type and its written text.** The column's type decides between them.
  When the database is at hand, values are cast in PostgreSQL, so equality is PostgreSQL's.
  Without it, what cannot be settled is refused rather than guessed. This covers:
  - numbers, intervals and timestamps;
  - bool and uuid copies;
  - arrays holding NULL (`array_nulls`).
- **Findings before deploy:** values a column cannot hold, unknown columns, duplicate keys
  (including under stricter expression indexes), and zero or null against a column default.

### Configuration

- Models in any schema, natural keys, `key_any_of` alternatives, references, `where` filters,
  `ignore`/`derived` columns and defaults.
- **Who owns what.** A model's `mode` is `sync`, `upsert` or `insert`; `insert_only` columns are
  written once; `ids: database` leaves ids to the sequence; `id: none` covers a table keyed by its
  parent's id.
- **Policies** for every finding kind and run-time situation: renames, deletes, id drift, duplicate
  keys, array NULLs, and `key_index`, which flags natural keys no unique index backs.
- **Ordering:** unique-value swaps and rotations under non-deferrable indexes are refused, with the
  way out.
- `lock_timeout`, `audit_table`, `migration_locks_table`, and a custom bun migrations table.

### The state file

- Format 2 holds:
  - the fixture files as the migrations leave a database;
  - a checksum, which refuses hand edits;
  - per-file line counts;
  - the migrations covered and its base, so two branches that each generated a migration conflict
    visibly and the merge runbook resolves it.
- Format 1 files are still read.

### Library

- `fixturemigrate.LoadProject` and one `Project` method per command (`Check`, `Export`, `Generate`,
  `Baseline`, `Status`, `Sync`). They have the commands' checks, typed errors (`ErrRefused`,
  `ErrFindings`, `ErrLineage`, `ErrUnmigrated`, `ErrStateConflict`), and results that marshal to the
  commands' JSON.

### Validation, tests and CI

- **Unit tests and fuzzing** of every parser of edited text: fixture files, numbers, intervals, the
  state file and generated Go.
- **Tests against PostgreSQL under both drivers:**
  - every policy and edge case;
  - the real `dbfixture`;
  - bun's real migrator in both of its modes;
  - premise tests for every claim about bun.
- **A property test** of random models, ownership modes, soft deletes and edits, checking that
  apply after generate equals the files, and that revert, check and export agree.
- **Long-running examples,** replayed release by release against bun's migrator: `examples/saas`
  (13 releases), and a commerce back office in `examples/commerce`.
- **CI:**
  - unit tests on Go 1.24, 1.26 and stable;
  - PostgreSQL 12–18 (19 beta allowed to fail);
  - both drivers;
  - bun master;
  - lint, vet, staticcheck and govulncheck;
  - the GitHub Action end to end.

  GitLab CI mirrors the pipeline, and templates are provided for projects that use the tool.

### Documentation

- [Usage](docs/usage.md), [production runbook](docs/production.md), [CI](docs/ci.md),
  [fixture files](docs/fixture-files.md), [reference](docs/reference.md),
  [troubleshooting](docs/troubleshooting.md), [concept and roadmap](docs/concept.md), and
  [architecture](docs/architecture.md) for contributors.
