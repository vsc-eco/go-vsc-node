# Go VSC Node - Instructions for Agents

## Overview

This repository contains the node implementation of Go VSC Node, the core of the Magi ecosystem that keeps the Magi network operational.

## Executables

### Main daemon

The main Go VSC Node daemon.

**Build command**: `go build -buildvcs=false -ldflags "-X vsc-node/modules/announcements.GitCommit=$(git rev-parse HEAD)" -o ./build/magid vsc-node/cmd/vsc-node`

### Contract deployer

Used to deploy contracts on the Magi network.

**Build command**: `go build -buildvcs=false -o ./build/contract-deployer vsc-node/cmd/contract-deployer`

### Genesis elector

Create and broadcast a genesis election, usually for a testnet.

**Build command**: `go build -buildvcs=false -o ./build/genesis-elector vsc-node/cmd/genesis-elector`

## Sensitive files

Any file named `identityConfig.json` **must not** be read as it contains sensitive credentials such as private keys.
