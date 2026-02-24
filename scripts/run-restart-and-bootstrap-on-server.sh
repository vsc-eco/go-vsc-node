#!/usr/bin/env bash
# Run on the SSH server: restart Hive devnet with known keypair (Tin Toy),
# then seed the network (tnman + magic-node1..3). Use after syncing scripts
# from local (sync-and-bootstrap-remote.sh or scp).
#
# Usage on server:
#   cd ~/vsc/go-vsc-node
#   ./scripts/run-restart-and-bootstrap-on-server.sh
#
# Prereqs: Docker, hive-devnet image (inertia/tintoy:latest). Compose file
# docker-compose.hive-devnet-only.yml can be present, or we use docker run.
set -euo pipefail

cd "$(dirname "$0")/.."
export HIVE_API="${HIVE_API:-http://127.0.0.1:8090}"

if [[ -f scripts/restart-devnet-with-known-keys.sh ]]; then
  echo "Using restart-devnet-with-known-keys.sh (fresh chain + seed)..."
  chmod +x scripts/*.sh 2>/dev/null || true
  COMPOSE=docker-compose.hive-devnet-only.yml ./scripts/restart-devnet-with-known-keys.sh --no-nodes
else
  echo "Restart script not found; doing manual restart + bootstrap..."
  docker rm -f hive-devnet 2>/dev/null || true
  if [[ -f docker-compose.hive-devnet-only.yml ]]; then
    docker compose -f docker-compose.hive-devnet-only.yml up -d hive-devnet
  else
    docker run -d --name hive-devnet -p 8090:8090 -p 8091:8091 -p 8080:8080 -p 2001:2001 --restart unless-stopped inertia/tintoy:latest
  fi
  echo "Waiting for Hive RPC..."
  for i in $(seq 1 30); do
    if curl -sf -X POST http://localhost:8090 -H "Content-Type: application/json" \
      -d '{"jsonrpc":"2.0","method":"condenser_api.get_dynamic_global_properties","params":[],"id":1}' >/dev/null 2>&1; then
      echo "  Hive RPC is up."
      break
    fi
    sleep 2
    echo "  attempt $i/30 ..."
  done
  ./scripts/run-devnet-bootstrap-on-server.sh
fi

echo ""
echo "Done. Devnet is restarted and seeded. From local:"
echo "  rsync -avz ubuntu@THIS_SERVER:vsc/go-vsc-node/testnet/ go-vsc-node/testnet/"
echo "  export HIVE_API=http://THIS_SERVER_IP:8090"
echo "  docker compose -f docker-compose.devnet-3nodes-remote.yml up -d"
echo ""
