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
test-e2e: dcs-up ## Run E2E tests (requires Consul and etcd via docker-compose)
	@echo "Waiting for Consul to be ready..."
	@sleep 3
	go test -v -race -tags=e2e ./e2e/... -timeout=10m
	@$(MAKE) dcs-down

.PHONY: test-all
test-all: test test-e2e ## Run all tests (unit + E2E)

.PHONY: dcs-up
dcs-up: consul-up etcd-up ## Start Consul and etcd for testing

.PHONY: dcs-down
dcs-down: ## Stop Consul and etcd
	docker-compose down -v

.PHONY: consul-up
consul-up: ## Start Consul for testing
	docker-compose up -d consul
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
	docker-compose stop consul

.PHONY: consul-logs
consul-logs: ## Show Consul logs
	docker-compose logs -f consul

.PHONY: etcd-up
etcd-up: ## Start etcd for testing
	docker-compose up -d etcd
	@echo "Waiting for etcd to be healthy..."
	@i=0; while [ $$i -lt 30 ]; do \
		if docker-compose exec -T etcd /usr/local/bin/etcdctl endpoint health >/dev/null 2>&1; then \
			echo "etcd is ready"; \
			exit 0; \
		fi; \
		i=$$((i+1)); \
		sleep 1; \
	done; \
	echo "etcd failed to start within 30 seconds"; \
	docker-compose logs etcd; \
	exit 1

.PHONY: etcd-down
etcd-down: ## Stop etcd
	docker-compose stop etcd

.PHONY: etcd-logs
etcd-logs: ## Show etcd logs
	docker-compose logs -f etcd

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
