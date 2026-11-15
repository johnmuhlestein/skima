# Contributing to skima

## Setup
You need:
- **Go 1.25 or newer.** The lint target needs Go 1.26 or newer, because it builds golangci-lint.
- **Docker** for the tests that run against postgres.
- **git**, since the changeset commands and their tests read the state files' history.

Then:
```
make help
make build
./skima --version
```

[docs/architecture.md](docs/architecture.md) shows how the packages fit together.

## Tests
| Target | Runs |
|---|---|
| `make test` | every test that needs no database |
| `make test-race` | the same, with the race detector, as CI runs them |
| `make test-db` | every test, including those against postgres |
| `make cover` | the tests that need no database, with a coverage report in `coverage.html` |

`make test-db` starts a disposable postgres 17 container (`skima-test-pg`, on port 55432) if it isn't already running,
and leaves it running for the next run. `make pg-stop` removes it.

The database tests are in `internal/db`, `internal/verify` and `cmd`. They are skipped unless `SKIMA_VERIFY_DATABASE=1`
is set and `SKM_DBCONN_*` points at a user that can create roles and schemas. `make test-db` sets both. To run them
against another postgres:
```
SKIMA_VERIFY_DATABASE=1 SKM_DBCONN_CONNSTRING=localhost SKM_DBCONN_PORT=5432 SKM_DBCONN_DATABASE=postgres \
  SKM_DBCONN_SCHEMA=public SKM_DBCONN_USERNAME=postgres SKM_DBCONN_PASSWORD=... go test -count=1 ./...
```
Each test creates its own schemas (and roles), named `skima_db_*` and `skima_cmd_*`, and drops them afterwards.

The plan golden files in `internal/plan/testdata/golden` hold the exact SQL skima plans for the fixture manifest. If a change
to the generated DDL is intended, regenerate them with `go test ./internal/plan -update` and review the diff.

## Lint and format
- `make lint` runs golangci-lint at the version pinned in the Makefile (errcheck, govet, staticcheck, unused,
  ineffassign and gofmt).
- `make fmt` formats the code.
- CI fails on any lint issue or unformatted file.

## CI
Every pull request runs:
- gofmt, `go vet` and `go test -race`
- golangci-lint
- a Docker build
- an integration job against postgres 17, which runs the database tests and then a scripted apply, plan and drift of
  `testdata/ci` and `testdata/ci-next` (see `.github/workflows/ci.yml`)

## Conventions
- **Errors:** library code in `internal/` returns errors and never exits or reads configuration. `cmd` turns errors into
  messages and exit codes.
- **Context:** every database call takes a `context.Context`, so Ctrl+C can stop it.
- **Messages and help text:** describe what happened and what to do next, e.g. "nothing was applied - rerun once that
  apply finishes".
- **Documentation:** a user-visible change updates the README, and the JSON schemas in `schemas/` if it changes a
  manifest file.
