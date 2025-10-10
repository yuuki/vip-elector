.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the binary
	go build -o vip-elector

.PHONY: test
test: ## Run unit tests
	go test -v -race ./... -tags='!e2e'

.PHONY: test-e2e
test-e2e: consul-up ## Run E2E tests (requires Consul via docker-compose)
	@echo "Waiting for Consul to be ready..."
	@sleep 3
	go test -v -race -tags=e2e ./e2e/... -timeout=3m
	@$(MAKE) consul-down

.PHONY: test-all
test-all: test test-e2e ## Run all tests (unit + E2E)

.PHONY: consul-up
consul-up: ## Start Consul for testing
	docker-compose up -d
	@echo "Waiting for Consul to be healthy..."
	@i=0; while [ $$i -lt 30 ]; do \
		if docker-compose exec -T consul consul members >/dev/null 2>&1; then \
			echo "Consul is ready"; \
			exit 0; \
		fi; \
		i=$$((i+1)); \
		sleep 1; \
	done; \
	echo "Consul failed to start within 30 seconds"; \
	docker-compose logs consul; \
	exit 1

.PHONY: consul-down
consul-down: ## Stop Consul
	docker-compose down -v

.PHONY: consul-logs
consul-logs: ## Show Consul logs
	docker-compose logs -f consul

.PHONY: clean
clean: ## Clean build artifacts and test binaries
	rm -f vip-elector
	rm -f e2e/vip-elector-test
	docker-compose down -v 2>/dev/null || true

.PHONY: lint
lint: ## Run linters
	go vet ./...
	go fmt ./...

.DEFAULT_GOAL := help
