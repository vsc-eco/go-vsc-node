#!/usr/bin/env bash
# ============================================================================
# launch-testnet-multinode-clive.sh
#
# Multi-node VSC testnet using **clive** to sign and broadcast all Hive
# transactions (account create, transfer, stake). Uses NAI format so testnet
# accepts amounts; no beem for broadcasting.
#
# Prerequisites:
#   - clive installed and on PATH (or use docker: alias clive='docker run -i --rm -v $(pwd)/testnet:/data hiveio/clive:v1.27.5.13')
#   - Beekeeper running (clive beekeeper spawn) and magic-man active key
#     imported (e.g. alias "magic-man")
#   - Optional: .env in project root or parent with BEEKEEPER_PASSWORD and
#     CLIVE_SIGN_ALIAS=magic-man
#
# Usage:
#   ./scripts/launch-testnet-multinode-clive.sh
#
# Env:
#   HIVE_API             Testnet API (default: https://testnet.techcoderx.com)
#   NUM_NODES            Number of nodes (default: 3)
#   BEEKEEPER_PASSWORD   Beekeeper password for clive (avoid prompt)
#   CLIVE_SIGN_ALIAS     Key alias in beekeeper (default: magic-man)
#   SKIP_SETUP           Skip tx generation (use existing testnet/*.json)
#   SKIP_CLIVE           Skip broadcasting (only generate tx files + configs)
# ============================================================================
set -euo pipefail

cd "$(dirname "$0")/.."
PROJECT_DIR="$(pwd)"
OUTPUT_DIR="${OUTPUT_DIR:-testnet}"
COMPOSE_FILE="docker-compose.testnet-multinode.yml"
HIVE_API="${HIVE_API:-https://testnet.techcoderx.com}"
NUM_NODES="${NUM_NODES:-3}"
STAKE="${STAKE:-1000}"
TRANSFER="${TRANSFER:-5000}"
CLIVE_SIGN_ALIAS="${CLIVE_SIGN_ALIAS:-magic-man}"
GATEWAY="vsc.testnet"
NODE_PREFIX="magic-node"

# Load .env from project or parent
for envfile in "$PROJECT_DIR/.env" "$PROJECT_DIR/../.env"; do
  if [[ -f "$envfile" ]]; then
    set +u
    set -a
    source "$envfile" 2>/dev/null || true
    set +a
    set -u
    break
  fi
done

echo "============================================================"
echo " VSC Multi-Node Testnet (clive)"
echo "============================================================"
echo "  API:       $HIVE_API"
echo "  Nodes:     $NUM_NODES"
echo "  Output:    $OUTPUT_DIR/"
echo "  Sign:      $CLIVE_SIGN_ALIAS"
echo ""

# ---- Generate tx files and configs ----
if [[ "${SKIP_SETUP:-0}" != "1" ]]; then
  echo "--- Generating tx files and identity configs ---"
  if command -v python3 &>/dev/null; then
    HIVE_API="$HIVE_API" NUM_NODES="$NUM_NODES" python3 scripts/testnet-multinode-setup-clive.py \
      --api "$HIVE_API" --nodes "$NUM_NODES" --stake "$STAKE" --transfer "$TRANSFER" --output "$OUTPUT_DIR"
  else
    echo "ERROR: python3 required to generate tx files" >&2
    exit 1
  fi
else
  echo "--- Skipping setup (SKIP_SETUP=1) ---"
fi

if [[ ! -d "$OUTPUT_DIR" ]]; then
  echo "ERROR: $OUTPUT_DIR not found" >&2
  exit 1
fi

# ---- Broadcast with clive ----
if [[ "${SKIP_CLIVE:-0}" != "1" ]]; then
  if ! command -v clive &>/dev/null; then
    echo "WARNING: clive not on PATH. Use Docker: docker run -it --rm -v $(pwd)/$OUTPUT_DIR:/data hiveio/clive:v1.27.5.13" >&2
    echo "  Then run: clive configure node set $HIVE_API" >&2
    echo "  And:     clive configure chain-id set 18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e" >&2
    echo "  Then for each tx: clive process transaction --from-file /data/tx-*.json --sign $CLIVE_SIGN_ALIAS --password <pwd>" >&2
    echo "" >&2
    echo "Skipping broadcast. Tx files are in $OUTPUT_DIR/" >&2
  else
    echo "--- Configuring clive for testnet ---"
    clive configure node set "$HIVE_API" 2>/dev/null || true
    clive configure chain-id set "18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e" 2>/dev/null || true

    echo "--- Broadcasting transactions with clive ---"
    # Create and transfer: sign with magic-man. Stake: sign with node account (magic-node1, etc.)
    for f in "$OUTPUT_DIR"/tx-create-"$GATEWAY".json "$OUTPUT_DIR"/tx-create-$NODE_PREFIX*.json \
             "$OUTPUT_DIR"/tx-transfer-"$NODE_PREFIX"*.json; do
      [[ -f "$f" ]] || continue
      echo "  Broadcasting $f (sign: $CLIVE_SIGN_ALIAS) ..."
      if [[ -n "${BEEKEEPER_PASSWORD:-}" ]]; then
        clive process transaction --from-file "$f" --sign "$CLIVE_SIGN_ALIAS" --password "$BEEKEEPER_PASSWORD" || true
      else
        clive process transaction --from-file "$f" --sign "$CLIVE_SIGN_ALIAS" || true
      fi
      sleep 3
    done
    for f in "$OUTPUT_DIR"/tx-stake-"$NODE_PREFIX"*.json; do
      [[ -f "$f" ]] || continue
      # Stake tx is signed by the node (e.g. magic-node1); alias should match account name
      sign_alias=$(basename "$f" .json | sed "s/tx-stake-//")
      echo "  Broadcasting $f (sign: $sign_alias) ..."
      if [[ -n "${BEEKEEPER_PASSWORD:-}" ]]; then
        clive process transaction --from-file "$f" --sign "$sign_alias" --password "$BEEKEEPER_PASSWORD" || true
      else
        clive process transaction --from-file "$f" --sign "$sign_alias" || true
      fi
      sleep 3
    done
  fi
else
  echo "--- Skipping clive broadcast (SKIP_CLIVE=1) ---"
fi

# ---- Permissions and Docker ----
echo ""
echo "--- Fixing permissions ---"
chmod -R a+rw "$OUTPUT_DIR/" 2>/dev/null || true

echo "--- Building and starting stack ---"
HIVE_API="$HIVE_API" docker compose -f "$COMPOSE_FILE" build 2>/dev/null || true
HIVE_API="$HIVE_API" docker compose -f "$COMPOSE_FILE" up -d

echo ""
HIVE_API="$HIVE_API" docker compose -f "$COMPOSE_FILE" ps 2>/dev/null || true
echo ""
echo "Done. Nodes: GraphQL 6600–6602, P2P 6610–6612"
echo "  Logs: docker compose -f $COMPOSE_FILE logs -f"
echo "  Keys: $OUTPUT_DIR/keys.json (keep safe)"
echo ""
