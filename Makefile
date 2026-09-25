DB_URL      ?= postgres://postgres@localhost:55432/forgerail?sslmode=disable
TEST_DB_URL ?= postgres://postgres@localhost:55432/forgerail_test?sslmode=disable

PROTOC_GEN_GO_VERSION      := $(shell go list -m -f '{{.Version}}' google.golang.org/protobuf)
PROTOC_GEN_GO_GRPC_VERSION := v1.5.1

.PHONY: build test test-db db-up db-down lint tools proto

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

# protoc plugins are installed into ./bin so they match go.mod
tools:
	GOBIN=$(CURDIR)/bin go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	GOBIN=$(CURDIR)/bin go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)

proto: tools
	protoc -I proto \
		--plugin=protoc-gen-go=bin/protoc-gen-go \
		--plugin=protoc-gen-go-grpc=bin/protoc-gen-go-grpc \
		--go_out=gen --go_opt=paths=source_relative \
		--go-grpc_out=gen --go-grpc_opt=paths=source_relative \
		proto/forgerail/v1/*.proto
