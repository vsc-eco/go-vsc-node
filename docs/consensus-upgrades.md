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

### Normal coordination

1. Committee proposes `vsc.propose_consensus_version` for target `major.consensus`.
2. Readiness finalizes once threshold is met.
3. Activation metadata (`next_activation`) is attested and persisted with:
   - mode = `normal`
   - target line
   - activation height
   - attested block + tx id
4. At activation height, runtime switches to the new executor line.

### Halt recovery coordination

1. `vsc.recovery_suspend` halts normal processing.
2. `vsc.recovery_require_version` sets required/adopted version.
3. Activation metadata may be postponed (baked into upgrade software policy):
   - mode = `recovery`
   - activation height > attestation height
4. Runtime switches when postponed activation height is reached.

## Data model notes

- `chain_consensus_state` now includes optional `next_activation`:
  - `mode`
  - `version`
  - `activation_height`
  - `attested_block_height`
  - `attested_tx_id`
- Use `SetNextActivation` and `ClearNextActivation` on `ConsensusState`.

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
