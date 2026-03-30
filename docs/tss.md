# TSS

## What TSS Does

The TSS subsystem manages threshold keys for VSC operations. At a high level it provides:

- threshold key creation (`keygen`)
- threshold signing of queued requests (`sign`)
- committee rotation without changing the logical key ID (`reshare`)
- on-chain commitment publication and BLS quorum attestation
- key lifecycle management: create, activate, renew, deprecate, and optionally retire
- blame / timeout reporting and automatic retry for some failed sessions

The runtime coordinator lives in `modules/tss/tss.go` and runs off the Hive block stream.

## Runtime Model

Every synced node runs a `TssManager`. On each block tick it decides whether this is a block where TSS work should happen:

- every `TSS_ROTATE_INTERVAL` blocks it checks for:
  - keys that should be reshared into the current epoch
  - newly created keys that still need their first keygen
- every `TSS_SIGN_INTERVAL` blocks it checks for:
  - unsigned signing requests in the TSS request DB

Only the slot leader is responsible for broadcasting the final Hive custom JSON operations. Non-leader nodes still run the local TSS session and contribute signatures or commitments.

## Feature: Key Creation

### What it does

Key creation turns a logical `key_id` in status `created` into a real threshold key owned by the current witness committee.

### How it starts

Key creation begins in state processing when a contract output emits a TSS op with `type == "create"`.

- `modules/state-processing/system_txs.go`
  - inserts a `TssKey` row with the requested algorithm and lifespan (`epochs`)
  - the key starts in DB state `created`

Later, during a rotate block, `TssManager.BlockTick()` calls `FindNewKeys()` and queues a `KeyGenAction`.

### How it is implemented

- `modules/tss/tss.go`
  - `BlockTick()` discovers new keys
  - `RunActions()` creates a `KeyGenDispatcher`
- `modules/tss/dispatcher.go`
  - `KeyGenDispatcher.Start()` starts a tss-lib keygen party for either ECDSA or EdDSA
  - on success it stores local save data in flatfs keystore
  - it returns a `KeyGenResult` containing:
    - the generated public key
    - the participant commitment bitset
    - session metadata (`session_id`, `key_id`, epoch, block height)

### When it becomes active

Keygen success alone does not activate the key. Activation happens only after the leader publishes a `vsc.tss_commitment` operation to Hive and the state engine verifies and ingests it.

- `modules/state-processing/state_engine.go`
  - parses `vsc.tss_commitment`
  - verifies the attached BLS quorum proof
  - writes the commitment to the TSS commitment DB
  - if the key was still `created` and a public key is present:
    - copies in the public key
    - marks the key `active`
    - sets `CreatedHeight`
    - sets `Epoch`
    - sets `ExpiryEpoch = Epoch + Epochs` when the key has finite lifetime

This is why a node can log a local hex public key before the key is globally active: local keygen completion and on-chain activation are separate stages.

## Feature: Threshold Signing

### What it does

Threshold signing signs queued messages with an already active or reshared threshold key.

### How it starts

Signing requests are inserted by state processing when a TSS op has `type == "sign"`.

- `modules/state-processing/system_txs.go`
  - stores a `TssRequest` with status `unsigned`

On sign interval blocks:

- `modules/tss/tss.go`
  - `BlockTick()` calls `FindUnsignedRequests()`
  - each request becomes a `SignAction`

### How it is implemented

- `modules/tss/dispatcher.go`
  - `KeySignDispatcher` loads the locally stored key share from flatfs
  - starts the tss-lib signing party with the current committee
  - returns a `KeySignResult` containing the raw signature

Only the leader broadcasts completed sign results:

- `modules/tss/tss.go`
  - successful `KeySignResult`s are collected
  - leader emits `vsc.tss_sign`

Then state processing verifies and persists the signature:

- `modules/state-processing/state_engine.go`
  - handles `vsc.tss_sign`
  - verifies against the key’s public key
  - updates the request status to `complete`

## Feature: Reshare / Committee Rotation

### What it does

Reshare moves an existing key from one committee/epoch to the next without changing the logical key ID. It is how keys survive witness set rotation.

### How it starts

On rotate blocks:

- `modules/tss/tss.go`
  - `BlockTick()` calls `FindEpochKeys(currentEpoch)`
  - each eligible active key becomes a `ReshareAction`

### How the participant set is chosen

`RunActions()` reconstructs the old committed committee from the last keygen/reshare commitment:

- loads the latest commitment for the key
- decodes the bitset describing who participated
- maps the bitset back to the election members
- builds:
  - old committed participants
  - new epoch participants

Reshare only starts if both sides satisfy threshold requirements:

- enough old participants to reconstruct the old share set
- enough new participants to receive the reshared key

### How it is implemented

- `modules/tss/dispatcher.go`
  - `ReshareDispatcher.Start()` recreates the old and new party ID sets
  - if the previous commitment came from a prior reshare, it reuses the epoch-modified party IDs so tss-lib can match saved key data
  - old-party members load the existing key share from flatfs
  - new-only members participate without old share data
  - on success it returns a `ReshareResult` with a new commitment bitset

As with keygen, reshare is not final until the commitment is BLS-attested and written by state processing.

## Feature: Commitment Publication And BLS Quorum Proof

### What it does

Keygen, reshare, timeout, and certain error outcomes are converted into commitments that must be acknowledged by the witness set before state processing will trust them.

### How it works

1. A dispatcher returns a result.
2. `RunActions()` serializes it into a `BaseCommitment`.
3. The leader asks witnesses for BLS signatures on the commitment CID.
4. Once enough weight signs, the leader broadcasts `vsc.tss_commitment`.
5. State processing verifies the BLS proof and writes the commitment to DB.

### Implementation details

- `modules/tss/tss.go`
  - `RunActions()` gathers `commitableResults`
  - `waitForSigs()` creates a BLS circuit and collects witness signatures until `> 2/3` committee weight is reached
  - leader broadcasts the resulting array as `vsc.tss_commitment`
- `modules/tss/p2p.go`
  - `ask_sigs` asks peers to sign a session result CID
  - `res_sig` returns the peer’s BLS signature
  - signature-only messages bypass normal session round filtering
- `modules/state-processing/state_engine.go`
  - parses either the newer array format or older legacy map format
  - reconstructs the commitment CID
  - verifies the BLS aggregate proof
  - persists the commitment if valid

This commitment layer is the bridge between local TSS success and globally accepted key state.

## Feature: Key Lifecycle Management

### States

`TssKey.Status` can move through:

- `created`
- `active`
- `deprecated`
- `retired`

### Create

- inserted by `system_txs.go` on `create`
- becomes `active` only after a verified keygen commitment is ingested

### Renew

- `system_txs.go` handles `renew`
- active or deprecated keys can extend their `ExpiryEpoch`
- deprecated keys are reactivated by setting:
  - `Status = active`
  - `DeprecatedHeight = 0`

### Deprecate

- `modules/state-processing/state_engine.go`
  - at block processing time, `FindDeprecatingKeys(currentEpoch)` marks expiring active keys as `deprecated`

### Retire

- retirement is block-height based and currently gated by:
  - `modules/db/vsc/tss/interface.go`
  - `KeyRetirementEnabled = false`

When enabled, deprecated keys whose grace period elapses become `retired`, and `TssManager.BlockTick()` also deletes their local keystore share from flatfs.

## Feature: Timeout, Blame, And Retry

### What it does

The runtime records unsuccessful sessions as structured outcomes rather than just dropping them. This allows later logic to reason about failures.

### How it is implemented

- `modules/tss/dispatcher.go`
  - timeouts become `TimeoutResult`
  - tss-lib failures become `ErrorResult`
  - results serialize into commitment objects

- `modules/tss/tss.go`
  - timeout and error results are added to `commitableResults`
  - for timeouts, `RunActions()` may schedule automatic retry
  - retry is capped by `MAX_RESHARE_RETRIES`
  - retry type is chosen carefully:
    - keygen timeout retries as `KeyGenAction`
    - reshare timeout retries as `ReshareAction`

The code also maintains blame-related DB queries and scoring hooks so repeated failures can be attributed to participants over time.

## Feature: P2P Delivery And Session Safety

### What it does

TSS party messages move over libp2p and must be associated with the correct session and round. The code tries to avoid stale or cross-session messages corrupting active sessions.

### How it is implemented

- `modules/tss/p2p.go`
  - `ValidateMessage()` rejects old-round or wrong-action TSS traffic
  - `ask_sigs` and `res_sig` are exempt because they use a separate BLS path
  - `SendMsg()` has retry logic and witness lookup

- `modules/tss/tss.go`
  - `actionMap`, `sessionMap`, and `messageBuffer` track active sessions
  - buffered early messages are replayed once the dispatcher is registered

## Persistence Map

The TSS subsystem persists data in three different places:

- `TssKeys`
  - logical key metadata and lifecycle state
- `TssRequests`
  - pending and completed sign requests
- `TssCommitments`
  - verified keygen / reshare / timeout / blame commitments
- flatfs keystore
  - local node share material used by tss-lib for later signing or resharing

This split is important:

- the DB tracks global protocol state
- flatfs tracks the local node’s private share material

## Important Operational Detail

The main debugging distinction is:

- local success: a node completed keygen or reshare and stored share data locally
- global success: the leader collected BLS quorum signatures, broadcast `vsc.tss_commitment`, and state processing verified it

If nodes log public keys but the key never becomes active, the failure is usually in the commitment/BLS/state-ingest path, not the raw tss-lib keygen path.

## Implementation Map

- `modules/tss/tss.go`
  - block scheduling, action dispatch, leader commit/sign broadcast, BLS quorum collection
- `modules/tss/dispatcher.go`
  - keygen, reshare, sign session execution via tss-lib
- `modules/tss/p2p.go`
  - libp2p transport, message validation, BLS `ask_sigs` / `res_sig`
- `modules/state-processing/system_txs.go`
  - creation, renewal, and sign-request insertion
- `modules/state-processing/state_engine.go`
  - commitment ingestion, BLS verification, key activation, signature completion, deprecation/retirement
- `modules/db/vsc/tss/interface.go`
  - TSS DB model and lifecycle state definitions
