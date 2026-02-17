# Magi/VSC Testnet Node

Run a VSC node on the **testnet** with configurable ports (e.g. 6600–6700) so you can run several test nodes side by side.

## Quick start

1. **Ports and network** are set via environment (defaults for testnet compose: GraphQL `6600`, P2P `6610`, Mongo `6620`).

2. **Identity (Hive account)** must be set in `data/config/identityConfig.json` before starting:
   - `HiveUsername`: your Hive testnet account (e.g. `magic-tester`)
   - `HiveActiveKey`: the **private key in WIF format** for that account (active key)

   The other fields (`BlsPrivKeySeed`, `Libp2pPrivKey`) are generated on first run if the file doesn’t exist.

3. **Start one testnet node** (ports 6600, 6610, 6620):

   ```bash
   docker compose -f docker-compose.yml -f docker-compose.testnet.yml up -d
   ```

   Or with explicit env:

   ```bash
   export VSC_GRAPHQL_PORT=6600
   export VSC_P2P_PORT=6610
   export MONGO_PORT=6620
   docker compose -f docker-compose.yml -f docker-compose.testnet.yml up -d
   ```

4. **Run multiple test nodes** by changing ports and project name:

   ```bash
   # Node 2
   VSC_GRAPHQL_PORT=6601 VSC_P2P_PORT=6611 MONGO_PORT=6621 \
     docker compose -f docker-compose.yml -f docker-compose.testnet.yml -p vsc-testnet-2 up -d
   ```

   Use a separate `data` directory per node (e.g. `./data2`) and set `VSC_DATA_DIR=data2` so each node has its own config and DB.

## Environment variables

| Variable           | Default   | Description |
|--------------------|-----------|-------------|
| `VSC_NETWORK`      | `mainnet` | Set to `testnet` for testnet. |
| `VSC_DATA_DIR`     | `data`    | Directory for config and TSS keys (identity in `VSC_DATA_DIR/config/`). |
| `VSC_GRAPHQL_PORT` | `8080`    | GraphQL API port (e.g. `6600` for first testnet node). |
| `VSC_P2P_PORT`     | `10720`   | P2P TCP/UDP port (e.g. `6610` for first testnet node). |
| `MONGO_URL`        | -         | MongoDB connection string (compose sets `mongodb://db:27017`). |

## Identity config template

Ensure `data/config/identityConfig.json` exists with your Hive testnet account. Example (replace with your WIF and username):

```json
{
  "BlsPrivKeySeed": "",
  "HiveActiveKey": "YOUR_ACTIVE_KEY_WIF",
  "HiveUsername": "magic-tester",
  "Libp2pPrivKey": ""
}
```

If the file is missing, the node creates it with defaults; you must then edit it to set `HiveUsername` and `HiveActiveKey` (WIF) and restart. The account `magic-tester` should use the active key from the key pairs created for testnet registration.

## Init and run

1. First run with init profile to initialise DB (if needed):

   ```bash
   docker compose -f docker-compose.yml -f docker-compose.testnet.yml --profile init run --rm init
   ```

2. Then start the node:

   ```bash
   docker compose -f docker-compose.yml -f docker-compose.testnet.yml up -d
   ```

GraphQL API: `http://localhost:6600/api/v1/graphql` (or the port you set).
