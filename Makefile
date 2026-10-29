COMPOSE := docker compose -f deploy/docker-compose.yml
GO      ?= go

.PHONY: help up down logs ps build test test-race test-int lint fmt vet bench load chaos reconcile tidy clean

help: ## Show targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-12s %s\n", $$1, $$2}'

up: ## Build and start the full stack (Postgres, Kafka, 6 Go services, React UI, Prometheus, Grafana)
	$(COMPOSE) up -d --build --wait

down: ## Stop the stack (keeps volumes)
	$(COMPOSE) down

clean: ## Stop the stack and delete volumes
	$(COMPOSE) down -v

logs: ## Tail all service logs
	$(COMPOSE) logs -f --tail=100

ps: ## Show container status
	$(COMPOSE) ps

build: ## Build all Go binaries into ./bin
	$(GO) build -o bin/ ./cmd/...

test: ## Run all unit, property, and PostgreSQL integration tests
	$(GO) test -short ./...

test-race: ## Run tests with the race detector
	$(GO) test -race -short ./...

test-1m: ## Run the 1,000,000-order invariant suite
	$(GO) test -v ./internal/engine -run TestInvariants1M

bench: ## Matching engine microbenchmarks
	$(GO) test -run=^$$ -bench=. -benchmem ./internal/engine/...

load: ## Run loadgen engine + crash-recovery benchmark (or against live stack with GATEWAY=...)
	$(GO) run ./cmd/loadgen -engine-only -orders 100000

chaos: ## Kill matching-engine mid-stream with SIGKILL, restart, and reconcile ledger
	./scripts/chaos.sh

reconcile: ## Audit the double-entry ledger and reservations
	$(GO) run ./cmd/ofctl reconcile

lint: ## golangci-lint
	golangci-lint run ./...

fmt: ## gofmt
	gofmt -s -w .

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy
