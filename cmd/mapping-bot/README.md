# VSC Mapping Bot

Multi-chain mapping bot that bridges UTXO blockchains (BTC, LTC, DASH, DOGE, BCH) and VSC by monitoring blocks for deposits to mapped addresses, then submitting corresponding mapping transactions to the VSC network via a Hive custom JSON operation.

## General Flow

1. **Startup**: Parse CLI flags (`-chain btc|ltc|dash|doge|bch`, `-chain-network mainnet|testnet|regtest`), load configs, connect to MongoDB.
2. **HTTP Server**: Start an HTTP server (default port 8000) to accept mapping registration and signing requests.
3. **Main Loop** (interval based on chain block time: 10min BTC/BCH, 2.5min LTC/DASH, 1min DOGE):
   - **Cleanup**: Delete address mappings older than 30 days, clean up txs stuck in "sent" for >7 days (runs once per 24h).
   - **HandleUnmap**: Fetch pending transaction spends from the contract, check for TSS signatures, and broadcast any fully-signed transactions (with retry, up to 3 attempts).
   - **HandleConfirmations**: Check sent txs against the blockchain, call `confirmSpend` on the contract to promote unconfirmed UTXOs (with retry).
   - **Block Processing**: Fetch the next block via the chain's API, parse it for transactions to mapped addresses, generate merkle proofs, and call the mapping contract (with retry).
   - **HandleExistingTxs**: Check for pre-existing transactions on any newly registered addresses.

### Architecture Diagram

```
                      ┌──────────────┐
  Users ──POST /──►   │  HTTP Server │──── GET /health ──► monitoring
                      └──────┬───────┘
                             │ registers mapping
                             ▼
                      ┌──────────────┐      ┌─────────────────┐
                      │   MongoDB    │◄────►│   Main Loop     │
                      └──────────────┘      └────┬───────┬────┘
                                                 │       │
                              ┌──────────────────┘       └──────────────────┐
                              ▼                                             ▼
                    ┌───────────────────┐                         ┌──────────────────┐
                    │  Chain API        │                         │ Hive Blockchain  │
                    │  (mempool.space)  │                         │ (vsc.call ops)   │
                    │  blocks/txs/post  │                         │ map/confirmSpend │
                    └───────────────────┘                         └──────────────────┘
```

## HTTP Server Endpoints

### `GET /health`

Health check endpoint for monitoring.

**Response** (JSON):

| Field             | Type     | Description                                                    |
| ----------------- | -------- | -------------------------------------------------------------- |
| `status`          | string   | `"starting"`, `"ok"`, or `"unhealthy"`                         |
| `blockHeight`     | number   | Last processed block height                                    |
| `lastBlockAt`     | string   | ISO 8601 timestamp of last processed block                     |
| `staleSecs`       | number   | Seconds since last block (only when stale)                     |
| `pendingSentTxs`  | number   | Transactions broadcast but not yet confirmed on chain          |
| `pendingUnsigned` | number   | Signature hashes awaiting TSS signatures                       |
| `issues`          | string[] | Specific problems detected (only when `unhealthy`)             |

**Status codes**: `200` for `ok`/`starting`, `503` for `unhealthy`.

**Unhealthy conditions** (any triggers 503):
- Block processing stale (no new block for 2x chain block interval)
- Transactions broadcast but unconfirmed
- Signature hashes waiting on TSS

---

### `POST /`

Register a new chain-to-VSC address mapping. Generates a P2WSH address that the bot will monitor for deposits.

**Request body** (JSON):

| Field         | Type   | Required | Description                       |
| ------------- | ------ | -------- | --------------------------------- |
| `instruction` | string | yes      | VSC address or instruction string |

**How it works**:

1. Fetches primary and backup public keys from the VSC contract via GraphQL.
2. Creates a SHA-256 hash of the instruction as a tag.
3. Generates a P2WSH address with a script containing:
   - **Primary path**: `OP_IF <primaryPubKey> OP_CHECKSIGVERIFY <tag>`
   - **Backup path**: `OP_ELSE <csvBlocks> OP_CHECKSEQUENCEVERIFY OP_DROP <backupPubKey> OP_CHECKSIG OP_ENDIF`
   - CSV timeout is 4320 blocks (~30 days) on mainnet, 2 blocks on testnet.
4. Stores the BTC address and instruction in MongoDB.
5. Checks the address for any pre-existing transactions.

**Status codes**: `201` created, `409` mapping already exists, `400` invalid request, `500` server error.

---

### `POST /sign`

Submit backup signatures for pending transactions (used when the primary TSS signing path is unavailable).

**Request body** (JSON):

| Field                    | Type   | Required | Description                      |
| ------------------------ | ------ | -------- | -------------------------------- |
| `tx_id`                  | string | yes      | Transaction ID                   |
| `signatures`             | array  | yes      | Array of signature objects (>=1) |
| `signatures[].index`     | number | yes      | Input index to sign              |
| `signatures[].signature` | string | yes      | Hex-encoded signature            |

**How it works**:

1. Looks up the pending transaction by `tx_id`.
2. For each provided signature, marks the corresponding input as backup-signed.
3. Increments the transaction's `currentSignatures` counter.

**Status codes**: `200` signatures applied, `400` invalid request, `404` transaction not found, `500` server error.

## Configuration

### CLI Flags

| Flag              | Default   | Description                                                     |
| ----------------- | --------- | --------------------------------------------------------------- |
| `-init`           | `false`   | Initialize config files and exit                                |
| `-debug`          | `false`   | Enable debug logging                                            |
| `-network`        | `mainnet` | VSC network: `mainnet`, `testnet`, or `devnet`                  |
| `-chain`          | `btc`     | Chain to monitor: `btc`, `ltc`, `dash`, `doge`, `bch`           |
| `-chain-network`  | `mainnet` | Chain network: `mainnet`, `testnet`, `regtest`                  |
| `-port`           | `0`       | HTTP port override (0 = use config file value, default 8000)    |
| `-data-dir`       | `data`    | Data directory for config files and storage                     |

### Mapping Bot Config (`config.json`)

Defined in `mapper/config.go`. Created on first run with `-init`.

| Field                  | Type   | Default                           | Description                                                                                          |
| ---------------------- | ------ | --------------------------------- | ---------------------------------------------------------------------------------------------------- |
| `ContractId`           | string | `ADD_MAPPING_CONTRACT_ID`         | The VSC contract ID for the chain's mapping contract. Must be set before running.                    |
| `ConnectedGraphQLAddr` | string | `0.0.0.0:8080`                    | Address of the VSC node's GraphQL API for contract state, public keys, and TSS signatures.           |
| `HttpPort`             | uint16 | `8000`                            | Port the mapping bot's HTTP server listens on.                                                       |
| `SignApiKey`           | string | `""`                              | API key for the `/sign` endpoint. If empty, `/sign` is disabled.                                     |
| `BotEthPrivKey`        | string | auto-generated on first run       | secp256k1 key used to sign VSC L2 transactions (did:pkh:eip155 caller). Used when a payload exceeds Hive's 8192-byte custom_json cap — e.g. large BTC txs with many inputs or witness data. |

### L2 Signing Key Setup (first run)

Small map calls (<~7 KB envelope) are broadcast via Hive custom_json using
`HiveActiveKey` as before — no operator action required.

When a map payload would exceed Hive's 8192-byte `custom_json` limit, the bot
auto-routes through the VSC L2 transaction pool (16384-byte cap) using the
`BotEthPrivKey` above. On first run, the bot generates a fresh key, logs the
derived `did:pkh:eip155:…` address, and persists the key. The derived DID
needs a small HBD balance to pay for RCs (DIDs have no free allotment, unlike
`hive:` accounts). Transfer ~1 HBD on VSC L2 to the logged DID — that buys
roughly 500 large-payload map calls. Top up when the balance drops.

### Other Config Files

The bot also loads these standard VSC config files from the data directory:

- **Identity config** (`identityConfig.json`): Hive account name and posting key used to broadcast `vsc.call` custom JSON operations.
- **Hive config** (`hiveConfig.json`): List of Hive API node URIs.
- **DB config** (`dbConfig.json`): MongoDB connection string.

## Running All Chains

Run one instance per chain, each with its own data directory and port:

```bash
# BTC on port 8000
./mapping-bot -chain btc -port 8000 -data-dir data/btc &

# LTC on port 8001
./mapping-bot -chain ltc -port 8001 -data-dir data/ltc &

# DASH on port 8002
./mapping-bot -chain dash -port 8002 -data-dir data/dash &

# DOGE on port 8003
./mapping-bot -chain doge -port 8003 -data-dir data/doge &

# BCH on port 8004
./mapping-bot -chain bch -port 8004 -data-dir data/bch &
```

Each instance gets its own MongoDB database (auto-named `btc-mapping-bot`, `ltc-mapping-bot`, etc.), config files, and health endpoint. Initialize configs first with `-init`:

```bash
for chain in btc ltc dash doge bch; do
  ./mapping-bot -chain $chain -data-dir data/$chain -init
done
# Then edit each data/<chain>/config.json to set ContractId
```
