#!/usr/bin/env bash
# Restart Hive devnet (Tin Toy) with a clean chain so the network is seeded
# with the known keypair (tintoy). Use before bootstrap to get deterministic
# initminer/tnman keys and a fresh chain.
#
# Usage:
#   ./scripts/restart-devnet-with-known-keys.sh [--no-nodes]
#
# Options:
#   --no-nodes   Only restart hive-devnet; do not bring up VSC nodes or DBs.
#
# Flow: stop stack -> remove hive-devnet container (fresh state) -> start
# hive-devnet -> wait for RPC -> run devnet-create-accounts.sh (seed tnman + nodes).
# =============================================================================
set -euo pipefail

cd "$(dirname "$0")/.."
# On SSH server often only hive-devnet-only.yml is present
if [[ -n "${COMPOSE:-}" ]]; then
  :
elif [[ -f docker-compose.devnet-3nodes.yml ]]; then
  COMPOSE=docker-compose.devnet-3nodes.yml
else
  COMPOSE=docker-compose.hive-devnet-only.yml
fi
NO_NODES=false
for arg in "$@"; do
  case "$arg" in
    --no-nodes) NO_NODES=true ;;
  esac
done

for cmd in docker; do
  if ! command -v "$cmd" &>/dev/null; then
    echo "ERROR: $cmd is required" >&2
    exit 1
  fi
done

echo "============================================================"
echo " Restart devnet with known keypair (Tin Toy)"
echo "============================================================"

echo ""
echo "--- Stopping devnet stack ---"
docker compose -f "$COMPOSE" down 2>/dev/null || true

echo ""
echo "--- Removing hive-devnet container for fresh chain ---"
docker rm -f hive-devnet 2>/dev/null || true

echo ""
echo "--- Starting Hive devnet (Tin Toy) ---"
docker compose -f "$COMPOSE" up -d hive-devnet

echo ""
echo "--- Waiting for Hive RPC (up to ~60s) ---"
for i in $(seq 1 30); do
  if curl -sf -X POST http://localhost:8090 \
    -H "Content-Type: application/json" \
    -d '{"jsonrpc":"2.0","method":"condenser_api.get_dynamic_global_properties","params":[],"id":1}' >/dev/null 2>&1; then
    echo "  Hive RPC is up."
    break
  fi
  sleep 2
  echo "  attempt $i/30 ..."
done

if ! curl -sf -X POST http://localhost:8090 \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"condenser_api.get_dynamic_global_properties","params":[],"id":1}' >/dev/null 2>&1; then
  echo "ERROR: Hive RPC did not become ready" >&2
  exit 1
fi

echo ""
echo "--- Seeding network (tnman + node accounts) ---"
./scripts/devnet-create-accounts.sh

if [[ "$NO_NODES" == true ]]; then
  echo ""
  echo "Done. Devnet is up and seeded (no VSC nodes started)."
  echo "  Start nodes: docker compose -f $COMPOSE up -d"
  exit 0
fi

echo ""
echo "--- Starting 3 VSC nodes + MongoDBs ---"
docker compose -f "$COMPOSE" up -d --build

echo ""
echo "============================================================"
echo " Devnet restarted with known keypair and seeded"
echo "============================================================"
echo "  Hive RPC:  http://localhost:8090"
echo "  Node1:     http://localhost:6600  Node2: 6601  Node3: 6602"
echo "  Logs:      docker compose -f $COMPOSE logs -f"
echo ""
