# Go VSC Node - Instructions for Agents

## Overview

This repository contains the node implementation of Go VSC Node, the core of the Magi ecosystem that keeps the Magi network operational.

## Executables

| Executable | Description | Build Command |
|------------|-------------|---------------|
| magid | The main Go VSC Node daemon | `make magid` |
| contract-deployer | Used to deploy contracts on the Magi network | `make contract-deployer` |
| genesis-elector | Create and broadcast a genesis election, usually for a testnet/devnet | `make genesis-elector` |
| devnet-setup | Initialize and bootstrap a Magi test network, usually a devnet | `make devnet-setup` |
| mapping-bot | Bot for processing native asset mappings | `make mapping-bot` |

## Sensitive files

Any file named `identityConfig.json` **must not** be read as it contains sensitive credentials such as private keys.
