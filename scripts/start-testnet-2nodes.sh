#!/usr/bin/env bash
# Run on the server: start 2 VSC testnet nodes (TSS stress test).
# Requires: code in current dir, data/config and data2/config (identityConfig.json in each).
# Usage: cd go-vsc-node && HIVE_API=https://testnet.techcoderx.com ./scripts/start-testnet-2nodes.sh

set -e
cd "$(dirname "$0")/.."

HIVE_API="${HIVE_API:-https://testnet.techcoderx.com}"
export HIVE_API

mkdir -p data/config data2/config

for dir in data data2; do
  cfg="$dir/config/identityConfig.json"
  if [[ ! -s "$cfg" ]]; then
    echo "Creating placeholder $cfg (replace with real HiveUsername + HiveActiveKey for this node)"
    cat > "$cfg" << 'IDENTITY'
{
  "BlsPrivKeySeed": "",
  "HiveActiveKey": "",
  "HiveUsername": "",
  "Libp2pPrivKey": ""
}
IDENTITY
  fi
done

if [[ ! -d data/chain ]]; then
  echo "Running init for node 1..."
  docker compose -f docker-compose.yml -f docker-compose.testnet.yml --profile init run --rm init
fi

echo "Starting 2-node stack (HIVE_API=$HIVE_API)..."
docker compose -f docker-compose.yml -f docker-compose.testnet.yml -f docker-compose.testnet-2nodes.yml up -d

docker compose -f docker-compose.yml -f docker-compose.testnet.yml -f docker-compose.testnet-2nodes.yml ps
