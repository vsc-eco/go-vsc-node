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

### Config-pinned floor (simple rollout, no proposal)

For a coordinated rollout where the operator already knows out-of-band that the supermajority
will be upgraded by the activation epoch, the floor can be pinned directly in network config —
no `vsc.propose_consensus_version`, no `ScheduledActivation`, no stake-readiness guard.

1. Set on `ConsensusParams`:
   - `ConsensusVersionFloorEpoch` — the first election epoch at/after which the floor applies
     (typically `PendulumSeedEpoch + 1`, the first post-rollout election). `0` disables the pin.
   - `ConsensusVersionFloorMajor` / `ConsensusVersionFloorConsensus` — the target floor (e.g. `0` / `1`).
2. In `GenerateFullElection`, `ConsensusParams.PinnedVersionFloor(newEpoch)` raises the floor to
   the configured target once `newEpoch >= ConsensusVersionFloorEpoch`; witnesses below it are
   excluded exactly as in the proposal path. The floor only ever rises, so this composes with the
   (dormant) propose/recovery paths.
3. Determinism: the target comes from **config, not the running binary**, and the gate is a pure
   function of config + `newEpoch`. So every signer — even one not yet upgraded — regenerates the
   identical post-cutover election and CID; a node whose own announced version is below the floor
   simply excludes itself. This is why no stake-readiness guard is needed: the pin does not measure
   the fleet, it asserts the cutover, and the operator owns that assertion.

Because there is no readiness guard, the contested-window caveat in the next section applies in
full — if the supermajority is **not** actually upgraded by `ConsensusVersionFloorEpoch`, the
post-pin election can fail to reach its ratification quorum and elections stall until enough nodes
are on the new version. Only pin the epoch when that upgrade is genuinely assured.

## Rollout and exclusion enforcement

Raising the floor is what removes old-code nodes from committee membership. The floor
is a **single value**: the election-active version baked into each on-chain election,
read via `ActiveConsensusVersion` / `TssMinimumConsensusVersion`. Old binaries announce
`{0,0,0}` (the announcement hardcodes `protocol_version: 0` with no `version_major`);
new binaries announce the triple compiled in via `consensusversion.RunningVersion()`
(ldflags).

> **The election body encoding is not backward-compatible — this is a coordinated
> cutover, not a gradual CID-compatible rollout.** `ElectionData` now always encodes
> `version_major` / `version_non_consensus` (their `refmt` tags carry **no
> `omitempty`**, so the keys are present in the CID even at `0` — see
> `TestElectionDataCid`), and rotations additionally carry the pendulum `settlement`
> record. The election CID is `cbornode.WrapObject(ElectionData)`, and a proposed
> election is ratified by the **previous** committee recomputing that CID over their
> own local data (`electionResult.MemberKeys()` → 2/3 BLS). An old-code member
> recomputes without those keys, gets a different CID, and will not sign; a new-code
> member rejects an old-code election for the same reason. So old and new binaries
> **cannot co-ratify from the first block** — not just after the floor rises. The floor
> filter's job is to drop old-code witnesses from membership so the new-code committee
> can reach its own quorum; the encoding change is what makes the two binaries
> mutually exclusive.

Three enforcement points read that floor:

1. **Election committee membership** — `GenerateFullElection` deletes any witness whose
   announced `ConsensusVersionTriple()` does not `MeetsConsensusMin(floor)`
   (`modules/election-proposer/election-proposer.go`, the carry-forward filter and the
   post-activation re-filter after the stake-readiness guard). Old-code witnesses fall
   out of the next committee.
2. **TSS ready-set** — sign and reshare admit only peers whose BLS-signed
   `ReadyAttestation` advertises a version meeting the floor at that height
   (`modules/tss/tss.go`); the version is inside the signed CID, so re-gossipers cannot
   forge it. A node also self-gates: it won't broadcast its own readiness when its
   running version is below the floor.
3. **Per-member election version** — each member carries its witness-time triple
   (`HasPerMemberVersion` / `elections.MemberConsensusVersion`), the deterministic
   snapshot TSS gates against, rather than re-reading live witness records.

Operational implications for a rollout:

- **There is a contested window the moment the committee is split across binaries.**
  Because the election body diverges immediately (above), old-code and new-code members
  produce different CIDs and cannot sign the same election. While one binary still holds
  ≥2/3 of the prior committee, its elections keep ratifying; once the committee is split
  such that neither binary holds 2/3, *no* election ratifies and elections stall until
  the floor rises and old witnesses leave the membership. Keep this window short — this
  is what "transition most nodes within an hour" is really about, not just reaching
  quorum. (Exactly how a node treats an already-ratified election in the *other* format
  on ingest — reject vs. accept-verbatim — depends on whether `TxElectionResult` replays
  the data CID; worth confirming, but it does not change the co-ratification stall.)
- **A raised-floor election only lands once ≥2/3 of the prior committee is new-code.**
  The new election is signed by `electionResult.MemberKeys()` (the committee being
  replaced), so the cutover completes only when the upgraded nodes already hold the
  ratification supermajority of the *previous* committee.
- **The stake-readiness guard (`ConsensusVersionActivationNum/Den`, default 80%) is the
  safety — but only on the propose path.** Via `vsc.propose_consensus_version`, the floor
  will not rise until that fraction of committee stake already announces the target, so it
  cannot flip the floor before enough nodes are ready (it still does not eliminate the
  contested window above; it bounds when the floor is allowed to move). The config-pinned
  floor and the forced-recovery path **bypass this guard** — there the operator asserts the
  cutover, so the supermajority must genuinely be upgraded by the pinned epoch.
- **Replay/reindex caveat — verify before mainnet.** Unlike `settlement` (deliberately
  `omitempty` so nil is omitted and historical bytes still hash identically on replay —
  see `TestElectionDataCid`), `version_major` / `version_non_consensus` are *not*
  `omitempty`. A from-genesis reindex with the new binary therefore encodes these keys
  into every historical `ElectionData`, whose CID then differs from the originally
  signed on-chain `data`. Confirm the replay/verify path does not recompute and compare
  those CIDs (or gate the version-field encoding by height) before relying on a full
  reindex.

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
- `consensus_activation_epoch`
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
