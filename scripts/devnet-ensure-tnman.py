#!/usr/bin/env python3
"""
Ensure tnman exists on a Hive devnet (Tin Toy) and has enough balance to create
magic-node1..N and stake. Use from devnet-create-accounts.sh when hive-devnet
(Docker) or GET_DEV_KEY is available.

- If tnman does not exist: create it (initminer + tnman keys from get_dev_key).
- If tnman exists but balance < MIN_TNMAN_BALANCE: transfer from initminer to tnman.

Keys can be supplied by:
  1. Docker: shell gets keys via `docker exec hive-devnet get_dev_key tintoy ...`
             and exports TNMAN_OWNER_KEY, TNMAN_ACTIVE_KEY, TNMAN_POSTING_KEY,
             TNMAN_MEMO_KEY, INITMINER_KEY.
  2. Local: set GET_DEV_KEY to path to get_dev_key; script runs it for tnman keys.
            INITMINER_KEY must be set (e.g. from get_dev_key tintoy wit-block-signing-0).

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
    from beemgraphenebase.account import PrivateKey
except ImportError:
    sys.exit("ERROR: Install beem: pip install beem")

# Import helpers from testnet-multinode-setup.py, or use minimal inline fallback
setup_mod = None
try:
    import importlib.util
    _setup_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "testnet-multinode-setup.py")
    _spec = importlib.util.spec_from_file_location("testnet_setup", _setup_path)
    setup_mod = importlib.util.module_from_spec(_spec)
    _spec.loader.exec_module(setup_mod)
    connect_hive = setup_mod.connect_hive
    broadcast_create_account = setup_mod.broadcast_create_account
    broadcast_transfer = setup_mod.broadcast_transfer
    get_creation_fee = setup_mod.get_creation_fee
    detect_chain_params = setup_mod.detect_chain_params
    detect_asset_symbol = setup_mod.detect_asset_symbol
    get_balance = setup_mod.get_balance
    account_exists = setup_mod.account_exists
except Exception:
    def _ensure_tests_chain(chain_id=None, prefix="TST"):
        try:
            import beemgraphenebase.chains as _c
            cid = chain_id or "18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e"
            if "HIVETEST" not in getattr(_c, "known_chains", {}):
                _c.known_chains["HIVETEST"] = {"chain_id": cid, "min_version": "0.20.0", "prefix": prefix, "chain_assets": [{"asset": "@@000000013", "symbol": "TBD", "precision": 3, "id": 0}, {"asset": "@@000000021", "symbol": "TESTS", "precision": 3, "id": 1}, {"asset": "@@000000037", "symbol": "VESTS", "precision": 6, "id": 2}]}
        except Exception:
            pass
    def detect_chain_params(api_url):
        for method in ("database_api.get_config", "condenser_api.get_config"):
            try:
                r = http_requests.post(api_url, json={"jsonrpc": "2.0", "method": method, "params": [] if "condenser" in method else {}, "id": 1}, timeout=15)
                res = r.json().get("result", {})
                if res: return res.get("HIVE_CHAIN_ID", ""), res.get("HIVE_ADDRESS_PREFIX", "TST"), "TESTS" if res.get("IS_TEST_NET") else "HIVE"
            except Exception:
                pass
        return "", "TST", "TESTS"
    def detect_asset_symbol(api_url):
        r = http_requests.post(api_url, json={"jsonrpc": "2.0", "method": "condenser_api.get_accounts", "params": [["initminer"]], "id": 1}, timeout=15)
        accts = r.json().get("result", []); return accts[0].get("balance", "0.000 HIVE").split()[-1] if accts else "HIVE"
    def get_creation_fee(api_url, asset_symbol):
        r = http_requests.post(api_url, json={"jsonrpc": "2.0", "method": "database_api.get_witness_schedule", "params": {}, "id": 1}, timeout=15)
        fee = r.json().get("result", {}).get("median_props", {}).get("account_creation_fee", {}); amt = int(fee.get("amount", 3000)); prec = fee.get("precision", 3)
        return f"{amt / (10 ** prec):.{prec}f} {asset_symbol}"
    def get_balance(api_url, account_name):
        r = http_requests.post(api_url, json={"jsonrpc": "2.0", "method": "condenser_api.get_accounts", "params": [[account_name]], "id": 1}, timeout=15)
        accts = r.json().get("result", []); return float(accts[0].get("balance", "0.000 HIVE").split()[0]) if accts else 0.0
    def account_exists(api_url, account_name):
        r = http_requests.post(api_url, json={"jsonrpc": "2.0", "method": "condenser_api.get_accounts", "params": [[account_name]], "id": 1}, timeout=15)
        return len(r.json().get("result", [])) > 0
    def connect_hive(api_url, keys, chain_id=None, prefix="STM", asset_symbol="HIVE", no_broadcast=False):
        kwargs = dict(node=[api_url], keys=keys, no_broadcast=no_broadcast, num_retries=3, timeout=30)
        if chain_id:
            if asset_symbol == "TESTS":
                _ensure_tests_chain(chain_id, prefix)
                tst_chain = {"chain_id": chain_id, "min_version": "0.0.0", "prefix": prefix, "chain_assets": [{"asset": "@@000000013", "symbol": "TBD", "precision": 3, "id": 0}, {"asset": "@@000000021", "symbol": "TESTS", "precision": 3, "id": 1}, {"asset": "@@000000037", "symbol": "VESTS", "precision": 6, "id": 2}]}
                kwargs["custom_chains"] = {"HIVE": tst_chain}
                # So get_network() fallback returns a chain with TESTS when node uses database_api
                import beemgraphenebase.chains as _c
                _c.known_chains["HIVE"] = tst_chain
                kwargs["keys"] = []  # add after override so wallet uses TST prefix
                h = Hive(**kwargs)
                h.get_network = lambda use_stored_data=True, config=None: tst_chain
                h.wallet.setKeys(keys)  # now prefix is TST so pubkey derivation matches chain
                return h
            else:
                hbd = "HBD"
                kwargs["custom_chains"] = {"HIVE": {"chain_id": chain_id, "min_version": "0.0.0", "prefix": prefix, "chain_assets": [{"asset": asset_symbol, "symbol": asset_symbol, "precision": 3, "id": 0}, {"asset": hbd, "symbol": hbd, "precision": 3, "id": 1}, {"asset": "VESTS", "symbol": "VESTS", "precision": 6, "id": 2}]}}
        return Hive(**kwargs)
    def broadcast_create_account(hive_inst, creator, name, keys, fee):
        tx = TransactionBuilder(blockchain_instance=hive_inst)
        tx.appendOps(beem_ops.Account_create(**{"fee": fee, "creator": creator, "new_account_name": name, "owner": {"weight_threshold": 1, "account_auths": [], "key_auths": [[keys["owner"]["public"], 1]]}, "active": {"weight_threshold": 1, "account_auths": [], "key_auths": [[keys["active"]["public"], 1]]}, "posting": {"weight_threshold": 1, "account_auths": [], "key_auths": [[keys["posting"]["public"], 1]]}, "memo_key": keys["memo"]["public"], "json_metadata": ""}))
        tx.appendSigner(creator, "active"); tx.sign(); return tx.broadcast()
    def broadcast_transfer(hive_inst, from_acct, to_acct, amount, asset, memo=""):
        prefix = hive_inst.chain_params.get("prefix", "STM")
        tx = TransactionBuilder(blockchain_instance=hive_inst)
        tx.appendOps(beem_ops.Transfer(**{"from": from_acct, "to": to_acct, "amount": f"{amount:.3f} {asset}", "memo": memo, "prefix": prefix})); tx.appendSigner(from_acct, "active"); tx.sign(); return tx.broadcast()

MIN_TNMAN_BALANCE = 20000.0  # enough for 3 nodes (creation + stake + transfer)
TRANSFER_AMOUNT = 50000.0   # amount to send initminer -> tnman when funding


def wif_to_public(wif, prefix="STM"):
    """Derive public key from WIF (fallback when *_PUBLIC not in env)."""
    try:
        pk = PrivateKey(wif, prefix=prefix)
        return str(pk.pubkey)
    except Exception:
        return ""


def get_tnman_keys_from_env(prefix="STM"):
    """Build tnman keys dict from env TNMAN_OWNER_KEY, TNMAN_ACTIVE_KEY, etc.
    Prefer TNMAN_*_PUBLIC when set (from shell parsing get_dev_key output)."""
    roles = ["owner", "active", "posting", "memo"]
    keys = {}
    for role in roles:
        r = role.upper()
        wif = os.environ.get(f"TNMAN_{r}_KEY")
        pub = os.environ.get(f"TNMAN_{r}_PUBLIC")
        if not wif or not wif.startswith("5"):
            return None
        if not pub and wif:
            pub = wif_to_public(wif, prefix)
        keys[role] = {
            "private": wif,
            "public": pub or wif_to_public(wif, prefix),
        }
    return keys


def get_tnman_keys_from_get_dev_key(get_dev_key_bin, secret="tintoy"):
    """Run get_dev_key for owner/active/posting/memo tnman; return keys dict."""
    out = subprocess.run(
        [get_dev_key_bin, secret, "owner-tnman", "active-tnman", "posting-tnman", "memo-tnman"],
        capture_output=True,
        text=True,
        timeout=10,
    )
    if out.returncode != 0 or not out.stdout.strip():
        return None
    arr = json.loads(out.stdout)
    by_name = {item["account_name"]: item for item in arr}
    keys = {}
    for role in ["owner", "active", "posting", "memo"]:
        name = f"{role}-tnman"
        if name not in by_name:
            return None
        keys[role] = {
            "private": by_name[name]["private_key"],
            "public": by_name[name]["public_key"],
        }
    return keys


def main():
    api = os.environ.get("HIVE_API", "http://localhost:8090")
    initminer_key = os.environ.get("INITMINER_KEY")
    if not initminer_key or not initminer_key.startswith("5"):
        sys.exit("ERROR: Set INITMINER_KEY (e.g. from get_dev_key tintoy wit-block-signing-0)")

    chain_id, prefix, asset_symbol = detect_chain_params(api)
    real_asset = detect_asset_symbol(api)
    if real_asset != "HIVE":
        asset_symbol = real_asset
    if setup_mod and hasattr(setup_mod, "_ensure_tests_chain"):
        setup_mod._ensure_tests_chain()
    elif setup_mod is None and "TESTS" in (asset_symbol or ""):
        _ensure_tests_chain(chain_id)

    # Resolve tnman keys: env (from Docker) or GET_DEV_KEY
    tnman_keys = get_tnman_keys_from_env(prefix=prefix)
    if not tnman_keys:
        get_dev_key_bin = os.environ.get("GET_DEV_KEY")
        if get_dev_key_bin and os.path.isfile(get_dev_key_bin):
            secret = os.environ.get("TINTOY_SECRET", "tintoy")
            tnman_keys = get_tnman_keys_from_get_dev_key(get_dev_key_bin, secret=secret)
    if not tnman_keys:
        sys.exit("ERROR: Set TNMAN_OWNER_KEY, TNMAN_ACTIVE_KEY, TNMAN_POSTING_KEY, TNMAN_MEMO_KEY (from docker exec get_dev_key), or set GET_DEV_KEY")

    hive = connect_hive(api, [initminer_key],
                       chain_id=chain_id, prefix=prefix, asset_symbol=asset_symbol)

    # 1) Create tnman if missing
    if not account_exists(api, "tnman"):
        print("Creating tnman on devnet ...")
        creation_fee = get_creation_fee(api, asset_symbol)
        broadcast_create_account(hive, "initminer", "tnman", tnman_keys, creation_fee)
        print("  tnman created.")
    else:
        print("tnman already exists.")

    # 2) Fund tnman if balance too low
    balance = get_balance(api, "tnman")
    if balance < MIN_TNMAN_BALANCE:
        print(f"Funding tnman (have {balance:.0f} {asset_symbol}, need >= {MIN_TNMAN_BALANCE:.0f}) ...")
        broadcast_transfer(hive, "initminer", "tnman", TRANSFER_AMOUNT, asset_symbol, memo="fund tnman")
        print(f"  Transferred {TRANSFER_AMOUNT:.0f} {asset_symbol} to tnman.")
    else:
        print(f"tnman balance OK: {balance:.0f} {asset_symbol}")


if __name__ == "__main__":
    main()
