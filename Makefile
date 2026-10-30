# Local multi-architecture build validation tool.
#
# Run `make help` for the available targets.

SHELL := /bin/bash

BINARY   := bin/konflux-test
PACKAGE  := ./cmd/konflux-test
GO       ?= go
GOFMT    ?= gofmt
GOLANGCI ?= golangci-lint

# Environment file used by the setup helper. When the file is absent the helper
# validates the cluster from exported environment variables only.
ENV_FILE ?= .konflux-test.env
PROVIDER ?=

.DEFAULT_GOAL := build

.PHONY: build test fmt fmt-check vet lint verify setup clean help

build: ## Build the CLI into bin/konflux-test
	$(GO) build -o $(BINARY) $(PACKAGE)

test: ## Run all test suites
	$(GO) test ./...

fmt: ## Format Go sources in place
	$(GO) fmt ./...

fmt-check: ## Fail when Go sources are not formatted
	@unformatted="$$($(GOFMT) -l .)"; \
	if [[ -n "$$unformatted" ]]; then \
		printf 'Unformatted files:\n%s\n' "$$unformatted" >&2; \
		exit 1; \
	fi

vet: ## Run go vet
	$(GO) vet ./...

lint: ## Run golangci-lint
	$(GOLANGCI) run ./...

verify: fmt-check vet test ## Check formatting, vet, and tests

setup: build ## Validate kubeconfig, cluster server guard, and prerequisites
	./scripts/run-local.sh --setup "$(ENV_FILE)" $(PROVIDER)

clean: ## Remove local build artifacts
	rm -rf bin

help: ## Print this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  %-12s %s\n", $$1, $$2}'
