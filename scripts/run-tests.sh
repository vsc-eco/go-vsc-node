#!/bin/bash
TEST_RESULTS_DIR="$(dirname $0)/../test-results"
go install github.com/gotesttools/gotestfmt/v2/cmd/gotestfmt@latest
go test -json -v -coverprofile="$TEST_RESULTS_DIR/coverage.txt" $(go list ./... | grep -v /experiments/) 2>&1 | tee "$TEST_RESULTS_DIR/gotest.log" | ~/go/bin/gotestfmt 