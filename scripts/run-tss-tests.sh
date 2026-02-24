#!/usr/bin/env bash
# Run TSS-related tests (unit tests; integration tests need devnet + MongoDB).
#
# Usage:
#   ./scripts/run-tss-tests.sh           # unit tests only
#   ./scripts/run-tss-tests.sh --integration   # requires devnet + Mongo on 27017
set -euo pipefail

cd "$(dirname "$0")/.."

UNIT_ONLY=true
if [[ "${1:-}" == "--integration" ]]; then
  UNIT_ONLY=false
fi

echo "============================================================"
echo " TSS tests"
echo "============================================================"

echo ""
echo "--- Unit tests (modules/tss) ---"
go test ./modules/tss/ -v -count=1 -timeout 5m

if [[ "$UNIT_ONLY" == true ]]; then
  echo ""
  echo "Done. Integration tests are skipped (need devnet + MongoDB)."
  echo "To run with integration: ./scripts/run-tss-tests.sh --integration"
  echo "  (Start devnet first: ./scripts/launch-devnet.sh)"
  exit 0
fi

echo ""
echo "--- Integration tests (if Mongo reachable) ---"
go test ./modules/tss/ -v -count=1 -run TestVtss -timeout 2m 2>&1 || true
go test ./test/integration/ -v -count=1 -timeout 5m 2>&1 || true

echo ""
echo "Done."
