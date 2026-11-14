# Spec: state editor API and changeset builder (iteration 1)

**Status:** draft for review
**Goal:** make a schema change once — by editing the state — and have skima write the changeset for it.

## Problem

Every schema change is written twice today: once in the state files (what the schema should look like) and once as a
delta in a changeset (how to get an existing database there). The two must agree exactly — `skima manifest validate`
now catches many mismatches, but writing the delta is still manual, repetitive and easy to get wrong.

Iteration 1 adds an embedded HTTP server, `skima serve`, with a REST API that reads the state, edits it incrementally,
and builds the changeset from the edits. Iteration 2 puts a web UI on top of this API.

## Decisions

| Topic | Decision | Alternatives considered |
|---|---|---|
| How deltas are produced | **Diff + intent hints.** Diff a baseline against the current state files; the API also records intent a diff cannot infer (renames, backfills for new `NOT NULL` columns). | *Journal of API edits* — exact intent, but blind to hand edits and git pulls. *Diff only* — renames come out as drop + add, which loses data. |
| Baseline (the "before" side) | **A git ref** — by default the last commit that changed `deltas/`. Overridable. | *Snapshot at draft start* — goes stale on rebase. *Live database* — needs DB access to build a delta, and postgres doesn't keep all intent. |
| UI | **API only in iteration 1.** UI in iteration 2, built on a stable API. | Minimal embedded UI now. |
| Editable objects | **Tables first** — tables and everything on them. Views, enums, sequences and materialized views are read-only through the API until iteration 2. | All state kinds from the start. |

Because deltas come from a diff, changes made by hand or pulled from git produce deltas too — the API is a convenient
way to edit, not the only way.

## Scope

**In iteration 1**
- `skima serve`: a local HTTP server exposing the API below
- Read every state object (all six kinds)
- Incremental edits to tables: create/drop tables; add/alter/rename/drop columns; primary key; foreign keys; indexes; constraints; triggers; table and column comments
- Detect changes against the baseline and generate the changeset deltas, with warnings and the SQL preview from `pkg/plan`
- Build: write the next `deltas/v*.json`
- Verify: prove a generated changeset against a database (`POST /changes/verify`, `skima changeset diff --verify`)
- CLI equivalents of the change detection and build (`skima changeset diff|build`), sharing the same engine
- `skima manifest fmt`, so files can be normalized once before the API starts rewriting them

**Not in iteration 1**
- A UI
- Editing views, enums, sequences, materialized views, functions or state SQL files through the API (their changes are *detected* and reported, but deltas for them are written by hand — see [Unsupported changes](#unsupported-changes))
- Table renames — the changeset format has no table-rename delta
- Authentication, multiple users, remote access
- Git operations — skima writes files; the user commits

## Workflow example

```
skima serve -w ./myschema            # http://127.0.0.1:7012, baseline = last commit that changed deltas/

POST  /api/v1/state/tables/documents/columns          {"name":"status","datatype":"text","nullable":false,"backfill":{"expression":"'draft'"}}
PATCH /api/v1/state/tables/documents/columns/file_loc {"name":"file_location"}          # rename - recorded as a hint
POST  /api/v1/state/tables/documents/indexes          {"name":"documents_status_idx","columns":["status"]}

GET   /api/v1/changes                                 # what changed, the deltas it generates, warnings, SQL
POST  /api/v1/changesets                              {"description":"document status"}   # writes deltas/v1_0_4.json
```

`deltas/v1_0_4.json` would contain, in this order: the column rename, the `status` column added as nullable, a script
delta backfilling `'draft'`, an alter setting `status` NOT NULL, and the index add.

## The server

```
skima serve [--port 7012] [--bind 127.0.0.1] [--baseline <git ref>] [--cors-origin <origin>]
```

- Uses the global `--workdir` / `SKM_WORKDIR` like every other command. Database settings are only needed for
  [verify](#verify-needs-a-database).
- Binds to `127.0.0.1` by default. There is no authentication, so binding any other address prints a warning.
- Rejects requests whose `Host` header is not `localhost`, `127.0.0.1` or `[::1]` (with the port), to block DNS
  rebinding attacks from web pages open in the same browser.
- No CORS headers unless `--cors-origin` is given (for a UI dev server in iteration 2).
- Built on the standard library's `net/http` routing (Go 1.22+ method and path patterns) — no new dependencies.
- Writes are serialized with a mutex; [ETags](#concurrency) protect against edits made outside the server.

## Files and writes

**Format.** The API reads and returns state objects in exactly their file format (`primary-key`, `foreign-keys`, ...),
so an object from `GET` can be written back unchanged. Writes use one canonical format: 2-space indent, properties in
the order the README documents, optional properties omitted when unset, `nullable` always written. Every file the
writer produces is validated against its JSON schema in tests.

The first API write to a hand-formatted file reformats the whole file. To keep that out of feature diffs, run
`skima manifest fmt` once and commit the result before using the API.

**Validation on write.** Each edit is applied in memory, then every state file is validated (schema and references).
The write is rejected (`422`) if it introduces an error that was not there before — e.g. dropping a column another
table's foreign key references. Errors that already existed are returned as warnings, so an unrelated broken file does
not block every edit.

**Concurrency.** Each state object's `GET` returns an `ETag` (a hash of its file). Writes require `If-Match` (`428`
without it) and fail with `412` if the file changed since it was read — e.g. edited by hand in the meantime.

**Cascading edits.** Renaming a column also renames it in the table's primary key, indexes, constraints and foreign
keys, in other tables' foreign keys that reference it, and in the `ownedby` of sequences it owns. SQL that mentions the
old name (index, constraint and trigger DDL, views) can't be rewritten safely, so it is returned as a warning.

Dropping something other objects use is rejected with the list of what uses it, unless the request cascades - then
those are dropped too, as postgres does, and each is listed in the warnings:
- a column: the primary key, indexes, constraints and foreign keys on the table that include it, other tables' foreign
  keys that reference it, and sequences it owns
- a table: other tables' foreign keys that reference it, and sequences its columns own
- a primary key: other tables' foreign keys that reference its columns

## API

Base path `/api/v1`. Request and response bodies are JSON.

### Reading

| Method and path | Returns |
|---|---|
| `GET /manifest` | Working directory, baseline ref and commit, latest changeset version, counts per kind, validation summary |
| `GET /state/{kind}` | Names of the objects of a kind: `tables`, `views`, `enums`, `sequences`, `matviews` |
| `GET /state/{kind}/{name}` | The object in file format, with an `ETag` header |
| `GET /validation` | Full validation results, as `skima manifest validate --output=json` |
| `GET /openapi.json` | The OpenAPI 3.1 description of this API |

### Editing tables

Every write takes `If-Match` with the table's ETag and returns the updated table, its new `ETag`, and any warnings.

| Method and path | Edit |
|---|---|
| `POST /state/tables` | Create a table (body: a table object) |
| `DELETE /state/tables/{table}` | Drop a table (`?cascade=true` to drop what references it) |
| `PATCH /state/tables/{table}` | Set or clear the table comment |
| `POST /state/tables/{table}/columns` | Add a column; optional `backfill` (`expression` or `script`) for a `NOT NULL` column (see [hints](#intent-hints)) |
| `PATCH /state/tables/{table}/columns/{column}` | Change `datatype`, `nullable`, `default`, `dfltfunc`, `comment`; changing `name` renames |
| `DELETE /state/tables/{table}/columns/{column}` | Drop a column (`?cascade=true` to drop what references it) |
| `PUT` / `DELETE /state/tables/{table}/primary-key` | Set or remove the primary key (`?cascade=true` to drop foreign keys that reference it) |
| `PUT` / `DELETE /state/tables/{table}/foreign-keys/{name}` | Add or replace / remove a foreign key |
| `PUT` / `DELETE /state/tables/{table}/indexes/{name}` | Add or replace / remove an index |
| `PUT` / `DELETE /state/tables/{table}/constraints/{name}` | Add or replace / remove a constraint |
| `PUT` / `DELETE /state/tables/{table}/triggers/{name}` | Add or replace / remove a trigger |

### Changes and changesets

| Method and path | Does |
|---|---|
| `GET /changes` | The detected changes since the baseline, the deltas they generate, warnings, unsupported changes, and the SQL preview. `?baseline=<ref>` overrides the baseline |
| `GET /changes/hints` | The recorded intent hints |
| `DELETE /changes/hints/{id}` | Discard a hint (e.g. treat a rename as drop + add after all) |
| `POST /changes/verify` | [Verify](#verify-needs-a-database) the generated changeset against a database |
| `POST /changesets` | Build: write the changeset file (see [Building](#building-the-changeset)) |

### Errors

```json
{"error": {"code": "invalid_edit", "message": "the edit would make the manifest invalid",
           "issues": [{"file": "state/orders-table.json", "severity": "error", "location": "/foreign-keys/0/reference/columns/0",
                       "message": "referenced column 'uid' is not a column of table 'users'"}]}}
```

| Status | When |
|---|---|
| `400` | Malformed request |
| `404` | Unknown object |
| `409` | Conflict with existing content — e.g. a column or changeset that already exists, or building with unacknowledged unsupported changes |
| `412` / `428` | `If-Match` doesn't match the file / is missing |
| `422` | The edit would introduce validation errors (`issues` lists them) |

## Change detection

### Baseline

The baseline is the state files at a git commit, read with `git show <commit>:<path>`; the current side is the working
tree. By default the baseline is the last commit reachable from `HEAD` that changed the `deltas/` directory — the state
the latest changeset brought the schema to. `--baseline` (or `?baseline=` on `/changes`) overrides it, e.g. `origin/main`.
The working directory must be inside a git repository.

### From state differences to deltas

| Change between baseline and current | Generated deltas |
|---|---|
| Table added | `table add` (creates its indexes, constraints, triggers, comments; its foreign keys are added at the end of the changeset) |
| Table removed | `table drop` — **destructive** warning |
| Column added | `column add`; `NOT NULL` without a default on an existing table: see [hints](#intent-hints) |
| Column removed | `column drop` — **destructive** warning |
| Column renamed (hint) | `column rename` |
| Column `datatype` changed | `column alter` with `mods.datatype` — warning: rewrites the table, fails if postgres has no implicit cast |
| Column `nullable` changed | `column alter` with `mods.nullable` — `NOT NULL` warns that existing nulls make it fail |
| Column `default` / `dfltfunc` changed | `column alter` with `mods.default` |
| Primary key added / removed / changed | `pk add` / `pk drop` / drop then add — named `<table>_pk`, as the state build names it |
| Foreign key, index, constraint or trigger added / removed | `add` / `drop` delta of that object |
| Foreign key, index, constraint or trigger changed | `drop` then `add` |
| Table or column comment set / cleared / changed | `comment add` / `comment drop` / `comment add` |

Each changed attribute of a column becomes its own `column alter` delta, since an alter changes exactly one attribute.

**Order within the changeset**, so dependents go before what they depend on and destructive steps come last:
1. foreign key, constraint, index, trigger and primary key drops
2. column renames
3. table adds
4. column adds (with their backfills)
5. column alters
6. primary key, constraint, index and trigger adds, then foreign key adds
7. comment changes
8. column drops
9. table drops

### Intent hints

A diff can't tell some intents apart, so the API records them as hints in `<workdir>/.skima/draft.json`:

- **Rename** — renaming a column through the API records `{table, from, to}`. Without the hint the diff sees a drop
  and an add. Renames chain (`a → b → c` becomes `a → c`; renaming back removes the hint), and dropping the column
  removes its hint.
- **Backfill** — adding a `NOT NULL` column with no default to a table that existed at the baseline fails on a populated
  table. A backfill makes the generator use skima's override pattern: add the column as nullable (`def`), fill it, then
  an alter to `NOT NULL`. The backfill is one of:
  - `{"expression": "<sql expression>"}` — generates a script delta `update <table> set <column> = <expression> where <column> is null`
  - `{"script": "<file>.sql"}` — a script delta running that file from `deltas/scripts`, for backfills that need joins
    or several statements. The file must exist when the changeset is built

  Without a backfill, a plain `column add` is generated with a warning that it only works on an empty table.

The pending change itself is the state files — the difference between the working tree and the baseline. The draft
file only holds these hints, which is all a diff can't infer.

Hints are reconciled with the diff each time: one that no longer matches the files (e.g. a rename undone by hand) is
dropped with a warning. The file lives outside `state/` and `deltas/`, so validation and apply ignore it. Building a
changeset removes it.

**The draft is local, not committed.** skima writes `.skima/.gitignore` (containing `*`) when it creates the directory,
so it is ignored however the repository is laid out. The cost: continuing the branch from another clone loses the
hints, and a rename would come out as a drop and an add. To catch that, the diff warns about **likely renames** — a
table losing one column and gaining another of the same datatype, with no rename hint — so the rename can be recorded
again before building.

### Unsupported changes

Changes the generator doesn't handle in iteration 1 are listed under `unsupported` in `/changes`, each with the reason,
so nothing is silently left out of the changeset:
- views, enums, sequences, materialized views — no generated deltas yet; write those deltas by hand
- a table rename — appears as a removed and an added table; there is no table-rename delta
- turning `primarykey` / `generate` (identity) on or off for an existing column
- edits to state SQL files (`state/sql/pre|post`) — e.g. a changed function needs a `function add` delta

### Warnings

Generated deltas carry warnings rather than being blocked: destructive drops, type changes, `NOT NULL` on existing
columns, plain `NOT NULL` adds without a backfill, likely renames without a hint, and dropped hints.

## Building the changeset

`POST /changesets` (and `skima changeset build`):

```json
{"description": "document status", "bump": "patch", "transaction": true, "acknowledgeUnsupported": false}
```

- **Version** — the next `patch` (default), `minor` or `major` version after the highest changeset in `deltas/`, or an
  explicit `"version"` (pre-releases like `1.1.0-rc_1` allowed). It must be newer than every existing changeset; `409`
  if the file already exists.
- `transaction` defaults to `true`, as in the changeset format.
- `409` if there are no changes, or if there are unsupported changes and `acknowledgeUnsupported` is not `true`.
- The written file is validated like any changeset (schema, file name, references — as errors, since it is pending)
  before the response returns. The response includes the file path, its content and the SQL preview.
- The hints file is removed. Nothing is committed to git.

## Verify (needs a database)

`POST /changes/verify` proves the generated changeset is right, using `pkg/drift`'s comparison. In a transaction that
is always rolled back, it builds the baseline state, applies the generated changeset, and compares the resulting schema
with a direct build of the current state. Any difference means the generator (or a hand-written part of the changeset)
is wrong. It needs the database settings `drift` uses, changes nothing and takes no apply lock.

*Implementation risk:* `drift` builds state into temporary tables, which only needs the TEMP privilege. Applying a whole
changeset on top may instead need a scratch schema created inside the rolled-back transaction, which needs the CREATE
privilege on the database. Milestone 4 should settle which, before committing to verify running in CI and locally.

## CLI equivalents

The change detection and build engine is a package the server and the CLI share:

```
skima changeset diff  [--baseline <ref>] [--output json] [--check]
skima changeset build --description "..." [--bump patch|minor|major | --version X.Y.Z] [--acknowledge-unsupported]
skima manifest fmt    [file...]
```

`skima changeset diff --check` exits 1 when the state has changed since the baseline but no changeset covers it —
useful in CI to catch a state edit pushed without its changeset.

## Implementation plan

Packages:
- `pkg/editor` — load state objects with ETags, apply table edits, cascade renames, validate, write canonically
- `pkg/changes` — baseline from git, diff, hint reconciliation, delta generation, ordering, warnings
- `pkg/server` — HTTP handlers, error mapping, Host check, OpenAPI document
- `cmd/serve.go`, `cmd/changeset.go`, `cmd/fmt.go`

Milestones, each a PR:
1. **Editor and canonical format** — `pkg/editor`, `skima manifest fmt`
2. **Change detection and build** — `pkg/changes`, `skima changeset diff|build`
3. **Server** — `pkg/server`, `skima serve`, OpenAPI
4. **Verify** — `POST /changes/verify` and `skima changeset diff --verify`

## Testing

- **Editor** — each edit and cascade; rejected edits return the right issues; written files validate and round-trip
- **Diff to deltas** — golden tests: pairs of baseline and current manifests with the expected changeset, covering
  every row of the mapping table, ordering, hints and unsupported changes
- **Equivalence in CI** — for every golden case, run verify against the CI postgres service: baseline + generated
  changeset must equal a direct build of the current state. This is the main guard against generator bugs
- **Server** — `httptest` tests for each route, status code and the `Host` check; a test that every route is in the OpenAPI document

## Resolved questions

1. **Draft file** — `.skima/draft.json` is local and git-ignored (skima writes `.skima/.gitignore`). Mitigated by the
   likely-rename warning.
2. **Verify** — part of iteration 1 (milestone 4), including the CI equivalence tests.
3. **Port** — `7012` by default (registered only to an obscure service, "Talon Engine").
4. **Backfill** — a SQL expression or a script file in `deltas/scripts`.

## Open questions

1. **Changing a generated delta's intent** (e.g. drop + add instead of rename) — beyond discarding hints, should the
   draft allow overriding how a change becomes deltas? Revisit after milestone 2, once `skima changeset diff` shows
   generated changesets on real changes.

## Iteration 2 preview

A web UI embedded in the binary and served by `skima serve`: browse the schema, edit tables, see the pending changes
and their SQL, and build the changeset. Editing views, enums, sequences and materialized views through the API, with
generated deltas for them.
