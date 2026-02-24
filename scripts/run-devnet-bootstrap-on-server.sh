#!/usr/bin/env bash
# Run on the SSH server where hive-devnet (Tin Toy) is running.
# Ensures tnman exists and is funded, creates magic-node1..N, writes testnet/node1,2,3.
# Then you can rsync testnet/ back to your local machine and start the 3-node stack there
# with HIVE_API pointing at this server.
set -euo pipefail

cd "$(dirname "$0")/.."
export HIVE_API="${HIVE_API:-http://127.0.0.1:8090}"

if ! docker ps -q -f name="^hive-devnet$" 2>/dev/null | grep -q .; then
  echo "ERROR: hive-devnet container not running. Start it first (e.g. scripts/start-hive-devnet-on-server.sh)" >&2
  exit 1
fi

# Ensure Python deps (beem, requests) for devnet-ensure-tnman.py and testnet-multinode-setup.py
if [[ -x .venv/bin/python3 ]]; then
  : # venv exists
elif command -v python3 &>/dev/null; then
  echo "Creating .venv and installing beem, requests, cryptography..."
  python3 -m venv .venv
  .venv/bin/pip install -q beem requests cryptography
else
  echo "ERROR: python3 required. Install and re-run." >&2
  exit 1
fi

./scripts/devnet-create-accounts.sh

echo ""
echo "To use from your local machine:"
echo "  1. Rsync testnet/ from this server to local go-vsc-node/testnet/"
echo "  2. export HIVE_API=http://THIS_SERVER_IP:8090"
echo "  3. docker compose -f docker-compose.devnet-3nodes-remote.yml up -d"
echo ""
