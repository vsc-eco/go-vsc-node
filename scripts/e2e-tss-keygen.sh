#!/usr/bin/env bash
# End-to-end test: restart devnet with known keypair, seed the network,
# then run TSS key generation integration test (TestVtss).
#
# TestVtss spawns 3 in-process nodes, runs KeyGen + KeyReshare, and
# requires MongoDB on 127.0.0.1:27017. This script starts a temporary
# Mongo if needed. The test seeds witness records (account -> peer_id) so
# TSS witness lookup succeeds; for a real 3-node testnet, ensure nodes
# have announced so the witnesses collection has entries at the block
# height used for TSS.
#
# Usage:
#   ./scripts/e2e-tss-keygen.sh
#   ./scripts/e2e-tss-keygen.sh --no-restart   # skip devnet restart; assume RPC + Mongo already up
#
# Requirements: docker, go, hive-devnet (or --no-restart with RPC + Mongo).
# =============================================================================
set -euo pipefail

cd "$(dirname "$0")/.."
COMPOSE="${COMPOSE:-docker-compose.devnet-3nodes.yml}"
NO_RESTART=false
STARTED_MONGO=false
MONGO_CONTAINER="vsc-e2e-mongo"

for arg in "$@"; do
  case "$arg" in
    --no-restart) NO_RESTART=true ;;
  esac
done

cleanup_mongo() {
  if [[ "$STARTED_MONGO" == true ]]; then
    echo ""
    echo "--- Stopping temporary MongoDB ---"
    docker rm -f "$MONGO_CONTAINER" 2>/dev/null || true
  fi
}
trap cleanup_mongo EXIT

echo "============================================================"
echo " E2E: TSS key generation (KeyGen + KeyReshare)"
echo "============================================================"

if [[ "$NO_RESTART" != true ]]; then
  echo ""
  echo "--- Restarting devnet with known keypair and seeding network ---"
  ./scripts/restart-devnet-with-known-keys.sh --no-nodes
else
  echo ""
  echo "--- Skipping devnet restart (--no-restart) ---"
  if ! curl -sf -X POST http://localhost:8090 \
    -H "Content-Type: application/json" \
    -d '{"jsonrpc":"2.0","method":"condenser_api.get_dynamic_global_properties","params":[],"id":1}' >/dev/null 2>&1; then
    echo "ERROR: Hive RPC not reachable at http://localhost:8090" >&2
    exit 1
  fi
  echo "  Hive RPC is up."
fi

echo ""
echo "--- Ensuring MongoDB on 127.0.0.1:27017 (required by TestVtss) ---"
mongo_port_open() {
  (command -v nc &>/dev/null && nc -z 127.0.0.1 27017 2>/dev/null) || \
  (command -v python3 &>/dev/null && python3 -c "import socket; s=socket.socket(); s.settimeout(1); s.connect(('127.0.0.1',27017)); s.close()" 2>/dev/null)
}
if ! mongo_port_open; then
  if docker ps -q -f name="^${MONGO_CONTAINER}$" 2>/dev/null | grep -q .; then
    echo "  Using existing container $MONGO_CONTAINER"
  else
    echo "  Starting temporary MongoDB (port 27017)..."
    docker run -d --name "$MONGO_CONTAINER" -p 127.0.0.1:27017:27017 mongo:8.0.4
    STARTED_MONGO=true
    sleep 3
  fi
else
  echo "  MongoDB already listening on 27017"
fi

echo ""
echo "--- Running TSS key generation integration test (TestVtss) ---"
if ! go test ./modules/tss/ -v -count=1 -run TestVtss -timeout 5m; then
  echo ""
  echo "FAILED: TSS key generation e2e test did not pass" >&2
  exit 1
fi

echo ""
echo "============================================================"
echo " E2E TSS key generation: PASSED"
echo "============================================================"
