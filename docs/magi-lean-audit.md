# Magi Lean Formal Audit

Date: 2026-04-29  
Scope: current uncommitted pendulum diff (`ledger-system`, `state_engine`, `modules/incentive-pendulum/settlement`, related tests)

## Findings (ordered by severity)

### 1) Settlement boundary handler is effectively disabled
- **Severity:** Critical
- **Where:** `modules/state-processing/state_engine.go`
- **Issue:** `pendulumsettlement.New(...)` is initialized with `selfID = ""`, but `Engine.ProcessBlock()` only runs `onBoundary` when `leader == selfID`.
- **Impact:** Epoch-boundary settlement logic never executes on any real node (unless a schedule leader is the empty string, which should not happen).
- **Evidence:**

```1665:1687:modules/state-processing/state_engine.go
se.pendulumSettle = pendulumsettlement.New(
	se.electionDb,
	"",
	...
)
```

```82:85:modules/incentive-pendulum/settlement/engine.go
leader := schedule[0]
if leader != e.selfID {
	return nil
}
```

### 2) Duplicate-tx guard is process-local memory, not consensus-state safe
- **Severity:** High
- **Where:** `modules/ledger-system/ledger_system.go`
- **Issue:** `pendulumTxIDLedger` stores seen tx IDs in in-memory map only.
- **Impact:** Behavior can differ after restart/replay (same historical block can be accepted/rejected differently depending on process lifetime). This is not consensus-safe for state-relevant operations.
- **Evidence:**

```45:61:modules/ledger-system/ledger_system.go
pendulumMu         sync.Mutex
pendulumTxIDLedger map[string]struct{}
...
func (ls *ledgerSystem) recordPendulumTxID(txID string) bool {
	...
}
```

### 3) Duplicate-tx guard is global across op types; blocks valid multi-entry settlement flows
- **Severity:** High
- **Where:** `modules/ledger-system/ledger_system.go`
- **Issue:** `PendulumAccrue`, `PendulumConvert`, and `PendulumDistribute` all call the same `recordPendulumTxID`.
- **Impact:** A single settlement op carrying multiple conversions/distributions under one transaction ID can fail after first entry. The guard should be scoped to operation identity (or deterministic sub-id), not globally by `txID`.

### 4) Deterministic payload check is currently tautological
- **Severity:** Medium
- **Where:** `modules/state-processing/state_engine.go`
- **Issue:** `expectedPayload` and `gotPayload` are built from the exact same in-memory inputs in the same function.
- **Impact:** Validation cannot catch divergence between leader payload and replayed computation; it always passes unless serialization code itself is broken.
- **Evidence:**

```1725:1742:modules/state-processing/state_engine.go
expectedPayload := pendulumsettlement.BuildSettlementPayload(...)
gotPayload := pendulumsettlement.BuildSettlementPayload(...)
if err := pendulumsettlement.ValidateSettlementPayloadDeterministic(expectedPayload, gotPayload); err != nil {
	...
}
```

### 5) Leader selection uses first schedule entry, not explicit slot leader
- **Severity:** Medium
- **Where:** `modules/incentive-pendulum/settlement/engine.go`
- **Issue:** `leader := schedule[0]` assumes first schedule element is current slot leader.
- **Impact:** Settlement may run for wrong node or never run depending on schedule layout; this can diverge from existing slot-selection logic in `state_engine`.

### 6) Consensus-path preview still uses float math
- **Severity:** Medium
- **Where:** `modules/incentive-pendulum/settlement/calculator.go`
- **Issue:** `CalculateSplitPreview` converts int -> float and calls float `pendulum.Split`.
- **Impact:** Not aligned with the stated no-float consensus requirement for pendulum paths; potential cross-platform drift risk.

## Test Coverage Audit

### What is covered
- `settlement/engine_test.go`
  - verifies one epoch transition triggers callback for matching `selfID`.
- `settlement/calculator_test.go`
  - verifies residual assignment behavior for node distribution.
- `settlement/payload_test.go`
  - verifies deterministic sorting/equality for payload builder.
- `state-processing/ledger_system_test.go` (`TestPendulumLedgerOps`)
  - verifies success/failure return values for new ledger methods.

### Critical coverage gaps

1. **No test catches the disabled settlement handler bug**
- Missing integration assertion that boundary callback runs in real `StateEngine.New(...)` wiring.

2. **No semantic assertions for ledger writes**
- Current test only checks `result.Ok`; it does not verify:
  - owner bucket naming (`pendulum:pool:*`, `pendulum:global:HBD_R`)
  - asset correctness
  - signed amounts (debit negative / credit positive)
  - tx IDs and op types persisted.

3. **No restart/replay idempotency tests**
- No tests around restart semantics for in-memory duplicate guard.

4. **No tests for multi-entry settlement using shared tx ID**
- Missing test showing whether multiple conversions/distributions in one settlement are accepted.

5. **No negative/pathological tests for settlement engine**
- Missing tests for:
  - non-leader node should not execute,
  - empty/invalid schedule handling,
  - election lookup failure behavior,
  - correct slot leader extraction.

6. **No consensus-parity tests for split preview**
- No invariant tests that preview math matches required integer implementation (`SplitInt`) once available.

## Recommended Remediation Order

1. Wire a real node identity into settlement engine (`selfID`) and verify with integration test.
2. Replace process-local tx duplicate tracking with deterministic persisted idempotency keying.
3. Scope idempotency key to per-entry operation ID (e.g., `txID#index`), not raw tx ID.
4. Replace tautological validation with comparison against externally supplied settlement payload.
5. Fix leader selection to use explicit current slot account from schedule metadata.
6. Move split preview path to integer math implementation and add parity/determinism tests.

## Audit Verdict

- **Current status:** Not merge-ready for consensus-path settlement behavior.
- **Reason:** One critical execution blocker + multiple high-risk determinism/idempotency gaps + insufficient semantic test coverage on ledger mutation paths.

---

## Remediation Update (2026-04-29)

The following issues from this audit have now been addressed in the working diff:

- `selfID == ""` no longer disables settlement boundary handling. Engine now only enforces leader filtering when `selfID` is explicitly set.
- Leader detection now resolves by matching slot start height using schedule entries (`Account`, `SlotHeight`) rather than relying on `schedule[0]`.
- In-memory `pendulumTxIDLedger` duplicate guard was removed.
  - Idempotency now relies on deterministic ledger record IDs (upsert semantics in real DB).
  - IDs are scoped with operation target (`pool` / `destination`) to support multi-entry settlement under one tx id.
- Tautological expected-vs-got payload check in `state_engine` callback was removed.
- Test coverage was expanded:
  - non-leader/leader/no-self-filter settlement-engine behavior,
  - payload mismatch detection,
  - semantic assertions for pendulum ledger records (bucket owner, debit/credit signs, op types),
  - shared tx-id multi-destination distribution acceptance.

Remaining note:
- Consensus-path split computation in settlement preview still calls float `pendulum.Split`; this remains intentionally interim until `SplitInt` path is wired in.

---

## Lean Audit Pass 2 (2026-04-29)

Scope delta: latest uncommitted changes after remediation update (redirect bucket + slash-aware preview + expanded tests).

### Findings (ordered by severity)

### 1) `R` read path is currently inconsistent with pendulum ledger op types
- **Severity:** Critical
- **Where:** `modules/state-processing/state_engine.go`, `modules/ledger-system/ledger_state.go`
- **Issue:** settlement preview reads `R` via `LedgerSystem.GetBalance("pendulum:global:HBD_R", ..., "hbd")`, but `LedgerState.GetBalance(..., "hbd")` only includes ledger op types `unstake` and `deposit`.
- **Impact:** `pendulum_convert` and `pendulum_distribute` records on `pendulum:global:HBD_R` are excluded from balance computation; `R` can appear as zero even when bucket has funds, suppressing settlement execution.

### 2) Pendulum distribute op can create unbounded negative global bucket
- **Severity:** High
- **Where:** `modules/ledger-system/ledger_system.go`
- **Issue:** `PendulumDistribute` debits `pendulum:global:HBD_R` without checking available balance.
- **Impact:** invalid settlement payload (or bug) can mint effective debt in the global bucket; no guardrail in typed op layer.

### 3) Slash source is tick-local, not epoch-aggregated
- **Severity:** Medium
- **Where:** `modules/state-processing/state_engine.go`
- **Issue:** slash bps are read from `FeedTracker.LastTick()` only.
- **Impact:** does not represent the full closed-epoch slash total described by W5 plan; payout inputs can diverge from intended economics.

### 4) Consensus-path preview still uses float split
- **Severity:** Medium
- **Where:** `modules/incentive-pendulum/settlement/calculator.go`
- **Issue:** `CalculateSplitPreview` still uses float `pendulum.Split`.
- **Impact:** deterministic replay risk remains until integer split path is wired.

## Test Coverage Pass 2

### Improved coverage observed
- Added semantic assertions for pendulum ledger writes (`owner`, signed debit/credit, op types).
- Added shared-tx multi-destination distribution case.
- Added redirect bucket accrual test.
- Added slash calculator unit test and settlement engine leader/non-leader variants.

### Remaining high-value test gaps
1. **Missing regression test for `R` visibility through `LedgerSystem.GetBalance`**
- Need a test that accrues/convert/distribute on `pendulum:global:HBD_R` then asserts `LedgerSystem.GetBalance("pendulum:global:HBD_R", ...)` reflects expected value.

2. **No insufficient-balance test for `PendulumDistribute`**
- Need negative-path test proving distribution rejects when global bucket lacks funds.

3. **No state-engine integration test for non-zero `R` boundary path**
- Need test that boundary callback reaches split/distribution path when bucket has funds.

4. **No epoch aggregation test for slashes**
- Need test validating slash inputs are aggregated over closed epoch (once implemented).

## Pass 2 Verdict

- **Current status:** improved but still not merge-ready for full W5 settlement correctness.
- **Primary blocker:** critical `R`-balance read mismatch between settlement logic and ledger balance filters.

---

## Lean Audit Pass 3 (2026-04-29)

Scope delta: fixes applied after Pass 2 (pendulum bucket balance path + insufficient-funds guard + regression tests).

### Remediations confirmed

1. **Resolved: `R`-balance read mismatch**
- `state_engine` now reads settlement `R` via pendulum-aware API:
  - `LedgerSystem.PendulumBucketBalance("pendulum:global:HBD_R", bh)`
- This bypasses generic `GetBalance("hbd")` op-type filters that excluded pendulum ledger types.

2. **Resolved: global bucket underflow guard**
- `PendulumDistribute` now checks global bucket availability before debit.
- Returns explicit failure (`insufficient pendulum global balance`) when funds are insufficient.

3. **Resolved: regression coverage for above**
- Added/updated tests in `modules/state-processing/ledger_system_test.go`:
  - insufficient global-bucket distribution fails,
  - pendulum bucket balance reflects convert/distribute net effects.

### Remaining important gaps

1. **W5 still scaffold-level (not full settlement op execution path)**
- no full `conversions` construction from closed-epoch per-pool native buckets,
- no on-chain emit/apply for `vsc.pendulum_settlement`,
- no replay-side payload-vs-recompute validation against incoming op body.

2. **Slash timing still tick-local**
- currently derived from latest tick snapshot rather than full closed-epoch aggregation.

3. **Consensus-int math pending**
- settlement preview split still uses float `pendulum.Split`; should migrate to integer split path once available.

## Pass 3 Verdict

- **Current status:** materially improved and safer for incremental development; major blockers from Pass 2 fixed.
- **Still not complete for W5 finalization:** orchestration/emission/replay validation and integer settlement math remain.
