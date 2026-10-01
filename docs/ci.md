# CI

The checks worth running, and ready-made jobs for GitHub Actions and GitLab CI. The
[production runbook](production.md#the-pipeline) says where each one fits in a deploy.

| Job | Needs a database | Fails when |
| --- | --- | --- |
| `status -offline` | no | a fixture edit came without its migration; a change `generate -allow-partial` left out is not migrated; a fixture migration from another branch is not in the state file; two migrations share a name |
| `plan -strict` | a copy of production, or production | a pending fixture migration would fail or skip a change |
| `status -require-applied` | the deployed database | a migration in the directory is not applied; a deploy that died left bun's lock behind. Add `-strict-order` to fail on a pending migration named before one already applied |
| `check` | the deployed database | the database and the fixture file disagree (exit 3) |

Exit code 3 always means "found something" and 1 "could not run", so a job can treat drift as a
warning and a broken connection as a failure.

`status -offline` compares the fixture files with the state file. Until a project has one, it
compares them with their last commit, which needs git in the job's image and a checkout; with
neither it exits 1 rather than pass a change it cannot see. Run `baseline` once and commit the
state file to make the job need nothing but the checkout. Every command takes `-json` for a report to archive
or alert on; see the [reference](reference.md#json-output).

Pin the tool to one version, the same as the `fixtureapply` your migrations import through
`go.mod`. Build it with a supported Go release: the tool's own `go.mod` names the oldest Go it
builds with, and only supported releases get security fixes.

## GitHub Actions

The repository is a composite action. It installs the command and runs one command with it.

```yaml
name: fixture data
on: [pull_request]

jobs:
  migrations:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v6
        with:
          go-version: stable
      - uses: RELAXccc/bun-fixture-migrate@<tag>
        with:
          command: status
          args: -offline
```

| Input | Default | |
| --- | --- | --- |
| `command` | | `status`, `check`, `plan`, `generate`, `baseline`, `export`, `sync` or `version`; required |
| `args` | | further arguments, split on whitespace |
| `config` | `fixture-migrate.yml` | the configuration, relative to `working-directory` |
| `working-directory` | `.` | where the command runs |
| `version` | | a version to `go install`; empty builds the action's own checkout, which is the `@<tag>` of the `uses:` line |
| `go-version` | | a Go to set up first; empty uses the Go on the runner |

The output `exit-code` is the command's exit code; the step fails unless it is 0. Inputs reach the
command through environment variables, never by being pasted into a script.

A database is whatever the configuration names, usually `database: env:DATABASE_URL`; give the step
that variable from a secret:

```yaml
  plan:
    runs-on: ubuntu-latest
    environment: production
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v6
        with:
          go-version: stable
      - uses: RELAXccc/bun-fixture-migrate@<tag>
        env:
          DATABASE_URL: ${{ secrets.PRODUCTION_READONLY_DSN }}
        with:
          command: plan
          args: -strict
```

`plan` writes and rolls back, so its database user needs the rights the migrations need. `check`
and `status` only read, and are fine with a read-only user or a standby.

To treat drift as a warning in a scheduled job, let the step fail softly and look at the code:

```yaml
      - id: drift
        continue-on-error: true
        uses: RELAXccc/bun-fixture-migrate@<tag>
        env:
          DATABASE_URL: ${{ secrets.PRODUCTION_READONLY_DSN }}
        with:
          command: check
          args: -json
      - if: steps.drift.outputs.exit-code == '3'
        run: echo "::warning::master data drifted in production; see the check step's report"
      - if: steps.drift.outputs.exit-code == '1'
        run: exit 1
```

## GitLab CI

[`ci/gitlab/bun-fixture-migrate.gitlab-ci.yml`](../ci/gitlab/bun-fixture-migrate.gitlab-ci.yml)
holds hidden jobs to extend:

```yaml
include:
  - remote: https://raw.githubusercontent.com/RELAXccc/bun-fixture-migrate/<tag>/ci/gitlab/bun-fixture-migrate.gitlab-ci.yml

variables:
  BFM_VERSION: <tag>

fixture migrations:
  extends: .bun-fixture-migrate:status
  variables:
    BFM_DIR: backend

plan against production:
  extends: .bun-fixture-migrate:plan
  environment: production
  rules:
    - if: $CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH

drift:
  extends: .bun-fixture-migrate:check
  rules:
    - if: $CI_PIPELINE_SOURCE == "schedule"
```

| Job | Runs |
| --- | --- |
| `.bun-fixture-migrate:status` | `status -offline` |
| `.bun-fixture-migrate:plan` | `plan -strict` |
| `.bun-fixture-migrate:check` | `check`; exit 3 is allowed to fail, so drift shows as a warning |
| `.bun-fixture-migrate:applied` | `status -require-applied` |
| `.bun-fixture-migrate` | `BFM_COMMAND` with `BFM_ARGS` |

Variables, from the pipeline or a job: `BFM_VERSION` (default `latest`), `BFM_DIR` (default `.`),
`BFM_CONFIG` (default `fixture-migrate.yml`) and `BFM_SOURCE`, a checkout to build from instead of
installing. The template gives them no values of its own, because a job's variables win over the
pipeline's and would hide a version pinned for the whole pipeline. Each job sets `BFM_COMMAND` and
`BFM_ARGS`, which a job extending it overrides. The database DSN goes in a masked CI/CD variable named
as the configuration's `database: env:NAME` says.

## This repository's own CI

[`.github/workflows/test.yml`](../.github/workflows/test.yml), mirrored by
[`.gitlab-ci.yml`](../.gitlab-ci.yml):

- **lint**: gofmt, `go mod tidy -diff`, `go vet` and staticcheck on all three modules; govulncheck
  under the current Go;
- **unit**: the tests that need no database, under Go 1.24 and the current Go, with the race detector,
  and a short run of every fuzz target;
- **postgres**: the database suite against PostgreSQL 12, 13, 14, 15, 16, 17 and 18 under `pgdriver`,
  and against 12 and 18 under `pgx`;
- **action**: the action on `examples/basic`, deployed by its own migrate step, then checked and
  drifted on purpose;
- **bun-master**: everything against bun's master branch, allowed to fail: every premise about bun
  has a test, so a change upstream shows here before it is released.

A weekly scheduled run catches a new PostgreSQL image, a new advisory or a change on bun's master
while the repository is quiet.
