#!/usr/bin/env python3
"""
Create the tnman account on a native Hive devnet (no Docker) using initminer.
Uses the same key derivation as Tin Toy: get_dev_key <secret> owner-tnman active-tnman posting-tnman memo-tnman.
Run after hived is up (e.g. scripts/run-devnet-native.sh) and before devnet-create-accounts.sh.

Usage:
  INITMINER_KEY="5J..." GET_DEV_KEY=/path/to/get_dev_key python3 scripts/create-tnman-native.py
  # or with HIVE_SRC set, GET_DEV_KEY can be omitted (script will use HIVE_SRC/build/.../get_dev_key)

Requires: beem, requests. Same venv as testnet-multinode-setup.py.
"""
import os
import sys
import json
import subprocess

try:
    import requests as http_requests
except ImportError:
    sys.exit("ERROR: Install requests: pip install requests")

try:
    from beem import Hive
    from beem.transactionbuilder import TransactionBuilder
    from beembase import operations as beem_ops
except ImportError:
    sys.exit("ERROR: Install beem: pip install beem")

# Import helpers from testnet-multinode-setup.py (filename has hyphen so use importlib)
import importlib.util
_setup_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "testnet-multinode-setup.py")
_spec = importlib.util.spec_from_file_location("testnet_setup", _setup_path)
setup_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(setup_mod)
connect_hive = setup_mod.connect_hive
broadcast_create_account = setup_mod.broadcast_create_account
get_creation_fee = setup_mod.get_creation_fee
detect_chain_params = setup_mod.detect_chain_params
detect_asset_symbol = setup_mod.detect_asset_symbol


def get_dev_key_keys(get_dev_key_bin, secret="tintoy"):
    """Run get_dev_key for owner-tnman, active-tnman, posting-tnman, memo-tnman; return keys dict."""
    out = subprocess.run(
        [get_dev_key_bin, secret, "owner-tnman", "active-tnman", "posting-tnman", "memo-tnman"],
        capture_output=True,
        text=True,
        timeout=10,
    )
    if out.returncode != 0 or not out.stdout.strip():
        raise SystemExit(f"get_dev_key failed: {out.stderr or out.stdout}")
    arr = json.loads(out.stdout)
    # get_dev_key returns list of { "private_key", "public_key", "account_name" }
    by_name = {item["account_name"]: item for item in arr}
    keys = {}
    for role in ["owner", "active", "posting", "memo"]:
        name = f"{role}-tnman"
        if name not in by_name:
            raise SystemExit(f"get_dev_key did not return {name}")
        keys[role] = {
            "private": by_name[name]["private_key"],
            "public": by_name[name]["public_key"],
        }
    return keys


def main():
    api = os.environ.get("HIVE_API", "http://localhost:8090")
    initminer_key = os.environ.get("INITMINER_KEY")
    if not initminer_key or not initminer_key.startswith("5"):
        sys.exit("ERROR: Set INITMINER_KEY to initminer/skeleton WIF (e.g. from get_dev_key tintoy wit-block-signing-0)")

    get_dev_key_bin = os.environ.get("GET_DEV_KEY")
    if not get_dev_key_bin or not os.path.isfile(get_dev_key_bin):
        hive_src = os.environ.get("HIVE_SRC", os.path.expanduser("~/Repos/hive"))
        cand = os.path.join(hive_src, "build", "programs", "util", "get_dev_key")
        if os.path.isfile(cand):
            get_dev_key_bin = cand
    if not get_dev_key_bin or not os.path.isfile(get_dev_key_bin):
        sys.exit("ERROR: Set GET_DEV_KEY to path to get_dev_key binary, or set HIVE_SRC and build hive (testnet)")

    secret = os.environ.get("TINTOY_SECRET", "tintoy")

    print(f"Creating tnman on {api} using initminer and keys from get_dev_key {secret} ...")
    keys = get_dev_key_keys(get_dev_key_bin, secret=secret)

    chain_id, prefix, asset_symbol = detect_chain_params(api)
    real_asset = detect_asset_symbol(api)
    if real_asset != "HIVE":
        asset_symbol = real_asset
    creation_fee = get_creation_fee(api, asset_symbol)
    if hasattr(setup_mod, "_ensure_tests_chain"):
        setup_mod._ensure_tests_chain()

    hive = connect_hive(api, [initminer_key], chain_id=chain_id, prefix=prefix, asset_symbol=asset_symbol)
    try:
        broadcast_create_account(hive, "initminer", "tnman", keys, creation_fee)
        print("OK — tnman created. You can run ./scripts/devnet-create-accounts.sh")
    except Exception as e:
        if "already exists" in str(e).lower() or "exists" in str(e).lower():
            print("tnman already exists.")
        else:
            raise


if __name__ == "__main__":
    main()
