COMPOSE := docker compose -f deploy/docker-compose.yml
GO      ?= go

.PHONY: help up down logs ps build test test-race test-int lint fmt vet bench tidy clean

help: ## Show targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-12s %s\n", $$1, $$2}'

up: ## Build and start the full stack, wait until healthy
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

test: ## Unit tests
	$(GO) test ./...

test-race: ## Unit tests with the race detector
	$(GO) test -race ./...

test-int: ## Integration tests (requires Docker)
	$(GO) test -tags=integration -count=1 -timeout 10m ./...

bench: ## Matching engine benchmarks
	$(GO) test -run=^$$ -bench=. -benchmem ./internal/engine/...

lint: ## golangci-lint
	golangci-lint run ./...

fmt: ## gofmt
	gofmt -s -w .

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

