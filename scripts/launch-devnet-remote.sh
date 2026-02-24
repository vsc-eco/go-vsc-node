#!/usr/bin/env bash
# Launch 3-node VSC TSS testnet using a Hive devnet running elsewhere (e.g. SSH server).
#
# Prerequisite: Hive devnet must be running on the remote and reachable (port 8090).
# On the server you can run: ./scripts/start-hive-devnet-on-server.sh
#
# Usage (from go-vsc-node):
#   export HIVE_API=http://YOUR_SERVER:8090
#   # Optional: create tnman + node accounts (requires GET_DEV_KEY for remote devnet)
#   GET_DEV_KEY=/path/to/get_dev_key ./scripts/launch-devnet-remote.sh
#
# Or one-shot with account creation (need GET_DEV_KEY from local Hive build):
#   HIVE_API=http://naf:8090 GET_DEV_KEY=$HOME/Repos/hive/build/programs/util/get_dev_key ./scripts/launch-devnet-remote.sh
#
set -euo pipefail

cd "$(dirname "$0")/.."
COMPOSE="docker-compose.devnet-3nodes-remote.yml"
NUM_NODES="${NUM_NODES:-3}"

if [[ -z "${HIVE_API:-}" ]]; then
  echo "ERROR: Set HIVE_API to the remote Hive RPC URL (e.g. http://your-server:8090)" >&2
  exit 1
fi

echo "============================================================"
echo " VSC 3-node TSS testnet (remote Hive devnet)"
echo "============================================================"
echo "  Hive RPC:     $HIVE_API"
echo "  Node1 GraphQL: http://localhost:6600  P2P: 6610"
echo "  Node2 GraphQL: http://localhost:6601  P2P: 6611"
echo "  Node3 GraphQL: http://localhost:6602  P2P: 6612"
echo "============================================================"
echo ""

for cmd in docker curl; do
  if ! command -v "$cmd" &>/dev/null; then
    echo "ERROR: $cmd is required" >&2
    exit 1
  fi
done

# Wait for remote Hive RPC
echo "--- Waiting for Hive RPC at $HIVE_API ---"
for i in $(seq 1 30); do
  if curl -sf -X POST "$HIVE_API" \
    -H "Content-Type: application/json" \
    -d '{"jsonrpc":"2.0","method":"condenser_api.get_dynamic_global_properties","params":[],"id":1}' >/dev/null 2>&1; then
    echo "  Hive RPC is up."
    break
  fi
  sleep 2
  echo "  attempt $i/30 ..."
done

if ! curl -sf -X POST "$HIVE_API" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"condenser_api.get_dynamic_global_properties","params":[],"id":1}' >/dev/null 2>&1; then
  echo "ERROR: Hive RPC not reachable at $HIVE_API" >&2
  exit 1
fi

# Create node accounts if not already present
mkdir -p testnet/node1/config testnet/node2/config testnet/node3/config
if [[ ! -f testnet/node1/config/identityConfig.json ]]; then
  echo ""
  echo "--- Creating $NUM_NODES node accounts on remote devnet ---"
  echo "  (For remote devnet you need GET_DEV_KEY from a local Hive build, e.g. \$HOME/Repos/hive/build/programs/util/get_dev_key)"
  NUM_NODES="$NUM_NODES" ./scripts/devnet-create-accounts.sh
else
  echo "  Using existing testnet/node1,2,3 configs."
fi

# Start 3 VSC nodes + MongoDBs
echo ""
echo "--- Starting 3 VSC nodes + MongoDBs ---"
docker compose -f "$COMPOSE" up -d --build

echo ""
echo "============================================================"
echo " Devnet is running (3 nodes for TSS, remote Hive)"
echo "============================================================"
echo "  Hive RPC:      $HIVE_API"
echo "  Node1:         http://localhost:6600  (GraphQL)"
echo "  Node2:         http://localhost:6601"
echo "  Node3:         http://localhost:6602"
echo ""
echo "  Logs:  docker compose -f $COMPOSE logs -f"
echo "  Stop:  docker compose -f $COMPOSE down"
echo ""
