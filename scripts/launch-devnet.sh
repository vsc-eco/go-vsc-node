#!/usr/bin/env bash
# =============================================================================
# Launch local Hive devnet (Tin Toy) + 3 VSC nodes for TSS testing.
#
# TSS (keygen/reshare) requires at least 3 nodes. No external network.
#
# Usage:
#   cd go-vsc-node
#   ./scripts/launch-devnet.sh
#
# Flow: start Hive RPC -> create 3 node accounts + gateway -> start 3 VSC nodes.
# =============================================================================
set -euo pipefail

cd "$(dirname "$0")/.."
COMPOSE="docker-compose.devnet-3nodes.yml"
NUM_NODES="${NUM_NODES:-3}"

echo "============================================================"
echo " VSC local devnet – 3 nodes (TSS)"
echo "============================================================"
echo "  Hive RPC:     http://localhost:8090"
echo "  Node1 GraphQL: http://localhost:6600  P2P: 6610"
echo "  Node2 GraphQL: http://localhost:6601  P2P: 6611"
echo "  Node3 GraphQL: http://localhost:6602  P2P: 6612"
echo "============================================================"
echo ""

for cmd in docker; do
  if ! command -v "$cmd" &>/dev/null; then
    echo "ERROR: $cmd is required" >&2
    exit 1
  fi
done

if ! docker compose version &>/dev/null 2>&1; then
  echo "ERROR: docker compose plugin is required" >&2
  exit 1
fi

mkdir -p testnet/node1/config testnet/node2/config testnet/node3/config
chmod -R a+rw testnet data 2>/dev/null || true

# 1) Start Hive devnet only so we can create accounts
echo "--- Step 1: Starting Hive devnet (Tin Toy) ---"
docker compose -f "$COMPOSE" up -d hive-devnet

echo ""
echo "--- Waiting for Hive RPC (may take ~30s) ---"
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

# 2) Create node accounts and gateway (tnman = devnet authority)
echo ""
echo "--- Step 2: Creating $NUM_NODES node accounts + gateway on devnet ---"
NUM_NODES="$NUM_NODES" ./scripts/devnet-create-accounts.sh

# 3) Start MongoDBs and VSC nodes
echo ""
echo "--- Step 3: Starting 3 VSC nodes + MongoDBs ---"
docker compose -f "$COMPOSE" up -d --build

echo ""
echo "============================================================"
echo " Devnet is running (3 nodes for TSS)"
echo "============================================================"
echo "  Hive RPC:      http://localhost:8090"
echo "  Node1:         http://localhost:6600  (GraphQL)"
echo "  Node2:         http://localhost:6601"
echo "  Node3:         http://localhost:6602"
echo ""
echo "  Logs:  docker compose -f $COMPOSE logs -f"
echo "  Stop:  docker compose -f $COMPOSE down"
echo ""
