# TSS Implementation: Magi vs Maya / THORChain

Analysis of differences between this codebase's TSS implementation and the Maya Protocol / THORChain approach (both use threshold ECDSA/EdDSA for keygen, signing, and reshare).

---

## 1. Library and Stack

| Aspect | Magi (this repo) | Maya / THORChain |
|--------|------------------|-------------------|
| **TSS library** | `github.com/bnb-chain/tss-lib/v2` v2.0.2 (direct) | Same family: BNB tss-lib or THORChain's fork on GitLab (`thorchain/tss/tss-lib`). THORChain also has a **go-tss** wrapper (archived). |
| **Transport** | **libp2p + go-libp2p-gorpc**: TSS messages sent as RPCs (`ReceiveMsg`), plus pubsub for commitment/signature collection (`ask_sigs` / `res_sig`) and **gossip-based readiness** (`ready_gossip`) | go-tss used its own **p2p** and **messages** packages; often **broadcast + unicast** with leader/round coordination. |
| **Identity / routing** | **Account-based**: Peer identified by Hive witness account; `witnessDb.GetWitnessesByPeerId` maps peer ID → account; session uses account IDs for party list | **Pubkey / node-based**: Blame and nodes keyed by pubkey; `blame.Node` has `Pubkey`, `BlameData`, `BlameSignature`. |

So: same underlying crypto (BNB-style tss-lib), but **transport and identity model** differ (RPC + Hive accounts vs classic broadcast/unicast + pubkeys).

---

## 2. Architecture: Wrapper vs In-Process Parties

| Aspect | Magi | THORChain go-tss |
|--------|------|-------------------|
| **Structure** | **Single process**: `TssManager` holds dispatchers (keygen/reshare/sign); each session is a `LocalParty` from tss-lib driven by a **Dispatcher**; messages arrive via gorpc and are pushed into the party's channel | **Dedicated TSS service**: go-tss is a separate **keygen / keysign / blame** service; tss-lib parties run inside that service; communication with "outside" is via defined message types |
| **Session lifecycle** | **Block-driven + explicit API**: `BlockTick` decides when to create keygen/reshare actions from election + commitments; `KeyGen()` / `KeyReshare()` also enqueue actions; one **actionMap** per session ID. Reshare/sign intervals are **configurable** via sysconfig `TssParams` (`RotateInterval`, `SignInterval`), defaulting to 100 and 50 L1 blocks respectively. | **Request-driven**: Keygen/keysign triggered by external requests; rounds and blame handled inside the TSS layer |
| **Message path** | P2P → **RPC handler** → `actionMap[sessionId]` → `HandleP2P` → party's `p2pMsg` channel → tss-lib `Update()`. Messages include **Round**, **Action**, and **Session** fields for filtering stale/irrelevant messages. | P2P → **message layer** (with validation MR) → routing to correct party/round |

Magi is "tss-lib in-process with a block-aware scheduler and Hive identity"; THORChain go-tss is "standalone TSS service with its own message and blame types".

---

## 3. Blame and Failure Handling

| Aspect | Magi | THORChain go-tss (blame package) |
|--------|------|----------------------------------|
| **Blame representation** | **Commitment-based**: Blame stored as `TssCommitment` (type `"blame"`) with a **commitment** (bitset of blamed parties), optional **Metadata** (e.g. `Error: "timeout"` / `"TIMEOUT"`), and persistence in DB | **In-memory struct**: `Blame` has `FailReason`, `BlameNodes` (each with `Pubkey`, `BlameData`, `BlameSignature`), `IsUnicast`; helpers: `NewBlame`, `AddBlameNodes`, `AlreadyBlame()` |
| **Blame accumulation** | **All blames in window**: Reads ALL blame commitments within `BLAME_EXPIRE` (24h / 28800 blocks) for a given key, not just the most recent. Each blame is decoded against **its own epoch election** (not the current election). | **Culprits from tss-lib** processed into `blame.Node` (pubkey + blame data/sig); single "reason" string |
| **Threshold-based exclusion** | **Dual threshold**: Short-term per-key exclusion at **TSS_BLAME_THRESHOLD_PERCENT = 33%** (node must appear in >33% of accumulated blames to be excluded). Long-term cross-epoch bans at **TSS_BAN_THRESHOLD_PERCENT = 60%** via `BlameScore()`. | Blame is **data only**; no built-in "ban" in the snippet; exclusion would be done by the caller (e.g. vault/network layer) |
| **Systemic blame protection** | **Safety valve**: Blames where `blamedCount > (members - threshold - 1)` are **skipped** — fewer than threshold+1 nodes remain, so the protocol couldn't have succeeded anyway. Nodes also **refuse to BLS-sign** blame commitments that are systemic failures. Prevents blame death spirals from SSID mismatches or network partitions. | Not visible in blame.go; likely handled at use site |
| **Ban / exclusion** | **Explicit ban layer**: `BlameScore()` builds score map across `TSS_BLAME_EPOCH_COUNT` (27) past epochs; **TSS_BAN_THRESHOLD_PERCENT = 60%**, **TSS_BAN_GRACE_PERIOD_EPOCHS = 3**; banned nodes excluded from **next reshare** and from keygen participant list | Blame is **data only**; no built-in "ban" in the snippet; exclusion would be done by the caller |
| **Expiry** | **BLAME_EXPIRE** (28800 blocks / ~24h); old blame not counted in per-key exclusion or in `BlameScore()` | Not visible in blame.go; likely handled at use site |

So: Magi has a **full blame → accumulate → threshold-exclude → score → ban → exclusion** pipeline with systemic blame protection and persists blame; THORChain's blame package is a **reason + list of nodes** model, with ban logic likely elsewhere.

---

## 4. Reshare and Timeouts

| Aspect | Magi | THORChain / Maya (inferred) |
|--------|-----|-----------------------------|
| **Reshare trigger** | **Block + election**: Reshare created when commitment exists for current key at current height; **configurable `RotateInterval`** (default 100 blocks); also **KeyReshare(keyId)** API | Reshare triggered when **validator set changes** or by policy; coordination via TSS layer |
| **Timeout** | **Per-dispatcher**: `BaseDispatcher.baseStart()` runs a **timeout goroutine**; **configurable `ReshareTimeout`** (default 2 min) for reshare, `DefaultTimeout` (1 min) for others; `lastMsg` updated on each message; on timeout, **timeout blame** with metadata | go-tss has round/timeout handling; exact values and "last message" style not visible in the blame snippet |
| **Participant readiness** | **BLS-signed gossip attestations**: Nodes broadcast `ReadyAttestation` structs (BLS-signed over account + target block) via pubsub `ready_gossip` messages during a **readiness window** (`DEFAULT_READINESS_OFFSET = 30` blocks before target). Attestations are verified against election consensus keys and re-gossiped for amplification. A **settle period** (`DEFAULT_SETTLE_BLOCKS = 3`) prevents late attestations from causing churn — during the last 3 blocks, only re-gossip of existing attestations is accepted. All nodes converge to the same BLS-verified ready set, making party lists **deterministic**. A secondary libp2p `Connectedness` check in the dispatcher (`waitForParticipantReadiness`) confirms threshold+1 peers are connected before starting btss parties. | Common pattern is to **wait for N peers** or **leader coordination** before starting; go-tss has p2p and message layers for this |
| **Message buffering** | **Early messages**: If no dispatcher for `sessionId` yet, message is **buffered** (`messageBuffer`); replayed when dispatcher registers; **configurable `BufferedMessageMaxAge`** (default 1 min); buffer capped at 100 msgs/session, 50 sessions, 256KB max message, 80-block age limit | go-tss had a **message validator** refactor; no direct equivalent to "buffer by session and replay" in the fetched snippets |
| **Message retry** | **Dispatcher-level retry**: `retryFailedMsgs()` runs on a **3s ticker** with max `TSS_MESSAGE_RETRY_COUNT` (3) attempts. `SendMsg()` itself is single-attempt; retry logic is centralized in the dispatcher. | Not visible in blame; typically retries are at the vault/orchestration layer |
| **Retry** | **Automatic reshare retry**: On timeout/result, code can **re-queue reshare** for the same key (recovery path) | Not visible in blame; typically retries are at the vault/orchestration layer |

Magi adds **BLS-signed gossip readiness with settle period**, **session-based message buffering**, **configurable timeouts**, **dispatcher-level message retry**, and **automatic reshare retry** on top of tss-lib.

---

## 5. P2P and Message Transport

| Aspect | Magi | THORChain go-tss |
|--------|------|-------------------|
| **TSS protocol messages** | **Unicast (and broadcast) via gorpc**: `TssRpc.ReceiveMsg` receives; sender from `gorpc.GetRequestSender(ctx)`; **sessionId** in request; dispatcher looked up by session; **configurable `RpcTimeout`** (default 30s) in sender. Messages carry **Round**, **Action**, and **Session** fields; `ValidateMessage()` checks action type matching to reject stale or irrelevant messages. | Custom **messages** and **p2p** packages; broadcast/unicast and round/type in message |
| **Commitment / signature collection** | **Pubsub**: `ask_sigs` / `res_sig` over pubsub; BLS used to sign/verify commitment CID; `sigChannels[sessId]` for responses. Nodes **refuse to BLS-sign** systemic blame commitments (where blamedCount > members - threshold - 1). | Not in the blame snippet; likely separate channel or message type for commitments |
| **Readiness gossip** | **Pubsub**: `ready_gossip` messages carry bundles of `ReadyAttestation` structs; BLS-verified per account; deduplicated by (account, targetBlock); re-gossiped for amplification | No equivalent — readiness is typically via p2p pings or leader coordination |
| **Session / round** | **SessionId** = e.g. `"reshare-{height}-{idx}-{keyId}"`; single RPC type `ReceiveMsg`; action type validated in `ValidateMessage()` | Messages carry round/type; routing by party and round |

So: Magi uses **gorpc for TSS payloads**, **pubsub for commitment gathering**, and **pubsub for gossip-based readiness**; THORChain go-tss uses its own **message types and p2p** for both.

---

## 6. Integration with Chain / Consensus

| Aspect | Magi | Maya / THORChain |
|--------|------|-------------------|
| **Participant set** | From **election/schedule**: `scheduler.GetSchedule(slotHeight)` → witnesses; **electionDb** for current set; participants = **Hive accounts**; party IDs derived from account names | Vaults / node keys; participant set from **validator list** or similar |
| **Commitment persistence** | **Mongo (tss_db)**: `TssCommitments`, `TssKeys`, `TssRequests`; commitments (keygen/reshare/blame) stored by keyId, height, type | **storage** package in go-tss; keygen/keysign results and blame stored for audit and recovery |
| **Trigger cadence** | **Block tick**: `BlockTick(bh, headHeight)`; only if `bh` near head and above activation height; slot/leader from **state engine**. Intervals configurable via sysconfig `TssParams` (`RotateInterval` default 100, `SignInterval` default 50, `ReadinessOffset` default 30). | Time- or event-based; when a new key is needed or validator set changes |

Magi is **tightly coupled to Hive block height, elections, and witnesses** with **configurable intervals**; THORChain/Maya are **validator/vault-centric**.

---

## 7. Summary Table

| Area | Magi | Maya / THORChain |
|------|------|-------------------|
| **TSS core** | bnb-chain tss-lib v2 (keygen/reshare/sign) | Same library family; go-tss wraps it |
| **Transport** | libp2p + gorpc (TSS msgs) + pubsub (commitments + readiness gossip) | Custom p2p + messages (broadcast/unicast) |
| **Identity** | Hive account → peer ID; blame by account | Pubkey/node; blame by `Node.Pubkey` |
| **Blame** | DB commitment + metadata; **accumulated** over BLAME_EXPIRE window; **threshold-based** exclusion (33% short-term, 60% long-term); **systemic blame protection** (skip when >1/3 blamed); ban score + grace; expiry | Struct with reason + list of nodes; ban elsewhere |
| **Readiness** | **BLS-signed gossip attestations** per height via pubsub; 30-block readiness window; 3-block settle period; deterministic convergence | Peer pings or leader coordination |
| **Reshare** | Block + election; gossip readiness; configurable timeout (default 2m); buffering; reshare retry | Validator-set driven; round/timeout in TSS layer |
| **Message handling** | Action-type validation in `ValidateMessage()`; dispatcher-level retry (3s/3 attempts); systemic blame BLS refusal | Round/type routing in message layer |
| **Scheduling** | Block-driven + KeyGen/KeyReshare API; configurable intervals | Request-/event-driven |

---

## 8. Possible Improvements (inspired by THORChain/Maya)

- ~~**Blame data**: Consider adding **BlameSignature** (or similar) per blamed node for verifiable blame, not only a bitset.~~ Partially addressed: systemic blame protection and accumulated threshold-based exclusion provide stronger blame guarantees, though individual per-node blame signatures are not yet implemented.
- ~~**Message validator**: Introduce a **message validator** layer (round, type, session) before pushing to the party, to reject malformed or out-of-order messages.~~ **Implemented**: `ValidateMessage()` now checks action type matching; `p2pMessage` struct carries Round, Action, and Session fields for filtering.
- **Leader / round coordination**: Optional **explicit round or leader** in messages to align with other implementations and simplify debugging. Not yet implemented.
- **Unicast vs broadcast**: Document and optionally **restrict which messages are broadcast** to reduce load and align with THORChain-style unicast where appropriate. Not yet implemented.
- **Observability**: Metrics are in place (reshare duration, timeouts, blame counts, message retries); consider exposing them in the same shape as other TSS stacks (e.g. counters per round/type) for easier comparison.

This doc reflects the Magi codebase (modules/tss, dispatcher, rpc, p2p, db) and public THORChain go-tss/blame structure and docs; Maya uses the same TSS stack as THORChain.

---

## 9. Which Differences Could Cause the Problem-Statement Symptoms?

The problem statement is about **reshare stability**: timeouts, blame, retries, message buffering, single-node failure, network partition, high latency, and rapid churn. Below, only differences that **could cause or worsen** those symptoms are called out, with status notes on what has been addressed.

### Addressed by recent changes

1. **Staggered node startup / non-deterministic readiness (was: Transport + Architecture)**
   - **Was**: `checkParticipantReadiness()` used RPC pings with 5s timeout — non-deterministic results caused SSID mismatch between nodes when they disagreed on who was reachable.
   - **Fix**: Replaced with **BLS-signed gossip attestations** (`ReadyAttestation`). Nodes broadcast readiness 30 blocks before target; attestations are BLS-verified and re-gossiped. All nodes converge to the same ready set deterministically. A 3-block settle period prevents late attestation churn.
   - **Residual risk**: Nodes that come online after the settle period cutoff will be excluded from the ready set and miss the session.

2. **Single blame read + wrong-epoch decoding (was: Blame)**
   - **Was**: Only read the single most recent blame per key; decoded against `currentElection` instead of the blame's own epoch.
   - **Fix**: Now reads **all blame commitments** in the `BLAME_EXPIRE` window. Each blame is decoded against **its own epoch election**. Exclusion is **threshold-based** (>33% of accumulated blames), not absolute.

3. **Blame death spirals from systemic failures (was: Blame)**
   - **Was**: No protection against cascading blame when the protocol itself couldn't succeed (e.g., SSID mismatch affecting many nodes).
   - **Fix**: **Systemic blame protection**: `BlameScore()` skips blames where `blamedCount > (members - threshold - 1)`. Nodes also **refuse to BLS-sign** systemic blame commitments. Prevents blame from accumulating when the failure is network-wide.

4. **No round/type message validation (was: Message path)**
   - **Was**: Messages pushed to party by session ID only; no validation of round or action type.
   - **Fix**: `ValidateMessage()` now checks action type matching. `p2pMessage` struct includes Round, Action, and Session fields for filtering stale or irrelevant messages.

5. **Message retry in sender (was: P2P transport)**
   - **Was**: `SendMsg()` had inline retry with exponential backoff; retry in sender could cause duplicate/reordered delivery.
   - **Fix**: `SendMsg()` is now single-attempt. Retry logic centralized in dispatcher's `retryFailedMsgs()` (3s ticker, max `TSS_MESSAGE_RETRY_COUNT` attempts), which has better visibility into session state.

### Still present (secondary)

6. **Unicast RPC instead of broadcast (Transport)**
   - **Symptom**: If the receiver hasn't registered a dispatcher yet, the message is buffered. If buffer age limit is exceeded, messages are dropped.
   - **Mitigation**: Gossip readiness attestations significantly reduce the "staggered startup" problem. Buffering + replay covers the remaining gap. Configurable `BufferedMessageMaxAge` allows tuning.

7. **30s RPC timeout in sender (P2P transport)**
   - **Symptom**: High-latency environments can hit the sender-side RPC timeout, treating the send as failed.
   - **Mitigation**: `RpcTimeout` is now configurable via `TssParams`. Single-attempt `SendMsg()` with dispatcher-level retry reduces duplicate delivery risk.

8. **Single process + block tick (Architecture)**
   - **Symptom**: If BlockTick or other work holds the process, TSS message handling is delayed.
   - **Mitigation**: Gossip readiness is proactive (30-block window), reducing sensitivity to exact timing of dispatcher registration. Configurable intervals allow tuning.

9. **Ban logic and grace period (Blame)**
   - **Symptom**: Overly aggressive or insufficient banning can amplify churn.
   - **Mitigation**: Dual-threshold system (33% short-term, 60% long-term) and systemic blame protection reduce false positives. Grace period prevents new nodes from being banned prematurely.

### Summary: current state of problem-statement mitigations

| Original Issue | Status | Mitigation |
|---------------|--------|------------|
| Non-deterministic readiness → SSID mismatch | **Fixed** | BLS-signed gossip attestations with settle period |
| Single/wrong-epoch blame reads | **Fixed** | Accumulated blame, per-epoch decoding, threshold exclusion |
| Blame death spirals | **Fixed** | Systemic blame protection + BLS signing refusal |
| No message type validation | **Fixed** | ValidateMessage() with action type matching |
| Sender-side retry issues | **Fixed** | Single-attempt SendMsg + dispatcher-level retry |
| Unicast RPC (no broadcast) | **Mitigated** | Gossip readiness reduces staggered startup; buffering covers remainder |
| 30s RPC timeout | **Mitigated** | Configurable via TssParams; reduced duplicate risk |
| Single process blocking | **Mitigated** | Proactive readiness window; configurable intervals |
| Ban logic false positives | **Mitigated** | Dual threshold + systemic protection |
