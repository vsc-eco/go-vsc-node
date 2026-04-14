# VSC Node — Documentation Index

The VSC node is the core of the VSC ecosystem, holding the code necessary for keeping the VSC network operational. It implements a layer-2 network on top of the Hive blockchain, providing threshold signing, contract execution, and bridging to external chains.

- **Deployment:** [vsc-eco/vsc-deployment](https://github.com/vsc-eco/vsc-deployment)
- **Community:** [Discord](https://discord.gg/F5Eqh2XYuY)

---

## Architecture Overview

```
Hive L1 (block stream)
        │
        ▼
Block Consumer (consumer.go)
        │
   ┌────┴────┐
   ▼         ▼
State    BlockTick
Engine   (TSS, elections, signing)
   │
   ▼
MongoDB (elections, commitments, keys, witnesses, ledger)
        │
        ▼
GraphQL API + P2P listener
```

**Core boot sequence** (`cmd/vsc-node/main.go`):
Init DB → Init P2P → Init DataLayer → Start Hive block consumer → Create StateEngine → Wire modules (TSS, elections, block producer, oracle, transactions) → Start GraphQL API + P2P listener

**Networks:** `vsc-mainnet`, `vsc-testnet`, `vsc-devnet` — per-network params in `modules/common/system-config/`

**Build:**
```bash
make              # Build all 5 binaries to ./build/
make magid        # Build vsc-node only
go test ./...     # Run all tests
```

---

## Binaries (`cmd/`)

| Binary | Description | Docs |
|--------|-------------|------|
| [vsc-node](../cmd/vsc-node/) | Main daemon (`magid`) — runs the full VSC node: Hive block consumer, state engine, all protocol modules, GraphQL API, P2P listener | — |
| [mapping-bot](../cmd/mapping-bot/) | Multi-chain bridge bot — monitors UTXO chains (BTC, LTC, DASH, DOGE, BCH), exposes HTTP API for mapping registration and backup signing | [README](../cmd/mapping-bot/README.md) |
| [contract-deployer](../cmd/contract-deployer/) | Deploys WASM contracts to the VSC network | — |
| [genesis-elector](../cmd/genesis-elector/) | Orchestrates the initial witness set and elections during devnet bring-up | — |
| [devnet-setup](../cmd/devnet-setup/) | Creates Hive accounts, per-node configs, and staking setup for a local devnet | — |

---

## Modules (`modules/`)

### Consensus & Cryptography

| Module | Description |
|--------|-------------|
| [tss/](../modules/tss/) | Threshold signature scheme — keygen, reshare/committee rotation, threshold signing, BLS quorum attestation, blame tracking, P2P message dispatch. The most complex module; see [TSS docs](tss.md) before touching. |
| [p2p/](../modules/p2p/) | libp2p networking — peer connections, gorpc unicast, pubsub broadcast, readiness checks |
| [announcements/](../modules/announcements/) | Node discovery and peer announcement |

### State & Block Processing

| Module | Description |
|--------|-------------|
| [state-processing/](../modules/state-processing/) | Central transaction and contract execution engine — processes all VSC operations from Hive blocks |
| [block-producer/](../modules/block-producer/) | Produces VSC L2 blocks |
| [transaction-pool/](../modules/transaction-pool/) | Mempool — buffers pending VSC transactions before inclusion in blocks |
| [hive/](../modules/hive/) | Hive L1 block streaming — connects to Hive node, feeds blocks to the block consumer |

### Contract Execution

| Module | Description |
|--------|-------------|
| [wasm/](../modules/wasm/) | WasmEdge runtime — executes WASM smart contracts |
| [contract/](../modules/contract/) | Contract execution context — wraps WASM runtime with VSC state access |

### Ledger & Economics

| Module | Description |
|--------|-------------|
| [ledger-system/](../modules/ledger-system/) | Token balance tracking and transfers |
| [rc-system/](../modules/rc-system/) | Resource credits — limits compute usage per account |

### Elections & Governance

| Module | Description |
|--------|-------------|
| [election-proposer/](../modules/election-proposer/) | Proposes witness elections on-chain |
| [gateway/](../modules/gateway/) | P2P multisig gateway |

### Data & Oracles

| Module | Description |
|--------|-------------|
| [oracle/](../modules/oracle/) | Oracle data feeds — fetches and attests external data for contracts |
| [data-availability/](../modules/data-availability/) | Data availability proofs — client/server for verifying block data |
| [aggregate/](../modules/aggregate/) | Aggregation logic for oracle and other data sources |

### API & Infrastructure

| Module | Description |
|--------|-------------|
| [gql/](../modules/gql/) | GraphQL API — schema at `modules/gql/schema.graphql`. Regenerate with `go run github.com/99designs/gqlgen generate`. Playground at server URL + `/sandbox`. |
| [db/](../modules/db/) | MongoDB collection definitions and query helpers |
| [config/](../modules/config/) | Node configuration loading and management |
| [common/](../modules/common/) | Network-specific system config for mainnet, testnet, and devnet |
| [start-status/](../modules/start-status/) | Node startup status tracking |
| [e2e/](../modules/e2e/) | End-to-end test helpers |

---

## Documentation

### Operations

| Doc | Description |
|-----|-------------|
| [testnet.md](testnet.md) | Testnet access — API endpoints, explorers, wallet setup (Metamask Snap, Ledger, Clive CLI) |
| [testnet-stress-test.md](testnet-stress-test.md) | Runbook for TSS reshare stress testing on testnet with 2+ nodes — registration, Docker Compose, deployment scripts, observation |
| [bitcoin-prune-node.md](bitcoin-prune-node.md) | Running a local Bitcoin prune node for bridge/mapping operations — `bitcoin.conf` and environment variables |

### Protocol & Architecture

| Doc | Description |
|-----|-------------|
| [tss.md](tss.md) | TSS subsystem overview — keygen, threshold signing, reshare/committee rotation, commitment publication, key lifecycle, blame/retry, P2P session safety, persistence model |
| [tss-vs-thorchain-maya.md](tss-vs-thorchain-maya.md) | Comparative analysis of VSC TSS vs THORChain/Maya — library stack, transport, architecture, blame handling, problem-statement analysis, recommended improvements |

### Testing

| Doc | Description |
|-----|-------------|
| [tests/devnet/README.md](../tests/devnet/README.md) | Devnet integration test guide — architecture, startup sequence, writing tests, config reference, API reference, full test inventory (30+ tests) |
| [tests/devnet/TEST-IDEAS.md](../tests/devnet/TEST-IDEAS.md) | TSS test ideas and future test roadmap |

### Developer Reference

| Doc | Description |
|-----|-------------|
| [.claude/CLAUDE.md](../.claude/CLAUDE.md) | In-depth TSS architecture — fundamental constraints (all-parties required, deterministic party lists, identical CIDs), complete reshare flow with file:line references, known bugs, mandatory pre-proposal safety checks |
| [experiments/README.md](../experiments/README.md) | Disclaimer for the `experiments/` directory — packages are not audited and not included in builds |
