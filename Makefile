SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

BINS := mart mre
PREFIX ?= $(HOME)/.local

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

# Tools run via `go run ...@pinned` to avoid global installs.
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2

.PHONY: all build test vet lint lint-check install clean help
.DEFAULT_GOAL := help

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-12s %s\n", $$1, $$2}'

build: ## Build mart and mre
	@for b in $(BINS); do go build -ldflags "$(LDFLAGS)" -o $$b ./cmd/$$b; done

test: ## Run unit tests
	go test ./... 2>&1 | tee artifacts/test.log

vet: ## Run go vet
	go vet ./...

lint: ## Run linter with auto-fix
	$(GOLANGCI_LINT) run --fix ./... 2>&1 | tee artifacts/lint.log

lint-check: ## Run linter without auto-fix (for CI)
	$(GOLANGCI_LINT) run ./...

install: build ## Install binaries into PREFIX/bin
	install -d $(PREFIX)/bin
	@for b in $(BINS); do install -m 0755 $$b $(PREFIX)/bin/$$b; done

clean: ## Remove built binaries
	rm -f $(BINS)
