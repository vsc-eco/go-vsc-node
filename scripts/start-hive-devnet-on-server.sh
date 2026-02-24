#!/usr/bin/env bash
# Start Hive devnet (Tin Toy) on this machine (e.g. SSH server).
# Use compose if docker-compose.hive-devnet-only.yml is present, else docker run.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

COMPOSE_FILE="docker-compose.hive-devnet-only.yml"
CONTAINER_NAME="hive-devnet"
IMAGE="inertia/tintoy:latest"

if [[ -f "$COMPOSE_FILE" ]]; then
  echo "Starting Hive devnet via docker compose ($COMPOSE_FILE) ..."
  docker compose -f "$COMPOSE_FILE" up -d
  echo "Hive devnet started. RPC: http://0.0.0.0:8090"
  exit 0
fi

# Fallback: no compose file (e.g. minimal clone). Run Tin Toy directly.
if docker ps -q -f name="^${CONTAINER_NAME}$" 2>/dev/null | grep -q .; then
  echo "Container $CONTAINER_NAME already running."
  exit 0
fi
if docker ps -aq -f name="^${CONTAINER_NAME}$" 2>/dev/null | grep -q .; then
  echo "Starting existing container $CONTAINER_NAME ..."
  docker start "$CONTAINER_NAME"
  exit 0
fi
echo "Starting Hive devnet (Tin Toy) with docker run ..."
docker run -d --name "$CONTAINER_NAME" \
  -p 8090:8090 -p 8091:8091 -p 8080:8080 -p 2001:2001 \
  --restart unless-stopped \
  "$IMAGE"
echo "Hive devnet started. RPC: http://0.0.0.0:8090"
