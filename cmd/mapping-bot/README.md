# VSC Mapping Bot

The mapping bot bridges Bitcoin and VSC by monitoring Bitcoin blocks for deposits to mapped addresses, then submitting corresponding mapping transactions to the VSC network via a Hive custom JSON operation.

## General Flow

1. **Startup**: Parse CLI flags, load configs (mapping bot, identity, Hive, DB), connect to MongoDB (database: `btc-mapping-bot`).
2. **HTTP Server**: Start an HTTP server (default port 8000) to accept mapping registration and signing requests.
3. **Main Loop** (every 1m on mainnet, 10s on testnet):
   - **Cleanup**: Delete address mappings older than 30 days (runs once per 24h).
   - **HandleUnmap**: Fetch pending transaction spends from the contract, check for TSS signatures, and broadcast any fully-signed Bitcoin transactions.
   - **Block Processing**: Fetch the next Bitcoin block via the mempool.space API, parse it for transactions to mapped addresses, generate merkle proofs, and call the VSC mapping contract on Hive.
   - **HandleExistingTxs**: Check for pre-existing transactions on any newly registered addresses.

### Architecture Diagram

```
                      ┌──────────────┐
  Users ──POST /──►   │  HTTP Server │
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
                    │ mempool.space API │                         │ Hive Blockchain  │
                    │ (BTC blocks/txs)  │                         │ (vsc.call ops)   │
                    └───────────────────┘                         └──────────────────┘
```

## HTTP Server Endpoints

### `GET /health`

Health check endpoint for monitoring.

**Response** (JSON):

| Field         | Type   | Description                                      |
| ------------- | ------ | ------------------------------------------------ |
| `status`      | string | `"starting"`, `"ok"`, or `"unhealthy"`           |
| `blockHeight` | number | Last processed Bitcoin block height              |
| `lastBlockAt` | string | ISO 8601 timestamp of last processed block       |
| `staleSecs`   | number | Seconds since last block (only when `unhealthy`) |

**Status codes**: `200` for `ok`/`starting`, `503` for `unhealthy` (no block processed in >20 minutes).

---

### `POST /`

Register a new BTC-to-VSC address mapping. Generates a P2WSH Bitcoin address that the bot will monitor for deposits.

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

| Flag           | Default   | Description                                                     |
| -------------- | --------- | --------------------------------------------------------------- |
| `-init`        | `false`   | Initialize config files and exit                                |
| `-debug`       | `false`   | Enable debug logging                                            |
| `-network`     | `mainnet` | VSC network: `mainnet`, `testnet`, or `devnet`                  |
| `-btc-network` | `mainnet` | Bitcoin network: `mainnet`, `testnet4`, `testnet3`, or `regnet` |
| `-data-dir`    | `data`    | Data directory for config files and storage                     |

### Mapping Bot Config (`config.json`)

Defined in `mapper/config.go`. Created on first run with `-init`.

| Field                  | Type   | Default                       | Description                                                                                              |
| ---------------------- | ------ | ----------------------------- | -------------------------------------------------------------------------------------------------------- |
| `ContractId`           | string | `ADD_BTC_MAPPING_CONTRACT_ID` | The VSC contract ID for the BTC mapping contract. Must be set to a valid contract ID before running.     |
| `ConnectedGraphQLAddr` | string | `0.0.0.0:8080`                | Address of the VSC node's GraphQL API used for querying contract state, public keys, and TSS signatures. |
| `HttpPort`             | uint16 | `8000`                        | Port the mapping bot's HTTP server listens on.                                                           |

### Other Config Files

The bot also loads these standard VSC config files from the data directory:

- **Identity config** (`identityConfig.json`): Hive account name and posting key used to broadcast `vsc.call` custom JSON operations.
- **Hive config** (`hiveConfig.json`): List of Hive API node URIs.
- **DB config** (`dbConfig.json`): MongoDB connection string.
