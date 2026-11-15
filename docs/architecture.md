# skima architecture

skima is one Go binary: `main.go` calls `cmd.Execute`, and all other code is in `internal/` except the published JSON
schemas in `schemas/`. This page covers how the packages fit together and how an apply moves through them. For how a
feature behaves, see the [README](../README.md). For a design in more depth, see `docs/specs/`.

## Packages

| Package | What it does |
|---|---|
| `cmd` | The command line. There is one cobra command per skima command, built fresh by `NewRootCmd`. `cmd/config.go` is the only code that reads configuration (viper: flags, `SKM_` environment variables, `.skima.yaml`). It passes the working directory and a `db.Config` to everything else explicitly. Commands return errors, and `Execute` turns them into exit codes. |
| `internal/manifest` | Reads the manifest's files: the state files (tables, views, enums, sequences, materialized views, sql files) and the changesets in `deltas/`. It validates them against `schemas/`, formats them, and generates the DDL they describe, quoting names when they need it. |
| `internal/plan` | Works out every statement an apply would run, in order, without a database. `Planner{Workdir, Schema}` plans the state build (`ForState`), a rerun of the state sql files (`ForStateSql`), or the changesets after a version (`ForChangesets`). |
| `internal/db` | Talks to postgres. It applies a planned unit (`ApplyUnit`), records history, takes the per-schema advisory lock, bootstraps a role and schema, and reads a schema from the catalog. `db.Open(cfg)` returns a `*DB`, a connection pool whose search path is the managed schema. |
| `internal/drift` | Compares two schemas read from the catalog: the database as it is, and a throwaway build of the state files. |
| `internal/changes` | Works out how the state files changed since a baseline (a git commit) and generates the changeset deltas for those changes, using rename and backfill hints. |
| `internal/verify` | Proves a changeset. It builds the baseline in a scratch schema, applies the changeset on top, and compares the result with a build of the current state. Everything runs in a transaction that is always rolled back. |
| `internal/editor` | Edits the state files in memory, validates the result, then writes only the files that changed. |
| `internal/server` | `skima serve`, a localhost HTTP API over the editor, changes and verify. |
| `internal/util` | Semantic versions, a generic `Set`, and atomic file writes. |
| `schemas` | The JSON schemas, embedded in the binary and published at `https://raw.githubusercontent.com/johnmuhlestein/skima/main/schemas/`. |

Dependencies point one way:
- `cmd` and `server` sit on top.
- `verify` uses `changes`, `plan`, `db` and `drift`.
- `db` and `changes` use `plan`, which uses `manifest`.
- `manifest` uses only `util` and `schemas`.
- `drift` and `util` use no other skima package.

## From manifest to database: plan, then execute

Every command that changes or reads a database follows the same flow. The **plan** is the single description of
what will run:

```
cmd (skima apply)
 │  reads config ─► workdir, db.Config
 │  db.Open, LockApply  ─────────────────────────────── advisory lock per schema
 │  GetApplyHistory     ─► deployed version (or none)
 │  validate            ─► state files + pending changesets (manifest.ValidateForApply)
 ▼
plan.Planner.ForState / ForChangesets
 │  manifest: parse state files and deltas ─► DDL
 │  ─► plan.Plan{Units: [Unit{Kind, Version, Transactional, Steps: [Step{Sql, Tolerate, Record, ...}]}]}
 ▼
db.ApplyUnit (one per unit)
 │  beginRun: a connection and, unless the unit opts out, a transaction
 │  executor.run: each step's statements in order
 │     - inside a transaction, each statement runs in a savepoint, so a tolerated error is undone and the run continues
 │     - views that reference a relation not created yet are retried
 │     - every statement is recorded in skima_schema_statements, on a separate connection so the record outlives a rollback
 │  finish: commit, or roll back and mark the run 'rolled back' / 'cancelled'
 ▼
FinalizeHistory ─► skima_schema_history row: success, failed, rolled back, cancelled ...
```

- `skima plan` stops after the planning step and prints the plan.
- `apply --dry-run` does the same.
- `skima drift` and `changeset verify` don't apply the plan to the schema. They run it in a scratch schema inside a
  rolled-back transaction, then read the result from the catalog to compare. Because both sides are normalized by
  postgres, the comparison is a plain text comparison.

Cancellation follows the context:
- The first Ctrl+C cancels the command's context.
- pgx asks the server to cancel the running statement instead of closing the connection.
- The run is rolled back on that same connection.
- The history is written with a context detached from the cancellation, bounded by a 30s timeout.
- The exit code is 130.

## State and changesets

- The state files say what the schema is. A state build creates it from nothing.
- The changesets in `deltas/` say how to get from one version to the next.
- Changesets take their definitions from the current state files. For example, `{"object": "table", "action": "add",
  "name": "tags"}` builds `tags` from `state/tags-table.json`.
- The deployed version is the newest successful run in `skima_schema_history`. An `apply --sql` run doesn't count.
- `apply` plans the changesets newer than the deployed version.
- `skima changeset build` generates a changeset from the git diff of the state files, and `changeset verify` proves it.
  See `docs/specs/state-editor-api.md`.

## Tests

- Most packages test against fixture manifests, and need neither a database nor network access.
- `internal/plan` has golden files of the planned SQL. Any change to generated DDL shows up there.
- Tests in `internal/db`, `internal/verify` and `cmd` need postgres and are skipped unless `SKIMA_VERIFY_DATABASE=1`.
  See [CONTRIBUTING.md](../CONTRIBUTING.md).
- The `cmd` tests run commands in-process through `NewRootCmd`.
