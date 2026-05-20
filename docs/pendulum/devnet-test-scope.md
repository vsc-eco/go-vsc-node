# Pendulum end-to-end devnet test scope

**Status:** scoping. No tests in this scope landed yet. The existing
devnet tests on this branch (`fixes/pendulum-devnet-tests`) cover
narrow PR-specific fixes (BLS proof-of-possession propagation,
election consensus determinism, the PR #181 unstake guard, and the
critical-audit pending-actions cursor); none of them exercise the
pendulum pipeline.

This document is the working spec for a follow-on test workstream
that covers the pendulum pipeline end-to-end on a real multi-node
docker devnet — feed publish → oracle window → settlement payload →
reward distribution + bond reduction. It's a milestone plan you
should treat as a strawman, not a contract: open questions are
called out as **Q:**.

## Why this needs devnet, not just unit

Pendulum's unit-test coverage is strong (see [`README.md`](README.md)
"Implemented" table). What unit tests cannot prove:

- **Consensus determinism**: every node must compute identical
  settlement payloads from the same oracle inputs. Float math, map
  iteration, or BSON-vs-canonical drift in the settlement compose
  path would diverge across nodes — only multi-node devnet exposes
  this.
- **Feed propagation timing**: the oracle window (`window.go`) and
  moving average (`movingavg.go`) consume `feed_publish` L1 ops
  produced by `cmd/feed-publisher`. Unit tests stub the input; only
  devnet proves the published payload actually flows through Hive →
  HAF → magid → oracle DB on every node.
- **Slashing → bond reduction**: a node that misses attestations
  must see its committee bond reduced via
  `ApplyRewardReductionsToBonds`. The unit test
  `TestApplyRewardReductionsToBonds_*` proves the math, but the
  bare-key → `hive:`-namespace fix from `8130b95e` only matters in
  the live ingest path where committee bonds are read from chain
  state and reductions arrive keyed by bare account name from
  `m.Account`. Devnet is the only place that loop closes.
- **State-engine application**: `modules/state-processing/
  pendulum_settlement.go` translates settlement payloads into ledger
  writes. Cross-validation between what settlement produces and
  what the state engine ingests is consensus-critical.

## Pipeline to cover

```
                  cmd/feed-publisher                      Hive L1
                          |                                  |
                          v                                  v
              broadcast feed_publish ops  --->  HAF stream ingests
                                                            |
                                                            v
                                          state_engine.processFeedPublish
                                                            |
                                                            v
                              incentive-pendulum/oracle (FeedTracker,
                                  Window, MovingAvg, Geometry)
                                                            |
              committee bonds ----+        +--- pendulum.Quote / Split
              (read from chain)   |        |    StabilizerMultiplier
                                  v        v
                          incentive-pendulum/settlement
                        (compose.go, calculator.go, payload.go)
                                                            |
                                                            v
                                 state-processing/pendulum_settlement
                                       (apply payloads to ledger)
                                                            |
                                                            v
                              ledger / committee bonds updated
                                                            |
                                                            v
                          (next epoch) ApplyRewardReductionsToBonds
                          drains slashed bonds further
```

Each arrow is a place an E2E test can plant an observable check.

## Phase boundaries and milestones

Test work is sized to roughly one milestone per session-day. Each
milestone produces one focused devnet test plus whatever helper
plumbing it needs.

### M1 — Infrastructure bootstrap (1-2 days)

Before any pendulum test can land, we need:

- **Confirm feed-publisher inside devnet docker-compose.** It's
  built by the Makefile (`cmd/feed-publisher`); need to verify
  `tests/devnet/docker-compose.yml` or the generated nodes-override
  starts it. If not, add it as a sidecar service so feeds flow
  during tests.
- **Settlement-result reader helper.** Devnet-side accessor that
  reads the last N settlement payloads from a node's mongo
  (collection TBD — likely `pendulum_settlements` based on
  `modules/db/vsc/pendulum_settlements/` schema).
- **Committee-bond reader helper.** Same pattern, reading the
  per-account bond ledger at a given block height. Used by both
  the happy-path test and the slashing test.
- **Pendulum-aware test config.** Variant of `tssTestConfig()` that
  also enables shorter pendulum settlement intervals and a fast
  feed-publisher tick.

Deliverable: helpers in a new file like `pendulum_helpers_test.go`,
no test functions yet. Verified by spinning up the devnet and
hand-checking the readers return non-empty data.

### M2 — TestPendulumFeedPropagation (1-2 days)

Cheapest pendulum-only test. Proves the feed-publish path is wired
correctly end-to-end across every node.

- Spin up devnet. Wait for genesis + first running election.
- Wait until the feed-publisher has broadcast N feeds (verifiable
  from L1 block contents via existing Hive helpers).
- For each node, read the oracle DB / FeedTracker state.
- Assert: every node has the same number of stored feed
  observations, with the same price values per L1 block height.

This is the pendulum equivalent of `TestBlsProofOfPossessionPropagation`:
prove the L1 → ingest → store path round-trips identically per node.

### M3 — TestPendulumSettlementHappyPath (2-3 days)

The flagship test. Drives a full settlement cycle and verifies the
output landed on every node deterministically.

- Spin up devnet with enough nodes for a real committee (5+).
- Let feeds run for at least one full pendulum settlement window
  (Q: how long is that on devnet — what's the settlement interval
  in `tssTestConfig`'s pendulum overrides?).
- Wait for a settlement payload to land (epoch boundary in
  pendulum_settlement.go).
- For each node:
    - Read the settlement payload from `pendulum_settlements`.
    - Assert the payload byte-matches across nodes.
    - Read committee bonds before/after settlement.
    - Assert bond deltas match the settlement payload's
      distributions to within rounding.
- Sanity sweep: no node logged "settlement validation failed" or
  "pendulum compose drift" patterns.

### M4 — TestPendulumSlashingReductionApplied (2-3 days)

The test that actually exercises the bare-key → `hive:`-namespace
fix from `8130b95e`. Without slashing evidence in the oracle
window, reductions never compute; this test makes that loop close.

- Spin up devnet, let it settle one normal epoch as a baseline.
- Stop one non-genesis node (`d.StopNode`) for a full window's
  worth of feeds so it accumulates `OracleEvidence` of missed
  attestations.
- Restart the node.
- Wait for the next settlement.
- Read the settlement payload's `RewardReductionApplied` section.
- Assert the stopped node appears with `Bps > 0`.
- Read the node's committee bond before and after.
- Assert the bond was reduced by `floor(orig * Bps / 10000)` (matches
  the `MulDivFloorI64` formula in calculator.go).

**Critical:** under the bug (`out[acc]` vs `out["hive:"+acc]`),
`orig` was always 0 so this assertion would fail with `eff == orig`
(no reduction applied) — exactly the regression signal.

### M5 — TestPendulumStabilizerEngages (1-2 days, optional)

Drives the oracle into a state where the stabilizer (m(s,r)) caps
or pushes the fee. Less consensus-critical than M3/M4 but proves
the §5 logic from the PDF works end-to-end. Defer if M1-M4 already
took a week.

## Out of scope for this workstream

- The "Not implemented" entries in [`README.md`](README.md):
  streaming swaps (§4/§12), redirect execution (§9), stabilizer
  surplus settlement (§9/§12). Devnet tests cannot precede the
  implementation.
- Table 1 dollar-scenario reproduction. That's a deterministic
  numeric regression — pure unit test territory, no devnet value.
- Float vs fixed-point money rework (called out in README as a TODO).
  Devnet tests should assume float consensus money for now and
  follow the implementation if it changes.
- Performance / throughput characterization. Out of scope for
  correctness tests.

## Open questions for the workstream owner

**Q1 — feed-publisher topology.** Does devnet's docker-compose
already include a feed-publisher sidecar, or does test code need
to start one? Affects M1 sizing significantly.

**Q2 — settlement interval on devnet.** What's the smallest sane
pendulum settlement interval that still produces meaningful
observation windows in test runtime? Need a `SysConfigOverrides`
knob analogous to `RotateInterval` / `SignInterval` for TSS, scaled
down from mainnet defaults.

**Q3 — settlement collection name.** Confirm `pendulum_settlements`
is the right collection on each node's mongo; if not, what is.

**Q4 — committee bond storage location.** Bonds are referenced via
`ReadCommitteeBonds` in calculator.go — where do they live on
chain? Per-account ledger entries? A dedicated collection? This
needs a code read before M1 can ship the bond reader helper.

**Q5 — multi-version compat.** Should M3 also run as a multi-version
test (some nodes on old code, some new), like the existing
`TestTSSMultiVersion`? Probably yes long-term but adds 1-2 days
per test — defer unless we expect a network with pendulum behind a
hardfork gate.

## Calibration

These tests must match the operational shape of
`tests/devnet/bls_pop_test.go` and `tests/devnet/review2_keystore_test.go`:

- Skip on `testing.Short()`.
- Use `startDevnet*` helpers, never inline lifecycle.
- Per-node assertions surface as `t.Run` sub-tests so a single
  failing node doesn't mask the others.
- Log substrings (warnings, validation failures) get a final sweep
  with `d.Logs` to catch class-of-bug failures even when the
  numeric assertions happen to align.

## Effort budget

| Milestone | Days | Cumulative |
|---|---|---|
| M1 helpers + infra | 1-2 | 1-2 |
| M2 FeedPropagation | 1-2 | 2-4 |
| M3 SettlementHappyPath | 2-3 | 4-7 |
| M4 SlashingReduction | 2-3 | 6-10 |
| M5 StabilizerEngages | 1-2 (optional) | 7-12 |

Total: **6-10 days** for core (M1-M4), **7-12 days** with M5.

Devnet runtime per test is non-trivial (~10-30min each), so CI
gating should be opt-in (workflow_dispatch + nightly). Each test
runs under `go test -v -run ... -timeout 30m ./tests/devnet/`
locally.
