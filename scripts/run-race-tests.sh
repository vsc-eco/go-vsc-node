#!/bin/bash
TEST_RESULTS_DIR="$(dirname $0)/../test-results"
go install github.com/gotesttools/gotestfmt/v2/cmd/gotestfmt@latest
go test -json -v -coverprofile="$TEST_RESULTS_DIR/race-coverage.txt" $(go list ./... | grep -v /experiments/) 2>&1 | tee "$TEST_RESULTS_DIR/go-race-test.log" | ~/go/bin/gotestfmt 