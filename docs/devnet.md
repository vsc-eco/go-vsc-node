# Local Hive devnet (short-term)

Run a **local Hive devnet** and 3 VSC nodes for **TSS testing** (keygen/reshare need ≥3 nodes). No external testnet or RC needed.

## What runs

- **Tin Toy (tintoy)**: minimal Hive testnet in a container (~2000 accounts, RPC on 8090).
- **3 VSC nodes**: node1 (6600/6610), node2 (6601/6611), node3 (6602/6612), each with its own MongoDB.

## Quick start

```bash
cd go-vsc-node
./scripts/launch-devnet.sh
```

If the Tin Toy image pull fails (e.g. `tls: bad record MAC`), use the **native (no-Docker)** devnet below — it uses your local Hive source (e.g. `~/Repos/hive`) to build and run `hived` with the same key derivation as Tin Toy.

**Run TSS tests (no devnet required for unit tests):**

```bash
./scripts/run-tss-tests.sh
```

This starts the Hive devnet, creates 3 node accounts + gateway (using tnman), then starts 3 VSC nodes. Then:

- **Hive RPC:** http://localhost:8090  
- **Node1 GraphQL:** http://localhost:6600  
- **Node2 GraphQL:** http://localhost:6601  
- **Node3 GraphQL:** http://localhost:6602  

## Create accounts on the devnet

Tin Toy uses a fixed secret so you can derive keys for the authority account `tnman` (can sign for the testnet).

### 1. Get tnman active key

From the host (with devnet running):

```bash
docker exec hive-devnet get_dev_key tintoy active-tnman 2>/dev/null | tr -d '"'
```

Use the **private key** (starts with `5J` or `5K`) as the creator key.

### 2. Create node accounts and gateway

The helper script creates 3 node accounts by default (for TSS):

```bash
./scripts/devnet-create-accounts.sh
```

Or set `NUM_NODES` and run the Python setup manually (creator = tnman):

```bash
export HIVE_API=http://localhost:8090
export MAGIC_MAN_KEY="<active-tnman-key-from-step-1>"
export CREATOR=tnman
.venv/bin/python3 scripts/testnet-multinode-setup.py \
  --nodes 3 --api http://localhost:8090 --stake 1000 --transfer 5000 --output testnet
```

That populates `testnet/node1/`, `testnet/node2/`, `testnet/node3/` with identity configs. The 3-node compose mounts these dirs. Alternatively use **clive** against `http://localhost:8090` to create accounts and broadcast transfer + `vsc.consensus_stake`.

### 3. identityConfig for the VSC node

Ensure `data/config/identityConfig.json` exists with:

- **HiveUsername**: an account that exists on the devnet and has been staked (e.g. `magic-node1`).
- **HiveActiveKey**: that account’s active private key (WIF).

If you used the Python setup, it writes configs under `testnet/node1/config/`. Copy that to `data/config/`:

```bash
cp -r testnet/node1/config/identityConfig.json data/config/
```

For the 3-node stack, configs are in `testnet/node1`, `node2`, `node3`; the compose mounts them. Restart if you changed config: `docker compose -f docker-compose.devnet-3nodes.yml restart node1 node2 node3`.

## Tin Toy details

- **Default secret:** `tintoy` (used to derive keys).
- **Authority account:** `tnman` (can sign for accounts on the testnet).
- **Key derivation:** inside the container, `get_dev_key tintoy owner-tnman`, `active-tnman`, `posting-tnman`, `memo-tnman`.
- **Ports:** 8090 = JSON-RPC, 8091 = hived RPC, 8080 = condenser, 2001 = p2p.
- **Chain:** testnet (prefix may be `TST`; asset typically TESTS). The VSC node uses `HIVE_API`; no RC needed on this local devnet.

## Stop / reset

```bash
docker compose -f docker-compose.devnet-3nodes.yml down
# optional: remove DB volumes
docker compose -f docker-compose.devnet-3nodes.yml down -v
```

## Running the devnet without Docker

When Docker is unavailable (e.g. image pull fails) or you prefer to run Hive locally, use your **Hive source** (e.g. `~/Repos/hive` or `~/Repos/haf`’s hived) to build and run `hived` in testnet mode. The same **Tin Toy key derivation** (secret `tintoy`) is used so `tnman` and node accounts match the Docker flow.

### 1. Build and run hived

From `go-vsc-node`:

```bash
# Uses HIVE_SRC=${HIVE_SRC:-$HOME/Repos/hive}; builds hived + get_dev_key if needed
./scripts/run-devnet-native.sh
```

This builds `hived` and `get_dev_key` in `$HIVE_SRC/build` with `BUILD_HIVE_TESTNET=ON` (see [Repos/hive/doc/building.md](https://github.com/openhive-network/hive/blob/master/doc/building.md)), creates `testnet/hived-data/` with a minimal `config.ini`, and starts `hived` in the foreground (RPC on **8090**). Stop with Ctrl+C.

Optional env: `HIVE_SRC`, `HIVED_PATH`, `DATA_DIR`, `HTTP_PORT`, `GET_DEV_KEY`.

### 2. Create tnman (one-time, after first start)

In another terminal, after the node is up and producing blocks:

```bash
cd go-vsc-node
# Skeleton key = wit-block-signing-0 from get_dev_key
export INITMINER_KEY="$(./path/to/get_dev_key tintoy wit-block-signing-0 | jq -r '.[0].private_key')"
export HIVE_API=http://localhost:8090
# If get_dev_key is not on PATH:
export GET_DEV_KEY="$HOME/Repos/hive/build/programs/util/get_dev_key"
.venv/bin/python3 scripts/create-tnman-native.py
```

(Or set `HIVE_SRC` so the script finds `get_dev_key` under `$HIVE_SRC/build`.)

### 3. Create node accounts and start VSC nodes

Same as Docker flow: `devnet-create-accounts.sh` will use **local** `get_dev_key` when the Docker container is not running (it looks for `GET_DEV_KEY` or `$HIVE_SRC/build/programs/util/get_dev_key`).

```bash
export HIVE_API=http://localhost:8090
./scripts/devnet-create-accounts.sh
docker compose -f docker-compose.devnet-3nodes.yml up -d
```

So with a native hived you only run the Hive node yourself; the 3 VSC nodes can still run via Docker, or you can run them locally and point `HIVE_API` at `http://localhost:8090`.

### HAF / haf_api_node

If you use **HAF** (`~/Repos/haf`) or **haf_api_node** (`~/Repos/haf_api_node`), those setups are Docker-oriented (hived in containers). For a **no-Docker** devnet, building and running `hived` from **`~/Repos/hive`** as above is the straightforward path; you don’t need Postgres/HAF for the VSC node — it only needs the JSON-RPC (e.g. condenser_api) on port 8090.

## Hive devnet on a remote (SSH) server

You can run the Hive devnet on a separate machine (e.g. your SSH server) and run the 3-node VSC TSS stack locally, pointing at that remote RPC.

### 1. Start Hive devnet on the server

On the SSH server (with Docker), either:

- **Using the compose file** (if the repo is present):
  ```bash
  cd go-vsc-node
  docker compose -f docker-compose.hive-devnet-only.yml up -d
  ```
- **Without the repo** (Tin Toy only):
  ```bash
  docker run -d --name hive-devnet -p 8090:8090 -p 8091:8091 --restart unless-stopped inertia/tintoy:latest
  ```

Or from your local machine, run the helper on the server via SSH:
  ```bash
  ssh YOUR_SERVER 'cd vsc/go-vsc-node && ./scripts/start-hive-devnet-on-server.sh'
  ```
  (Sync the repo to the server first so `docker-compose.hive-devnet-only.yml` and the script exist.)

Ensure port **8090** is reachable from where you will run the 3-node stack (firewall / security group). Tin Toy may take 1–2 minutes after start before RPC accepts connections.

### 2. Create accounts (once) and start the 3-node stack locally

**Option A — Run account creation on the server (recommended)**  
On the server, the devnet script can **build and fund** tnman (create if missing, transfer from initminer), then create magic-node1..N. Sync the repo to the server, then:

```bash
# On the SSH server (where hive-devnet is running):
cd vsc/go-vsc-node   # or your path to the repo
./scripts/run-devnet-bootstrap-on-server.sh
```

This ensures tnman exists and is funded, creates the 3 node accounts, and writes `testnet/node1`, `node2`, `node3`. Then from your **local** machine:

```bash
# Rsync testnet/ from server to local go-vsc-node/testnet/
rsync -avz YOUR_SERVER:vsc/go-vsc-node/testnet/ go-vsc-node/testnet/

# Start the 3-node stack pointing at the server (use server IP so Docker can resolve):
export HIVE_API=http://YOUR_SERVER_IP:8090
docker compose -f docker-compose.devnet-3nodes-remote.yml up -d
```

**Option B — Run account creation locally**  
You need `get_dev_key` (e.g. from a local Hive build) so the script can derive tnman’s key when the devnet is remote:

```bash
export HIVE_API=http://YOUR_SERVER:8090
export GET_DEV_KEY="$HOME/Repos/hive/build/programs/util/get_dev_key"
./scripts/devnet-create-accounts.sh
docker compose -f docker-compose.devnet-3nodes-remote.yml up -d
```

Or use the all-in-one script (after the remote devnet is up and reachable):

```bash
export HIVE_API=http://YOUR_SERVER:8090
export GET_DEV_KEY="$HOME/Repos/hive/build/programs/util/get_dev_key"  # required for account creation
./scripts/launch-devnet-remote.sh
```

### 3. Summary

| Where        | What runs |
|-------------|-----------|
| SSH server  | Hive devnet (Tin Toy) on port 8090 |
| Local       | 3 VSC nodes + MongoDBs, `HIVE_API` = `http://SERVER:8090` |

- **Compose files:** `docker-compose.hive-devnet-only.yml` (server), `docker-compose.devnet-3nodes-remote.yml` (local).
- **Scripts:** `scripts/start-hive-devnet-on-server.sh` (run on server), `scripts/launch-devnet-remote.sh` (run locally with `HIVE_API` set).

## Single-node devnet

For one VSC node only (no TSS), use the single-node compose:

```bash
docker compose -f docker-compose.devnet.yml up -d
```

Create one account, then copy `testnet/node1/config/identityConfig.json` to `data/config/` and restart the app service.
