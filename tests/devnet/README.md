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
docker-compose.nodes.yml    Generated per-test (magi-1 ... magi-N)
.env                        Generated per-test (ports, paths, node count)

Dockerfile.devnet           Multi-binary image with iptables
```

All runtime data goes into `.devnet/<random>/` under the repo root and is
cleaned up on `Stop()` via `sudo rm -rf` (HAF creates postgres-owned files).

### Startup sequence

`Devnet.Start()` automates the full manual devnet setup:

1. Create HAF data directories, write `config.ini` and `pgtune.conf`
2. Write `.env` and `docker-compose.nodes.yml`
3. Build devnet Docker image (parallel with HAF startup)
4. Start HAF + MongoDB, wait for healthy
5. `docker compose run --rm devnet-setup` (Hive accounts, node configs, staking)
6. Start all magi nodes
7. Stop genesis node, run `genesis-elector`, restart it
8. Publish price feed + transfer TBD/TESTS to first witness for contract fees

## TSS readiness model

TSS reshare uses **off-chain gossip readiness**: each node signs a BLS
attestation ("I am ready for reshare of key X at block Y") and gossips it
via libp2p pubsub. All honest connected nodes converge to the same
attestation set, which is used to build deterministic party lists. No
chain transactions are needed for readiness signaling.

The gossip protocol has two phases:

- **Announce** (blocks B-offset to B-settle): nodes sign and broadcast attestations
- **Settle** (last few blocks before reshare): no new attestations accepted, existing ones continue to propagate

This replaces the earlier on-chain `vsc.tss_ready` approach which flooded
the chain with transactions.

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

| Field              | Default                 | Description                                        |
| ------------------ | ----------------------- | -------------------------------------------------- |
| `Nodes`            | 5                       | Number of magi nodes (min 4)                       |
| `GQLBasePort`      | 18080                   | Host port for magi-1 GraphQL (increments per node) |
| `P2PBasePort`      | 10720                   | P2P port for magi-1 (increments per node)          |
| `MongoPort`        | 18057                   | Host port for MongoDB                              |
| `HivePort`         | 18091                   | Host port for Hive RPC                             |
| `LogLevel`         | `"error,tss=trace"`     | Magi log level                                     |
| `ElectionInterval` | 0 (devnet default: 40)  | Override election interval in blocks               |
| `GenesisNode`      | 5                       | Which node runs the genesis election               |
| `KeepRunning`      | false                   | Don't tear down containers on Stop()               |
| `DataDir`          | auto (`.devnet/<rand>`) | Override data directory                            |
| `ProjectName`      | auto                    | Docker compose project name                        |
| `OldCodeSourceDir` | `""`                    | Path to old version repo for multi-version tests   |
| `OldCodeNodes`     | `[]`                    | Which nodes (1-indexed) run old code               |

## Devnet API

### Lifecycle

| Method                       | Description                                    |
| ---------------------------- | ---------------------------------------------- |
| `New(cfg) (*Devnet, error)`  | Create a devnet (does not start containers)    |
| `Start(ctx) error`           | Full startup sequence (build, HAF, nodes, etc) |
| `Stop() error`               | Tear down everything + clean data directory    |
| `StartNode(ctx, node) error` | Start a single stopped node                    |

### Endpoints

| Method              | Returns                                  |
| ------------------- | ---------------------------------------- |
| `GQLEndpoint(node)` | `http://localhost:<port>/api/v1/graphql` |
| `HiveRPCEndpoint()` | `http://localhost:<port>`                |
| `MongoURI()`        | `mongodb://localhost:<port>`             |

### Contracts

| Method                      | Description                               |
| --------------------------- | ----------------------------------------- |
| `BuildCallTssContract(ctx)` | Build call-tss WASM via Docker TinyGo     |
| `DeployContract(ctx, opts)` | Deploy a .wasm file to the running devnet |

### Network partitioning

| Method                                          | Description                                    |
| ----------------------------------------------- | ---------------------------------------------- |
| `Partition(ctx, nodeA, nodeB)`                  | Block all traffic between two specific nodes   |
| `Heal(ctx, nodeA, nodeB)`                       | Restore traffic between two partitioned nodes  |
| `Disconnect(ctx, node)`                         | Isolate a node from all other containers       |
| `Reconnect(ctx, node)`                          | Restore all traffic to a disconnected node     |
| `AddOutboundLatency(ctx, from, to, ms, jitter)` | One-directional delay from one node to another |
| `RemoveLatency(ctx, node)`                      | Remove all tc latency rules from a node        |

These use iptables/tc rules inside the containers (requires `NET_ADMIN` capability,
which is set automatically in the generated compose file).

### Debugging

| Method               | Description                             |
| -------------------- | --------------------------------------- |
| `Logs(ctx, service)` | Fetch docker compose logs for a service |
| `DataDir()`          | Path to the data directory              |
| `ComposeFile()`      | Path to docker-compose.yml              |

## File layout

```
tests/devnet/
  config.go               Config struct, defaults, validation
  devnet.go               Devnet lifecycle (Start, Stop, compose helpers)
  compose.go              .env + nodes override generation
  hive.go                 HAF data directory setup, embedded config files
  contract.go             Contract build + deploy
  funding.go              Account funding (price feed + TBD transfer)
  partition.go            Network partition (iptables/tc-based)
  mongo.go                MongoDB query helpers (commitments, elections, keys)
  tss_helpers_test.go     Shared test helpers (startDevnet, insertTssKey, etc)
  devnet_test.go          Infrastructure smoke tests
  docker-compose.yml      Static compose (HAF, Mongo, setup services)
  Dockerfile.devnet       Multi-binary image with iptables + tc
  testdata/
    config.ini            Hived config (initminer, small shared memory)
    pgtune.conf           PostgreSQL tuning for tests
  contracts/
    call-tss/             TSS test contract (Go/TinyGo -> WASM)
```

## Test inventory

Run a single test:

```bash
go test -v -run TestTSSReshareHappyPath -timeout 25m ./tests/devnet/
```

Run all integration tests (sequential, ~3-4 hours):

```bash
go test -v -run 'TestTSS|TestBlame|TestEdge|TestGossip' -timeout 300m ./tests/devnet/
```

### Infrastructure / smoke tests

| Test                    | File             | Description                                                                                                          |
| ----------------------- | ---------------- | -------------------------------------------------------------------------------------------------------------------- |
| `TestDevnetSetup`       | `devnet_test.go` | Spins up full devnet (HAF, MongoDB, magi nodes), verifies Hive RPC is reachable. Basic infrastructure smoke test.    |
| `TestDeployCallTss`     | `devnet_test.go` | Builds the call-tss WASM contract, starts devnet, deploys it. Validates the contract deployment pipeline end-to-end. |
| `TestEnvFileGeneration` | `devnet_test.go` | Unit test: verifies `.env` file is generated with correct variable substitutions. No Docker needed.                  |
| `TestComposeFileExists` | `devnet_test.go` | Unit test: verifies static `docker-compose.yml` contains all expected service definitions and variables.             |
| `TestNodesOverride`     | `devnet_test.go` | Unit test: verifies the generated nodes override YAML contains the right number of magi services.                    |
| `TestHAFDataDirs`       | `devnet_test.go` | Unit test: verifies HAF data directory structure (config.ini, pgtune.conf, pgdata dirs) is created correctly.        |

### Reshare happy path

| Test | File | Description |
| ---- | ---- | ----------- |

### Readiness gate

| Test                                    | File                | Description                                                                                                                                                                                                                                                                        |
| --------------------------------------- | ------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `TestTSSOfflineNodeExcludedByReadiness` | `readiness_test.go` | Disconnects a node before the gossip readiness window so it never sends an attestation. The node is excluded from the party list entirely — no blame needed. Reshare succeeds with the remaining nodes.                                                                            |
| `TestTSSFalseReadinessProducesBlame`    | `readiness_test.go` | Node sends a gossip attestation then disconnects before the reshare protocol starts. It's in the party list but doesn't send btss messages. All online nodes see the same WaitingFor() result, produce identical CIDs, BLS succeeds — blame lands targeting the disconnected node. |

### Blame cycle / accumulation

| Test                                | File                  | Description                                                                                                                                                                                                                                                   |
| ----------------------------------- | --------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `TestTSSBlameExcludesNodeNextCycle` | `blame_cycle_test.go` | Disconnects a node, waits for blame to land on-chain, then verifies the blamed node is excluded from the party list in the next reshare cycle — either via blame-based exclusion or readiness gate (both are valid).                                          |
| `TestTSSBlameEpochDecode`           | `blame_cycle_test.go` | Causes blame at epoch 0, then unstakes a node to change election membership at epoch 1. Verifies blame bitset is decoded against epoch 0's election (6 members), not epoch 1's (5 members with shifted positions). Wrong decode would exclude the wrong node. |
| `TestTSSBlameAccumulation`          | `blame_cycle_test.go` | Blames node 3 in cycle 1 and node 4 in cycle 2, then verifies BOTH are excluded in cycle 3. The old code read only the most recent blame via `GetCommitmentByHeight`; the fix reads all blames in the BLAME_EXPIRE window.                                    |

### Blame tests (contract-based keygen)

| Test                            | File                    | Description                                                                                                                                                                                                        |
| ------------------------------- | ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `TestBlameExcludesNodeOnRetry`  | `blame_test.go`         | Deploys call-tss contract, triggers keygen via contract call, disconnects a node before reshare, waits for blame, verifies exclusion on retry. Uses the full contract-based keygen path (not MongoDB seeding).     |
| `TestBlameSSIDMismatch`         | `blame_ssid_test.go`    | Verifies SSID mismatch detection under fault conditions with the full contract-based setup. Tests that nodes detect and report SSID divergence when party lists don't match.                                       |
| `TestBlamePartialLatency`       | `blame_partial_test.go` | 2 of 7 nodes have asymmetric outbound latency (6s). Tests that gossip readiness produces deterministic party lists despite non-deterministic network conditions. Must recover within 1 epoch.                      |
| `TestBlameBidirectionalLatency` | `blame_bidir_test.go`   | Bidirectional latency variant — verifies that healthy nodes are NOT incorrectly blamed when slow nodes' delayed responses affect traffic in both directions. Only the actual slow nodes should appear as culprits. |

### Disconnected node

| Test                        | File                       | Description                                                                                                                                                                                                                                                                   |
| --------------------------- | -------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `TestBlameDisconnectedNode` | `blame_disconnect_test.go` | Fully disconnects a node (drops ALL traffic via iptables) after its gossip attestation has been sent. The node is in the party list but can't send or receive anything. Verifies blame targets only the disconnected node, and reshare succeeds without it on the next cycle. |

### Partition / recovery

| Test                          | File                         | Description                                                                                                                                                                                                                              |
| ----------------------------- | ---------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `TestTSSPartitionAndRecovery` | `partition_recovery_test.go` | Partitions 2 specific nodes from each other (both still connected to the other 3). Reshare should still succeed since messages can relay through connected nodes. Heals partition, verifies continued operation with no SSID mismatches. |

### Multi-version / gossip resilience

| Test                               | File                        | Description                                                                                                                                                                                                                                                                                                                                                                                    |
| ---------------------------------- | --------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `TestTSSMultiVersion`              | `multiversion_test.go`      | Runs 4 nodes on current code and 1 node on an older version (no gossip readiness support). Old node never sends attestations and is excluded from party lists. Old node does NOT crash. Validates graceful degradation.                                                                                                                                                                        |
| `TestGossipResilienceMultiVersion` | `gossip_resilience_test.go` | 7 nodes: 1 old code + 6 new code. Key is keygen'd with all 7 (old node holds a real key share). Then 1 new-code node is disconnected, another gets 5s outbound latency. Reshare must succeed with the remaining 5 nodes, resharing the key away from the old-code node and disconnected node simultaneously. Tests gossip convergence under combined multi-version + network fault conditions. |

### Full lifecycle

| Test                       | File                    | Description                                                                                                                                                                                               |
| -------------------------- | ----------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `TestTSSFullRecoveryCycle` | `recovery_full_test.go` | End-to-end lifecycle: keygen -> first reshare (all healthy) -> disconnect node -> blame lands -> reconnect node -> second reshare succeeds. Proves the complete recovery path works from start to finish. |

### Edge cases

| Test                            | File                 | Description                                                                                                                                                                                                                                                             |
| ------------------------------- | -------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `TestEdgeLeaderCrashDuringBLS`  | `edge_cases_test.go` | Stops the leader node mid-BLS signature collection after the reshare protocol completes. The `ask_sigs` pubsub message is one-shot with no retry — if the leader dies, the commitment is lost. Verifies the network recovers on the next cycle with a different leader. |
| `TestEdgePartitionNoQuorum`     | `edge_cases_test.go` | Splits 7 nodes into groups of 3 and 4. Neither group has btss quorum (need 5). Both halves fail all reshare attempts. After healing the partition, verifies the full network recovers cleanly.                                                                          |
| `TestEdgeNodeRestartMidCycle`   | `edge_cases_test.go` | Restarts a node (stop + start container) between its gossip attestation and the reshare trigger. Attestation exists but the node has no preparams, no P2P connections, no session state. Verifies blame targets the restarted node and reshare recovers.                |
| `TestEdgePreparamsExhaustion`   | `edge_cases_test.go` | Adds latency to one node causing 4+ consecutive blame cycles. Each failed reshare consumes one set of preparams. Verifies healthy nodes regenerate preparams fast enough between cycles and recover after the fault is removed.                                         |
| `TestEdgeKeygenBlameCrossEpoch` | `edge_cases_test.go` | Disconnects a node during keygen to produce blame at epoch 0. Waits for epoch 1 (different election). Verifies the keygen blame from epoch 0 doesn't interfere with reshare in epoch 1.                                                                                 |
| `TestEdgeFlappingNode`          | `edge_cases_test.go` | A node toggles between online and offline every reshare cycle for 4 cycles. Tests whether blame accumulation handles repeat offenders correctly, and whether the network can converge despite the oscillation pattern.                                                  |
| `TestEdgeSimultaneousRestart`   | `edge_cases_test.go` | All 7 nodes restart at the same time (simulating coordinated deployment). Tests CPU contention during simultaneous preparams generation, P2P reconnection timing, and whether the network recovers within 1-2 reshare cycles after all nodes are back.                  |
