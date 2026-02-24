#!/usr/bin/env bash
# Write Tin Toy (tintoy) devnet keys to scripts/devnet-keys.plain.
# Run once with hive-devnet container up. Keys are deterministic (testnet only).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="$SCRIPT_DIR/devnet-keys.plain"
SECRET="${TINTOY_SECRET:-tintoy}"

if docker ps -q -f name="^hive-devnet$" 2>/dev/null | grep -q .; then
  docker exec hive-devnet get_dev_key "$SECRET" wit-block-signing-0 active-initminer owner-tnman active-tnman posting-tnman memo-tnman
else
  docker run --rm inertia/tintoy:latest get_dev_key "$SECRET" wit-block-signing-0 active-initminer owner-tnman active-tnman posting-tnman memo-tnman
fi | DEVNET_KEYS_OUT="$OUT" python3 -c "
import json, sys, os
d = json.load(sys.stdin)
if not isinstance(d, list):
    d = [d]
by_name = {x['account_name']: x for x in d}
out = []
w = by_name.get('wit-block-signing-0', {})
if w:
    out.append('INITMINER_KEY=' + w.get('private_key', ''))
for role, aname in [('OWNER', 'owner-tnman'), ('ACTIVE', 'active-tnman'), ('POSTING', 'posting-tnman'), ('MEMO', 'memo-tnman')]:
    x = by_name.get(aname, {})
    if x:
        out.append('TNMAN_' + role + '_KEY=' + x.get('private_key', ''))
        out.append('TNMAN_' + role + '_PUBLIC=' + x.get('public_key', ''))
path = os.environ['DEVNET_KEYS_OUT']
with open(path, 'w') as f:
    f.write('\n'.join(out) + '\n')
print('Wrote', path)
"