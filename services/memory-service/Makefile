.PHONY: build test test-unit test-integration lint fmt up down clean

PG_CONTAINER   ?= cortex-memory-test
NATS_CONTAINER ?= cortex-nats-test
TEST_DSN       ?= postgres://postgres:postgres@localhost:5433/postgres
TEST_NATS      ?= nats://localhost:4223

build:
	go build ./...

# Unit tests only. Everything that needs Postgres or NATS skips itself, so this
# is what runs on a laptop and in a pre-commit hook.
test-unit:
	go test ./...

# The bi-temporal invariants live half in SQL — the partial unique index, the
# interval constraint, the row lock — so they are tested against real servers.
test-integration: up
	MEMORY_TEST_DSN='$(TEST_DSN)' MEMORY_TEST_GRAPH=1 MEMORY_TEST_NATS='$(TEST_NATS)' go test ./... -count=1

test: test-integration

up:
	@docker inspect $(PG_CONTAINER) >/dev/null 2>&1 || \
		docker run -d --name $(PG_CONTAINER) -p 5433:5432 -e POSTGRES_PASSWORD=postgres apache/age:latest
	@docker inspect $(NATS_CONTAINER) >/dev/null 2>&1 || \
		docker run -d --name $(NATS_CONTAINER) -p 4223:4222 nats:latest -js
	@for i in $$(seq 1 30); do \
		docker exec $(PG_CONTAINER) pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; \
	done

down:
	-docker rm -f $(PG_CONTAINER) $(NATS_CONTAINER)

fmt:
	gofmt -w .

lint:
	go vet ./...
	@test -z "$$(gofmt -l .)" || { echo "unformatted files:"; gofmt -l .; exit 1; }

clean: down
	go clean -testcache
