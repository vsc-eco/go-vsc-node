#!/usr/bin/env bash
# ============================================================================
# launch-testnet-multinode.sh
#
# One-command launcher for a 3-node VSC testnet talking to the techcoder
# Hive testnet.  Creates Hive accounts, generates identity configs, builds
# the Docker image, and brings up the stack.
#
# Usage:
#   export MAGIC_MAN_KEY="5Kxxx..."     # active key for magic-man on testnet
#   ./scripts/launch-testnet-multinode.sh
#
# Prefer clive for broadcasting (testnet uses TESTS/NAI): use
#   ./scripts/launch-testnet-multinode-clive.sh
# instead. Then keys can stay in .env; clive uses beekeeper (import key once).
#
# Optional env vars:
#   HIVE_API      Testnet API URL  (default: https://testnet.techcoderx.com)
#   NUM_NODES     Number of nodes  (default: 3)
#   STAKE         HIVE staked per node  (default: 1000)
#   TRANSFER      HIVE sent to each node account  (default: 5000)
#   SKIP_SETUP    Set to 1 to skip Python account setup (use existing configs)
#
# Note: techcoderx testnet requires RC (Resource Credits). If magic-man has 0 RC,
# power up some TESTS on the testnet first, or use launch-testnet-multinode-clive.sh
# with clive/beekeeper to sign (same RC requirement).
# ============================================================================
set -euo pipefail

cd "$(dirname "$0")/.."
PROJECT_DIR="$(pwd)"

# Load .env from project root or parent (vsc/) if MAGIC_MAN_KEY not set
if [[ -z "${MAGIC_MAN_KEY:-}" ]]; then
    for envfile in "$PROJECT_DIR/.env" "$PROJECT_DIR/../.env"; do
        if [[ -f "$envfile" ]]; then
            # .env may be VAR=value or raw key pairs. Try sourcing first.
            set +u
            set -a
            source "$envfile" 2>/dev/null || true
            set +a
            set -u
            # If still no MAGIC_MAN_KEY and file has raw WIFs (lines starting 5J or 5K),
            # use 2nd WIF as active key (Hive order: owner, active, posting, memo)
            if [[ -z "${MAGIC_MAN_KEY:-}" ]]; then
                wif=$(grep -E '^5[JK]' "$envfile" 2>/dev/null | sed -n '2p')
                [[ -n "$wif" ]] && export MAGIC_MAN_KEY="$wif"
            fi
            break
        fi
    done
fi

HIVE_API="${HIVE_API:-https://testnet.techcoderx.com}"
NUM_NODES="${NUM_NODES:-3}"
STAKE="${STAKE:-1000}"
TRANSFER="${TRANSFER:-5000}"
OUTPUT_DIR="testnet"
COMPOSE_FILE="docker-compose.testnet-multinode.yml"

echo "============================================================"
echo " VSC Multi-Node Testnet Launcher"
echo "============================================================"
echo "  API:       $HIVE_API"
echo "  Nodes:     $NUM_NODES"
echo "  Stake:     $STAKE HIVE per node"
echo "  Transfer:  $TRANSFER HIVE per node"
echo "  Output:    $OUTPUT_DIR/"
echo ""

# ---- Prerequisites ----
for cmd in docker python3; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "ERROR: '$cmd' is required but not found" >&2
        exit 1
    fi
done

if ! docker compose version &>/dev/null 2>&1; then
    echo "ERROR: 'docker compose' plugin is required" >&2
    exit 1
fi

# ---- Step 1: Hive account setup ----
if [[ "${SKIP_SETUP:-0}" != "1" ]]; then
    if [[ -z "${MAGIC_MAN_KEY:-}" ]]; then
        echo "ERROR: Set MAGIC_MAN_KEY to magic-man's active private key" >&2
        echo "  export MAGIC_MAN_KEY='5Kxxx...'" >&2
        exit 1
    fi

    echo "--- Step 1: Python (use venv if present) ---"
    PYTHON="python3"
    if [[ -f "$PROJECT_DIR/.venv/bin/python3" ]]; then
        PYTHON="$PROJECT_DIR/.venv/bin/python3"
    else
        pip3 install --quiet beem cryptography requests 2>/dev/null \
            || pip install --quiet beem cryptography requests
    fi

    echo ""
    echo "--- Step 2: Creating Hive accounts & generating configs ---"
    MAGIC_MAN_KEY="$MAGIC_MAN_KEY" \
    HIVE_API="$HIVE_API" \
    NUM_NODES="$NUM_NODES" \
    "$PYTHON" scripts/testnet-multinode-setup.py \
        --nodes "$NUM_NODES" \
        --api "$HIVE_API" \
        --stake "$STAKE" \
        --transfer "$TRANSFER" \
        --output "$OUTPUT_DIR"
else
    echo "--- Skipping account setup (SKIP_SETUP=1) ---"
    if [[ ! -d "$OUTPUT_DIR/node1/config" ]]; then
        echo "ERROR: $OUTPUT_DIR/node1/config not found. Run without SKIP_SETUP first." >&2
        exit 1
    fi
fi

# ---- Ensure container can read/write the data dirs ----
echo ""
echo "--- Fixing permissions on $OUTPUT_DIR/ ---"
chmod -R a+rw "$OUTPUT_DIR/" 2>/dev/null || true

# ---- Step 3: Build Docker image ----
echo ""
echo "--- Step 3: Building VSC node Docker image ---"
HIVE_API="$HIVE_API" docker compose -f "$COMPOSE_FILE" build

# ---- Step 4: Start the stack ----
echo ""
echo "--- Step 4: Starting $NUM_NODES-node testnet ---"
HIVE_API="$HIVE_API" docker compose -f "$COMPOSE_FILE" up -d

echo ""
echo "--- Status ---"
HIVE_API="$HIVE_API" docker compose -f "$COMPOSE_FILE" ps

echo ""
echo "============================================================"
echo " Multi-node testnet is running!"
echo "============================================================"
echo ""
for i in $(seq 1 "$NUM_NODES"); do
    gql_port=$((6599 + i))
    p2p_port=$((6609 + i))
    echo "  Node $i:  GraphQL http://localhost:$gql_port  |  P2P :$p2p_port"
done
echo ""
echo "  Logs:   docker compose -f $COMPOSE_FILE logs -f"
echo "  Stop:   docker compose -f $COMPOSE_FILE down"
echo "  Reset:  docker compose -f $COMPOSE_FILE down -v && rm -rf $OUTPUT_DIR/"
echo ""
echo "  Keys are in $OUTPUT_DIR/keys.json (keep safe!)"
echo ""
