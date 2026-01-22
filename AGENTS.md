# Go VSC Node - Instructions for Agents

## Overview

This repository contains the node implementation of Go VSC Node, the core of the Magi ecosystem that keeps the Magi network operational.

## Executables

### Main daemon

The main Go VSC Node daemon.

* Package: `vsc-node/cmd/vsc-node`
* Build command: `go build -buildvcs=false -ldflags "-X vsc-node/modules/announcements.GitCommit=$(git rev-parse HEAD)" -o ./magid vsc-node/cmd/vsc-node`

### Contract deployer

Used to deploy contracts on the Magi network.

* Package: `vsc-node/cmd/contract-deployer`
* Build command: `go build -buildvcs=false -o ./contract-deployer vsc-node/cmd/contract-deployer`
