# TSS Protocol Architecture Reference

## Overview

The TSS module (`modules/tss/`) orchestrates threshold signature operations using the external library `github.com/bnb-chain/tss-lib/v2` (aliased as `btss` in code). The orchestration layer handles party selection, P2P messaging, blame tracking, and session lifecycle. The external library implements the cryptographic protocol rounds.

## Key Constants (tss.go)

```
TSS_SIGN_INTERVAL           = 50        // Sign every 50 L1 blocks
TSS_ROTATE_INTERVAL         = 100       // Reshare every 100 blocks (~5 min)
TSS_BAN_THRESHOLD_PERCENT   = 60        // Failure rate for ban
TSS_BAN_GRACE_PERIOD_EPOCHS = 3         // New node protection
BLAME_EXPIRE                = 28800     // ~24 hours in blocks
TSS_BLAME_EPOCH_COUNT       = 27        // Scoring window (27 epochs = ~6.75 days)
```

## Threshold Calculation

`helpers/tss_helpers.go:GetThreshold()`:
```
threshold = ceil(N * 2/3) - 1
```
For 19 nodes: t=12, requiring t+1=13 for reconstruction.

This threshold governs the mathematical minimum for Lagrange interpolation. It does NOT govern btss's message requirements (see below).

## Dispatcher Architecture (dispatcher.go)

### Types

- `BaseDispatcher` — shared fields: `party`, `tssErr`, `blameCulprits`, `timeout`, `p2pMu`, `done`, `msgCtx`/`cancelMsgs`, `doneMu`/`doneSignalled`, `lastMsg`, `failedMsgs`
- `ReshareDispatcher` — embeds BaseDispatcher, adds `newParty`, `newParticipants`, `oldPids`/`newPids`, `origOldSize`/`origNewSize`, `newEpoch`
- `SignDispatcher` — embeds BaseDispatcher
- `KeyGenDispatcher` — embeds BaseDispatcher

### Session Lifecycle

1. `Start()` — creates btss LocalParty, starts `baseStart()` which launches timeout monitor and `retryFailedMsgs()`
2. `HandleP2P()` — receives messages, spawns goroutine per message, calls `party.UpdateFromBytes()`. Errors stored in `blameCulprits` map.
3. Timeout monitor in `baseStart()` — checks `lastMsg` against configurable timeout. On timeout: sets `dispatcher.timeout = true`, calls `cancelMsgs()` + `drainP2pMsg()`, signals `done`.
4. `signalDone()` — called on success or internal error. Has `doneMu`/`doneSignalled` guard for exactly-once semantics. Calls `cancelMsgs()` then `drainP2pMsg()` then signals `done`.
5. `Done()` — waits on `done` channel. Checks timeout -> tssErr -> err -> success. Timeout branch merges `blameCulprits` (protocol errors) with `WaitingFor()` (timeout culprits).

### Result Types

- `TimeoutResult` — carries culprit list, serializes to blame commitment bitset with metadata `Error: "timeout"`
- `ErrorResult` — carries `*btss.Error`, serializes using `tssErr.Culprits()` with the actual error string
- `KeyGenResult`, `ReshareResult` — success types

### Blame Accumulation

`blameCulprits map[string]string` on BaseDispatcher, protected by `p2pMu`. Populated in HandleP2P error paths. First error preserved in `tssErr` (nil-check). All culprits accumulated in map. Merged with `WaitingFor()` in Done() timeout branch — error culprits take priority over timeout label.

## btss Library Internals

### Party IDs and Indexing

Party IDs are created with `btss.NewPartyID(account, moniker, key)` where `key` is a `*big.Int` derived from the account string. `SortPartyIDs()` sorts by key and assigns `Index` = position in sorted order. ALL message storage, save data, and ok-tracking arrays are indexed by this position.

For reshare with epoch modification: key is multiplied by `epoch+1` to create unique IDs across epochs.

### Message Flow: BaseUpdate (tss/party.go)

```
ValidateMessage(msg) -> StoreMessage(msg) -> round.Update() -> CanProceed()? -> advance() -> round.Start()
```

- `StoreMessage` stores at `messages[msg.GetFrom().Index]`
- `round.Update()` sets `ok[j] = true` for verified messages, returns `(false, nil)` if not all received yet
- `CanProceed()` returns true only when ALL `ok[j]` are true
- `advance()` moves `p.rnd` to `NextRound()`, then `Start()` runs the new round's initialization
- If `Start()` fails (e.g., SSID mismatch in round 2), error propagates up through `BaseUpdate`
- `BaseUpdate` recursively calls itself after advancing to process any buffered messages

### SSID (Session-Specific Identifier)

Computed in Round 1 `Start()` via `getSSID()`:
```
hash(EC_params || ALL_party_keys || ALL_BigXj || ALL_NTildej || ALL_H1j || ALL_H2j || round_number || nonce)
```

Used as proof context for Schnorr proofs, FacProofs, ModProofs throughout the protocol. Changing party set = different SSID = all proofs invalid. Cannot drop a party mid-protocol.

### Reshare Protocol Rounds

**Round 1 (old -> new)**: Old committee computes SSID, creates VSS shares for ALL new parties, broadcasts commitment + SSID. `Update()` waits for ALL old party messages.

**Round 2 (new step 1)**: New committee verifies SSID consistency across ALL old parties (loop, first mismatch = abort). Generates Paillier keys, DLN proofs. `Update()` waits for ALL new party messages.

**Round 3 (old step 2)**: Old committee sends shares + decommitments to ALL new parties. `Update()` waits for ALL old party messages (both msg types).

**Round 4 (new step 2)**: New committee verifies DLN/mod proofs from ALL new parties. Reconstructs shares from ALL old parties — iterates `j=0..len(oldParties)-1`, accumulates `vjc[j]` and `newXi` via raw sum. Verifies VSS shares. Computes `BigXj` for ALL new parties. Sends FacProof to ALL new parties. `Update()` waits for ALL new party messages.

**Round 5 (new step 3)**: Saves `BigXj`, `Ks`, `Xi`. Verifies FacProof from ALL new parties. Outputs save data via `end` channel.

### Keygen Protocol

Same `CanProceed()` pattern. Round 3 accumulates Vc from ALL parties and computes BigXj for ALL parties. Same full-set dependency.

### Signing Protocol

More complex — MtA (multiplicative-to-additive) in rounds 2-3 is pairwise between ALL signing parties. Rounds 5, 7, 9 accumulate contributions from ALL parties. Fundamentally all-to-all interactive, not a simple threshold operation. Cannot be forked to t+1 the way reshare can.

However, signing uses `BuildLocalSaveDataSubset()` from keygen save data to remap indices when signing with a subset of the original keygen parties. This remapping happens BEFORE the protocol starts.

## Blame Scoring System (tss.go:281-453)

`BlameScore()` computes ban decisions:

1. Loads current election + 27 previous elections
2. For each epoch: loads all blame commitments, adds `len(blames)` to weight for EVERY member
3. For each blame: decodes bitset, increments score for blamed members
4. Ban if `score > weight * 60 / 100` and past grace period (3 epochs)

**Dilution issue**: Weight accumulates from ALL blame events in ALL epochs, including clean ones. A node going bad needs 17/27 epochs (~4.25 days) to reach 60% threshold if blame events exist in every epoch.

## Reshare Session Setup (tss.go:681-816)

1. Load current election
2. `BlameScore()` -> banned nodes
3. Build old committee from commitment bitset against the commitment's epoch election, filtering banned/blamed
4. Track `fullOldCommitteeSize` (pre-filter) for threshold calculation
5. Build new committee from current election, filtering banned/blamed
6. `origOldThreshold = GetThreshold(fullOldCommitteeSize)`, `origNewThreshold = GetThreshold(len(newParticipants))`
7. Readiness check: old committee with `keepTimeouts=true` (for SSID consistency), new committee counted only
8. Proceed if `len(oldMembers) >= origOldThreshold+1` and `newReady >= origNewThreshold+1`

## P2P and Readiness (p2p.go)

`checkParticipantReadiness()` — pings each participant with RPC "ready" call (5s timeout). If `keepTimeouts=true`, timed-out nodes are KEPT in the list (for SSID determinism). Only definitive failures (no_witness, bad_peer_id, rpc_error) cause exclusion.

`countReadyParticipants()` — same ping but returns count only, never filters the list. Used for new committee pre-flight check.

## Network Parameters (common/params.go, common/params/params.go)

- Block time: 3 seconds (Hive L1)
- Epoch length: 7,200 blocks (6 hours)
- Slot length: 10 blocks (30 seconds)
- Election interval: 7,200 blocks (same as epoch)
- Reshare timeout: 2 minutes
- Default timeout (sign/keygen): 1 minute

## File Locations

- Dispatcher: `modules/tss/dispatcher.go` (~1860 lines)
- Main TSS manager: `modules/tss/tss.go`
- P2P/readiness: `modules/tss/p2p.go`
- Helpers/threshold: `modules/tss/helpers/tss_helpers.go`
- System config: `modules/common/params.go`, `modules/common/params/params.go`
- btss (external): `github.com/bnb-chain/tss-lib/v2@v2.0.2`
- Logging: `lib/vsclog/vsclog.go` — levels: Trace(-12), Verbose(-8), Debug(-4), Info(0), Warn(4), Error(8)
