# Go VSC Node - Instructions for Agents

## Overview

This repository contains the node implementation of Go VSC Node, the core of the Magi ecosystem that keeps the Magi network operational.

## Executables

| Executable | Description | Build Command |
|------------|-------------|---------------|
| magid | The main Go VSC Node daemon | `go build -buildvcs=false -ldflags "-X vsc-node/modules/announcements.GitCommit=$(git rev-parse HEAD)" -o ./build/magid vsc-node/cmd/vsc-node` |
| contract-deployer | Used to deploy contracts on the Magi network | `go build -buildvcs=false -o ./build/contract-deployer vsc-node/cmd/contract-deployer` |
| genesis-elector | Create and broadcast a genesis election, usually for a testnet/devnet | `go build -buildvcs=false -o ./build/genesis-elector vsc-node/cmd/genesis-elector` |
| devnet-setup | Initialize and bootstrap a Magi test network, usually a devnet | `go build -buildvcs=false -o ./build/devnet-setup vsc-node/cmd/devnet-setup` |
| mapping-bot | Bot for processing native asset mappings | `go build -buildvcs=false -o ./build/mapping-bot vsc-node/cmd/mapping-bot` |

## Sensitive files

Any file named `identityConfig.json` **must not** be read as it contains sensitive credentials such as private keys.
