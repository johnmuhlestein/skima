# Spec: rolling back a changeset

**Status:** decisions agreed. There is no code yet.
**Goal:** when a release's schema change goes wrong after it was applied, get the database back to the previous
version quickly, with a rollback that was reviewed and proven before it was needed.

## Problem

A changeset that fails while applying is already handled: it runs in a transaction, so a failure rolls the whole
changeset back and nothing is left half-applied. This spec is about a changeset that **applied successfully** but has to
be undone, for example because the release built on it is being reverted.

Today there are two ways to undo one, and both are slow in an incident:
- **Roll forward:** revert the state files in git, `skima changeset build` a new changeset, verify it, commit it and
  deploy it. This works today and keeps history linear, but it needs a code change and a pipeline run. It also has to
  be worked out in the middle of the incident.
- **By hand:** run SQL against the database, then fix the history tables so skima agrees. This is error-prone, and the
  schema no longer matches anything in the manifest.

A deploy pipeline normally rolls back by **redeploying the previous artifact**. With skima, that means running
`skima apply` with the older manifest (say at 1.0.3) against a database at 1.0.4. That manifest doesn't contain
`v1_0_4.json`, so skima can't know how to undo it. Today apply just reports nothing newer than 1.0.4 and exits 0, which
leaves the database ahead of the code without saying so.

A further problem is that deltas don't contain their definitions. They read them from the state files, for example
`{"object": "column", "action": "add", "name": "legacy_ref", "reference": "documents"}`. Undoing a *drop* means
re-adding something the current state files no longer describe.

## Decisions

| Topic | Decision | Alternatives considered |
|---|---|---|
| Where a rollback comes from | **Generated at build time.** `skima changeset build` also writes the changeset's down file, `deltas/down/v1_0_4.json`, by diffing in the other direction: the current state back to the baseline. It is reviewed and committed with the up changeset. Being in `deltas/down/` keeps down files from being mistaken for changesets. **Down files are optional**: deleting one opts that changeset out of rollback, and validation doesn't warn about a missing one. | *Hand-written down files only* (Flyway style): exact intent, but most are never written, or are written wrong and never run. *Computed when rolling back*: needs git and both versions' state files at the worst possible moment. |
| What a down file contains | **Literal SQL.** It is a changeset of `script` deltas with the statements generated from the *baseline's* state files. It needs no state files when it runs, so the old and new manifests can both use it. | *Ordinary deltas* (`add column ...`): shorter, but they would read today's state files, where the dropped object no longer exists. |
| Running a rollback | **Explicit only: `skima rollback --to 1.0.3`**. It runs the down files of every changeset newer than 1.0.3, newest first, under the apply lock. **Each changeset's down runs in its own transaction**, as apply runs changesets. If one fails, the changesets already rolled back stay rolled back, and the database is left at a known version. `--dry-run` / `skima plan --rollback-to 1.0.3` shows the statements. | *Automatic in `apply`* when the manifest is older than the database: too easy to trigger by mistake, such as deploying a stale branch. *One transaction for the whole rollback*: changesets that opt out of transactions make that impossible anyway. |
| An older manifest during `apply` | **A no-op that says so.** It still exits 0, since nothing failed, but the message names the changesets the database has that the manifest doesn't, and asks whether a rollback was meant. See [below](#apply-with-an-older-manifest). | *Exit non-zero*: an older manifest isn't an error, and pipelines would fail for nothing. *An `--allow-rollback` flag*: rollback stays its own command. |
| Rolling back from an older manifest | **The database keeps the down SQL.** When apply applies a changeset, it records the changeset's down file in a new `down` column (`jsonb`, null when the changeset has none) on `skima_schema_history`. `skima rollback` with the 1.0.3 manifest uses the recorded down for 1.0.4. No one uses skima's history tables in production yet, so changing them isn't a breaking change. The table definition gains the column, and `CreateHistTables` adds it to existing tables with `alter table ... add column if not exists`. | *Only the manifest's files*: the redeploy-previous-artifact flow wouldn't work, and that is the common one. *A separate `skima_schema_rollbacks` table*: more moving parts for one value per run. |
| History | **A `rollback` run.** Each changeset rolled back is recorded as a run with apply type `rollback`, the version it went back to, and the down file. The deployed version is still the newest successful run, so it becomes 1.0.3. A later `apply` with the 1.0.4 manifest applies 1.0.4 again. | *Deleting or marking the 1.0.4 run*: the history would no longer say what happened. |
| Proof | **`changeset verify` checks both directions.** Baseline + up = current state, as today. Then current + down = baseline, built in the same scratch schema and transaction. A hand-edited down file is proven the same way. | Proving only up: an untested down is the main failure of down-migrations in practice. |

## What can and can't be reversed

Each generated down step is classified, and the classification is written into the down file:

| Class | Examples | Behaviour |
|---|---|---|
| **reversible** | add/drop index, constraint, fk, pk, trigger, comment, view, matview; rename column (rename back); alter column default or nullability; add/drop enum or sequence; re-creating a table or column the up dropped | Generated and run as is. A re-created table or column comes back **empty**, since its data went when the up ran, and the plan says so. |
| **lossy** | the down of *add table* or *add column*, which removes an object that may hold data written since the up | Handled according to `--lossy` (below). |
| **manual** | `script` deltas and pre/post hooks (arbitrary SQL); *alter enum* (postgres can't remove a value); a datatype change that may not convert back | No SQL is generated. The down file holds a placeholder that fails validation until it is written by hand or marked `"irreversible": true`. |

### Lossy steps: `--lossy`

`skima rollback --lossy=true|false` decides what a lossy step does, so the person rolling back makes the call:

- **`--lossy=false` (the default) keeps the data.** The table or column is renamed aside to `_skima_rolled_back_<name>`
  instead of being dropped. A renamed column loses its `NOT NULL` constraint and its default, so the older code, which
  doesn't know about it, can still insert rows. Foreign keys that reference a renamed table are dropped. `skima drift`
  reports these objects as extra. Re-applying the changeset doesn't reuse them, so clearing them out later is a
  deliberate step: drop them by hand, or roll back again with `--lossy=true`.
- **`--lossy=true` drops them**, and the data goes with them.

The dry run and `plan --rollback-to` show each lossy step and what it does under the chosen `--lossy`.

A changeset with no down, or whose down is marked irreversible, can't be rolled back past. `skima rollback --to` stops
before it and says why. The changesets rolled back before it stay rolled back, each in its own transaction.

## Apply with an older manifest

When the deployed version is newer than every changeset in the manifest, `apply` changes nothing and exits 0, as it does
today. The message changes to say why nothing was applied, and how to roll back if that was the intent:

```
The database is at 1.0.4, newer than this manifest's 1.0.3 - nothing was applied.
The database has changesets this manifest doesn't: 1.0.4
To roll the database back to this manifest's version: skima rollback --to 1.0.3
```

`--detailed-exitcode` still exits 0, since no changes are pending. A pipeline that redeploys the previous artifact
runs `skima rollback --to <its version>` as an explicit step.

## Workflow

1. **Build:** `skima changeset build` writes `deltas/v1_0_4.json` and `deltas/down/v1_0_4.json`, and lists any
   **lossy** or **manual** steps.
2. **Review:** fill in the manual steps, or mark them irreversible.
3. **Verify:** `skima changeset verify` proves both directions.
4. **Commit and deploy:** `skima apply` applies 1.0.4 and records its down SQL in the history.
5. **Incident:** `skima rollback --to 1.0.3`, with either the current manifest or the previous artifact's. Then
   `skima plan` with the 1.0.3 manifest shows nothing pending. `skima drift` shows no drift with `--lossy=true`; with
   `--lossy=false` it shows only the objects renamed aside.

## Scope

**First iteration**
- Generating down files in `changeset build`, for the deltas the diff generates
- The down file format and its JSON schema: a changeset of `script` deltas, plus a `class` on each step (and, for a
  lossy step, the SQL for both `--lossy` choices) and `irreversible`
- The `down` column on `skima_schema_history`, and recording the down SQL at apply time
- `skima rollback --to` with `--lossy`, its `--dry-run`, and `plan --rollback-to`
- The clearer message from `apply` with an older manifest
- Verify in both directions
- Gated postgres tests and a CI step: apply 1.0.1, then rollback to 1.0.0, with no drift against the 1.0.0 manifest

**Not in the first iteration**
- **Restoring data:** a lossy rollback stays lossy. Backups are the answer to that.
- **Rolling back the state build** or a `--baseline`: there is no "before" to go back to.
- **Down files for changesets that already exist.** `skima changeset down v1_0_2.json --baseline <ref>` could generate
  one later from git history.
- The state editor API and UI.

## Resolved questions

1. **An older manifest during `apply`:** rollback is explicit. `apply` stays a no-op and exits 0, and says why and how to
   roll back. See [Apply with an older manifest](#apply-with-an-older-manifest).
2. **Recording the down SQL:** a `down` column on `skima_schema_history`. No one uses the history tables yet, so this
   isn't a breaking change.
3. **Transactions:** each changeset's down runs in its own transaction.
4. **Are down files required?** No. They are optional, with no validation warning when one is missing.
5. **File location:** `deltas/down/v1_0_4.json`.
6. **Lossy steps:** the person rolling back decides with `--lossy=true|false`. `false`, the default, renames aside;
   `true` drops. A rollback without the flag runs with the default; it doesn't refuse.
7. **Recording runs:** one `rollback` run per changeset rolled back, so each run matches the transaction it ran in.
