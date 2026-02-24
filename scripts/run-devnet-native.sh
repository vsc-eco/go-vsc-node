#!/usr/bin/env bash
# Run a local Hive devnet without Docker using hived built from ~/Repos/hive.
# Uses the same key derivation as Tin Toy (secret: tintoy) so tnman and node
# accounts match the Docker-based flow.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

HIVE_SRC="${HIVE_SRC:-$HOME/Repos/hive}"
HIVED_PATH="${HIVED_PATH:-}"
DATA_DIR="${DATA_DIR:-$REPO_ROOT/testnet/hived-data}"
HTTP_PORT="${HTTP_PORT:-8090}"
SECRET="${SECRET:-tintoy}"

# ---------------------------------------------------------------------------
# Resolve hived and get_dev_key binaries
# ---------------------------------------------------------------------------
get_dev_key_bin() {
  if [[ -n "${GET_DEV_KEY:-}" && -x "$GET_DEV_KEY" ]]; then
    echo "$GET_DEV_KEY"
    return
  fi
  local build_dir="$HIVE_SRC/build"
  local exe="$build_dir/programs/util/get_dev_key"
  if [[ -x "$exe" ]]; then
    echo "$exe"
    return
  fi
  echo ""
}

hived_bin() {
  if [[ -n "$HIVED_PATH" && -x "$HIVED_PATH" ]]; then
    echo "$HIVED_PATH"
    return
  fi
  local build_dir="$HIVE_SRC/build"
  local exe="$build_dir/programs/hived/hived"
  if [[ -x "$exe" ]]; then
    echo "$exe"
    return
  fi
  echo ""
}

# Build hived and get_dev_key in HIVE_SRC with BUILD_HIVE_TESTNET=ON
build_hive() {
  local build_dir="$HIVE_SRC/build"
  if [[ -x "$build_dir/programs/hived/hived" && -x "$build_dir/programs/util/get_dev_key" ]]; then
    echo "Using existing hived and get_dev_key from $build_dir"
    return 0
  fi
  echo "Building hived (testnet) and get_dev_key in $HIVE_SRC ..."
  echo "  (Requires: cmake, ninja, and deps; see Repos/hive/doc/building.md)"
  mkdir -p "$build_dir"
  ( cd "$build_dir" && cmake -DCMAKE_BUILD_TYPE=Release -DBUILD_HIVE_TESTNET=ON -GNinja "$HIVE_SRC" )
  ( cd "$build_dir" && ninja hived get_dev_key )
  if [[ ! -x "$build_dir/programs/hived/hived" || ! -x "$build_dir/programs/util/get_dev_key" ]]; then
    echo "ERROR: Build failed or binaries not found" >&2
    return 1
  fi
  echo "Build done."
}

# Get WIF private key from get_dev_key output (first key in JSON array)
get_wif() {
  local key_name="$1"
  local raw
  raw="$("$GET_DEV_KEY" "$SECRET" "$key_name" 2>/dev/null)" || true
  if [[ -z "$raw" ]]; then
    echo ""
    return
  fi
  if command -v jq &>/dev/null; then
    echo "$raw" | jq -r 'if type == "array" then .[0].private_key else .private_key end'
  else
    echo "$raw" | python3 -c "import sys,json; d=json.load(sys.stdin); d=d[0] if isinstance(d,list) else d; print(d.get('private_key',''))" 2>/dev/null
  fi
}

# ---------------------------------------------------------------------------
# Create datadir and config.ini
# ---------------------------------------------------------------------------
write_config() {
  local sk="$1"
  mkdir -p "$DATA_DIR/blockchain"
  cat > "$DATA_DIR/config.ini" << EOF
# Minimal devnet config (Tin Toy–compatible key derivation)
plugin = webserver p2p chain
plugin = database_api condenser_api network_broadcast_api block_api
plugin = witness

shared-file-dir = blockchain
shared-file-size = 1G

webserver-http-endpoint = 0.0.0.0:$HTTP_PORT
webserver-ws-endpoint = 0.0.0.0:$((HTTP_PORT+1))

enable-stale-production = true
required-participation = 0

witness = "initminer"
private-key = $sk
EOF
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
  if [[ ! -d "$HIVE_SRC" ]]; then
    echo "ERROR: Hive source not found at HIVE_SRC=$HIVE_SRC" >&2
    echo "  Clone with: git clone --recurse https://github.com/openhive-network/hive $HIVE_SRC" >&2
    exit 1
  fi

  build_hive

  GET_DEV_KEY="$(get_dev_key_bin)"
  HIVED="$(hived_bin)"
  if [[ -z "$GET_DEV_KEY" || -z "$HIVED" ]]; then
    echo "ERROR: get_dev_key or hived not found. Set GET_DEV_KEY and HIVED_PATH or build in $HIVE_SRC" >&2
    exit 1
  fi

  SK="$(get_wif "wit-block-signing-0")"
  if [[ -z "$SK" || "$SK" != 5* ]]; then
    echo "ERROR: Could not get witness key (wit-block-signing-0) from get_dev_key" >&2
    exit 1
  fi

  write_config "$SK"

  # Clean blockchain for a fresh genesis (optional; comment out to keep state)
  if [[ -d "$DATA_DIR/blockchain" ]]; then
    for f in blockchain/block_log blockchain/shared_memory.bin; do
      [[ -f "$DATA_DIR/$f" ]] && rm -f "$DATA_DIR/$f"
    done
  fi

  echo "Starting hived (data-dir=$DATA_DIR, RPC http://0.0.0.0:$HTTP_PORT) ..."
  echo "  Stop with: pkill -f 'hived.*$DATA_DIR' or Ctrl+C"
  echo "  Once RPC is up, in another terminal: create tnman (see docs/devnet.md), then ./scripts/devnet-create-accounts.sh"
  echo ""

  # Run in foreground so user can Ctrl+C; for background use: nohup "$HIVED" ...
  exec "$HIVED" -d "$DATA_DIR" --skeleton-key "$SK"
}

# If script is sourced, don't run main
case "${1:-}" in
  --write-config-only)
    build_hive
    GET_DEV_KEY="$(get_dev_key_bin)"
    HIVED="$(hived_bin)"
    SK="$(get_wif "wit-block-signing-0")"
    write_config "$SK"
    echo "Wrote $DATA_DIR/config.ini"
    exit 0
    ;;
  *)
    main
    ;;
esac

