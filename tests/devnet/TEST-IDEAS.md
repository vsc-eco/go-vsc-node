# TSS Edge Case Test Ideas

Scenarios that go beyond the current test coverage. Ordered by
estimated impact on mainnet reliability.

---

## 1. Leader crash during BLS collection

**Bug class:** Silent commitment loss — reshare succeeds locally but
the result is never committed to Hive.

**Scenario:** The leader node completes the reshare protocol
successfully, starts BLS signature collection (`waitForSigs`), then
crashes or is stopped. The `ask_sigs` pubsub message is one-shot with
no retry (tss.go ~line 1300). If the leader dies before collecting
2/3 signatures and broadcasting to Hive, the commitment is lost.

**What happens next:** All nodes have the new key locally (keystore),
but no on-chain commitment exists. At the next rotate interval:
- `FindEpochKeys` still returns the OLD key (on-chain epoch unchanged)
- Nodes try to reshare again from the old key's epoch
- But their local keystore has the NEW key from the lost session
- Does the keystore.Get(key, keyId, OLD_epoch) still work? Or does
  it return the new epoch's key, causing a mismatch?

**Test approach:**
- 5 nodes, complete keygen
- At the reshare block, identify the leader (witnessSlot.Account)
- Stop the leader container AFTER reshare completes but BEFORE the
  Hive broadcast (timing: after `waitForSigs OK` log, before
  `broadcasting commitment to Hive` log)
- Verify: no reshare commitment on-chain
- Restart the leader
- Verify: next reshare cycle recovers (re-reshares from the old key)

**Why it matters:** On mainnet, leader crashes are realistic (OOM,
restart, network partition to Hive). If the keystore state diverges
from on-chain state, recovery may be impossible without manual
intervention.

---

## 2. Network partition: two groups, neither has quorum

**Bug class:** Deadlock — all reshare attempts fail indefinitely
because no group can reach 2/3 for BLS.

**Scenario:** 7 nodes split into groups of 4 and 3. Both groups
attempt reshare. The 4-node group has quorum for the btss protocol
(threshold+1 = ceil(7*2/3) = 5... wait, 4 < 5). Actually with 7
nodes, threshold = ceil(7*2/3)-1 = 4, so threshold+1 = 5. Neither
group has 5 nodes, so BOTH groups fail.

But what about BLS? BLS needs signedWeight*3 >= weightTotal*2. With
7 equal-weight nodes, need 5 signatures. The 4-node group gets 4
signatures (not enough). So neither blame nor reshare lands.

**What should happen:** After healing the partition, the next reshare
should work with all 7 nodes. No stale blame from the split should
interfere.

**Test approach:**
- 7 nodes, keygen, wait for active key
- Partition into {1,2,3,4} and {5,6,7} (neither has btss quorum)
- Wait through 2 reshare cycles — verify nothing lands
- Heal partition
- Wait for next reshare — verify it succeeds with all 7

**Why it matters:** Mainnet nodes occasionally split due to Hive API
outages or P2P routing issues. The network must recover cleanly when
connectivity is restored.

---

## 3. Node restart between readiness broadcast and reshare

**Bug class:** False readiness with unprepared node — different from
a crash because the node is running but missing state.

**Scenario:** Node broadcasts `vsc.tss_ready` (on-chain), then
restarts (process restart, not disconnect). When the reshare fires,
the node is in the party list (readiness on-chain) and its P2P is
back up. BUT:
- It may not have preparams ready (takes 15-60s to generate)
- Its keystore may not be loaded yet
- It may be catching up on blocks (sync guard rejects it)

If it participates without preparams, it can't create the LocalParty.
If it's still syncing, it doesn't enter BlockTick. Either way, it
appears as "connected but no response" to other nodes.

**Test approach:**
- 5 nodes, keygen, wait for active key
- In the readiness window (5 blocks before reshare), restart node 3
  (stop + start container)
- The old readiness is on-chain, node 3 is in the party list
- Node 3 is restarting — may or may not be ready
- Verify: blame targets node 3 (if it couldn't participate) OR
  reshare succeeds (if it recovered fast enough)

**Why it matters:** Node restarts during reshare windows are common
on mainnet (deployments, watchdog restarts). The protocol must handle
a node that claimed ready but is actually restarting.

---

## 4. Rapid blame cycles exhausting preparams

**Bug class:** Resource exhaustion — repeated failures prevent
recovery.

**Scenario:** Each keygen/reshare attempt consumes one set of
preparams (Paillier keys + safe primes). If reshares fail repeatedly
(e.g., a slow node causes timeout every cycle), preparams are
consumed on each attempt. Generation takes 15-60 seconds, but with
RotateInterval=20 blocks (60s), there's barely enough time to
regenerate between cycles.

If preparams are exhausted and not regenerated in time, the node
can't participate in the next reshare — making the failure worse.

**Test approach:**
- 5 nodes, keygen, wait for active key
- Add latency to node 4 (causes timeout each cycle)
- Run through 5+ rapid reshare cycles
- Verify: nodes don't run out of preparams
- Verify: each node logs "preparams generated successfully" between
  each reshare attempt

**Why it matters:** Mainnet has seen 14 consecutive failed reshares.
If preparams are exhausted during such a streak, recovery becomes
harder even after the root cause is fixed.

---

## 5. Commitment type confusion: blame with keygen epoch

**Bug class:** Blame decode error — blame from a keygen session
decoded against wrong election.

**Scenario:** A keygen session times out and produces blame. The
blame commitment has Type="blame" and Epoch=keygen_epoch. When the
next reshare reads this blame:
- It calls GetElection(blame.Epoch) = GetElection(keygen_epoch)
- The keygen epoch might be the GENESIS epoch (epoch 0)
- Genesis election members might be ordered differently than the
  current election
- Bit positions in the blame bitset map to genesis election members

If the keygen blame bitset says "bit 2 is bad" and genesis has
member ordering [A,B,C,D,E], but current election has [A,C,B,E,D],
the wrong node gets excluded.

**Test approach:**
- 5 nodes, trigger keygen
- Disconnect one node during keygen → keygen blame at epoch 0
- Wait for epoch 1 (different election ordering)
- Verify: the blame from epoch 0 is decoded against epoch 0's
  election, and the correct node is excluded

**Why it matters:** This is the specific blame epoch decode bug from
the review (Change 4). The devnet currently can't trigger it because
all elections have the same members in the same order. To properly
test, we'd need elections with different member orderings (e.g., a
new node joining changes the sort order of consensus keys).

---

## 6. Stale readiness from previous cycle used in current cycle

**Bug class:** Determinism violation — readiness from cycle N leaks
into cycle N+1.

**Scenario:** The readiness query in RunActions uses
`FindCommitmentsSimple(&keyId, ["ready"], &bh, ...)` where `bh` is
the reshare block height. But readiness records use `Epoch` field
for `targetBlock`. If the query doesn't filter by targetBlock
precisely, readiness from a previous cycle (targeting block N-20)
could leak into the current cycle (block N).

A node that was ready in the previous cycle but is now offline would
appear ready in the current cycle — false readiness.

**Test approach:**
- 5 nodes, keygen, reshare succeeds at cycle 1
- Disconnect node 3 BEFORE the readiness window of cycle 2
- Verify: node 3's old readiness (from cycle 1) is NOT used
  for cycle 2's party list
- Verify: reshare at cycle 2 excludes node 3

**Why it matters:** Stale readiness records could silently poison
party lists. The query must strictly filter by the target reshare
block.
