# VSC Testnet Stress Test – TSS Reshare

Guide to run the **fix/tss-reshare-stability** branch on the Hive/VSC testnet with 2+ nodes to stress-test TSS reshare in the wild.

## Prerequisites

- **Testnet API**: [Techcoderx testnet](https://testnet.techcoderx.com/) (explorer: https://testnet.techcoderx.com/explorer/@magic-man)
- **Hive testnet RPC** for nodes: set `HIVE_API` to the testnet legacy JSON-RPC URL (e.g. `https://testnet.techcoderx.com` or the URL that serves `condenser_api`).
- **Accounts**: One Hive testnet account per VSC node. You can create accounts via the testnet (e.g. using @magic-man or the API) and fund them.
- **Keys**: Active key (WIF) for each node account, stored in `.env` or loaded by **clive** for broadcasting the registration transaction.
- **clive**: Preferred for wallet actions (transfer + custom_json). Install from [openhive-network/clive](https://github.com/openhive-network/clive).

## 1. Register each VSC node on the testnet

Each node must be registered with a **transfer** to the gateway plus **vsc.consensus_stake** custom_json.

### Operation shape

- **Transfer**: From your funding account to the gateway (testnet gateway may be `vsc.gateway` or `vsc.testnet` – use the one your testnet uses).
- **custom_json** `vsc.consensus_stake`: Same transaction, with:
  - `from`: `hive:<funding-account>`
  - `to`: `hive:<node-account>` (the account this node will run as)
  - `asset`: `"hive"`
  - `net_id`: `"vsc-testnet"`
  - `amount`: e.g. `"18000.000"` (must meet testnet consensus minimum, e.g. 1 HIVE; 18k is a typical stake).

Example (one node, account `milo.magi`, funded by `milo-hpr`):

```json
{
  "operations": [
    {
      "type": "transfer_operation",
      "value": {
        "amount": { "amount": "18000000", "nai": "@@000000021", "precision": 3 },
        "from": "milo-hpr",
        "memo": "",
        "to": "vsc.gateway"
      }
    },
    {
      "type": "custom_json_operation",
      "value": {
        "id": "vsc.consensus_stake",
        "json": "{\"from\":\"hive:milo-hpr\",\"to\":\"hive:milo.magi\",\"asset\":\"hive\",\"net_id\":\"vsc-testnet\",\"amount\":\"18000.000\"}",
        "required_auths": ["milo-hpr"],
        "required_posting_auths": []
      }
    }
  ]
}
```

Use the script **`scripts/testnet-register-node.sh`** to generate the operations JSON for your accounts, then broadcast with **clive** (or any Hive RPC client) against the **testnet** node:

```bash
./scripts/testnet-register-node.sh <from_account> <to_node_account> [amount_hive] [gateway]
# Writes /tmp/vsc-register-ops.json; broadcast with clive using the from_account active key.
```

### Consensus minimum

- Testnet: **1 HIVE** (1.000) minimum stake.
- Use the same gateway account name as your testnet (e.g. `vsc.gateway` or `vsc.testnet`).

## 2. Run 2+ VSC nodes (testnet + TSS branch)

Each node needs:

- Its own **data directory** and **identity** (`HiveUsername` = node account, `HiveActiveKey` = that account’s active WIF).
- **Testnet** mode and **testnet Hive API**.
- **Build from branch** `fix/tss-reshare-stability` so TSS reshare changes are included.

### Environment (per node)

| Variable        | Example (node 1) | Example (node 2) |
|-----------------|-------------------|-------------------|
| `VSC_NETWORK`   | `testnet`        | `testnet`        |
| `VSC_DATA_DIR`  | `data`           | `data2`          |
| `VSC_GRAPHQL_PORT` | `6600`        | `6601`           |
| `VSC_P2P_PORT`  | `6610`           | `6611`           |
| `MONGO_PORT`    | `6620`           | `6621`           |
| `HIVE_API`      | testnet RPC URL  | testnet RPC URL  |

Set `HIVE_API` to the testnet legacy RPC (e.g. `https://testnet.techcoderx.com` or the exact condenser API URL for techcoderx testnet).

### Identity (per node)

For each node, create or edit `$VSC_DATA_DIR/config/identityConfig.json`:

```json
{
  "BlsPrivKeySeed": "",
  "HiveActiveKey": "<ACTIVE_KEY_WIF_FOR_THIS_NODE_ACCOUNT>",
  "HiveUsername": "<NODE_ACCOUNT_NAME>",
  "Libp2pPrivKey": ""
}
```

Leave `BlsPrivKeySeed` and `Libp2pPrivKey` empty; the node can generate them on first run.

### Docker Compose (multiple nodes)

From the repo root (branch `fix/tss-reshare-stability`).

**Option A – Two nodes with one compose file**

```bash
export HIVE_API=https://testnet.techcoderx.com
# Create identity for node 1 and node 2
mkdir -p data/config data2/config
# Edit data/config/identityConfig.json  (HiveUsername + HiveActiveKey for node 1)
# Edit data2/config/identityConfig.json  (HiveUsername + HiveActiveKey for node 2)

docker compose -f docker-compose.yml -f docker-compose.testnet.yml --profile init run --rm init
docker compose -f docker-compose.yml -f docker-compose.testnet.yml -f docker-compose.testnet-2nodes.yml up -d
```

Node 1: GraphQL 6600, P2P 6610, data in `./data`.  
Node 2: GraphQL 6601, P2P 6611, data in `./data2`.

**Option B – Node 2 as a separate stack**

```bash
export VSC_DATA_DIR=data2
export VSC_GRAPHQL_PORT=6601
export VSC_P2P_PORT=6611
export MONGO_PORT=6621
export VSC_NETWORK=testnet
export HIVE_API=https://testnet.techcoderx.com
docker compose -f docker-compose.yml -f docker-compose.testnet.yml -p vsc-testnet-2 up -d
```

Ensure `data2/config/identityConfig.json` exists with the second node’s account and active key. For a second stack you must override the app volume to use `./data2` (e.g. copy docker-compose.testnet.yml and set `volumes: - ./data2:/home/app/app/data2` for the app service).

Repeat with `data3`, ports 6602/6612/6622, etc., for more nodes.

### Bootstrap peers (optional)

If you have a stable testnet node, add its multiaddr to `TESTNET_BOOTSTRAP` in `modules/common/system-config/bootstrap.go` so new nodes can discover it. Example:

```go
var TESTNET_BOOTSTRAP = []string{
    "/ip4/<IP>/tcp/<P2P_PORT>/p2p/<PEER_ID>",
    "/ip4/<IP>/udp/<P2P_PORT>/quic-v1/p2p/<PEER_ID>",
}
```

## 3. Deploying via SSH (e.g. your server)

Deployment uses **SSH-MCP** (or plain SSH) to prepare the server and run the 2-node stack.

### 3.1 One-command deploy from your machine (rsync + SSH)

From the **repo root** on a machine that has SSH access to the server (and the repo code):

```bash
# Sync code and start 2 nodes on the remote (replace naf with your SSH host)
./scripts/deploy-testnet-rsync.sh naf
# Or: SSH_HOST=user@my.server ./scripts/deploy-testnet-rsync.sh
```

This rsyncs the repo to `~/vsc/` on the remote and runs `go-vsc-node/scripts/start-testnet-2nodes.sh` there (init if needed, then `docker compose up -d`). Set `HIVE_API` in the environment or it defaults to `https://testnet.techcoderx.com`.

### 3.2 Remote prepared via SSH-MCP

The server can be prepared via **SSH-MCP** so that once the code is present, starting is a single command:

- On the remote, `~/vsc/go-vsc-node/` has been created with:
  - `data/config/identityConfig.json` and `data2/config/identityConfig.json` (placeholder; replace with real `HiveUsername` and `HiveActiveKey` per node).
  - `.env.testnet` with `HIVE_API=https://testnet.techcoderx.com` (source it or export before running compose).
- To start after code is on the server (e.g. after rsync or clone):

  ```bash
  cd ~/vsc/go-vsc-node
  source .env.testnet   # or export HIVE_API=...
  ./scripts/start-testnet-2nodes.sh
  ```

### 3.3 Manual SSH deployment

- Clone or rsync the repo on the server and checkout `fix/tss-reshare-stability`.
- Build the app (and Docker image if you use Docker) on the server.
- For each node:
  - Create a data dir and `config/identityConfig.json` with the right `HiveUsername` and `HiveActiveKey`.
  - Run with `VSC_NETWORK=testnet`, `HIVE_API=<testnet RPC>`, and the same port/data dir scheme as above.
- Do **not** put WIF keys in the repo or in scripts; load them from a local `.env` or clive key store when broadcasting the registration tx.

## 4. Stress-testing TSS

- With 2+ nodes registered and running the TSS-enabled build:
  - Let the network run and trigger keygen/reshare (rotation interval or manual triggers depending on your testnet).
  - Watch logs for `[TSS]` (reshare, timeouts, blame, retries, metrics).
- Optional: stop one node during a reshare to verify timeout, blame, and retry behavior from the plan.

## 5. Quick reference – testnet URLs and gateways

- Explorer: https://testnet.techcoderx.com/explorer/@magic-man  
- Testnet RPC: set `HIVE_API` to the techcoderx testnet legacy JSON-RPC URL.
- Gateway: use the gateway account used by your testnet for deposits (e.g. `vsc.gateway` or `vsc.testnet`).
- Net ID in `vsc.consensus_stake`: `vsc-testnet`.
