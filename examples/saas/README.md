# Example: a SaaS application over twelve releases

A bun application whose master data changes release after release, kept in dbfixture files and
changed through generated migrations, deployed to databases that are each in a different state.
Where [`examples/basic`](../basic) shows one fixture change, this project shows a year of them.

Everything under `migrations/` and `fixtures/`, `fixture-migrate.yml` and `models.go` is the project as
it looks after the last release, and the generated files in it are not written by hand: the test
`dbtest/longrun_test.go` replays the history in [`timeline/`](timeline) with the real command, deploys
every release, and checks that what it produced is byte for byte what is committed here.

## The domain

Master data, loaded by dbfixture into a new database and changed by fixture migrations afterwards:

| Model | Table | What it shows |
| --- | --- | --- |
| `Currency` | `currencies` | `char(3)` codes, a zero (`minor_units` of the yen) in a column without a default |
| `Country` | `countries` | a reference to a currency, a `numeric(5,4)` tax rate |
| `Plan` | `plans` | a `uuid` key from `gen_random_uuid()` the file never names, an enum tier, `jsonb` settings, `text[]` tags, a retired plan |
| `PlanPrice` | `plan_prices` | effective-dated, keyed (plan, currency, valid_from), `numeric(12,2)`; invoices point at it |
| `Feature` | `features` | explicit serial ids, a nullable unit |
| `PlanFeature` | `plan_features` | a join table with a composite primary key, no id, a nullable quota |
| `Role`, `Permission`, `RolePermission` | `roles`, `permissions`, `role_permissions` | RBAC; master roles share the table, and the id sequence, with tenants' custom roles (`where: tenant_id IS NULL`) |
| `Translation` | `translations` | keyed (locale, key), unicode and multi-line text, 1500 rows in a file of their own |
| `Category` | `categories` | a tree through `parent_id` |

The application's own data points at the master data: `tenants` (a country), `users` (a role),
`subscriptions` (a plan and a currency) and `invoices` (a plan price).

## The deploy step

```
export DATABASE_URL='postgres://postgres:secret@localhost:5432/saas?sslmode=disable'
go run . migrate     # the pending migrations, then the seed of a new database
go run . status
go run . rollback    # the last group
```

`main.go` is what every replica runs on deploy. It builds the migrator
`WithMarkAppliedOnSuccess(true)`, runs the migrations, and seeds a database whose seed guard table
is empty with dbfixture, then moves the sequences with `fixtureapply.SyncSequences`. Two details go
beyond `examples/basic`:

- The fixture files, the seed guard table and the tables to move sequences in are read from the
  embedded `fixture-migrate.yml`, so the seed loads exactly the files the command generates
  migrations from, in the same order, and nothing has to change when the files are split.
- bun's migrator lock does not wait: a replica that finds it taken gets an error at once. `migrate`
  retries it for `SAAS_LOCK_WAIT` (60s by default) and says how long it waited.

## The history

Each directory of `timeline/` is one release, as a developer makes it: the fixture files, models
(`models.go.txt`, kept out of the go tool's way) and SQL migrations it changes, and a `release.yml`
that says what the developer runs, what happens to the databases between releases, and what every
command is expected to say. The generated migrations and the state file are not in it.

| | Release | What it exercises |
| --- | --- | --- |
| r01 | schema, master data, `baseline` | production and the on-premises customer go live |
| r02 | new team prices from April, a scale plan, an audit log feature and its grants | effective-dated inserts; staging is created |
| r03 | `plans.trial_days NOT NULL DEFAULT 14`, a free plan with a trial of 0, French | `generate` refuses the zero against the default; the column default is dropped instead. `plan` without `-with-sql` fails on the missing column, with it succeeds |
| r04 | an admin edits two translations in production; the release changes the same two rows | a change to another column of the edited row is made, one to the same column is skipped; `check` reports the drift. Rollback on a copy of production |
| r05 | the admin's values folded into the file, `billing.write` renamed to `billing.manage` (`renames: update`), a billing administrator role | id ranges for master rows in a table tenants insert into; probes: the next free id is a tenant's, an id-less row cannot be seeded, a tenant's role code gets the master grants |
| r06 | `ALTER TYPE plan_tier ADD VALUE 'enterprise'` and an enterprise plan in the same release | a non-transactional `.up.sql`; `plan -with-sql` reports a failure the deploy does not have; the on-premises customer catches up five releases |
| r07 | the fixture file split into four, listed under `fixtures:` | nothing to generate; a new database is seeded from four files and ends up the same |
| r08 | priority support removed with its grants, the scale plan retired (`active: false`) | deleting it is refused by `generate`, and with deletes allowed by the run time (invoices point at its prices) |
| r09 | the category tree rearranged: a root renamed, a subtree moved, a new root | deployed from three replicas at once; an admin renames a category (drift) |
| r10 | two branches each add a feature with id 7 and are merged | the state file conflicts on purpose; the runbook's resolution: the id fixed in the fixture file as the merge resolves it, one branch's migration deleted and generated again on top of the other's |
| r11 | `generate -from-db` against production for the admin's rename; `sort_order` renamed to `position` by SQL | `baseline -force` for the rename, then a reorder |
| r12 | the help centre: 1500 translations in a fifth file | timing; staging and the on-premises customer catch up, the latter across six releases and the column rename. Probes: rolling back staging, exporting into five files, dropping a change production cannot make |

### What the history found

The history was first replayed against the tool as it then was, and every problem it ran into is
written down here as a finding, `Fn`. A release that works around a finding still open says so with
`known: Fn`, in its `release.yml` and in the test, so the workaround can go when the tool is fixed.
Six have been fixed since; the commit that fixed each is named by its subject. Two of their
workarounds are still in the history, as it was made, and harmless now: `Role`'s
`defaults: {tenant_id: ~}` (F1) and the categories numbered 10 and 11 in r09 (F8).

| | What it was | Now |
| --- | --- | --- |
| F1 | `export` wrote every column of the table, including ones the fixture files never write (`roles.tenant_id`), so the runbook's export-then-generate was refused. Worked around with `defaults: {tenant_id: ~}` on `Role` | fixed: an export writes the columns the fixture files hold ("export the columns and ids the fixture files hold, not the database's"); the workaround stays, and changes nothing |
| F2 | `export` wrote the source database's ids, including the plans' `gen_random_uuid()` keys and the serial ids of prices and translations the file left to the database; adopted, every other database drifted and later deletes were skipped there | fixed by the same change: an id the fixture files leave to the database is not exported |
| F3 | a table the application inserts into too has no safe id for a new master row: the next id is a tenant's in production, and an id-less row cannot be seeded after rows with ids | open: worked around by moving the sequence to 10000 (`20260504090000_roles_master_id_range`) and numbering master roles below it |
| F4 | references and the insert guard ignored `where`, so a master role whose code a tenant's custom role has was skipped and its grants went to the tenant's role | fixed: a model's `where` holds in every statement a migration runs ("carry a model's where into the migration, and keep every statement inside it") |
| F5 | `plan -with-sql` runs the SQL and the fixture migrations in one transaction, so a new enum value cannot be used there, and the plan reported a failure the deploy does not have | open, and said: the plan is inconclusive there (exit 1) and names the cause |
| F6 | `plan -with-sql` leaves the non-transactional effects (`setval`) of the SQL migrations it ran | open: PostgreSQL's sequences are outside every transaction; plan notes such a migration |
| F7 | bun's migrator lock fails rather than waits, and a crashed deploy leaves it taken | open, and bun's: `main.go` retries the lock, and `status` reports one left behind |
| F8 | the database snapshot read rows in text order of the id (1, 10, 2, ...) and resolved a self-reference only to rows read before it: a parent with id 9 and a child with id 10 broke `check`, `export`, `sync`, `generate -from-db`. Worked around by numbering the new categories 10 and 11 | fixed: rows are read in the table's own order, and a tree whatever the order of its ids ("order rows by the table's own id and key, not by their text, so 9 comes before 10"; "read a tree whose parents come after their children, export it parents first, and read every table in a fixed order") |
| F9 | after the merge, `status -offline` found nothing once `baseline -force` ran, and before it pointed at `generate`, which would have turned the id both branches took into a rename | fixed: the state file keeps its history, and `status`, `generate` and `baseline` refuse a migration generated on another branch; the runbook deletes one of the two and generates it again ("keep the state file's history: what it covers and what that was generated against"; "refuse two branches' migrations from one state, and generate one of them again"). r10 follows it |
| F10 | a rollback inverts changes the migration found already made: staging gets the name production's admin gave a category | open: the runbook's [rolling back](../../docs/production.md#rolling-back) says what `Revert` assumes, and r12 shows it |
| F11 | the runbook's remedy for a missing row, dropping the change from the migration, left every other database without it while the state file said they had it | fixed in the runbook, which sets `MissingRow: "warn"` in that one migration's `Policy` instead ("do not tell anybody to drop a change for a missing row from the migration"; "name a missing-row remedy that keeps the change, and say what Revert assumes"). r12's probe still shows what the old remedy did |

## Running the history

```
cd dbtest
BUN_FIXTURE_MIGRATE_POSTGRES='postgres://postgres:pg@127.0.0.1:5432/postgres?sslmode=disable' \
    go test -run TestLongRunningSaaSProject -v .
```

It creates and drops databases named `bfm_saas_*`, and takes about a minute. After a change to the
timeline or to the tool's output, `BFM_LONGRUN_UPDATE=1` rewrites this directory instead of comparing
it; review the diff like any other. `BFM_LONGRUN_WORK=<dir>` keeps the project copy, the binaries and
a log of every command per release; `BFM_LONGRUN_KEEP=1` keeps the databases;
`BFM_LONGRUN_UNTIL=r05` stops after a release.

`release.yml` has these keys, all optional:

| Key | |
| --- | --- |
| `description` | what the release is |
| `steps` | what the developer does, in order: `overlay` a directory, `remove` files, `generate` (`name`, `at`, `flags`), run a `cli` command or the `app` against an `env`, run `sql`, `edit` a file, `merge` branches with git, check a `file`. Each takes `exit`, `output`, `absent`, `known`, `note` |
| `create` | environments this release creates |
| `deploy` | environments this release is deployed to; dev (fresh) and the workstation always are |
| `replicas` | deploy production from this many processes at once |
| `before_deploy` | SQL per environment: an admin's edit, a tenant's action |
| `plan` | steps run before the deploy, usually `plan` against an environment |
| `expect` | what `migrate` says, and what `check` says if not that the database and the files agree |
| `after` | steps after the deploy |
| `probes` | what would have happened: steps on a copy of the project (`from: start` for the project before the release) and of a database (`clone: prod`, or `fresh`), before or after the deploy |
| `known` | the findings this release works around |
