DB_URL      ?= postgres://postgres@localhost:55432/forgerail?sslmode=disable
TEST_DB_URL ?= postgres://postgres@localhost:55432/forgerail_test?sslmode=disable

.PHONY: build test test-db db-up db-down lint

build:
	go build -o bin/ ./cmd/...

test:
	go test -race ./...

# runs the Postgres conformance tests too (needs `make db-up` first)
test-db:
	FORGERAIL_TEST_DATABASE_URL="$(TEST_DB_URL)" go test -race -count=1 ./...

db-up:
	./scripts/dev-postgres.sh start

db-down:
	./scripts/dev-postgres.sh stop

lint:
	gofmt -l . | tee /dev/stderr | (! read)
	go vet ./...
