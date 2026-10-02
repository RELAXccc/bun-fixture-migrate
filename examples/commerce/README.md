# Example: a shop's back office over twelve releases

The second long-running example, next to [`examples/saas`](../saas): an online shop whose master
data lives in three PostgreSQL schemas, is split between what the fixture files own and what the
back office owns, and changes release after release through generated migrations deployed to
databases that are each in a different state.

Everything under `migrations/` and `fixtures/`, `fixture-migrate.yml` and `models.go` is the project as
it looks after the last release, and the generated files in it are not written by hand: the test
`dbtest/longrun_commerce_test.go`, with the replay in `dbtest/longrun_test.go`, replays the history in
[`timeline/`](timeline) with the real command, deploys every release, and checks that what it produced
is byte for byte what is committed here.

## The domain

| Model | Table | What it shows |
| --- | --- | --- |
| `Currency` | `billing.currencies` | keyed by its ISO 4217 code, which is its primary key too (`id: code`); every other table names a currency by code |
| `Country` | `countries` | ISO codes, a jsonb display name per locale, a reference into another schema |
| `TaxRate` | `billing.tax_rates` | an enum class, effective-dated (`valid_from`, `valid_to`), one open rate per country and class under a partial unique index; never deleted |
| `PaymentProvider` | `billing.payment_providers` | `mode: upsert`: ops add local providers; an enum kind, `char(3)[]` currencies, jsonb names |
| `Category` | `catalog.categories` | a tree through `parent_id`, a `citext` slug, jsonb names, a `text[]` of tags, sibling positions under a `DEFERRABLE` unique constraint |
| `CategorySEO` | `catalog.category_seo` | a 1:1 extension keyed by the category's id (`id: none`), a nullable jsonb |
| `ShippingZone` | `shipping_zones` | `mode: insert`: seeded once, logistics owns it afterwards |
| `ShippingMethod` | `shipping_methods` | `insert_only: [base_price]`; one default per zone under a partial unique index |
| `MenuItem` | `menu_items` | positions unique per menu under a constraint checked after every statement |
| `SenderIdentity` | `sender_identities` | addresses unique whatever their case, under an expression index on `lower(email)` |
| `LoyaltyTier` | `loyalty_tiers` | removed with its table in r12 |
| `AdminRole`, `AdminPermission`, `AdminRolePermission` | `admin_roles`, ... | built-in roles next to the merchant's custom roles: `mode: upsert` and `ids: database` on an identity column; the grants of custom roles kept out with a `where` |
| `HsCode` | `billing.hs_codes` | 1200 customs codes imported in r10, `char(6)` with leading zeros |

The application's own data points at the master data: products (a category, a tax class, a
currency), customers (a country), orders (a shipping method, a payment provider), order lines (the
tax rate they were taxed at), admin users (a role). Between releases the replay places orders and
creates custom roles, and after every deploy checks that none of it moved.

`main.go` is the deploy step, as in the SaaS example: the migrations, then the seed of a new database
from the files `fixture-migrate.yml` lists, then `fixtureapply.SyncSequences`.

## The history

Five long-lived databases: production (every release, with traffic and admin edits), staging
(created at r02, deployed about every other release), the outlet store (the same software on its own
database, deployed at r01, r06 and r12, so it catches up five or six releases at once), plus a fresh
database per release and the developer's workstation. After every deploy the master data of each one
that check finds in agreement equals a fresh seed's.

| | Release | What it exercises |
| --- | --- | --- |
| r01 | the schema in three schemas, five fixture files, `baseline` | production and the outlet store go live |
| r02 | German VAT 19% closed and 20% opened in one release; Poland: a currency, a country, rates, a zone, a method | effective dating under a partial unique index (update before insert); staging is created |
| r03 | the category tree restructured: a category inserted between siblings, a subtree moved under a new root, three siblings rotated; French names; SEO rows for new categories | a rotation under a `DEFERRABLE` constraint in one migration; `id: none` rows inserted after their category; a NULL jsonb (F2); probe: a null inside an i18n map (F1) |
| r04 | sale to the front of the header menu; standard shipping becomes Germany's default | the menu's circle refused under a non-deferrable unique, then the refusal's way out: park one item in a migration of its own; the default moved in two migrations (F4, and a probe of the one-migration flip); back-office edits under `mode: insert`, `insert_only` and `mode: upsert` that are no drift |
| r05 | Klarna; a permission and a role renamed in place; a finance role | `renames: update`, under `ids: database` too; a new role numbered by the identity in production and by the file in a new database; probe: a custom role named like the new built-in one (F6) |
| r06 | `audit_table` turned on; a sender address in lower case, a marketplace sender, a platinum tier | `status` reports what each audited run did; the outlet catches up five releases; probe: two senders trade addresses under `lower(email)` (F3) |
| r07 | the autumn campaign, rolled back with bun's `Rollback` and deployed again | the audit table: the revert undoes what the run made and leaves the change it found made by hand |
| r08 | Austria's reduced VAT halved, as a hotfix | `apply -file` to plan it, `apply -yes -record` refused while bun's lock is held, then applied and recorded; the deploy finds nothing to run in production; a second `-record` refused |
| r09 | production edited by hand: a category name, a sender name, a footer link | `check` reports the drift, `status` does not; `export` from production and `generate` (only the three edits come out, across every ownership mode); the file wins for the link with `generate -from-db`; the comments the export dropped put back, which generates nothing |
| r10 | 1200 HS customs codes, a model and a fixture file of their own | a bulk insert written as `Concat` of parts, planned and timed; leading zeros in a `char(6)` |
| r11 | Denmark on one branch, a smart-home category on another | the state file conflicts; `status` and `generate` refuse its markers; the runbook: keep one migration, generate the other again on top; staging had applied the deleted migration from a preview and finds the new one made |
| r12 | the loyalty programme ends: its column, its table and its model go | removing a model in one go stops every command (F5); `baseline -force` records the `DROP TABLE` as the migration covering it; probe: delete the rows first, then the model (F7); the outlet catches up six releases |

The soft delete (`soft_delete`) and the natural-key lint (`key_index`) landed on main while this
history was written. The lint already reports, as a warning, the two keys no unique index backs:
`TaxRate`'s (only open rates are unique) and `HsCode`'s (a plain lookup index). Releases that retire a
shipping method by soft delete and turn `key_index` to `error` are the next ones to write.

### What the history found

Every problem the history ran into is a finding, `Fn`. A release that works around one or shows it
says so with `known: Fn`, in its `release.yml`, so the mark can go when the tool is fixed.

| | Severity | What it is | Known in |
| --- | --- | --- | --- |
| F1 | medium | a null inside a jsonb mapping (`{en: Garden, fr: ~}`, or a comma in a flow mapping) is stored as `""` by a `map[string]string` field and read as the JSON null by the tool: nothing reports it until `check`, after the first seed, finds the row different | r03 probe |
| F2 | low | a SQL NULL in a nullable jsonb column written through a `nullzero` map is a `null against a default` finding, said otherwise only with the global `policy.null_default: warn`, after which every command repeats the warning and its advice to set it | r03 |
| F3 | medium | a unique index over an expression (`lower(email)`) orders and refuses nothing: two rows trading addresses are written as one migration, which fails when it runs | r06 probe |
| F4 | medium | a partial unique index orders nothing: a zone's one default moved from one method to another fails in one migration; two migrations get through | r04, r04 probe |
| F5 | low | a model taken out of the files and the configuration at once stops every command with `model "LoyaltyTier" is in the fixture file but not in the configuration` (it is in the state file), and no runbook says how to retire a model | r12 |
| F6 | low | the insert of a row the model's `where` would leave out fails on the table's key before the `where` is checked: a bare unique violation where the diagnosis exists | r05 probe |
| F7 | low | against a database without the model's table, the deletes of all its rows are refused with "with the database configured ... the tool reads the types", while one is | r12 probe |

## Running the history

```
cd dbtest
BUN_FIXTURE_MIGRATE_POSTGRES='postgres://postgres:pg@127.0.0.1:5432/postgres?sslmode=disable' \
    go test -run TestLongRunningCommerceProject -v .
```

It creates and drops databases named `bfm_commerce_*`, runs on PostgreSQL 12 and later, and takes
about a minute and a half. The variables of the SaaS history work here too: `BFM_LONGRUN_UPDATE=1`
rewrites this directory instead of comparing it, `BFM_LONGRUN_WORK=<dir>` keeps the project copies,
the binaries and a log of every command per release in `<dir>/commerce`, `BFM_LONGRUN_KEEP=1` keeps
the databases, `BFM_LONGRUN_UNTIL=r05` stops after a release. The `release.yml` keys are
[the SaaS example's](../saas/README.md#running-the-history); a merge also takes `unresolved`, steps
run while the merge stands in its conflicts.
