.DEFAULT_GOAL := help

BASE_URL  ?= http://localhost:8080
ACME_KEY  ?= acme-secret-key-0001
CUSTOMER  ?= 0x1111111111111111111111111111111111111111

.PHONY: help
help: ## Show available targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "} {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

## --- Docker ---

.PHONY: up
up: ## Build and start postgres + service in the background
	docker compose up -d --build

.PHONY: down
down: ## Stop containers, keep data
	docker compose down

.PHONY: reset
reset: ## Stop containers and drop the database volume
	docker compose down -v

.PHONY: logs
logs: ## Follow service logs
	docker compose logs -f app

.PHONY: psql
psql: ## Open psql in the postgres container
	docker compose exec postgres psql -U payments payments

## --- Go ---

.PHONY: build
build: ## Build the server binary into ./bin
	go build -o bin/server ./cmd/server

.PHONY: test
test: ## Run all tests (needs Docker for testcontainers)
	go test ./...

.PHONY: test-race
test-race: ## Run all tests with the race detector
	go test -race -count=1 ./...

.PHONY: lint
lint: ## gofmt check and go vet
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	go vet ./...

.PHONY: fmt
fmt: ## Format the code
	gofmt -w .

## --- Demo (service must be running: make up) ---

.PHONY: demo
demo: ## Happy path: invoice -> transfer -> 12 confirmations -> credit
	@set -e; \
	inv=$$(curl -sf -X POST $(BASE_URL)/v1/invoices -H "Authorization: Bearer $(ACME_KEY)" \
		-H "Idempotency-Key: demo-$$(date +%s)" -d '{"amount":"100"}'); \
	id=$$(echo "$$inv" | sed -E 's/.*"id":"([^"]+)".*/\1/'); \
	addr=$$(echo "$$inv" | sed -E 's/.*"deposit_address":"([^"]+)".*/\1/'); \
	echo "invoice $$id -> $$addr"; \
	curl -sf -X POST $(BASE_URL)/sim/transfers \
		-d "{\"from\":\"$(CUSTOMER)\",\"to\":\"$$addr\",\"amount\":\"100\"}" >/dev/null; \
	curl -sf -X POST $(BASE_URL)/sim/mine -d '{"blocks":11}' >/dev/null; \
	echo "waiting for watcher and AML..."; sleep 4; \
	curl -s $(BASE_URL)/v1/invoices/$$id -H "Authorization: Bearer $(ACME_KEY)"; echo; \
	curl -s $(BASE_URL)/v1/balance -H "Authorization: Bearer $(ACME_KEY)"; echo
