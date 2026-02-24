#!/usr/bin/env python3
"""
VSC Testnet Multi-Node Setup — clive path

Generates Hive key pairs and builds **unsigned transaction JSON files** in NAI
format (no HIVE/TESTS symbol), plus VSC identity configs. You then broadcast
with clive: clive process transaction --from-file <file> --sign <alias> --password <pwd>

Requires: cryptography, requests, beem (beem only for Hive key derivation).

Usage:
  python3 scripts/testnet-multinode-setup-clive.py --output testnet [--nodes 3]
  # Then for each testnet/tx-*.json: clive process transaction --from-file testnet/tx-*.json --sign magic-man --password ...
"""

import os
import sys
import json
import secrets
import argparse
import time
from pathlib import Path

try:
    import requests
except ImportError:
    sys.exit("ERROR: pip install requests")

try:
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
    from cryptography.hazmat.primitives.serialization import (
        Encoding, PrivateFormat, PublicFormat, NoEncryption,
    )
except ImportError:
    sys.exit("ERROR: pip install cryptography")

# Hive testnet NAI (same as mainnet HIVE)
NAI_HIVE = "@@000000021"
CREATOR = "magic-man"
NODE_PREFIX = "magic-node"
GATEWAY = "vsc.testnet"
NET_ID = "vsc-testnet"
DEFAULT_API = "https://testnet.techcoderx.com"

# Op IDs (condenser_api)
OP_TRANSFER = 2
OP_ACCOUNT_CREATE = 9
OP_CUSTOM_JSON = 18


def get_ref_block(api_url):
    """Fetch ref_block_num, ref_block_prefix, expiration from API."""
    r = requests.post(
        api_url,
        json={"jsonrpc": "2.0", "method": "condenser_api.get_dynamic_global_properties", "params": [], "id": 1},
        timeout=15,
    )
    r.raise_for_status()
    data = r.json().get("result", {})
    head = int(data.get("head_block_number", 0))
    head_id = data.get("head_block_id", "")
    ts = data.get("time", "")
    # ref_block_num = lower 16 bits
    ref_block_num = head & 0xFFFF
    # ref_block_prefix = bytes 4-8 of block_id (little-endian uint32)
    import binascii
    try:
        bid = binascii.unhexlify(head_id)
        ref_block_prefix = int.from_bytes(bid[4:8], "little")
    except Exception:
        ref_block_prefix = 0
    # expiration = time + 30s
    from datetime import datetime, timedelta
    try:
        t = datetime.strptime(ts.replace("Z", "").split(".")[0], "%Y-%m-%dT%H:%M:%S") + timedelta(seconds=30)
        expiration = t.strftime("%Y-%m-%dT%H:%M:%S")
    except Exception:
        expiration = "2030-01-01T00:00:30"
    return ref_block_num, ref_block_prefix, expiration


def amount_nai(amount_str):
    """'1000.000' -> {amount: '1000000', nai: NAI_HIVE, precision: 3} (3 decimals -> integer)"""
    s = amount_str.strip().split(".")
    whole = (s[0] or "0").replace(" ", "")
    frac = (s[1] if len(s) > 1 else "").ljust(3, "0")[:3]
    amount_int = int(whole) * 1000 + int(frac)
    return {"amount": str(amount_int), "nai": NAI_HIVE, "precision": 3}


def authority(key_auths):
    """key_auths = [(pubkey, weight), ...] -> authority object."""
    return {
        "weight_threshold": 1,
        "account_auths": [],
        "key_auths": [[k, 1] for k in key_auths],
    }


def make_tx(api_url, operations):
    """Build unsigned transaction (operations in condenser format)."""
    ref_block_num, ref_block_prefix, expiration = get_ref_block(api_url)
    # Condenser format: operations as array of [op_id, value]
    ops_condenser = []
    for op in operations:
        op_id = op["id"]
        value = op["value"]
        ops_condenser.append([op_id, value])
    return {
        "ref_block_num": ref_block_num,
        "ref_block_prefix": ref_block_prefix,
        "expiration": expiration,
        "operations": ops_condenser,
        "extensions": [],
        "signatures": [],
    }


# Generate Hive key pairs (owner, active, posting, memo) using same derivation as beem
def generate_hive_keys(account_name):
    """Generate keys for a Hive account. Returns dict with owner/active/posting/memo {private, public}."""
    from beemgraphenebase.account import PasswordKey
    prefix = "STM"
    password = "P" + secrets.token_hex(32)
    keys = {"password": password}
    for role in ["owner", "active", "posting", "memo"]:
        pk = PasswordKey(account_name, password, role=role, prefix=prefix)
        keys[role] = {"private": str(pk.get_private_key()), "public": str(pk.get_public_key())}
    return keys


def generate_identity_config(hive_username, active_key_wif):
    """VSC node identityConfig.json content."""
    bls_seed = secrets.token_bytes(32).hex()
    ed_key = Ed25519PrivateKey.generate()
    priv_raw = ed_key.private_bytes(Encoding.Raw, PrivateFormat.Raw, NoEncryption())
    pub_raw = ed_key.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
    libp2p_key = (priv_raw + pub_raw).hex()
    return {
        "BlsPrivKeySeed": bls_seed,
        "HiveActiveKey": active_key_wif,
        "HiveUsername": hive_username,
        "Libp2pPrivKey": libp2p_key,
    }


def account_exists(api_url, name):
    r = requests.post(
        api_url,
        json={"jsonrpc": "2.0", "method": "condenser_api.get_accounts", "params": [[name]], "id": 1},
        timeout=15,
    )
    return len(r.json().get("result", [])) > 0


def main():
    ap = argparse.ArgumentParser(description="Generate tx files and configs for clive-based testnet setup")
    ap.add_argument("--nodes", type=int, default=int(os.getenv("NUM_NODES", "3")))
    ap.add_argument("--api", default=os.getenv("HIVE_API", DEFAULT_API))
    ap.add_argument("--output", default="testnet")
    ap.add_argument("--stake", type=float, default=1000.0)
    ap.add_argument("--transfer", type=float, default=5000.0)
    args = ap.parse_args()

    out = Path(args.output)
    out.mkdir(parents=True, exist_ok=True)
    keys_file = out / "keys.json"
    all_keys = {}
    if keys_file.exists():
        all_keys = json.loads(keys_file.read_text())

    # Hive key derivation (same as beem) — required for account_create
    try:
        from beemgraphenebase.account import PasswordKey
    except ImportError:
        sys.exit("ERROR: pip install beem required for Hive key generation (PasswordKey). Then re-run.")

    tx_files = []
    api = args.api

    # 1) Account creates (magic-node1..N, vsc.testnet)
    for name in [f"{NODE_PREFIX}{i}" for i in range(1, args.nodes + 1)] + [GATEWAY]:
        if account_exists(api, name):
            print(f"  {name} exists, skip create")
            continue
        keys = generate_hive_keys(name)
        all_keys[name] = keys
        fee = amount_nai("0.000")  # testnet 0 fee
        op_value = {
            "fee": fee,
            "creator": CREATOR,
            "new_account_name": name,
            "owner": authority([keys["owner"]["public"]]),
            "active": authority([keys["active"]["public"]]),
            "posting": authority([keys["posting"]["public"]]),
            "memo_key": keys["memo"]["public"],
            "json_metadata": "",
        }
        tx = make_tx(api, [{"id": OP_ACCOUNT_CREATE, "value": op_value}])
        path = out / f"tx-create-{name}.json"
        path.write_text(json.dumps(tx, indent=2))
        tx_files.append(path)
        print(f"  wrote {path}")
        time.sleep(0.5)

    keys_file.write_text(json.dumps(all_keys, indent=2))

    # 2) Transfers: magic-man -> each node
    for i in range(1, args.nodes + 1):
        name = f"{NODE_PREFIX}{i}"
        amt = amount_nai(f"{args.transfer:.3f}")
        op_value = {"from": CREATOR, "to": name, "amount": amt, "memo": "VSC testnet"}
        tx = make_tx(api, [{"id": OP_TRANSFER, "value": op_value}])
        path = out / f"tx-transfer-{name}.json"
        path.write_text(json.dumps(tx, indent=2))
        tx_files.append(path)
        print(f"  wrote {path}")
        time.sleep(0.3)

    # 3) Staking: each node -> gateway + vsc.consensus_stake custom_json
    for i in range(1, args.nodes + 1):
        name = f"{NODE_PREFIX}{i}"
        amt = amount_nai(f"{args.stake:.3f}")
        consensus_json = json.dumps({
            "from": f"hive:{name}", "to": f"hive:{name}", "asset": "hive",
            "net_id": NET_ID, "amount": f"{args.stake:.3f}",
        })
        tx = make_tx(api, [
            {"id": OP_TRANSFER, "value": {"from": name, "to": GATEWAY, "amount": amt, "memo": ""}},
            {"id": OP_CUSTOM_JSON, "value": {
                "required_auths": [name],
                "required_posting_auths": [],
                "id": "vsc.consensus_stake",
                "json": consensus_json,
            }},
        ])
        path = out / f"tx-stake-{name}.json"
        path.write_text(json.dumps(tx, indent=2))
        tx_files.append(path)
        print(f"  wrote {path}")

    # 4) Identity configs for nodes
    for i in range(1, args.nodes + 1):
        name = f"{NODE_PREFIX}{i}"
        if name not in all_keys:
            continue
        cfg_dir = out / f"node{i}" / "config"
        cfg_dir.mkdir(parents=True, exist_ok=True)
        cfg_path = cfg_dir / "identityConfig.json"
        cfg = generate_identity_config(name, all_keys[name]["active"]["private"])
        cfg_path.write_text(json.dumps(cfg, indent=2))
        print(f"  wrote {cfg_path}")

    print("")
    print("Next: broadcast each tx with clive (ensure clive is on testnet and key 'magic-man' is in beekeeper):")
    for f in tx_files:
        print(f"  clive process transaction --from-file {f} --sign magic-man --password '<pwd>'")
    print("")
    print("Or run: ./scripts/launch-testnet-multinode-clive.sh")
    return 0


if __name__ == "__main__":
    sys.exit(main())
