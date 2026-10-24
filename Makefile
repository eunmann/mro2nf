SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

BINS := mro2nf mre
# The call-graph data plane runs py and comp stages through mrjob, built from
# the Martian module go.mod pins (its `tool` directive).
MRJOB_PKG := github.com/martian-lang/martian/cmd/mrjob
PREFIX ?= $(HOME)/.local

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

# Tools run via `go run ...@pinned` to avoid global installs.
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
DEADCODE := go run golang.org/x/tools/cmd/deadcode@v0.47.0
SCC := go run github.com/boyter/scc/v3@v3.7.0

.PHONY: all build test cover test-e2e test-e2e-docker test-mrp-diff gen-diff record-goldens lint-nf bench test-scale vet lint lint-check deadcode docs-divergences notices cloc install clean help

# Minimum total statement coverage (cross-package) the cover gate accepts. Keep
# it a little under the current `make cover` total so refactors don't flap while
# a real regression still fails; ratchet it up as coverage grows.
COVER_MIN ?= 87
.DEFAULT_GOAL := help

help: ## Show this help
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-12s %s\n", $$1, $$2}'

# Static (no cgo), so a binary runs on any Linux libc, including inside the
# runtime images; -trimpath keeps the builder's paths out of the binaries.
# release.yml, deploy/runtime/Dockerfile and the e2e harness build the same way
# (TestBuildRecipesStaticAndTrimmed, TestBinariesStaticAndPathFree).
build: ## Build mro2nf, mre and mrjob
	@for b in $(BINS); do CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $$b ./cmd/$$b; done
	@CGO_ENABLED=0 go build -trimpath -o mrjob $(MRJOB_PKG)

test: ## Run unit tests
	go test ./... 2>&1 | tee artifacts/test.log

# Cross-package attribution (-coverpkg) so e.g. internal/cg exercised from
# cmd/mre tests counts; scoped to ./cmd + ./internal (deploy/ has no Go).
# -count=1: a cached result for a package that does not import a -coverpkg
# package replays that package's blocks from an older source, so the merged
# profile mixes two versions of the file and the total collapses.
cover: ## Unit-test coverage gate: fails below COVER_MIN% total statements
	@go test -count=1 -coverprofile=artifacts/cover.out -coverpkg=./cmd/...,./internal/... ./cmd/... ./internal/... >/dev/null
	@go tool cover -func=artifacts/cover.out | tail -1
	@go tool cover -func=artifacts/cover.out | awk -v min="$(COVER_MIN)" \
		'END { gsub(/%/,"",$$3); if ($$3+0 < min+0) { printf "FAIL: total coverage %s%% is below COVER_MIN=%s%%\n", $$3, min; exit 1 } \
		printf "OK: total coverage %s%% >= %s%%\n", $$3, min }'

# The e2e suites live in the test/e2e Go package (build tag e2e). -count=1 is
# load-bearing: the test cache would otherwise return a cached ok without
# running Nextflow. Partitioned by -run/-skip so CI runs the docker suite as
# its own job. test-e2e includes the mrp differential, which needs a
# Martian build at MARTIAN_BIN: it skips without one, and fails under
# E2E_REQUIRE=1 (CI).
# Each parallel case is an idle-heavy `nextflow run` (JVM startup + tiny
# tasks), so oversubscribing cores is a win; the harness caps every JVM at
# -Xmx512m, bounding 10 concurrent runs to ~5 GB.
E2E_PARALLEL ?= 10
GO_E2E := go test -tags e2e -count=1 -parallel $(E2E_PARALLEL) -v ./test/e2e/

test-e2e: build ## Run the e2e suite (golden table, mrp differential, cloud-sim, failure paths, knobs)
	$(GO_E2E) -timeout 30m -skip '^TestDocker|^TestGenerated|^TestRecordGoldens|^TestBench|^TestNextflowLint|^TestCellRanger|^TestScale'

test-e2e-docker: build ## Run pipelines under the generated docker profile, each in an image built FROM the runtime image (cloud isolation)
	$(GO_E2E) -timeout 30m -run '^TestDocker|^TestGenerated'

test-mrp-diff: build ## Differential test vs real Martian mrp (set MARTIAN_BIN; default ~/workdir/martian/bin)
	$(GO_E2E) -timeout 30m -run '^TestMrpDiff'

# The long generative differential (#350). test-e2e runs TestGenDiff on the
# checked-in smoke seeds; this runs GEN_COUNT seeds from GEN_SEEDS (a number,
# "random", or a comma list), within GEN_TIMEOUT. The other GEN_* knobs
# (prebuilt binaries, artifact dir, shrink and time budgets) are documented in
# test/e2e/gendiff_test.go.
GEN_SEEDS ?= random
GEN_COUNT ?= 200
GEN_TIMEOUT ?= 180m
gen-diff: build ## Generative differential vs mrp (GEN_SEEDS=<start>|random|<a,b,...> GEN_COUNT=<n>)
	GEN_SEEDS='$(GEN_SEEDS)' GEN_COUNT='$(GEN_COUNT)' $(GO_E2E) -timeout $(GEN_TIMEOUT) -run '^TestGenDiff$$'

# The only writer of testdata/*/expected/outs/: each recordable golden fixture
# runs under real mrp and its published outs/ tree replaces the committed one.
record-goldens: ## Re-record expected/outs/ golden trees from real mrp (set MARTIAN_BIN; default ~/workdir/martian/bin)
	GOLDEN_RECORD=1 $(GO_E2E) -timeout 30m -run '^TestRecordGoldens$$'

lint-nf: build ## Static-lint generated Nextflow with `nextflow lint` (needs Nextflow >= 26.04)
	$(GO_E2E) -timeout 25m -run '^TestNextflowLint'

bench: build ## Benchmark data movement (bytes/objects/tasks); BENCH_UPDATE=1 records baseline
	$(GO_E2E) -timeout 20m -run '^TestBench'

# The nightly scale tier (#352): each TestScale<Shape> runs a bench shape at
# a narrow and a wide width (SCALE_WIDTH overrides every selected shape's)
# and gates the overhead rule. SCALE_RUN selects shapes; the nightly runs one
# per job. Metrics and verdicts land in SCALE_ARTIFACTS (artifacts/scale).
# The shapes run one after another, 100 minutes each at the default widths
# (scaleShapeTimeout), so the default SCALE_TIMEOUT fits all seven: the six
# bench shapes and TestScaleWideFanin.
SCALE_RUN ?= ^TestScale
SCALE_TIMEOUT ?= 700m
test-scale: build ## Nightly scale tier: bench shapes at production widths (SCALE_RUN=<-run regexp> SCALE_WIDTH=<n>)
	$(GO_E2E) -timeout $(SCALE_TIMEOUT) -run '$(SCALE_RUN)'

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

# make test fails while the checked-in table is stale (TestFeatureCoverageIsCurrent).
docs-divergences: ## Regenerate FEATURE_COVERAGE.md's divergence table from test/e2e/divergences.json
	go run ./test/divergence/gendoc

# make test fails while THIRD_PARTY_NOTICES is stale (TestNoticesAreCurrent).
notices: ## Regenerate THIRD_PARTY_NOTICES from the modules the release binaries link
	go run ./test/thirdparty/gennotices

cloc: ## Count lines of code (respects .gitignore; excludes vendored deps)
	$(SCC) --exclude-dir vendor-martian .

install: build ## Install binaries into PREFIX/bin
	install -d $(PREFIX)/bin
	@for b in $(BINS) mrjob; do install -m 0755 $$b $(PREFIX)/bin/$$b; done

clean: ## Remove built binaries
	rm -f $(BINS) mrjob
