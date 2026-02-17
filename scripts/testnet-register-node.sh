#!/usr/bin/env bash
# Generate vsc.consensus_stake + transfer operations to register a VSC node on testnet.
# Use with clive or any Hive RPC client. Keys: use clive key store or source from .env (do not commit keys).
#
# Usage:
#   ./scripts/testnet-register-node.sh <from_account> <to_node_account> [amount_hive] [gateway]
#
# Example:
#   ./scripts/testnet-register-node.sh milo-hpr milo.magi 18000
#   # Then broadcast the generated JSON with clive (active key of from_account).

set -e
FROM_ACCOUNT="${1:?Usage: $0 <from_account> <to_node_account> [amount_hive] [gateway]}"
TO_ACCOUNT="${2:?Usage: $0 <from_account> <to_node_account> [amount_hive] [gateway]}"
AMOUNT_HIVE="${3:-18000.000}"
GATEWAY="${4:-vsc.gateway}"
NET_ID="vsc-testnet"

# Amount in legacy format (3 decimals, integer: 18000.000 -> 18000000)
AMOUNT_LEGACY=$(echo "$AMOUNT_HIVE" | sed 's/\.//g' | tr -d ' ')
[ -z "$AMOUNT_LEGACY" ] && AMOUNT_LEGACY=0
# Pad to at least 3 digits (e.g. 1 -> 1000)
while [ "${#AMOUNT_LEGACY}" -lt 3 ]; do
  AMOUNT_LEGACY="${AMOUNT_LEGACY}0"
done

CONSENSUS_JSON=$(jq -n \
  --arg from "hive:$FROM_ACCOUNT" \
  --arg to "hive:$TO_ACCOUNT" \
  --arg amount "$AMOUNT_HIVE" \
  --arg net_id "$NET_ID" \
  '{from: $from, to: $to, asset: "hive", net_id: $net_id, amount: $amount}')

OPS_JSON=$(jq -n \
  --arg from "$FROM_ACCOUNT" \
  --arg to "$GATEWAY" \
  --arg amount "$AMOUNT_LEGACY" \
  --argjson consensus "$CONSENSUS_JSON" \
  '[
    {
      type: "transfer_operation",
      value: {
        amount: { amount: $amount, nai: "@@000000021", precision: 3 },
        from: $from,
        memo: "",
        to: $to
      }
    },
    {
      type: "custom_json_operation",
      value: {
        id: "vsc.consensus_stake",
        json: ($consensus | tostring),
        required_auths: [$from],
        required_posting_auths: []
      }
    }
  ]')

OUTFILE="${OUTFILE:-/tmp/vsc-register-ops.json}"
echo "$OPS_JSON" | jq . > "$OUTFILE"
echo "Operations written to $OUTFILE (broadcast with clive using active key of $FROM_ACCOUNT):"
cat "$OUTFILE"
echo ""
echo "Broadcast with clive: load active key for $FROM_ACCOUNT, then broadcast the transaction with the operations in $OUTFILE."
