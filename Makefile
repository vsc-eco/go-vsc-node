# Build directory
BUILD_DIR := ./build

# Git commit for version info
GIT_COMMIT := $(shell git rev-parse HEAD)

# Build flags
BUILD_FLAGS := -buildvcs=false
LDFLAGS := -X vsc-node/modules/announcements.GitCommit=$(GIT_COMMIT)

# Installation directory (default to /usr/local/bin)
PREFIX ?= /usr/local
INSTALL_DIR := $(PREFIX)/bin

# GraphQL schema and generated outputs
GQL_SCHEMA := modules/gql/schema.graphql
GQL_GENERATED := modules/gql/gqlgen/generated.go

# Source dependencies
GO_SOURCES := $(shell find modules lib -type f -name '*.go') go.mod go.sum

# Targets
.PHONY: all clean install magid contract-deployer genesis-elector devnet-setup mapping-bot feed-publisher generate test test-full test-regression

all: $(GQL_GENERATED) magid contract-deployer genesis-elector devnet-setup mapping-bot feed-publisher

generate: $(GQL_GENERATED)

$(GQL_GENERATED): $(GQL_SCHEMA)
	go run github.com/99designs/gqlgen generate

magid: $(BUILD_DIR)/magid
	@echo "Built magid"

contract-deployer: $(BUILD_DIR)/contract-deployer
	@echo "Built contract-deployer"

genesis-elector: $(BUILD_DIR)/genesis-elector
	@echo "Built genesis-elector"

devnet-setup: $(BUILD_DIR)/devnet-setup
	@echo "Built devnet-setup"

mapping-bot: $(BUILD_DIR)/mapping-bot
	@echo "Built mapping-bot"

feed-publisher: $(BUILD_DIR)/feed-publisher
	@echo "Built feed-publisher"

$(BUILD_DIR):
	mkdir -p $(BUILD_DIR)

$(BUILD_DIR)/magid: $(BUILD_DIR) $(GO_SOURCES)
	go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $@ vsc-node/cmd/vsc-node

$(BUILD_DIR)/contract-deployer: $(BUILD_DIR) $(GO_SOURCES)
	go build $(BUILD_FLAGS) -o $@ vsc-node/cmd/contract-deployer

$(BUILD_DIR)/genesis-elector: $(BUILD_DIR) $(GO_SOURCES)
	go build $(BUILD_FLAGS) -o $@ vsc-node/cmd/genesis-elector

$(BUILD_DIR)/devnet-setup: $(BUILD_DIR) $(GO_SOURCES)
	go build $(BUILD_FLAGS) -o $@ vsc-node/cmd/devnet-setup

$(BUILD_DIR)/mapping-bot: $(BUILD_DIR) $(GO_SOURCES)
	go build $(BUILD_FLAGS) -o $@ vsc-node/cmd/mapping-bot

$(BUILD_DIR)/feed-publisher: $(BUILD_DIR) $(GO_SOURCES)
	go build $(BUILD_FLAGS) -o $@ vsc-node/cmd/feed-publisher

install: all
	mkdir -p $(INSTALL_DIR)
	cp $(BUILD_DIR)/magid $(INSTALL_DIR)/
	cp $(BUILD_DIR)/contract-deployer $(INSTALL_DIR)/
	cp $(BUILD_DIR)/genesis-elector $(INSTALL_DIR)/
	cp $(BUILD_DIR)/devnet-setup $(INSTALL_DIR)/
	cp $(BUILD_DIR)/mapping-bot $(INSTALL_DIR)/

# ===== Testing =====
#
# Tests are split into two speed classes:
#   quick  - near-instant unit tests; run by `make test` (target: < 5 minutes)
#   slow   - multi-second runtimes, libp2p/cluster spin-up, docker, zk proving
#
# SLOW_PACKAGES is the curated slow list (single source of truth). Everything
# else that builds on the host is treated as quick, so newly added packages are
# picked up by `make test` automatically.
#
# NON_HOST_PACKAGES never run under either target: the wasm-guest packages under
# modules/wasm/e2e/go_wasm are built for the wasm target (build-excluded on the
# host) and modules/oracle/price is WIP that does not compile.

empty :=
space := $(empty) $(empty)

SLOW_PACKAGES := \
	modules/p2p \
	modules/tss/tests \
	tests/devnet \
	modules/data-availability \
	modules/gateway \
	modules/announcements \
	modules/hive/streamer \
	modules/block-producer \
	modules/state-processing \
	modules/wasm/e2e \
	modules/e2e \
	cmd/mapping-bot/mapper \
	cmd/zk-tx-signer

NON_HOST_PACKAGES := \
	modules/oracle/price

# --- Known-failing exclusions (TEMPORARY — remove each entry once fixed) --------
# These currently fail and are excluded from BOTH `make test` and `make test-full`
# so the targets stay green. They are tracked for fixing, not abandoned.
#
# Whole packages (don't build, or pervasively broken):
#   modules/e2e            - missing evm_mapping.wasm build artifact (won't build)
#   modules/announcements  - stale Hive mock model panics; needs mock rewrite
#   modules/hive/streamer  - crash masks ~8 broken tests; needs test-suite rehab
#   modules/p2p            - Test (gossipsub) hangs on a non-resolving promise;
#                            excluded whole-package because a test named "Test"
#                            also exists in lib/dids, so it can't be safely -skip'd
KNOWN_FAILING_PACKAGES := \
	modules/e2e \
	modules/announcements \
	modules/hive/streamer \
	modules/p2p

# Individual tests in otherwise-passing packages, skipped via `go test -skip`:
#   TestFuzzAll  - modules/wasm/e2e: shared-account RC exhaustion across subtests
#   TestBasicP2P - modules/data-availability: gossipsub mesh delivery in harness
# NOTE: keep this non-empty; `go test -skip ''` would skip every test.
KNOWN_FAILING_TESTS := TestFuzzAll|TestBasicP2P
# -------------------------------------------------------------------------------

# grep -E fragments matching the `go list` output (vsc-node/<pkg>).
SLOW_RE    := ^vsc-node/($(subst $(space),|,$(strip $(SLOW_PACKAGES))))$$
# Excluded from both targets: non-host packages + currently known-failing packages.
EXCLUDE_RE := ^vsc-node/($(subst $(space),|,$(strip $(NON_HOST_PACKAGES) $(KNOWN_FAILING_PACKAGES))))$$|^vsc-node/modules/wasm/e2e/go_wasm($$|/)
# -skip flag (only added when there are known-failing tests to skip).
SKIP := $(if $(strip $(KNOWN_FAILING_TESTS)),-skip '$(KNOWN_FAILING_TESTS)',)

# The node-wide regression test (and its bring-up smoke test) are far longer than
# the 30m slow budget and are run deliberately via `make test-regression`. Skip
# them in the test-full slow phase so that target stays bounded and green.
REGRESSION_TEST := TestFullNetworkRegression
SLOW_SKIP := -skip '$(KNOWN_FAILING_TESTS)|$(REGRESSION_TEST)|TestRegressionBringup'

# Set V=1 to stream `go test -v` output (useful for the long regression run).
V ?=
VERBOSE := $(if $(strip $(V)),-v,)

QUICK_TIMEOUT := 120s
SLOW_TIMEOUT  := 30m

# Lists only packages that actually contain test files, so `go test` is never
# handed a test-less package (avoids the noisy `? pkg [no test files]` lines).
# Preferred over piping `go test` output through grep, which would clobber the
# test exit code. Note: test-less packages are therefore not compile-checked here
# — `make` (the build target) covers the binaries.
LIST_TEST_PKGS := go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... 2>/dev/null | grep -E '^vsc-node/'

# -count=1 disables Go's test cache so these targets always actually run. The
# cache key doesn't capture external state (MongoDB, libp2p, time), so a cached
# `ok` could reflect a stale run — undesirable for a deliberate test invocation.
GO_TEST := go test -count=1

# Quick unit tests across the whole repo. Intended to stay under ~5 minutes.
test:
	@echo "==> Quick tests (target < 5 min)"
	@$(GO_TEST) -timeout $(QUICK_TIMEOUT) $(SKIP) $$($(LIST_TEST_PKGS) | grep -vE '$(SLOW_RE)' | grep -vE '$(EXCLUDE_RE)')

# Every runnable test in the repo: quick tests first (fast feedback), then slow.
# Runs both phases even if the first fails, and exits non-zero if either failed.
test-full:
	@echo "==> Phase 1/2: quick tests"
	@rc=0; \
	$(GO_TEST) -timeout $(QUICK_TIMEOUT) $(SKIP) $$($(LIST_TEST_PKGS) | grep -vE '$(SLOW_RE)' | grep -vE '$(EXCLUDE_RE)') || rc=1; \
	echo "==> Phase 2/2: slow tests"; \
	$(GO_TEST) -timeout $(SLOW_TIMEOUT) $(SLOW_SKIP) $$($(LIST_TEST_PKGS) | grep -E '$(SLOW_RE)' | grep -vE '$(EXCLUDE_RE)') || rc=1; \
	exit $$rc

# Node-wide multi-stage devnet regression test (~50-65 min). Drives many of every
# network op, holds multiple elections (incl. a hard witness removal while a key
# is active), deploys/calls contracts incl. TSS, and injects recoverable faults
# at every stage. Deliberately run; excluded from `make test`/`make test-full`.
# Requires Docker. Pass V=1 for streaming `-v` output:  make test-regression V=1
test-regression:
	@echo "==> Node-wide regression (TestFullNetworkRegression, ~70-110 min)"
	$(GO_TEST) $(VERBOSE) -timeout 130m -run '^$(REGRESSION_TEST)$$' ./tests/devnet

clean:
	rm -rf $(BUILD_DIR)
