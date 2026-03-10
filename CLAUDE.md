# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Test Commands

```bash
make                    # Build all 5 binaries to ./build/
make magid              # Build only the main daemon
go test ./...           # Run all tests
go test ./modules/tss/  # Run tests in a specific package
go run github.com/99designs/gqlgen generate  # Regenerate GraphQL code
```

## Sensitive Files

**Never read `identityConfig.json`** — it contains private keys.

## Architecture

This is the **Go VSC (Magi) Node**, a layer-2 network on top of the Hive blockchain. The main daemon is `magid` (cmd/vsc-node).

### Core Flow (cmd/vsc-node/main.go)

Init DB + P2P + DataLayer → Start Hive block consumer (L1 listener) → Create StateEngine → Wire up modules (TSS, elections, block producer, oracle, transactions) → Start GraphQL API + P2P listener

### Key Directories

- **cmd/** — 5 binaries: `vsc-node` (magid), `contract-deployer`, `genesis-elector`, `devnet-setup`, `mapping-bot`
- **lib/** — Shared libraries: datalayer (IPFS), CBOR serialization, DIDs, Hive client, pubsub, IPC, logging
- **modules/** — 24 core modules (see below)

### Module Groups

**Consensus & Network**: `tss/` (threshold signatures, BFT consensus), `p2p/` (libp2p networking), `announcements/` (node discovery)

**State & Blocks**: `state-processing/` (central transaction/contract engine), `block-producer/`, `transaction-pool/` (mempool), `hive/` (L1 block streaming)

**Contract Execution**: `wasm/` (WasmEdge runtime), `contract/` (execution context)

**Ledger**: `ledger-system/` (token balances), `rc-system/` (resource credits)

**Elections & Data**: `election-proposer/`, `data-availability/` (proofs, client/server), `gateway/` (P2P multisig)

**API & Config**: `gql/` (GraphQL API, schema in `modules/gql/schema.graphql`), `config/`, `common/` (system config per network), `db/` (MongoDB collections)

### Key Dependencies

- **StateEngine** is the central coordinator — most modules depend on it
- **P2P** uses libp2p for peer discovery, consensus messaging, and data distribution
- **Hive** is the L1 chain — the node listens to Hive blocks and broadcasts operations back
- **MongoDB** is the database backend (see `modules/db/` for all collections)
- **IPFS** (via lib/datalayer) handles data storage and availability

### Networks

Mainnet (`vsc-mainnet`), Testnet (`vsc-testnet`), Devnet (`vsc-devnet`)— network-specific params in `modules/common/system-config/`

### GraphQL

Schema: `modules/gql/schema.graphql`. Auto-binds DB models via `gqlgen.yml`. Playground available at the GQL server URL + `/sandbox`.

### Docker

Multi-stage build (Go 1.24.1 → RockyLinux 9.3) with WasmEdge 0.13.4. Compose includes MongoDB 8.0 and optional btcd (Bitcoin core).
