#!/usr/bin/env python3
"""
VSC Testnet Multi-Node Setup

Creates Hive testnet accounts from 'magic-man', generates VSC node identity
configs, and stakes to VSC consensus — everything needed to spin up a
multi-node VSC testnet for p2p TSS testing.

Prerequisites:
    pip install beem cryptography requests

Usage:
    MAGIC_MAN_KEY="5Kxxx..." python3 scripts/testnet-multinode-setup.py

Environment:
    MAGIC_MAN_KEY   Active key WIF for 'magic-man' (required)
    HIVE_API        Testnet API URL (default: https://testnet.techcoderx.com)
    NUM_NODES       Number of nodes to create (default: 3)
"""

import os
import sys
import json
import secrets
import time
import argparse
import traceback
from pathlib import Path

try:
    import requests as http_requests
except ImportError:
    sys.exit("ERROR: Install requests: pip install requests")

try:
    from beem import Hive
    from beem.account import Account
    from beem.transactionbuilder import TransactionBuilder
    from beembase import operations as beem_ops
    from beemgraphenebase.account import PasswordKey
except ImportError:
    sys.exit("ERROR: Install beem: pip install beem")

try:
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
    from cryptography.hazmat.primitives.serialization import (
        Encoding, PrivateFormat, PublicFormat, NoEncryption,
    )
except ImportError:
    sys.exit("ERROR: Install cryptography: pip install cryptography")


CREATOR = os.environ.get("CREATOR", "magic-man")
NODE_PREFIX = os.environ.get("NODE_PREFIX", "magic-node")
GATEWAY = os.environ.get("GATEWAY", "vsc.testnet")
NET_ID = "vsc-testnet"
DEFAULT_API = "https://testnet.techcoderx.com"


# ---------------------------------------------------------------------------
# Chain detection helpers
# ---------------------------------------------------------------------------

def detect_chain_params(api_url):
    """Query the testnet node for chain_id, prefix, and asset symbol."""
    chain_id, prefix, asset_symbol = None, "STM", "HIVE"
    try:
        resp = http_requests.post(api_url, json={
            "jsonrpc": "2.0",
            "method": "database_api.get_config",
            "params": {},
            "id": 1,
        }, timeout=15)
        result = resp.json().get("result", {})
        chain_id = result.get("HIVE_CHAIN_ID", "")
        prefix = result.get("HIVE_ADDRESS_PREFIX", "STM")
        is_testnet = result.get("IS_TEST_NET", False)
        if is_testnet:
            asset_symbol = "TESTS"
    except Exception as exc:
        print(f"  Warning: chain detection failed ({exc}), using defaults")

    return chain_id, prefix, asset_symbol


def detect_asset_symbol(api_url):
    """Detect the native asset name from an existing account's balance."""
    try:
        resp = http_requests.post(api_url, json={
            "jsonrpc": "2.0",
            "method": "condenser_api.get_accounts",
            "params": [["initminer"]],
            "id": 1,
        }, timeout=15)
        accts = resp.json().get("result", [])
        if accts:
            bal = accts[0].get("balance", "0.000 HIVE")
            return bal.split()[-1]
    except Exception:
        pass
    return "HIVE"


def get_creation_fee(api_url, asset_symbol):
    """Get the median account_creation_fee. Returns (amount_str, asset).
    Testnet expects TESTS; beem must have TESTS in custom_chains.chain_assets."""
    try:
        resp = http_requests.post(api_url, json={
            "jsonrpc": "2.0",
            "method": "database_api.get_witness_schedule",
            "params": {},
            "id": 1,
        }, timeout=15)
        result = resp.json().get("result", {})
        fee_obj = result.get("median_props", {}).get("account_creation_fee", {})
        if isinstance(fee_obj, dict):
            amount = int(fee_obj.get("amount", 3000))
            precision = fee_obj.get("precision", 3)
            return f"{amount / (10 ** precision):.{precision}f} {asset_symbol}"
        if isinstance(fee_obj, str):
            return fee_obj
    except Exception:
        pass
    return f"3.000 {asset_symbol}"


def get_balance(api_url, account_name):
    """Get liquid balance for an account via JSON-RPC."""
    try:
        resp = http_requests.post(api_url, json={
            "jsonrpc": "2.0",
            "method": "condenser_api.get_accounts",
            "params": [[account_name]],
            "id": 1,
        }, timeout=15)
        accounts = resp.json().get("result", [])
        if accounts:
            bal_str = accounts[0].get("balance", "0.000 HIVE")
            return float(bal_str.split()[0])
    except Exception:
        pass
    return 0.0


def account_exists(api_url, account_name):
    """Check if a Hive account exists on-chain."""
    try:
        resp = http_requests.post(api_url, json={
            "jsonrpc": "2.0",
            "method": "condenser_api.get_accounts",
            "params": [[account_name]],
            "id": 1,
        }, timeout=15)
        return len(resp.json().get("result", [])) > 0
    except Exception:
        return False


# ---------------------------------------------------------------------------
# Beem connection
# ---------------------------------------------------------------------------

def connect_hive(api_url, keys, chain_id=None, prefix="STM",
                 asset_symbol="HIVE", no_broadcast=False):
    """Create a beem Hive instance with optional testnet params.
    For testnet (asset_symbol=TESTS), chain_assets must use TESTS so beem
    recognizes '0.000 TESTS' and the node accepts it. We patch get_network
    before Hive() so wallet key derivation uses the correct prefix (TST)."""
    kwargs = dict(
        node=[api_url],
        keys=keys,
        no_broadcast=no_broadcast,
        num_retries=3,
        timeout=30,
    )
    if chain_id:
        if asset_symbol == "TESTS":
            # Tin Toy devnet asset IDs
            tst_chain = {
                "chain_id": chain_id,
                "min_version": "0.0.0",
                "prefix": prefix,
                "chain_assets": [
                    {"asset": "@@000000013", "symbol": "TBD", "precision": 3, "id": 0},
                    {"asset": "@@000000021", "symbol": "TESTS", "precision": 3, "id": 1},
                    {"asset": "@@000000037", "symbol": "VESTS", "precision": 6, "id": 2},
                ],
            }
            kwargs["custom_chains"] = {"HIVE": tst_chain}
            import beemgraphenebase.chains as _c
            _c.known_chains["HIVE"] = tst_chain
            # Add keys after construct so wallet uses TST prefix for pubkey derivation.
            # Use in-memory store so setKeys() works without wallet unlock.
            keys = kwargs.get("keys", [])
            kwargs["keys"] = []
            try:
                from beemstorage import InRamPlainKeyStore
                kwargs["key_store"] = InRamPlainKeyStore()
            except ImportError:
                pass
            _orig_get_network = Hive.get_network
            _current_tst = [tst_chain]  # ref so closure can clear

            def _patched_get_network(self, use_stored_data=True, config=None):
                if _current_tst:
                    return _current_tst[0]
                return _orig_get_network(self, use_stored_data, config)

            Hive.get_network = _patched_get_network
            try:
                h = Hive(**kwargs)
                h.get_network = lambda use_stored_data=True, config=None: tst_chain
                if keys:
                    h.wallet.setKeys(keys)
                return h
            finally:
                _current_tst.clear()
                Hive.get_network = _orig_get_network
        hbd_sym = "HBD"
        kwargs["custom_chains"] = {
            "HIVE": {
                "chain_id": chain_id,
                "min_version": "0.0.0",
                "prefix": prefix,
                "chain_assets": [
                    {"asset": asset_symbol, "symbol": asset_symbol,
                     "precision": 3, "id": 0},
                    {"asset": hbd_sym, "symbol": hbd_sym,
                     "precision": 3, "id": 1},
                    {"asset": "VESTS", "symbol": "VESTS",
                     "precision": 6, "id": 2},
                ],
            }
        }
    return Hive(**kwargs)


# ---------------------------------------------------------------------------
# Key and config generation
# ---------------------------------------------------------------------------

def generate_hive_keys(account_name, prefix="STM"):
    """Derive owner/active/posting/memo keys from a random master password."""
    password = "P" + secrets.token_hex(32)
    keys = {"password": password}
    for role in ["owner", "active", "posting", "memo"]:
        pk = PasswordKey(account_name, password, role=role, prefix=prefix)
        keys[role] = {
            "private": str(pk.get_private_key()),
            "public": str(pk.get_public_key()),
        }
    return keys


def generate_identity_config(hive_username, active_key_wif):
    """Generate a complete VSC node identityConfig.json."""
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


# ---------------------------------------------------------------------------
# Broadcast helpers
# ---------------------------------------------------------------------------

def _ensure_tests_chain():
    """Add STM + TESTS to beem's known_chains so Amount('0.000 TESTS') works (techcoderx testnet)."""
    try:
        import beemgraphenebase.chains as _chains
        if "HIVETEST" in _chains.known_chains:
            return
        _chains.known_chains["HIVETEST"] = {
            "chain_id": "18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e",
            "min_version": "0.20.0",
            "prefix": "STM",
            "chain_assets": [
                {"asset": "@@000000013", "symbol": "TBD", "precision": 3, "id": 0},
                {"asset": "@@000000021", "symbol": "TESTS", "precision": 3, "id": 1},
                {"asset": "@@000000037", "symbol": "VESTS", "precision": 6, "id": 2},
            ],
        }
    except Exception:
        pass


def broadcast_create_account(hive_inst, creator, name, keys, fee):
    tx = TransactionBuilder(blockchain_instance=hive_inst)
    tx.appendOps(beem_ops.Account_create(**{
        "fee": fee,
        "creator": creator,
        "new_account_name": name,
        "owner": {
            "weight_threshold": 1,
            "account_auths": [],
            "key_auths": [[keys["owner"]["public"], 1]],
        },
        "active": {
            "weight_threshold": 1,
            "account_auths": [],
            "key_auths": [[keys["active"]["public"], 1]],
        },
        "posting": {
            "weight_threshold": 1,
            "account_auths": [],
            "key_auths": [[keys["posting"]["public"], 1]],
        },
        "memo_key": keys["memo"]["public"],
        "json_metadata": "",
    }))
    tx.appendSigner(creator, "active")
    tx.sign()
    return tx.broadcast()


def broadcast_transfer(hive_inst, from_acct, to_acct, amount, asset, memo=""):
    prefix = hive_inst.chain_params.get("prefix", "STM")
    amt_str = f"{amount:.3f} {asset}"
    tx = TransactionBuilder(blockchain_instance=hive_inst)
    tx.appendOps(beem_ops.Transfer(**{
        "from": from_acct,
        "to": to_acct,
        "amount": amt_str,
        "memo": memo,
        "prefix": prefix,
    }))
    tx.appendSigner(from_acct, "active")
    tx.sign()
    return tx.broadcast()


def broadcast_stake(hive_inst, node_name, gateway, stake_amount, asset):
    amt_str = f"{stake_amount:.3f} {asset}"
    tx = TransactionBuilder(blockchain_instance=hive_inst)
    tx.appendOps(beem_ops.Transfer(**{
        "from": node_name,
        "to": gateway,
        "amount": amt_str,
        "memo": "",
    }))
    stake_json = json.dumps({
        "from": f"hive:{node_name}",
        "to": f"hive:{node_name}",
        "asset": "hive",
        "net_id": NET_ID,
        "amount": f"{stake_amount:.3f}",
    })
    tx.appendOps(beem_ops.Custom_json(**{
        "required_auths": [node_name],
        "required_posting_auths": [],
        "id": "vsc.consensus_stake",
        "json": stake_json,
    }))
    tx.appendSigner(node_name, "active")
    tx.sign()
    return tx.broadcast()


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    parser = argparse.ArgumentParser(
        description="VSC Testnet Multi-Node Setup",
    )
    parser.add_argument("--nodes", type=int,
                        default=int(os.getenv("NUM_NODES", "3")))
    parser.add_argument("--api",
                        default=os.getenv("HIVE_API", DEFAULT_API))
    parser.add_argument("--stake", type=float, default=1000.0,
                        help="HIVE/TESTS to stake per node (default: 1000)")
    parser.add_argument("--transfer", type=float, default=5000.0,
                        help="HIVE/TESTS to send per node (default: 5000)")
    parser.add_argument("--output", default="testnet")
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()

    active_key = os.environ.get("MAGIC_MAN_KEY")
    if not active_key:
        sys.exit(
            "ERROR: Set MAGIC_MAN_KEY to magic-man's active private key\n"
            "  export MAGIC_MAN_KEY='5Kxxx...'"
        )

    output = Path(args.output)
    output.mkdir(parents=True, exist_ok=True)

    # ---- Ensure beem knows TESTS with STM prefix (techcoderx testnet) ----
    _ensure_tests_chain()

    # ---- Detect chain ----
    print(f"\nConnecting to {args.api} ...")
    chain_id, prefix, asset_symbol = detect_chain_params(args.api)
    real_asset = detect_asset_symbol(args.api)
    if real_asset != "HIVE":
        asset_symbol = real_asset
    creation_fee = get_creation_fee(args.api, asset_symbol)

    print(f"  Chain ID : {chain_id or '(mainnet default)'}")
    print(f"  Prefix   : {prefix}")
    print(f"  Asset    : {asset_symbol}")
    print(f"  Fee      : {creation_fee}")

    # ---- Connect ----
    hive = connect_hive(args.api, [active_key],
                        chain_id=chain_id, prefix=prefix,
                        asset_symbol=asset_symbol,
                        no_broadcast=args.dry_run)

    balance = get_balance(args.api, CREATOR)
    print(f"  {CREATOR} balance: {balance:.3f} {asset_symbol}")

    # RC (Resource Credits) are required to send transactions on Hive/testnet.
    # If magic-man has 0 RC, power up some TESTS on the testnet or wait for regeneration.

    needed = args.nodes * (args.transfer + 3.0) + 20
    if balance < needed:
        sys.exit(f"ERROR: Need ~{needed:.0f} {asset_symbol}, have {balance:.3f}")

    # ---- Load saved keys ----
    keys_file = output / "keys.json"
    all_keys = {}
    if keys_file.exists():
        all_keys = json.loads(keys_file.read_text())
        print(f"  Loaded existing keys from {keys_file}")

    def save_keys():
        keys_file.write_text(json.dumps(all_keys, indent=2))

    # ==================================================================
    # Phase 1 — Create node accounts + fund them
    # ==================================================================
    print(f"\n{'='*60}")
    print(f"Phase 1: Create & fund {args.nodes} node accounts")
    print(f"{'='*60}")

    created_nodes = []
    for i in range(1, args.nodes + 1):
        name = f"{NODE_PREFIX}{i}"
        print(f"\n  [{i}/{args.nodes}] {name}")

        if account_exists(args.api, name):
            print(f"    Account exists on-chain")
            if name in all_keys:
                print(f"    Keys available from previous run")
            else:
                print(f"    WARNING: no saved keys — set active key manually later")
            created_nodes.append(name)
            continue

        keys = generate_hive_keys(name, prefix=prefix)
        all_keys[name] = keys
        save_keys()

        print(f"    Creating account (fee {creation_fee}) ...")
        try:
            broadcast_create_account(hive, CREATOR, name, keys, creation_fee)
            print(f"    OK — created")
        except Exception as exc:
            print(f"    FAILED: {exc}")
            traceback.print_exc()
            continue

        time.sleep(4)

        print(f"    Transferring {args.transfer:.3f} {asset_symbol} ...")
        try:
            broadcast_transfer(hive, CREATOR, name, args.transfer,
                               asset_symbol,
                               memo="VSC testnet node funding")
            print(f"    OK — funded")
        except Exception as exc:
            print(f"    FAILED: {exc}")
            traceback.print_exc()
            continue

        created_nodes.append(name)
        time.sleep(3)

    # ==================================================================
    # Phase 2 — Gateway account
    # ==================================================================
    print(f"\n{'='*60}")
    print(f"Phase 2: Ensure gateway account '{GATEWAY}'")
    print(f"{'='*60}")

    if account_exists(args.api, GATEWAY):
        print(f"  Gateway exists")
    else:
        print(f"  Creating gateway '{GATEWAY}' ...")
        gw_keys = generate_hive_keys(GATEWAY, prefix=prefix)
        all_keys[GATEWAY] = gw_keys
        save_keys()
        try:
            broadcast_create_account(hive, CREATOR, GATEWAY, gw_keys,
                                     creation_fee)
            print(f"  OK — created gateway")
            time.sleep(4)
        except Exception as exc:
            print(f"  FAILED: {exc}")
            print(f"  Staking will fail without the gateway account!")

    # ==================================================================
    # Phase 3 — Stake each node
    # ==================================================================
    print(f"\n{'='*60}")
    print(f"Phase 3: Stake {args.stake:.3f} {asset_symbol} per node")
    print(f"{'='*60}")

    for name in created_nodes:
        if name not in all_keys:
            print(f"\n  {name}: skip (no keys)")
            continue

        node_key = all_keys[name]["active"]["private"]
        node_hive = connect_hive(args.api, [node_key],
                                 chain_id=chain_id, prefix=prefix,
                                 asset_symbol=asset_symbol,
                                 no_broadcast=args.dry_run)

        print(f"\n  {name}: staking {args.stake:.3f} {asset_symbol} ...")
        try:
            broadcast_stake(node_hive, name, GATEWAY, args.stake,
                            asset_symbol)
            print(f"    OK — staked")
        except Exception as exc:
            print(f"    FAILED: {exc}")
            traceback.print_exc()
        time.sleep(3)

    # ==================================================================
    # Phase 4 — Generate identity configs
    # ==================================================================
    print(f"\n{'='*60}")
    print(f"Phase 4: Generate VSC identity configs")
    print(f"{'='*60}")

    for i in range(1, args.nodes + 1):
        name = f"{NODE_PREFIX}{i}"
        if name not in all_keys:
            print(f"  {name}: skip (no keys)")
            continue

        node_dir = output / f"node{i}" / "config"
        node_dir.mkdir(parents=True, exist_ok=True)
        cfg_path = node_dir / "identityConfig.json"

        if cfg_path.exists():
            existing = json.loads(cfg_path.read_text())
            existing["HiveActiveKey"] = all_keys[name]["active"]["private"]
            existing["HiveUsername"] = name
            cfg_path.write_text(json.dumps(existing, indent=2))
            print(f"  {name}: updated Hive creds in {cfg_path}")
        else:
            config = generate_identity_config(
                name, all_keys[name]["active"]["private"])
            cfg_path.write_text(json.dumps(config, indent=2))
            print(f"  {name}: created {cfg_path}")

    # ==================================================================
    # Done
    # ==================================================================
    save_keys()
    print(f"\n{'='*60}")
    print(f"Setup complete!")
    print(f"{'='*60}")
    print(f"  Accounts : {', '.join(f'{NODE_PREFIX}{i}' for i in range(1, args.nodes + 1))}")
    print(f"  Gateway  : {GATEWAY}")
    print(f"  Asset    : {asset_symbol}")
    print(f"  Configs  : {output}/node*/config/identityConfig.json")
    print(f"  Keys     : {keys_file}")
    print(f"")
    print(f"  IMPORTANT: {keys_file} contains private keys — keep it safe!")
    print(f"")
    print(f"Next:")
    print(f"  docker compose -f docker-compose.testnet-multinode.yml up -d")


if __name__ == "__main__":
    main()
