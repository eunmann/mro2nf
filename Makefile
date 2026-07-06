SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

BINS := mro2nf mre
PREFIX ?= $(HOME)/.local

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

# Tools run via `go run ...@pinned` to avoid global installs.
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
DEADCODE := go run golang.org/x/tools/cmd/deadcode@v0.47.0
SCC := go run github.com/boyter/scc/v3@v3.7.0

.PHONY: all build test cover test-e2e test-e2e-docker test-mrp-diff lint-nf bench vet lint lint-check deadcode cloc install clean help

# Minimum total statement coverage (cross-package) the cover gate accepts. Keep
# it a little under the current `make cover` total so refactors don't flap while
# a real regression still fails; ratchet it up as coverage grows.
COVER_MIN ?= 76
.DEFAULT_GOAL := help

help: ## Show this help
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-12s %s\n", $$1, $$2}'

build: ## Build mro2nf and mre
	@for b in $(BINS); do go build -ldflags "$(LDFLAGS)" -o $$b ./cmd/$$b; done

test: ## Run unit tests
	go test ./... 2>&1 | tee artifacts/test.log

# Cross-package attribution (-coverpkg) so e.g. types.OutFilename exercised from
# cmd/mre tests counts; scoped to ./cmd + ./internal (deploy/ has no Go).
cover: ## Unit-test coverage gate: fails below COVER_MIN% total statements
	@go test -coverprofile=artifacts/cover.out -coverpkg=./cmd/...,./internal/... ./cmd/... ./internal/... >/dev/null
	@go tool cover -func=artifacts/cover.out | tail -1
	@go tool cover -func=artifacts/cover.out | awk -v min="$(COVER_MIN)" \
		'END { gsub(/%/,"",$$3); if ($$3+0 < min+0) { printf "FAIL: total coverage %s%% is below COVER_MIN=%s%%\n", $$3, min; exit 1 } \
		printf "OK: total coverage %s%% >= %s%%\n", $$3, min }'

# The e2e suites live in the test/e2e Go package (build tag e2e). -count=1 is
# load-bearing: the test cache would otherwise return a cached ok without
# running Nextflow. Partitioned by -run/-skip so CI can parallelize the docker
# job; the mrp differential needs a local Martian (skips without one).
# Each parallel case is an idle-heavy `nextflow run` (JVM startup + tiny
# tasks), so oversubscribing cores is a win; the harness caps every JVM at
# -Xmx512m, bounding 10 concurrent runs to ~5 GB.
E2E_PARALLEL ?= 10
GO_E2E := go test -tags e2e -count=1 -parallel $(E2E_PARALLEL) -v ./test/e2e/

test-e2e: build ## Run the e2e suite (golden table, cloud-sim, failure paths, knobs)
	$(GO_E2E) -timeout 30m -skip '^TestDocker|^TestGenerated|^TestMrpDiff|^TestBench|^TestNextflowLint|^TestCellRanger'

test-e2e-docker: build ## Run pipelines under the Nextflow docker executor (cloud isolation)
	$(GO_E2E) -timeout 30m -run '^TestDocker|^TestGenerated'

test-mrp-diff: build ## Differential test vs real Martian mrp (set MARTIAN_BIN; default ~/workdir/martian/bin)
	$(GO_E2E) -timeout 30m -run '^TestMrpDiff'

lint-nf: build ## Static-lint generated Nextflow with `nextflow lint` (needs Nextflow >= 25.04)
	$(GO_E2E) -timeout 25m -run '^TestNextflowLint'

bench: build ## Benchmark data movement (bytes/objects/tasks); BENCH_UPDATE=1 records baseline
	$(GO_E2E) -timeout 20m -run '^TestBench'

test-cellranger: build ## Real-CellRanger differential baseline (opt-in; set CELLRANGER_HOME=<extracted bundle>)
	$(GO_E2E) -timeout 30m -run '^TestCellRanger'

vet: ## Run go vet
	go vet ./...

# --build-tags e2e pulls the tag-gated test/e2e harness into the lint set; the
# tag is additive (no !e2e constraints exist), so every other package lints
# exactly as before.
lint: ## Run linter with auto-fix
	$(GOLANGCI_LINT) run --fix --build-tags e2e ./... 2>&1 | tee artifacts/lint.log

lint-check: ## Run linter without auto-fix (for CI)
	$(GOLANGCI_LINT) run --build-tags e2e ./...

deadcode: ## Fail on functions unreachable from the mro2nf/mre binaries
	@out="$$($(DEADCODE) ./cmd/...)"; \
	if [ -n "$$out" ]; then echo "$$out"; echo "deadcode: unreachable functions found"; exit 1; fi

cloc: ## Count lines of code (respects .gitignore; excludes vendored deps)
	$(SCC) --exclude-dir vendor-martian .

install: build ## Install binaries into PREFIX/bin
	install -d $(PREFIX)/bin
	@for b in $(BINS); do install -m 0755 $$b $(PREFIX)/bin/$$b; done

clean: ## Remove built binaries
	rm -f $(BINS)
