#!/usr/bin/env bash
# Create VSC node accounts on the local Hive devnet (Tin Toy) using tnman.
# Ensures tnman exists and is funded, then creates magic-node1..N and writes
# identity configs to testnet/node1, node2, node3.
# Run this after launch-devnet.sh (or with hive-devnet on remote) when RPC is up.
set -euo pipefail

cd "$(dirname "$0")/.."
HIVE_API="${HIVE_API:-http://localhost:8090}"
NUM_NODES="${NUM_NODES:-3}"
OUTPUT="${OUTPUT:-testnet}"
SECRET="${TINTOY_SECRET:-tintoy}"
PYTHON="${PYTHON:-.venv/bin/python3}"
[[ ! -x "$PYTHON" ]] && PYTHON=python3

# Prefer prepared plain-text keys (testnet only)
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DEVNET_KEYS="$SCRIPT_DIR/devnet-keys.plain"
if [[ -f "$DEVNET_KEYS" ]]; then
  set -a
  while IFS= read -r line; do
    [[ "$line" =~ ^#.*$ || -z "${line// }" ]] && continue
    [[ "$line" == *=* ]] && export "$line"
  done < "$DEVNET_KEYS"
  set +a
fi

if ! curl -sf -X POST "$HIVE_API" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"condenser_api.get_dynamic_global_properties","params":[],"id":1}' >/dev/null 2>&1; then
  echo "ERROR: Hive RPC not reachable at $HIVE_API. Start devnet first: ./scripts/launch-devnet.sh" >&2
  exit 1
fi

# ---- Bootstrap tnman: create if missing, fund if balance too low ----
USE_DOCKER=false
if docker ps -q -f name="^hive-devnet$" 2>/dev/null | grep -q .; then
  USE_DOCKER=true
fi

if [[ "$USE_DOCKER" == true ]]; then
  echo "--- Ensuring tnman exists and is funded ---"
  # Use INITMINER_KEY from devnet-keys.plain if set; else fetch from container
  if [[ -z "${INITMINER_KEY:-}" ]]; then
    INITMINER_RAW=$(docker exec hive-devnet get_dev_key "$SECRET" wit-block-signing-0 2>/dev/null) || true
    if [[ -n "$INITMINER_RAW" ]]; then
      if command -v jq &>/dev/null; then
        export INITMINER_KEY=$(echo "$INITMINER_RAW" | jq -r 'if type == "array" then .[0].private_key else .private_key end')
      else
        export INITMINER_KEY=$(echo "$INITMINER_RAW" | python3 -c "import sys,json; d=json.load(sys.stdin); d=d[0] if isinstance(d,list) else d; print(d.get('private_key',''))" 2>/dev/null)
      fi
    fi
  fi
  # Use TNMAN_* from devnet-keys.plain if set; else fetch from container
  if [[ -z "${TNMAN_ACTIVE_KEY:-}" ]]; then
    TNMAN_RAW=$(docker exec hive-devnet get_dev_key "$SECRET" owner-tnman active-tnman posting-tnman memo-tnman 2>/dev/null) || true
    if [[ -n "$TNMAN_RAW" ]]; then
      if command -v jq &>/dev/null; then
        export TNMAN_OWNER_KEY=$(echo "$TNMAN_RAW" | jq -r '.[] | select(.account_name=="owner-tnman") | .private_key')
        export TNMAN_OWNER_PUBLIC=$(echo "$TNMAN_RAW" | jq -r '.[] | select(.account_name=="owner-tnman") | .public_key')
        export TNMAN_ACTIVE_KEY=$(echo "$TNMAN_RAW" | jq -r '.[] | select(.account_name=="active-tnman") | .private_key')
        export TNMAN_ACTIVE_PUBLIC=$(echo "$TNMAN_RAW" | jq -r '.[] | select(.account_name=="active-tnman") | .public_key')
        export TNMAN_POSTING_KEY=$(echo "$TNMAN_RAW" | jq -r '.[] | select(.account_name=="posting-tnman") | .private_key')
        export TNMAN_POSTING_PUBLIC=$(echo "$TNMAN_RAW" | jq -r '.[] | select(.account_name=="posting-tnman") | .public_key')
        export TNMAN_MEMO_KEY=$(echo "$TNMAN_RAW" | jq -r '.[] | select(.account_name=="memo-tnman") | .private_key')
        export TNMAN_MEMO_PUBLIC=$(echo "$TNMAN_RAW" | jq -r '.[] | select(.account_name=="memo-tnman") | .public_key')
      else
        eval "$(echo "$TNMAN_RAW" | python3 -c "
import sys,json
d=json.load(sys.stdin)
d=d if isinstance(d,list) else [d]
by_name={x['account_name']:x for x in d}
for role, aname in [('OWNER','owner-tnman'),('ACTIVE','active-tnman'),('POSTING','posting-tnman'),('MEMO','memo-tnman')]:
    x=by_name.get(aname,{})
    print('export TNMAN_'+role+'_KEY='+repr(x.get('private_key','')))
    print('export TNMAN_'+role+'_PUBLIC='+repr(x.get('public_key','')))" 2>/dev/null)"
      fi
    fi
  fi
  if [[ -n "${INITMINER_KEY:-}" ]]; then
    HIVE_API="$HIVE_API" "$PYTHON" scripts/devnet-ensure-tnman.py
  fi
else
  GET_DEV_KEY="${GET_DEV_KEY:-}"
  if [[ -z "$GET_DEV_KEY" && -d "${HIVE_SRC:-$HOME/Repos/hive}/build" ]]; then
    GET_DEV_KEY="${HIVE_SRC:-$HOME/Repos/hive}/build/programs/util/get_dev_key"
  fi
  if [[ -n "$GET_DEV_KEY" && -x "$GET_DEV_KEY" ]]; then
    echo "--- Ensuring tnman exists and is funded (GET_DEV_KEY) ---"
    if [[ -z "${INITMINER_KEY:-}" ]]; then
      INITMINER_RAW=$("$GET_DEV_KEY" "$SECRET" wit-block-signing-0 2>/dev/null) || true
      if [[ -n "$INITMINER_RAW" ]]; then
        if command -v jq &>/dev/null; then
          export INITMINER_KEY=$(echo "$INITMINER_RAW" | jq -r 'if type == "array" then .[0].private_key else .private_key end')
        else
          export INITMINER_KEY=$(echo "$INITMINER_RAW" | python3 -c "import sys,json; d=json.load(sys.stdin); d=d[0] if isinstance(d,list) else d; print(d.get('private_key',''))" 2>/dev/null)
        fi
      fi
    fi
    if [[ -n "${INITMINER_KEY:-}" ]]; then
      export GET_DEV_KEY TINTOY_SECRET="$SECRET"
      HIVE_API="$HIVE_API" "$PYTHON" scripts/devnet-ensure-tnman.py
    fi
  elif [[ -n "${INITMINER_KEY:-}" ]]; then
    echo "--- Ensuring tnman exists and is funded (keys from devnet-keys.plain) ---"
    HIVE_API="$HIVE_API" "$PYTHON" scripts/devnet-ensure-tnman.py
  fi
fi

# Get tnman active key: from devnet-keys.plain, or Docker, or local get_dev_key
if [[ -n "${TNMAN_ACTIVE_KEY:-}" ]]; then
  KEY="$TNMAN_ACTIVE_KEY"
else
  RAW=$(docker exec hive-devnet get_dev_key "$SECRET" active-tnman 2>/dev/null) || true
  if [[ -z "$RAW" ]]; then
    GET_DEV_KEY="${GET_DEV_KEY:-}"
    if [[ -z "$GET_DEV_KEY" && -d "${HIVE_SRC:-$HOME/Repos/hive}/build" ]]; then
      GET_DEV_KEY="${HIVE_SRC:-$HOME/Repos/hive}/build/programs/util/get_dev_key"
    fi
    if [[ -n "$GET_DEV_KEY" && -x "$GET_DEV_KEY" ]]; then
      RAW=$("$GET_DEV_KEY" "$SECRET" active-tnman 2>/dev/null) || true
    fi
  fi
  if [[ -z "$RAW" ]]; then
    echo "ERROR: Could not get tnman key. Add TNMAN_* to scripts/devnet-keys.plain (run ./scripts/get-devnet-keys.sh) or start hive-devnet." >&2
    exit 1
  fi
  if command -v jq &>/dev/null; then
    KEY=$(echo "$RAW" | jq -r 'if type == "array" then .[0].private_key else .private_key end')
  else
    KEY=$(echo "$RAW" | python3 -c "import sys,json; d=json.load(sys.stdin); d=d[0] if isinstance(d,list) else d; print(d.get('private_key',''))" 2>/dev/null)
  fi
fi
if [[ -z "${KEY:-}" || "$KEY" != 5* ]]; then
  echo "ERROR: Could not parse tnman active key" >&2
  exit 1
fi

echo "--- Creating $NUM_NODES node account(s) in $OUTPUT/ ---"
CREATOR=tnman MAGIC_MAN_KEY="$KEY" HIVE_API="$HIVE_API" NUM_NODES="$NUM_NODES" \
  "$PYTHON" scripts/testnet-multinode-setup.py \
  --nodes "$NUM_NODES" \
  --api "$HIVE_API" \
  --stake 1000 \
  --transfer 5000 \
  --output "$OUTPUT"

echo ""
echo "Done. For 3-node TSS stack:"
echo "  Local devnet:  docker compose -f docker-compose.devnet-3nodes.yml up -d"
echo "  Remote Hive:   HIVE_API=http://SERVER:8090 docker compose -f docker-compose.devnet-3nodes-remote.yml up -d"
echo "  (Configs are in $OUTPUT/node1, node2, node3)"
