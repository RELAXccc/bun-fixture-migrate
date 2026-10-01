# Architecture

This document is for contributors: how the code is laid out, how one command flows through it, the
rules that keep generated migrations working for years, and how to add to it without breaking them.
Users want the [reference](reference.md) and the [usage guide](usage.md) instead.

## The modules and packages

| Path | Imported by | What it is |
| --- | --- | --- |
| `fixturechange` | generated migrations, everything | The types a generated migration carries: `Set`, `Table`, `Change`, `Value`, `Policy`. No logic beyond validation of its own values. |
| `fixtureapply` | generated migrations, `sync`, `plan`, `apply` | The run time: applies or reverts a `Set` inside a transaction, guarded. Needs bun and nothing else. |
| `.` (`fixturemigrate`) | the command, `dbtest`, Go users | The library: configuration, reading fixture files and databases, value canonicalisation, the diff, lints, export, scaffold, rendering, the state file, and every command as a `Project` method. |
| `dbschema` | the library | Reads PostgreSQL's catalog: tables, columns and types, defaults, keys, unique indexes and exclusion constraints, foreign keys, sequences. |
| `internal/pgerr` | the library, the run time | Reads SQLSTATE and messages the same way from `pgdriver` and `pgx`. |
| `cmd/bun-fixture-migrate` | users | The command: flags, printing, exit codes. The logic lives in the library's `Project`; the command is a thin shell around it. |
| `dbtest` (own module) | CI | Every test that needs PostgreSQL, the real `dbfixture` and bun's real migrator. A separate module so the library's `go.mod` stays small. |
| `examples/basic`, `examples/saas` (own modules) | CI, readers | A runnable bun project; and a SaaS back office whose master data evolves over 13 releases, replayed by `dbtest/longrun_test.go`. |
| `testdata/generated` | `dbtest/corpus_test.go` | Generated files frozen as earlier versions wrote them. Each must still compile, validate and run against today's run time. |

`fixturechange` and `fixtureapply` are the long-lived API: they are compiled into users'
migrations, which stay in their repositories forever. Everything else may change between versions.

## One command, end to end

`generate` with a database configured passes through every stage. The other commands are subsets.

1. **Configuration.** `LoadConfig` reads `fixture-migrate.yml` strictly, refusing unknown keys.
   `Config.Prepare` fills in defaults and refuses contradictions, such as an id that is also a
   reference, `insert_only` on a key column, `soft_delete` under `mode: insert`, or `deletes`
   next to an ownership mode. Policies resolve through `Config.ModelPolicy` and `Config.ModeOf`:
   a model's own setting wins over the `policy:` block.
2. **Fixture files.** `ParseDoc` (`fixture.go`) turns YAML nodes into `Cell`s. A cell keeps:
   - the text as written;
   - its YAML tag;
   - what a Go string field would get (`StringText`);
   - what a json column would get (`JSONText`);
   - a reason when no column type settles it (`Unsure`).

   `FixtureSnapshot` (`fixturesnap.go`) then emulates dbfixture: it registers `_id` anchors,
   evaluates whole-value templates in load order, resolves references, and keys every row by its
   model's natural key. The result is a `Snapshot` of `Entry`s (`snapshot.go`), with the findings
   that came up on the way.
3. **The database.** Everything runs in a read-only transaction (`ReadOnly`), with
   `PrepareSession` fixing `TimeZone`, `DateStyle`, `IntervalStyle` and the other settings that
   change how values print.
   - `dbschema.Load` reads the catalog of every schema a model lives in (`Config.Schemas`).
   - `DatabaseSnapshot` (`dbsnap.go`) reads the configured models' rows into the same shape as a
     fixture snapshot: live rows only for `soft_delete`, no ids for `ids: database`, and ordered
     by the table's own columns.
4. **Canonicalisation.** `Canonicalize` (`canon.go`) casts every fixture value to its column's
   type in PostgreSQL and reads it back as the database prints it, so equality is PostgreSQL's
   equality (`1.10` and `1.1` in a numeric). A value the column cannot hold becomes a finding.
   Without a database, `values.go` and `interval.go` settle what can be settled offline, and a
   change that depends on the rest is refused rather than guessed.
5. **Lints.**
   - `LintZeroDefaults`, `LintNullDefaults`, `LintColumns` and `LintSoftDelete` in `fixturesnap.go`
     cover what bun writes as `DEFAULT`, unknown columns, and soft-delete columns.
   - `LintKeys` (`keylint.go`) checks whether a unique index backs each natural key.
   - Each finding has a kind, and the policy decides whether it is an error, a warning or ignored.
6. **The diff.** `Compute(cfg, old, next)` (`diff.go`) compares two snapshots: the state file's
   and the files', or the database's and the files'. It returns a `Result`:
   - **Changes** to insert, update, delete or rename, each with its guard: the old values a row
     must still hold.
   - **Refusals:** changes no migration can write safely, such as an undecided value, a rename
     into a held key, a unique-value circle, or an id that drifted.
   - **Warnings.**
   - **Left-alone counts:** what the ownership configuration gives to the database.

   Changes are ordered by references, so parents come before children and deletes run in reverse,
   and around unique indexes, so a value is freed before it is taken. Deferrable indexes are left
   to the run time.
7. **Rendering.** `Render` (`render.go`) writes one Go file holding a `fixturechange.Set`
   literal, registered as
   `Migrations.MustRegister(fixtureapply.Up(set), fixtureapply.Down(set))`. The file imports only
   `fixtureapply` and `fixturechange`. Comments are escaped, and every map is sorted, so the output
   is byte-for-byte reproducible (`generate -at` fixes the timestamp).
8. **The state file.** `WriteState` (`state.go`) records the fixture files as the migrations leave
   a database. It holds:
   - format 2, with a checksum over every field;
   - per-file line counts;
   - the migrations it covers and its base.

   The next `generate` diffs against it, not against git.

`status`, `plan` and `apply` read generated files back without running them: `ReadMigrations` and
`ReadChangeSet` (`migfile.go`) parse the `Set` literal with `go/ast`. That is how `status` knows
what each migration changes, and how `plan` applies pending ones in a transaction it rolls back.

## The run time

`fixtureapply.Apply` and `Revert` (`apply.go`) take a `Set` and a `bun.IDB`:

1. **Validate** (`validate.go`). A set of a newer `Format` than this version knows, an unknown
   kind, or a malformed lock timeout is refused before anything runs.
2. **Open the transaction:**
   - take a transaction-scoped advisory lock, so change sets never interleave across replicas
     (`WaitForChangeSets`);
   - set the set's `lock_timeout` only after the advisory lock;
   - refuse if row-level security would hide rows, including on the audit table;
   - set deferrable constraints `DEFERRED`.
3. **Seed guard.** If `SeedGuardTable` is empty, the database has not been seeded, and the set
   is a no-op: `dbfixture` will insert the current state by itself.
4. **Each change:**
   - find its row by natural key (`match.go`), resolving references to other models' rows by
     their keys at run time (`references.go`);
   - write only if the row still holds the guard values, with a count guard (`onlyRow`) so a
     statement never touches more than one row;
   - when a statement finds no row, `diagnose.go` works out why (missing, changed, id drift,
     already made) and the set's `Policy` decides: fail, skip with a warning, or treat as done.
   - Explicit ids move their sequence forward (`sequence.go`).
   - Soft-delete tables set or clear their column instead of deleting (`softdelete.go`).
5. **Finish:**
   - check deferred constraints (`SET CONSTRAINTS ALL IMMEDIATE`), then restore the deferrable
     ones;
   - write the audit row, when `audit_table` is configured (`audit.go`). It records each change's
     outcome and the set's SHA-256, so `Revert` undoes only what that database's `Apply` did.
6. **On failure.** Unless the migrator was built `WithMarkAppliedOnSuccess`, bun has already
   recorded the migration as applied. `unrecord.go` removes exactly that record, so the next
   deploy tries again.

`Outcome` reports each change's status, action and message to `WithReport`, `WithLogger` and
`WithSlog`. A failure is a `*ChangeError` with a `Problem` kind.

## Rules that keep it working for years

Generated migrations are code in users' repositories, applied years after they were written, by
whatever version of `fixtureapply` the user has upgraded to. So:

- **`fixturechange` only grows.**
  - A new field must mean "as before" at its zero value: every generated file that predates it
    must keep doing what it did.
  - When an old run time must not apply a new shape, make sure it cannot. It should fail to
    compile (an unknown field in the composite literal), or `Validate` should refuse it through
    `Set.Format` / `CurrentFormat`.
  - `SetSHA256` must cover the new field, or an edited file goes unnoticed by the audit table.
- **The corpus.** After any change to what `Render` writes, add a directory to
  `testdata/generated` named `<commit>-<what>`, holding one file generated by that version.
  `dbtest/corpus_test.go` compiles every one of them against today's run time and runs it.
- **The state file only grows.**
  - `DecodeState` reads every earlier format (`testdata/state-format1-*.yml`).
  - A new format bumps `# format:` and writes the new one.
  - The checksum is refused on any hand edit.
- **Every claim about bun is a test.** What bun's migrator records, what `dbfixture` writes, and
  that UPDATE writes `DEFAULT` for a nil field are all tested in `dbtest` against the real
  packages. CI also runs against bun's master branch, so an upstream change shows up as a failing
  test.
- **Reading commands never write.** `check`, `status`, `export` and `scaffold` run in a read-only
  transaction. `plan` runs the migrations and rolls back, and fails loudly where a role or setting
  would make the deploy behave differently.
- **Refuse rather than guess.** Where a value, an order or an ownership question cannot be settled,
  the tool refuses with a sentence saying what to do. Each such message is listed in
  [troubleshooting](troubleshooting.md).
- **One place for each decision.**
  - Policies: `Config.ModelPolicy` and `ModeOf` at generate time, `Set.PolicyFor` at run time.
  - Natural-key matching: `KeyStr` and `identity` in the diff, `match.go` at run time.
  - Value equality: PostgreSQL's, through `Canonicalize`.

## Adding to it

- **A configuration key:**
  - a field in `config.go` with its yaml tag;
  - validation in `Prepare`;
  - a line in `fixture-migrate.example.yml` (`TestTheExampleConfigurationLoads` reads it);
  - a row in the reference's configuration table;
  - if `scaffold` can guess it, a commented `GUESS` in `scaffold.go`.
- **A policy:**
  - a `Policy` field and its default;
  - the per-model override in `ModelPolicy`;
  - if it acts at run time, a `fixturechange.Policy` field written by `Render`, and the rules above.
- **A finding kind:**
  - a `FindingKind` in `snapshot.go`;
  - its mode in `Config.FindingMode`;
  - its JSON name in `api_json.go`;
  - its message in troubleshooting.
- **A run-time behaviour:** `fixtureapply` plus a `dbtest` test under both drivers. If it changes
  what an existing generated file does, it needs a new field or format instead (see above).
- **A command or flag:**
  - the logic as a `Project` method (`api*.go`), with a `-json` shape in `api_json.go`;
  - flag parsing and printing in `cmd/bun-fixture-migrate`;
  - the reference's command and JSON sections.
- **A release of a long-running example:**
  - `examples/saas/timeline/rNN/` (or the commerce example's) holds the fixture files, schema SQL
    and `release.yml` with the expectations;
  - `BFM_LONGRUN_UPDATE=1` regenerates the example's migrations and state;
  - a `known:` mark in `release.yml` records a finding the release works around, and must go when
    the finding is fixed.

## Tests

| Layer | Where | Run with |
| --- | --- | --- |
| Unit, fuzz | `*_test.go` in each package | `go test ./...`; CI fuzzes `.` and `./cmd/bun-fixture-migrate` |
| PostgreSQL, both drivers | `dbtest/` | `BUN_FIXTURE_MIGRATE_POSTGRES=postgres://… go test ./...` in `dbtest`, again with `BUN_FIXTURE_MIGRATE_DRIVER=pgx` |
| Property | `dbtest/property_test.go` | Random models, modes, edits and soft deletes, checking apply ∘ generate = files, revert, check and export. `BFM_PROPERTY_ITERATIONS=2000` for a long run |
| Corpus | `dbtest/corpus_test.go` | Every frozen generated file compiles and runs |
| Long-running | `dbtest/longrun_test.go` | Replays each release of `examples/saas`. `BFM_LONGRUN_UNTIL=r05` stops early, `BFM_LONGRUN_KEEP=1` and `BFM_LONGRUN_WORK=dir` keep the work, `BFM_LONGRUN_UPDATE=1` rewrites the example |

Notes for running `dbtest`:
- CI connects with password (scram) authentication. A test role that logs in needs a `PASSWORD`,
  and the test must connect with `url.UserPassword`.
- Several suites can run in parallel only against separate PostgreSQL clusters: the tests create
  fixed database and role names.
- Some tests build a package inside the `dbtest` module, so dependencies resolve offline
  (`buildcheck/`, `corpuscheck/`, …). They remove it afterwards, and `.gitignore` lists them.

CI (`.github/workflows/test.yml`, mirrored in `.gitlab-ci.yml`) runs:
- unit tests on Go 1.24, 1.26 and stable;
- `dbtest` on PostgreSQL 12–18, plus 19 beta, which may fail;
- `pgx` on 12 and 18;
- a job against bun master;
- gofmt, `go mod tidy`, vet, staticcheck and govulncheck over every module;
- the composite GitHub Action end to end.

## Where to look next

- [Concept](concept.md): what is done, the known limitations, and what is planned, with the
  reasoning.
- [Reference](reference.md): every command, flag, key, exit code and JSON field.
- [Changelog](../CHANGELOG.md): what each body of work added.
