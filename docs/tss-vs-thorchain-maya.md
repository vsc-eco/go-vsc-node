# TSS Implementation: VSC vs Maya / THORChain

Analysis of differences between this codebase’s TSS implementation and the Maya Protocol / THORChain approach (both use threshold ECDSA/EdDSA for keygen, signing, and reshare).

---

## 1. Library and Stack

| Aspect | VSC (this repo) | Maya / THORChain |
|--------|------------------|-------------------|
| **TSS library** | `github.com/bnb-chain/tss-lib/v2` v2.0.2 (direct) | Same family: BNB tss-lib or THORChain’s fork on GitLab (`thorchain/tss/tss-lib`). THORChain also has a **go-tss** wrapper (archived). |
| **Transport** | **libp2p + go-libp2p-gorpc**: TSS messages sent as RPCs (`ReceiveMsg`), plus pubsub for commitment/signature collection (`ask_sigs` / `res_sig`) | go-tss used its own **p2p** and **messages** packages; often **broadcast + unicast** with leader/round coordination. |
| **Identity / routing** | **Account-based**: Peer identified by Hive witness account; `witnessDb.GetWitnessesByPeerId` maps peer ID → account; session uses account IDs for party list | **Pubkey / node-based**: Blame and nodes keyed by pubkey; `blame.Node` has `Pubkey`, `BlameData`, `BlameSignature`. |

So: same underlying crypto (BNB-style tss-lib), but **transport and identity model** differ (RPC + Hive accounts vs classic broadcast/unicast + pubkeys).

---

## 2. Architecture: Wrapper vs In-Process Parties

| Aspect | VSC | THORChain go-tss |
|--------|-----|-------------------|
| **Structure** | **Single process**: `TssManager` holds dispatchers (keygen/reshare/sign); each session is a `LocalParty` from tss-lib driven by a **Dispatcher**; messages arrive via gorpc and are pushed into the party’s channel | **Dedicated TSS service**: go-tss is a separate **keygen / keysign / blame** service; tss-lib parties run inside that service; communication with “outside” is via defined message types |
| **Session lifecycle** | **Block-driven + explicit API**: `BlockTick` decides when to create keygen/reshare actions from election + commitments; `KeyGen()` / `KeyReshare()` also enqueue actions; one **actionMap** per session ID | **Request-driven**: Keygen/keysign triggered by external requests; rounds and blame handled inside the TSS layer |
| **Message path** | P2P → **RPC handler** → `actionMap[sessionId]` → `HandleP2P` → party’s `p2pMsg` channel → tss-lib `Update()` | P2P → **message layer** (with validation MR) → routing to correct party/round |

VSC is “tss-lib in-process with a block-aware scheduler and Hive identity”; THORChain go-tss is “standalone TSS service with its own message and blame types”.

---

## 3. Blame and Failure Handling

| Aspect | VSC | THORChain go-tss (blame package) |
|--------|-----|----------------------------------|
| **Blame representation** | **Commitment-based**: Blame stored as `TssCommitment` (type `"blame"`) with a **commitment** (bitset of blamed parties), optional **Metadata** (e.g. `Error: "timeout"` / `"TIMEOUT"`), and persistence in DB | **In-memory struct**: `Blame` has `FailReason`, `BlameNodes` (each with `Pubkey`, `BlameData`, `BlameSignature`), `IsUnicast`; helpers: `NewBlame`, `AddBlameNodes`, `AlreadyBlame()` |
| **Culprits** | Taken from tss-lib **timeout/error result** (`result.Culprits`), then converted to commitment bitset and stored; **timeout vs error** distinguished via metadata for ban logic | **Culprits from tss-lib** processed into `blame.Node` (pubkey + blame data/sig); single “reason” string |
| **Ban / exclusion** | **Explicit ban layer**: `BlameScore()` builds score map; **TSS_BAN_THRESHOLD_PERCENT**, **TSS_BAN_GRACE_PERIOD_EPOCHS**; banned nodes excluded from **next reshare** and from keygen participant list | Blame is **data only**; no built-in “ban” in the snippet; exclusion would be done by the caller (e.g. vault/network layer) |
| **Expiry** | **BLAME_EXPIRE** (e.g. 24h in blocks); old blame not counted in score | Not visible in blame.go; likely handled at use site |

So: VSC has a **full blame → score → ban → exclusion** pipeline and persists blame; THORChain’s blame package is a **reason + list of nodes** model, with ban logic likely elsewhere.

---

## 4. Reshare and Timeouts

| Aspect | VSC | THORChain / Maya (inferred) |
|--------|-----|-----------------------------|
| **Reshare trigger** | **Block + election**: Reshare created when commitment exists for current key at current height; **TSS_ROTATE_INTERVAL**; also **KeyReshare(keyId)** API | Reshare triggered when **validator set changes** or by policy; coordination via TSS layer |
| **Timeout** | **Per-dispatcher**: `BaseDispatcher.baseStart()` runs a **timeout goroutine**; **TSS_RESHARE_TIMEOUT** (e.g. 2 min) for reshare, 1 min for others; `lastMsg` updated on each message; on timeout, **timeout blame** with metadata | go-tss has round/timeout handling; exact values and “last message” style not visible in the blame snippet |
| **Participant readiness** | **waitForParticipantReadiness**: Before starting reshare, checks that enough participants are **connected** (libp2p Connectedness); **threshold+1** required; 500 ms poll, max wait configurable | Common pattern is to **wait for N peers** or **leader coordination** before starting; go-tss has p2p and message layers for this |
| **Message buffering** | **Early messages**: If no dispatcher for `sessionId` yet, message is **buffered** (`messageBuffer`); replayed when dispatcher registers; **TSS_BUFFERED_MESSAGE_MAX_AGE** (e.g. 1 min) | go-tss had a **message validator** refactor; no direct equivalent to “buffer by session and replay” in the fetched snippets |
| **Retry** | **Automatic reshare retry**: On timeout/result, code can **re-queue reshare** for the same key (recovery path) | Not visible in blame; typically retries are at the vault/orchestration layer |

VSC adds **readiness checks**, **session-based message buffering**, **configurable timeouts**, and **automatic reshare retry** on top of tss-lib.

---

## 5. P2P and Message Transport

| Aspect | VSC | THORChain go-tss |
|--------|-----|-------------------|
| **TSS protocol messages** | **Unicast (and broadcast) via gorpc**: `TssRpc.ReceiveMsg` receives; sender from `gorpc.GetRequestSender(ctx)`; **sessionId** in request; dispatcher looked up by session; **30s RPC timeout** in sender | Custom **messages** and **p2p** packages; broadcast/unicast and round/type in message |
| **Commitment / signature collection** | **Pubsub**: `ask_sigs` / `res_sig` over pubsub; BLS used to sign/verify commitment CID; `sigChannels[sessId]` for responses | Not in the blame snippet; likely separate channel or message type for commitments |
| **Session / round** | **SessionId** = e.g. `"reshare-{height}-{idx}-{keyId}"`; single RPC type `ReceiveMsg`; type/action in `TMsg` | Messages carry round/type; routing by party and round |

So: VSC uses **gorpc for TSS payloads** and **pubsub for commitment gathering**; THORChain go-tss uses its own **message types and p2p** for both.

---

## 6. Integration with Chain / Consensus

| Aspect | VSC | Maya / THORChain |
|--------|-----|-------------------|
| **Participant set** | From **election/schedule**: `scheduler.GetSchedule(slotHeight)` → witnesses; **electionDb** for current set; participants = **Hive accounts**; party IDs derived from account names | Vaults / node keys; participant set from **validator list** or similar |
| **Commitment persistence** | **Mongo (tss_db)**: `TssCommitments`, `TssKeys`, `TssRequests`; commitments (keygen/reshare/blame) stored by keyId, height, type | **storage** package in go-tss; keygen/keysign results and blame stored for audit and recovery |
| **Trigger cadence** | **Block tick**: `BlockTick(bh, headHeight)`; only if `bh` near head and above **TSS_ACTIVATE_HEIGHT**; slot/leader from **state engine** | Time- or event-based; when a new key is needed or validator set changes |

VSC is **tightly coupled to Hive block height, elections, and witnesses**; THORChain/Maya are **validator/vault-centric**.

---

## 7. Summary Table

| Area | VSC | Maya / THORChain |
|------|-----|-------------------|
| **TSS core** | bnb-chain tss-lib v2 (keygen/reshare/sign) | Same library family; go-tss wraps it |
| **Transport** | libp2p + gorpc (TSS msgs) + pubsub (commitments) | Custom p2p + messages (broadcast/unicast) |
| **Identity** | Hive account → peer ID; blame by account | Pubkey/node; blame by `Node.Pubkey` |
| **Blame** | DB commitment + metadata (timeout/error); ban score + grace; expiry | Struct with reason + list of nodes; ban elsewhere |
| **Reshare** | Block + election; readiness check; 2 min timeout; buffering; reshare retry | Validator-set driven; round/timeout in TSS layer |
| **Scheduling** | Block-driven + KeyGen/KeyReshare API | Request-/event-driven |

---

## 8. Possible Improvements (inspired by THORChain/Maya)

- **Blame data**: Consider adding **BlameSignature** (or similar) per blamed node for verifiable blame, not only a bitset (like go-tss `Node` with `BlameData`/`BlameSignature`).
- **Message validator**: Introduce a **message validator** layer (round, type, session) before pushing to the party, to reject malformed or out-of-order messages (similar to go-tss MR).
- **Leader / round coordination**: Optional **explicit round or leader** in messages to align with other implementations and simplify debugging.
- **Unicast vs broadcast**: Document and optionally **restrict which messages are broadcast** to reduce load and align with THORChain-style unicast where appropriate.
- **Observability**: You already have **metrics** (reshare duration, timeouts, blame counts); consider exposing them in the same shape as other TSS stacks (e.g. counters per round/type) for easier comparison.

This doc reflects the VSC codebase (modules/tss, dispatcher, rpc, p2p, db) and public THORChain go-tss/blame structure and docs; Maya uses the same TSS stack as THORChain.

---

## 9. Which Differences Could Cause the Problem-Statement Symptoms?

The problem statement is about **reshare stability**: timeouts, blame, retries, message buffering, single-node failure, network partition, high latency, and rapid churn. Below, only differences that **could cause or worsen** those symptoms are called out.

### Likely to cause symptoms

1. **Unicast RPC instead of broadcast (Transport)**  
   - **Symptom**: Reshare timeouts; “late” or “staggered” nodes never get messages; one node times out and gets blamed.  
   - **Why**: With **broadcast**, every node gets every message when it’s sent; late joiners still see past round traffic. With **unicast gorpc**, the sender targets one peer; if that peer hasn’t registered a dispatcher yet (e.g. block tick not run, or slow), the message is **buffered**. If the peer registers **after** others have already moved on or timed out, we either replay (good) or drop old messages (TSS_BUFFERED_MESSAGE_MAX_AGE = 1 min). So **race between “when others send” and “when we register dispatcher”** can cause timeouts and blame. THORChain-style broadcast would reduce this ordering sensitivity.  
   - **Mitigation**: Buffering + replay; readiness check. If buffer max age is too short or registration is consistently late, symptoms persist.

2. **Block-driven dispatcher registration (Architecture / Scheduling)**  
   - **Symptom**: Timeouts; “staggered node startup” failures; reshare only works if all nodes see the same block and create the session at “the same” time.  
   - **Why**: Reshare session is created in **BlockTick** when commitment exists at current height. Nodes with **slightly different head** or **slower block application** register the dispatcher **later**. They then receive messages that were sent when they had no dispatcher → buffer. If they take longer than **TSS_BUFFERED_MESSAGE_MAX_AGE** to register, replayed messages are **dropped** (“Skipping old buffered message”) → protocol desync → timeout and blame.  
   - So **block-driven scheduling + unicast RPC + bounded buffer age** together can directly cause the “staggered startup” and timeout symptoms.

3. **30s RPC timeout in sender (P2P transport)**  
   - **Symptom**: Timeouts; “high latency” reshare failures; unnecessary blame.  
   - **Why**: If the **receiver** is slow (CPU, lock contention, or network), the **sender** can hit the **30s RPC timeout** and treat the send as failed, then retry (TSS_MESSAGE_RETRY_COUNT). If the first attempt actually succeeds late, we can get **duplicate** or **reordered** delivery; if retries also time out, the receiver never gets the message → timeout and blame. So a **fixed 30s** in a high-latency or loaded environment can cause exactly the “timeout / blame / retry” behavior in the problem statement.

4. **No explicit round/type validation before feeding the party (Message path)**  
   - **Symptom**: Wrong blame; protocol desync; “reshare fails in confusing ways”.  
   - **Why**: We push into the party’s channel by **sessionId** only; we don’t validate **round** or **message type** before calling `HandleP2P`. If a message from a **different round** or an **old session** is delivered (e.g. retry, replay, or bug), tss-lib may misbehave or we record blame incorrectly. THORChain’s **message validator** refactor was about validating and routing by round/type; without that, **out-of-order or stale messages** can contribute to timeouts and wrong blame.

5. **Buffer max age 1 minute (Message buffering)**  
   - **Symptom**: “Staggered node startup” and “late-arriving nodes” fail; reshare times out for slow nodes.  
   - **Why**: **TSS_BUFFERED_MESSAGE_MAX_AGE = 1 min**. If a node registers its dispatcher **more than 1 minute** after the first messages were sent (e.g. slow block sync, or readiness check taking a long time), we **skip** those buffered messages (“Skipping old buffered message”). The node never sees round 1 (or similar) → protocol never completes → timeout and blame. So this **difference** (we drop “old” buffered messages; a broadcast design wouldn’t have this) can directly cause the problem-statement symptoms for late or slow nodes.

### Could worsen symptoms (secondary)

6. **Single process + block tick (Architecture)**  
   - **Symptom**: More timeouts under load; “slow” message handling.  
   - **Why**: If **BlockTick** or other work (DB, state engine) holds the process for a long time, TSS message handling is delayed. A **dedicated TSS service** (like go-tss) can be more responsive. So this doesn’t create the bug by itself but can **worsen** timeout and blame rate when the node is busy.

7. **Ban logic and grace period (Blame)**  
   - **Symptom**: “Rapid node churn”; good nodes banned; or bad nodes not banned.  
   - **Why**: We have **TSS_BAN_THRESHOLD_PERCENT** and **TSS_BAN_GRACE_PERIOD_EPOCHS**. If timeout vs error is **misclassified** (e.g. metadata missing or wrong), we might ban on “timeout” when we shouldn’t (e.g. network partition), or not ban on real faults. That can **amplify** churn and make “reshare stability” worse. So buggy or overly aggressive ban logic can worsen symptoms; the **difference** (we have a full ban pipeline; they have blame data only) means we have more moving parts that can misbehave.

### Unlikely to cause the described symptoms

- **Identity (account vs pubkey)**: Affects how we name parties and blame; doesn’t by itself cause timeouts or message loss.  
- **Commitment/signature over pubsub**: Separate from TSS protocol message delivery; not the cause of reshare protocol timeouts.  
- **Blame representation (bitset + metadata vs BlameNodes[])**: Representation only; cause of blame is still message delivery / timeout / protocol desync.

### Summary: differences that can cause problem-statement symptoms

| Difference | Symptom(s) it can cause |
|------------|--------------------------|
| Unicast RPC (no broadcast) | Timeouts; late/staggered nodes; blame when messages arrive before dispatcher exists. |
| Block-driven registration + buffer max age | Staggered startup failure; late nodes never get early messages; timeouts. |
| 30s RPC timeout in sender | Timeouts and retries under load/latency; duplicate or reordered messages. |
| No round/type message validator | Wrong blame; protocol desync; confusing failures. |
| TSS_BUFFERED_MESSAGE_MAX_AGE = 1 min | Late or slow nodes miss messages; timeouts and blame. |

**Recommendation**: Prioritize (a) **increasing buffer max age** or making it configurable, (b) **adding round/type validation** before `HandleP2P`, (c) **reviewing RPC timeout and retry** (e.g. higher timeout or backoff for reshare), and (d) **tightening when we drop “old” buffered messages** (e.g. only drop by round, not only by wall-clock age). Optionally consider **broadcast for certain rounds** to reduce dependence on “everyone registered at the same time.”
