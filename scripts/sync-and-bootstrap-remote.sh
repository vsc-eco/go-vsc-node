#!/usr/bin/env bash
# Run from local go-vsc-node: sync scripts to the SSH server and run devnet
# bootstrap there (ensure tnman + create magic-node1..3). Then rsync testnet/ back.
#
# Prereqs: SSH access to the server (e.g. ssh-copy-id first).
# Set SSH_HOST (and optionally SSH_USER) before running:
#   export SSH_HOST=157.180.53.238
#   export SSH_USER=ubuntu
#   ./scripts/sync-and-bootstrap-remote.sh
#
# Or: SSH_HOST=naf SSH_USER=ubuntu ./scripts/sync-and-bootstrap-remote.sh
set -euo pipefail

cd "$(dirname "$0")/.."
SSH_HOST="${SSH_HOST:?Set SSH_HOST (e.g. 157.180.53.238 or your server hostname)}"
SSH_USER="${SSH_USER:-ubuntu}"
REMOTE_DIR="${REMOTE_DIR:-vsc/go-vsc-node}"

echo "Syncing scripts to ${SSH_USER}@${SSH_HOST}:${REMOTE_DIR}/ ..."
scp -q scripts/devnet-ensure-tnman.py \
        scripts/devnet-create-accounts.sh \
        scripts/run-devnet-bootstrap-on-server.sh \
        scripts/testnet-multinode-setup.py \
        scripts/restart-devnet-with-known-keys.sh \
        scripts/run-restart-and-bootstrap-on-server.sh \
        "${SSH_USER}@${SSH_HOST}:${REMOTE_DIR}/scripts/"
scp -q docker-compose.hive-devnet-only.yml "${SSH_USER}@${SSH_HOST}:${REMOTE_DIR}/" 2>/dev/null || true

# Restart devnet with known keypair then bootstrap (recommended), or bootstrap only
USE_RESTART="${USE_RESTART:-1}"
if [[ "$USE_RESTART" == "1" ]]; then
  echo "Running restart devnet + bootstrap on server..."
  ssh "${SSH_USER}@${SSH_HOST}" "cd ${REMOTE_DIR} && chmod +x scripts/*.sh && ./scripts/run-restart-and-bootstrap-on-server.sh"
else
  echo "Running bootstrap on server (no restart)..."
  ssh "${SSH_USER}@${SSH_HOST}" "cd ${REMOTE_DIR} && chmod +x scripts/*.sh && ./scripts/run-devnet-bootstrap-on-server.sh"
fi

echo ""
echo "Pulling testnet/ back to local..."
rsync -avz "${SSH_USER}@${SSH_HOST}:${REMOTE_DIR}/testnet/" testnet/

echo ""
echo "Done. Start the 3-node stack with remote Hive:"
echo "  export HIVE_API=http://${SSH_HOST}:8090"
echo "  docker compose -f docker-compose.devnet-3nodes-remote.yml up -d"
echo ""
