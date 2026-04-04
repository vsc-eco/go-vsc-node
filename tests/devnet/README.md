# Devnet Integration Tests

End-to-end tests that spin up a real Hive testnet (HAF), MongoDB, and N Magi
nodes in Docker containers. Unlike the `modules/tss/tss_test.go` tests that
mock the network layer with in-process libp2p, these tests exercise the full
stack including Hive block production, on-chain transactions, and real
container networking.

## Prerequisites

- Docker (with compose v2)
- ~4 GB free RAM (HAF + Mongo + N nodes)
- Docker TinyGo image (pulled automatically on first contract build)

## Quick start

```bash
# Basic devnet smoke test (~3 min after first build)
go test -v -run TestDevnetSetup -timeout 20m ./tests/devnet/

# Build + deploy call-tss contract (~5 min)
go test -v -run TestDeployCallTss -timeout 25m ./tests/devnet/

# Keep containers running after the test for manual inspection
DEVNET_KEEP=1 go test -v -run TestDevnetSetup -timeout 20m ./tests/devnet/

# Unit tests only (fast, no Docker)
go test -v -run 'TestEnvFile|TestComposeFile|TestHAFDataDirs|TestNodesOverride' ./tests/devnet/
```

## Architecture

```
docker-compose.yml          Static infrastructure (HAF, MongoDB, setup tools)
docker-compose.nodes.yml    Generated per-test (magi-1 … magi-N)
.env                        Generated per-test (ports, paths, node count)

Dockerfile.devnet           Builds magid + devnet-setup + genesis-elector +
                            contract-deployer + iptables (for partitioning)
```

All runtime data goes into `.devnet/<random>/` under the repo root and is
cleaned up on `Stop()` via `sudo rm -rf` (HAF creates postgres-owned files).

### Startup sequence

`Devnet.Start()` automates the full manual devnet setup:

1. Create HAF data directories, write `config.ini` and `pgtune.conf`
2. Write `.env` and `docker-compose.nodes.yml`
3. `docker compose build`
4. Start HAF + MongoDB, wait for healthy
5. `docker compose run --rm devnet-setup` (Hive accounts, node configs, staking)
6. Start all magi nodes
7. Stop genesis node, run `genesis-elector`, restart it
8. Publish price feed + transfer TBD/TESTS to first witness for contract fees

## Writing a new test

### Minimal template

```go
func TestMyFeature(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping devnet integration test in short mode")
    }
    requireDocker(t)

    cfg := DefaultConfig()
    cfg.Nodes = 7                    // more nodes if needed
    cfg.ElectionInterval = 20        // faster elections (20 blocks ~1 min)
    if os.Getenv("DEVNET_KEEP") != "" {
        cfg.KeepRunning = true
    }

    d, err := New(cfg)
    if err != nil {
        t.Fatalf("creating devnet: %v", err)
    }
    t.Cleanup(func() { d.Stop() })

    ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
    defer cancel()

    if err := d.Start(ctx); err != nil {
        t.Fatalf("starting devnet: %v", err)
    }

    // Your test logic here.
    // Available endpoints:
    //   d.GQLEndpoint(1)      "http://localhost:18080/api/v1/graphql"
    //   d.HiveRPCEndpoint()   "http://localhost:18091"
    //   d.MongoURI()          "mongodb://localhost:18057"
}
```

### With a contract

```go
func TestTssKeyCreate(t *testing.T) {
    // ... setup as above ...

    // Build the call-tss contract (cached after first run)
    wasmPath, err := BuildCallTssContract(ctx)
    if err != nil {
        t.Fatalf("building contract: %v", err)
    }

    if err := d.Start(ctx); err != nil {
        t.Fatalf("starting devnet: %v", err)
    }

    contractId, err := d.DeployContract(ctx, ContractDeployOpts{
        WasmPath: wasmPath,
        Name:     "call-tss",
    })
    if err != nil {
        t.Fatalf("deploying contract: %v", err)
    }

    // Now interact with the contract via Hive custom_json
    // or GraphQL to trigger TSS operations.
}
```

### With network partitioning

```go
func TestReshareWithPartition(t *testing.T) {
    // ... start devnet ...

    // Block traffic between node 1 and node 3
    if err := d.Partition(ctx, 1, 3); err != nil {
        t.Fatal(err)
    }

    // ... trigger reshare, observe behavior ...

    // Restore connectivity
    if err := d.Heal(ctx, 1, 3); err != nil {
        t.Fatal(err)
    }

    // Fully disconnect node 2 from all peers
    if err := d.Disconnect(ctx, 2); err != nil {
        t.Fatal(err)
    }

    // ... observe timeout behavior ...

    // Bring it back
    if err := d.Reconnect(ctx, 2); err != nil {
        t.Fatal(err)
    }
}
```

## Config reference

| Field              | Default                   | Description                                      |
|--------------------|---------------------------|--------------------------------------------------|
| `Nodes`            | 5                         | Number of magi nodes (min 4)                     |
| `GQLBasePort`      | 18080                     | Host port for magi-1 GraphQL (increments per node) |
| `P2PBasePort`      | 10720                     | P2P port for magi-1 (increments per node)        |
| `MongoPort`        | 18057                     | Host port for MongoDB                            |
| `HivePort`         | 18091                     | Host port for Hive RPC                           |
| `LogLevel`         | `"error,tss=trace"`       | Magi log level                                   |
| `ElectionInterval` | 0 (devnet default: 40)    | Override election interval in blocks             |
| `GenesisNode`      | 5                         | Which node runs the genesis election             |
| `KeepRunning`      | false                     | Don't tear down containers on Stop()             |
| `DataDir`          | auto (`.devnet/<rand>`)   | Override data directory                          |
| `ProjectName`      | auto                      | Docker compose project name                      |

## Devnet API

### Lifecycle

| Method                        | Description                                    |
|-------------------------------|------------------------------------------------|
| `New(cfg) (*Devnet, error)`   | Create a devnet (does not start containers)    |
| `Start(ctx) error`            | Full startup sequence (build, HAF, nodes, etc) |
| `Stop() error`                | Tear down everything + clean data directory    |

### Endpoints

| Method               | Returns                                   |
|----------------------|-------------------------------------------|
| `GQLEndpoint(node)`  | `http://localhost:<port>/api/v1/graphql`  |
| `HiveRPCEndpoint()`  | `http://localhost:<port>`                 |
| `MongoURI()`         | `mongodb://localhost:<port>`              |

### Contracts

| Method                            | Description                            |
|-----------------------------------|----------------------------------------|
| `BuildCallTssContract(ctx)`       | Build call-tss WASM via Docker TinyGo  |
| `DeployContract(ctx, opts)`       | Deploy a .wasm file to the running devnet |

### Network partitioning

| Method                        | Description                                        |
|-------------------------------|----------------------------------------------------|
| `Partition(ctx, nodeA, nodeB)`| Block all traffic between two specific nodes       |
| `Heal(ctx, nodeA, nodeB)`    | Restore traffic between two partitioned nodes      |
| `Disconnect(ctx, node)`      | Isolate a node from all other containers           |
| `Reconnect(ctx, node)`       | Restore all traffic to a disconnected node         |

These use iptables rules inside the containers (requires `NET_ADMIN` capability,
which is set automatically in the generated compose file).

### Debugging

| Method                   | Description                                 |
|--------------------------|---------------------------------------------|
| `Logs(ctx, service)`     | Fetch docker compose logs for a service     |
| `DataDir()`              | Path to the data directory                  |
| `ComposeFile()`          | Path to docker-compose.yml                  |

## File layout

```
tests/devnet/
  config.go               Config struct, defaults, validation
  devnet.go               Devnet lifecycle (Start, Stop, compose helpers)
  compose.go              .env + nodes override generation
  hive.go                 HAF data directory setup, embedded config files
  contract.go             Contract build + deploy
  funding.go              Account funding (price feed + TBD transfer)
  partition.go            Network partition (iptables-based)
  devnet_test.go          Test entry points
  docker-compose.yml      Static compose (HAF, Mongo, setup services)
  Dockerfile.devnet       Multi-binary image with iptables
  testdata/
    config.ini            Hived config (initminer, small shared memory)
    pgtune.conf           PostgreSQL tuning for tests
  contracts/
    call-tss/             TSS test contract (Go/TinyGo -> WASM)
```
