# Consensus Upgrade Development Guide

This document describes how to add and roll out new consensus lines in the
`major.consensus.non_consensus` model.

## Scope and principles

- Coordination is performed on `major.consensus` only.
- `non_consensus` is informational and does not require coordination.
- The current scaffold keeps runtime behavior unchanged (`passthrough` executor)
  while providing versioned executor plumbing and activation metadata.

## Current architecture

- Version coordination and recovery state:
  - `modules/db/vsc/consensus_state/*`
  - `modules/state-processing/consensus_version.go`
- Runtime line/executor scaffold:
  - `modules/state-processing/consensus_runtime.go`
- Local API visibility:
  - `modules/gql/schema.graphql` (`LocalNodeInfo`)
  - `modules/gql/gqlgen/schema.resolvers.go`

## Adding a new consensus executor (example: 1.3)

1. Implement a new executor type in `modules/state-processing` (or a sibling package):
   - implement:
     - `Name() string`
     - `Line() ConsensusLine` (e.g. `Major: 1, Consensus: 3`)
2. Register it during node startup:
   - call `StateEngine.RegisterConsensusExecutor(...)` in initialization wiring.
3. Keep behavior gated by line:
   - execution code that diverges between `1.2` and `1.3` should branch by
     `StateEngine.ActiveConsensusLine(blockHeight)` or resolved executor.
4. Add tests:
   - line resolution before/at/after activation height,
   - executor selection for matching and fallback lines.

## Upgrade flow

The active consensus version is a **pure function of the on-chain election**:
`activeVersion(blockHeight) = elections.ResultVersion(GetElectionByHeight(blockHeight))`.
There is no live "adopted version" singleton — version selection is resolved once per election
at the epoch boundary and ratified by the existing ~80% regenerate-and-sign mechanism. This is
what makes TSS gating and executor selection deterministic and replay-correct.

### Normal coordination (epoch-scheduled switch)

1. A committee member posts `vsc.propose_consensus_version` with `major`, `consensus`, and an
   optional `activation_epoch` (defaults to the next epoch). This records a `ScheduledActivation`
   in `chain_consensus_state`. A strictly-higher target replaces an existing schedule (monotone).
2. At each election build (`GenerateFullElection`), the proposer resolves the version floor:
   - floor carries forward from the previous election;
   - if the new election's epoch `>= ActivationEpoch` and the target exceeds the floor and the
     **stake-readiness guard** passes (≥ `ConsensusVersionActivationNum/Den`, default 80%, of
     committee stake already announces the target), the floor rises to the target.
   - witnesses below the floor are excluded; the floor is baked into the election version fields.
3. Because every signer regenerates the election at the same height and reads the schedule
   height-addressably (`ScheduledActivationForHeight`), all nodes compute the identical election
   CID and ratify it.
4. From the activated epoch onward, `activeVersion` (and the executor line) is the new version.
   Nodes carrying the new binary keep running old behavior until the activation epoch.

### Halt recovery coordination

1. `vsc.recovery_suspend` (recovery multisig) halts normal processing immediately.
2. `vsc.recovery_require_version` (recovery multisig) records a **Forced** `ScheduledActivation`
   (activates next epoch, skips the stake-readiness guard) and clears suspension.

## Data model notes

- `chain_consensus_state` holds only `processing_suspended` and an optional
  `scheduled_activation` (`{target_major, target_consensus, activation_epoch, forced, proposer,
  tx_id, block_height}`).
- Use `SetScheduledActivation` / `ClearScheduledActivation` /
  `SetForcedActivationAndClearSuspension` on `ConsensusState`.
- The node's own running version is `consensusversion.RunningVersion()` (build-time
  `NodeVersionMajor` / `NodeProtocolVersion` / `NodeVersionNonConsensus`), used for both the
  on-chain announcement and the version tag on TSS readiness gossip.

## GraphQL visibility

`LocalNodeInfo` exposes:

- `active_consensus_line`
- `active_consensus_executor`
- `consensus_activation_mode`
- `consensus_activation_height`
- `consensus_activation_version`
- `consensus_activation_attested_block`
- `consensus_activation_attested_txid`

After schema updates, regenerate gql code:

```bash
go run github.com/99designs/gqlgen generate
```

## Testing checklist

- `modules/state-processing/consensus_runtime_test.go`
- `modules/state-processing/consensus_version_e2e_test.go`
- recovery multisig config tests in
  `modules/state-processing/recovery_multisig_test.go`

Recommended command:

```bash
go test ./modules/state-processing/... ./modules/gql/... -count=1
```
