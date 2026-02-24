# Remote devnet bootstrap (Option A)

Run account creation on the SSH server where `hive-devnet` (Tin Toy) is running, then use the 3-node stack locally with `HIVE_API` pointing at the server.

## Restart devnet with known keypair (recommended)

To get a **fresh chain** seeded with deterministic Tin Toy keys (fixes initminer/tnman signing issues), on the **server** run:

```bash
cd ~/vsc/go-vsc-node
./scripts/run-restart-and-bootstrap-on-server.sh
```

Sync the scripts from local first (see below), or copy at least:
`restart-devnet-with-known-keys.sh`, `run-restart-and-bootstrap-on-server.sh`, `devnet-create-accounts.sh`, `devnet-ensure-tnman.py`, `testnet-multinode-setup.py`, `docker-compose.hive-devnet-only.yml`.

## One-shot (when SSH works)

From your **local** `go-vsc-node` directory:

```bash
# Use your server hostname or IP (and user if not ubuntu)
export SSH_HOST=157.180.53.238
export SSH_USER=ubuntu
./scripts/sync-and-bootstrap-remote.sh
```

That script will:
1. `scp` the needed scripts to the server
2. `ssh` in and run `./scripts/run-devnet-bootstrap-on-server.sh` (ensure tnman + create magic-node1..3)
3. `rsync` `testnet/` back from server to local

To **restart devnet with known keys** then bootstrap, sync the new scripts and run on the server:
`./scripts/run-restart-and-bootstrap-on-server.sh` (or add that to sync-and-bootstrap-remote.sh).

Then start the 3-node stack:

```bash
export HIVE_API=http://YOUR_SERVER_IP:8090
docker compose -f docker-compose.devnet-3nodes-remote.yml up -d
```

## If SSH from your machine isn’t set up yet

1. **Copy your SSH key to the server** (once):
   ```bash
   ssh-copy-id ubuntu@157.180.53.238
   ```
   (Replace with your server hostname or IP.)

2. **Or do it in two steps:**

   **On the server** (after you’ve copied the repo or at least these files into `~/vsc/go-vsc-node/`):
   - `scripts/devnet-ensure-tnman.py`
   - `scripts/devnet-create-accounts.sh`
   - `scripts/run-devnet-bootstrap-on-server.sh`
   - `scripts/testnet-multinode-setup.py`
   - (optional) `scripts/restart-devnet-with-known-keys.sh` + `scripts/run-restart-and-bootstrap-on-server.sh` + `docker-compose.hive-devnet-only.yml` for a fresh chain with known keys

   Then SSH in and run (bootstrap only, or full restart + bootstrap):
   ```bash
   cd ~/vsc/go-vsc-node
   ./scripts/run-devnet-bootstrap-on-server.sh
   # Or restart devnet with known keypair then seed:
   ./scripts/run-restart-and-bootstrap-on-server.sh
   ```

   **On your machine**, pull `testnet/` and start the stack:
   ```bash
   rsync -avz ubuntu@YOUR_SERVER:vsc/go-vsc-node/testnet/ go-vsc-node/testnet/
   export HIVE_API=http://YOUR_SERVER_IP:8090
   docker compose -f docker-compose.devnet-3nodes-remote.yml up -d
   ```
