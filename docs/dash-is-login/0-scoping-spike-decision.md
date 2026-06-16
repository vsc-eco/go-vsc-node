# Workstream-0 Scoping Spike — Decision Memo

**Date:** 2026-05-15 (started 2026-05-14)
**Spec reference:** `magi/testnet/docs/superpowers/specs/2026-05-14-dash-instantsend-login-design.md` (rev 6)
**Question asked:** v1 (lazy-attestation with Magi validator quorum) vs v2-true (BLS-in-WASM verification of Dash masternode quorum) — which to build first?

## TL;DR

**Go with v1 (lazy attestation against Magi validator BLS quorum).** All three blockers cleared. Total effort estimate revised down from the spec's 7-12 weeks to ~6-10 weeks. v2-true (Dash-masternode-direct verification) becomes additive future work, not a v1 alternative.

## The three questions, answered

### Q1: Is BLS verification cheaply callable from WASM contracts?

**YES. ~2-3 person-days.**

- Host functions registered as namespaced maps in [`modules/wasm/sdk/sdk.go:78-742`](../../modules/wasm/sdk/sdk.go). `bls_verify` fits naturally in the existing `"crypto"` namespace alongside `keccak256` and `ecrecover` ([sdk.go:615](../../modules/wasm/sdk/sdk.go#L615)).
- Existing BLS infrastructure ([`lib/dids/bls.go`](../../lib/dids/bls.go), using `github.com/protolambda/bls12-381-util`) is battle-tested in consensus — no new crypto, just expose what's there.
- Gas cost: ~5× keccak256 = `params.CYCLE_GAS_PER_RC * 5` (~500 RC equivalent). BLS aggregate verify is ~1-5ms on modern hardware.
- Determinism: 100% guaranteed (no RNG, used by consensus). Same code path used by every block validation.
- TinyGo binding via `//go:wasmimport sdk crypto.bls_verify` — proven pattern in existing mapping contracts.
- No blockers. Roughly 50 lines in `sdk.go`, 30 in TinyGo binding, 150 in tests.

### Q2: How rigid is the p2p layer for a new request-response message type?

**Very flexible. ~2 person-days.**

- libp2p + gossipsub. No central message-type registry — each module self-registers via [`modules/p2p/pubsub.go:74-208`](../../modules/p2p/pubsub.go#L74-L208).
- Pattern: implement `PubSubServiceParams[Msg]` interface → call `libp2p.NewPubSubService(...)` at module startup. Done.
- TSS already implements the exact gossip+channels request-response pattern we need ([`modules/tss/p2p.go:101-177`](../../modules/tss/p2p.go#L101-L177)) — requester broadcasts via pubsub, responders send back via pubsub, requester reads from a buffered channel. Direct port to IS-lock attestation flow.
- Topic versioning (`/islock-attestation/v1` → `/v2` later) handles forward-compatibility cleanly.
- Validator-targeted broadcast: just filter in `ValidateMessage()` callback by witness-set membership (TSS does this at [`modules/tss/p2p.go:51-92`](../../modules/tss/p2p.go#L51-L92)).
- No blockers. ~200 lines in a new `modules/islock-attestation/p2p.go` + 50 in `types.go` + 5 in wiring.

### Q3: Can validator BLS keys safely be reused for IS-lock attestations?

**YES with a domain-separation prefix. Precedented in existing code.**

- Validators have a single BLS key (`BlsPrivKeySeed` in `identityConfig.json`, see [`modules/common/config.go:54-86`](../../modules/common/config.go#L54-L86)) used for blocks, votes, TSS readiness, BLS PoP.
- PoP signing already uses domain prefix `"VSC-BLS-POP-v1"` ([`lib/dids/bls.go:185`](../../lib/dids/bls.go#L185)). We mirror this pattern.
- **Canonical signed message for our use:**
  ```
  H("dash-is-lock-v1\0" || chainID || epoch || txid || rawTxHex_hash || instruction_hash)
  ```
- The `"dash-is-lock-v1\0"` prefix prevents collision with any block CID (blocks use CBOR-encoded structures, never start with this ASCII pattern).
- Compromise scope is unchanged (one key controls all signing domains regardless of reuse). Recovery: manual validator removal + re-registration — same as today.
- No HKDF-derived key needed. The seed never leaves process memory; key reuse is operationally simple and audit-friendly.

## Decision

**Build v1 (lazy attestation with Magi validator BLS quorum).**

Reasoning:

1. **Both paths need BLS-in-WASM.** v1 verifies Magi-validator sigs in the contract; v2-true verifies Dash-masternode sigs. Same host function either way (~2-3 days).
2. **v2-true's extra scope is the LLMQ snapshot registry**: tracking Dash masternode set, oracle observation of rotations, contract storage of snapshots. That's a ~2-3 week chunk of net-new infrastructure.
3. **v1 reuses Magi's existing validator set** (already in `db.Elections`, already tracked for consensus). No new infrastructure for tracking external sets.
4. **Trust model is equivalent in practice.** v1 trusts Magi witness quorum (which Magi users already accept for all L2 ops). v2-true trusts Dash masternode quorum (which Magi users already implicitly accept for any mapped-DASH operation). Both inherit chain trust.
5. **v2-true remains additive.** When/if there's value in pure trustless verification, the BLS-in-WASM host function from v1 is the same primitive; we just add the LLMQ snapshot piece. No throwaway work.

## Revised effort estimate

| Workstream | v1 estimate |
|---|---|
| 0. Scoping spike (this doc) | ✓ Done |
| 1. `lib/dids/dash.go` + Parse/VerifyAddress | 3-5 days |
| 2. Magi VM `call_as` host function + `effectiveCaller` | 1 week |
| 3. `system-config.TrustedForwarders` field | 1-2 days |
| 4. Lazy-attestation p2p protocol + validator integration | **2 days** (revised down from 1-2 weeks) |
| 4a. BLS-in-WASM host function (`crypto.bls_verify`) | **2-3 days** (new, but needed for v1) |
| 5. `dash-mapping-contract` extensions | 2 weeks |
| 6. `dash-forwarder-contract` (new) | 1 week |
| 7. IS Service (new container) | 1-2 weeks |
| 8. Altera frontend integration | 1-2 weeks |
| 9. DEX router `effectiveCaller` opt-in | few days |
| 10. End-to-end devnet + testnet validation | 1-2 weeks |

**Total: ~6-10 weeks** focused work, ~4-7 weeks wall-time with 2-3 engineers in parallel. **Less than the spec's original 7-12 week estimate** because the scoping spike confirmed all the "if rigid then expensive" branches in the spec are false.

## What this changes in the spec (rev 7 candidate)

Minor updates to fold in the spike's findings:

1. **§9 Workstream 4** is split into 4 (p2p protocol) + 4a (BLS-in-WASM). Both move to "low-risk, ~1 week combined." The "biggest schedule risk" warning was wrong — risk is actually low.
2. **§5.2.6** canonical signed message updated to `H("dash-is-lock-v1\0" || chainID || epoch || txid || rawTxHex_hash || instruction_hash)` — add the domain prefix.
3. **§10.2 (BLS-in-WASM v2-true)** repositioned as additive future work, not an alternative. The BLS host function from this workstream gets used by both v1 (verify validator sigs) and v2-true (verify Dash masternode sigs).
4. **§4 locked-in decisions** updated with the spike's outcome.

These are small edits, ~30 lines total.

## Recommendations for proceeding

- **Open WIP PR early on the BLS-in-WASM host function** (workstream 4a). It's small, self-contained, and unblocks both v1's attestation verification and any future v2-true work. Get it reviewed and landed first.
- **Start workstream 1 (DashDID) in parallel** — independent, low-risk, ~3-5 days.
- **The p2p attestation protocol (workstream 4) can land after workstream 1 + 4a are review-stable** — no need to rush it.

## Files referenced during the spike

- `modules/wasm/sdk/sdk.go` — host function registration pattern
- `modules/common/params/params.go` — gas / RC constants (line 11: 1000 RC ≈ 1 HBD)
- `modules/transaction-pool/utils.go:54-59` — per-op RC costs
- `lib/dids/bls.go` — BLS verification implementation
- `modules/p2p/pubsub.go` — gossipsub service pattern
- `modules/p2p/libp2p.go` — libp2p layer
- `modules/tss/p2p.go` — request-response pattern reference
- `modules/tss/tss.go:110-143` — TSS readiness attestation (similar BLS signing pattern)
- `modules/block-producer/blockProducer.go:633, 709` — current BLS signing path
- `modules/common/config.go:54-86` — validator BLS key management
- `modules/wasm/e2e/go_wasm/sdk/env.go` — Env struct with `msg.payer` TODO
