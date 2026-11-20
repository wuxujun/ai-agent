-include .env
export

GO              ?= go
BIN             := bin/server
COMPOSE         := docker compose
DATABASE_URL    ?= postgres://wiki:wiki@localhost:5432/wiki?sslmode=disable
TEST_DATABASE_URL ?= $(DATABASE_URL)
MIGRATE_DB_URL  := postgres://$(or $(POSTGRES_USER),wiki):$(or $(POSTGRES_PASSWORD),wiki)@db:5432/$(or $(POSTGRES_DB),wiki)?sslmode=disable
MIGRATE_IMAGE   := migrate/migrate:v4.18.1
MIGRATE         := $(COMPOSE) run --rm migrate -path=/migrations -database=$(MIGRATE_DB_URL)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-18s %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the server binary into bin/
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BIN) ./cmd/server

.PHONY: run
run: ## Run the server on the host (needs `make db-up`)
	$(GO) run ./cmd/server

.PHONY: fmt
fmt: ## Format code
	gofmt -w .

.PHONY: lint
lint: ## gofmt check + go vet (incl. integration-tagged files)
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run make fmt" && exit 1)
	$(GO) vet ./...
	$(GO) vet -tags integration ./...

.PHONY: test
test: ## Run unit tests with the race detector
	$(GO) test -race -count=1 ./...

.PHONY: test-integration
test-integration: db-up ## Run pgvector + end-to-end integration tests against compose Postgres
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" $(GO) test -tags integration -race -count=1 ./...

.PHONY: cover
cover: ## Unit test coverage report
	$(GO) test -coverprofile=coverage.out ./... && $(GO) tool cover -func=coverage.out | tail -1

.PHONY: up
up: ## Start Postgres, run migrations, start the server (docker compose)
	$(COMPOSE) up -d --build

.PHONY: down
down: ## Stop all services (keeps data volume)
	$(COMPOSE) down

.PHONY: db-up
db-up: ## Start Postgres and apply migrations
	$(COMPOSE) up -d --wait db
	$(COMPOSE) run --rm migrate

.PHONY: db-reset
db-reset: ## Destroy the data volume and recreate the database
	$(COMPOSE) down -v
	$(MAKE) db-up

.PHONY: migrate-up
migrate-up: ## Apply all pending migrations
	$(MIGRATE) up

.PHONY: migrate-down
migrate-down: ## Roll back the most recent migration
	$(MIGRATE) down 1

.PHONY: migrate-version
migrate-version: ## Print the current migration version
	$(MIGRATE) version

.PHONY: migrate-create
migrate-create: ## Create a new migration: make migrate-create name=add_foo
	@test -n "$(name)" || (echo "usage: make migrate-create name=<name>" && exit 1)
	docker run --rm --user "$$(id -u):$$(id -g)" -v "$(CURDIR)/migrations:/migrations" $(MIGRATE_IMAGE) create -ext sql -dir /migrations -seq $(name)
