# skima development tasks - run make help for the list

GOLANGCI_LINT_VERSION := v2.15.0
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null)
LDFLAGS := -X github.com/johnmuhlestein/skima/cmd.version=$(VERSION)
OUT ?= skima

# the postgres the database tests run against: a disposable container on a port that won't clash with a local postgres
PG_CONTAINER := skima-test-pg
PG_PORT := 55432
PG_IMAGE := public.ecr.aws/docker/library/postgres:17-alpine
PG_ENV := SKIMA_VERIFY_DATABASE=1 SKM_DBCONN_CONNSTRING=localhost SKM_DBCONN_PORT=$(PG_PORT) SKM_DBCONN_DATABASE=postgres \
	SKM_DBCONN_SCHEMA=public SKM_DBCONN_USERNAME=postgres SKM_DBCONN_PASSWORD=pgtest

.PHONY: help build test test-race test-db pg-start pg-stop lint fmt cover

help: ## list the targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "} {printf "  %-10s %s\n", $$1, $$2}'

build: ## build ./skima (or OUT=path), with the version from git
	go build -ldflags "$(LDFLAGS)" -o $(OUT) .

test: ## run the tests that need no database
	go test ./...

test-race: ## run the tests with the race detector
	go test -race ./...

test-db: pg-start ## run every test, including those against postgres (started in docker)
	$(PG_ENV) go test -count=1 ./...

pg-start: ## start the test postgres container, if it isn't running
	@docker inspect -f '{{.State.Running}}' $(PG_CONTAINER) 2>/dev/null | grep -q true || \
		docker run -d --rm --name $(PG_CONTAINER) -e POSTGRES_PASSWORD=pgtest -p $(PG_PORT):5432 $(PG_IMAGE) >/dev/null
	@for i in $$(seq 1 30); do docker exec $(PG_CONTAINER) pg_isready -U postgres -q && exit 0; sleep 1; done; \
		echo "postgres did not start"; exit 1

pg-stop: ## stop the test postgres container
	-docker rm -f $(PG_CONTAINER) >/dev/null

lint: ## run golangci-lint (the pinned version, via go run)
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

fmt: ## format the code
	gofmt -w .

cover: ## write coverage.html for the tests that need no database
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
