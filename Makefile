SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

BINS := mro2nf mre
PREFIX ?= $(HOME)/.local

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

# Tools run via `go run ...@pinned` to avoid global installs.
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2

.PHONY: all build test test-e2e test-e2e-docker test-mrp-diff bench spike-13 vet lint lint-check install clean help
.DEFAULT_GOAL := help

help: ## Show this help
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-12s %s\n", $$1, $$2}'

build: ## Build mro2nf and mre
	@for b in $(BINS); do go build -ldflags "$(LDFLAGS)" -o $$b ./cmd/$$b; done

test: ## Run unit tests
	go test ./... 2>&1 | tee artifacts/test.log

test-e2e: build ## Run the end-to-end Nextflow differential test
	bash test/e2e/run.sh

test-e2e-docker: build ## Run pipelines under the Nextflow docker executor (cloud isolation)
	bash test/e2e/docker_iso.sh

test-mrp-diff: build ## Differential test vs real Martian mrp (set MARTIAN_BIN; default ~/workdir/martian/bin)
	bash test/e2e/mrp_diff.sh

bench: build ## Benchmark data movement (bytes/objects/tasks); BENCH_UPDATE=1 records baseline
	bash test/e2e/bench.sh

spike-13: build ## Validate the #13 de-bundle staging spike (local + S3-proxy)
	bash test/e2e/spike_debundle.sh

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
