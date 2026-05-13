# Incentive pendulum — documentation

This folder holds the **scope document**, the **formal math PDF**, and this README (implementation status, divergence notes, and **test coverage** for the `modules/incentive-pendulum` library).

| File | Description |
|------|-------------|
| [`scope.md`](scope.md) | Product scope (HackMD source: “MAGI INCENTIVE PENDULUM SCOPE”). |
| [`Magi.pdf`](Magi.pdf) | Formal mathematics: “The Incentive Pendulum — Formalized Mathematics (Magi Network)”, March 2026. |

**Code:** [`modules/incentive-pendulum`](../../modules/incentive-pendulum) in the `go-vsc-node` tree.

---

## Normative sources

- **PDF (math document):** Fee structure, closed-form reward split, stabilizer, operating bands, edge cases. Denoted below as **from the PDF**.
- **Scope document:** High-level product goals, single-side HBD tracking, optional min/max reward bounds. Denoted below as **from the scope document**.

Where the two differ (e.g. reward floors/ceilings), the implemented library follows **from the PDF** unless a future governance decision adopts **from the scope document** constraints.

---

## Implemented (from the PDF)

| Topic (PDF section) | Implementation |
|---------------------|----------------|
| §1 Variables \(s,u,w,R,r\), E/T/V/P | [`pendulum.go`](../../modules/incentive-pendulum/pendulum.go), [`dexfeed.go`](../../modules/incentive-pendulum/dexfeed.go) |
| §2 CLP fee \(x^2Y/(x+X)^2\) | `CLPFee` in [`fees.go`](../../modules/incentive-pendulum/fees.go) |
| §3 CLP/total fee mix, pendulum share of fees | `PendulumFeeFraction`; micro-case test |
| §5 Stabilizer \(m(s,r)\), cap, push 1.0 / 0.7 | `StabilizerMultiplier`, `QuoteSwapFees` |
| §6–§7 Split, yields, \(2s^2/(1-s)\) | `Split`, `YieldRatio`, tests |
| §8 Table 2 behaviour | `TestTable2Behaviour` in [`pendulum_test.go`](../../modules/incentive-pendulum/pendulum_test.go) |
| §9 Redirect *recommendation* (not execution) | `ProtocolFeeRedirectRecommended`, `ProtocolFeeRedirectToNodes` |
| Oracle participation slashing (window evidence) | `SlashParams`, `OracleEvidence`, `SlashBps` in [`slashing.go`](../../modules/incentive-pendulum/slashing.go) |
| §11 Safety bands (cliff, ideal, safe, warning, extreme low) | `CollateralReport` in [`collateral.go`](../../modules/incentive-pendulum/collateral.go) |

---

## Not implemented or only partial (from the PDF)

| Topic | Gap |
|-------|-----|
| §4, §12 **Streaming swaps** | No multi-block stream planner; no **aggregate \(r\)** over a stream for \(m(s,r)\). |
| §9 **Redirect execution** | Booleans only; no ledger moves of protocol-fee revenue to starved side. |
| §9 / §12 **Stabilizer surplus** | `SwapFeeQuote.StabilizerSurplus` is computed; no settlement policy for where surplus goes. |
| **Table 1** (dollar scenarios) | Not reproduced as full numeric regression tests. |
| **Consensus money** | All **float64**; no fixed-point or minor-unit rounding rules for chain state. |

---

## Implemented (from the scope document)

| Topic | Implementation |
|-------|----------------|
| Split LP vs nodes from LP/stake geometry | `Split`, `PendulumBolt.Evaluate` |
| Single-side HBD, vault aggregation \(V \approx 2\sum P_{\text{HBD}}\) | `PoolPendulumLiquidity`, `SumPendulumVault` |
| Global pool eligibility (current) | `PendulumBolt` defaults to DAO-owner-only pools (`hive:vsc.dao`, normalized) |
| Target **1.5×** overcollateral as **input** | \(u = T/E\) passed as `SplitInputs.U` / derived in `Evaluate`; **not enforced** as an invariant. |

---

## Not implemented (from the scope document)

| Topic | Gap |
|-------|-----|
| **Internal market** for HBD | Out of library; no DEX ops in this repo. |
| **User pools via DAO approval** | Planned: DAO-voted `(runtime, code_hash)` allowlist for eligibility; not yet wired. |
| **Min/max 90% / 10%** and **neither side 0%** | **Intentionally omitted** in code: these rules **conflict** with **from the PDF** §6.1 (0% LP at \(s \ge 1\)). Implemented behaviour follows **from the PDF**. |

---

## Oracle / participation (design notes, not in the PDF file)

| Topic | Implementation |
|-------|----------------|
| Rolling witness signature window | [`oracle/window.go`](../../modules/incentive-pendulum/oracle/window.go) |
| Trust: **≥4** signatures + feed update | `FeedTrust` in [`oracle/feed.go`](../../modules/incentive-pendulum/oracle/feed.go) |
| Trusted quote mean, MA ring | `TrustedHivePrice`, `MovingAverageRing` |
| Trusted running witness group (cap 20) + APR mode from properties | `RunningWitnessGroup`, `HBDAPRModeFromGroup` in [`oracle/properties.go`](../../modules/incentive-pendulum/oracle/properties.go) |
| Hive block ingestion + `height % 100` tick | [`oracle/tracker.go`](../../modules/incentive-pendulum/oracle/tracker.go) updated from [`state_engine.go`](../../modules/state-processing/state_engine.go) (`ProcessBlock`); `StateEngine.PendulumFeedTracker()` |
| Tick slashing evidence | `FeedTickSnapshot.WitnessSlashBps` and env key `pendulum.witness_slash_bps` |
| Custom ops / consensus-enforced oracle tx | **Not** wired |

---

## Test coverage

Run from repository root:

```bash
go test ./modules/incentive-pendulum/... -count=1
go test ./modules/incentive-pendulum -fuzz=FuzzSplitConservesR -fuzztime=10s
```

### Package `pendulum` (`modules/incentive-pendulum`)

| Test file | What it covers |
|-----------|------------------|
| [`pendulum_test.go`](../../modules/incentive-pendulum/pendulum_test.go) | PDF **Table 2** behaviour (\(u=1.5\), \(w=2/3\)); hard cliff \(s \ge 1\); **§7** yield ratio identity; CLP fee sanity; **§3** pendulum fee fraction micro-trade; stabilizer **\(m=1\)** at \(s=0.5\). |
| [`property_test.go`](../../modules/incentive-pendulum/property_test.go) | **Conservation:** `FinalNodeShare + FinalPoolShare = R` over a grid; cliff case; **§7** `nodeYield/poolYield` vs `YieldRatio(s)`; **§5** charged total ≥ base subtotal, `AccrueToPendulumR == CLP`; `SumPendulumVault`; `PendulumBolt.Evaluate` with safe-growth \(s\); missing `T` handling; **`FuzzSplitConservesR`** on random valid inputs. |
| [`slashing_test.go`](../../modules/incentive-pendulum/slashing_test.go) | Slashing schedule invariants: compliant witness = 0 bps, additive penalties (signature deficit/update/equivocation), and cap enforcement. |
| [`collateral_test.go`](../../modules/incentive-pendulum/collateral_test.go) | **§11** band flags (`IdealZone`, `SafeGrowth`, `WarningZone`, `ExtremeLow`, `UnderSecured`); `EffectiveBondHBD` edge cases. |
| [`integration_test.go`](../../modules/incentive-pendulum/integration_test.go) | End-to-end: signature window → `FeedTrust` → `TrustedHivePrice` → `MovingAverageRing` → `PendulumBolt.Evaluate` split conservation. |

### Package `oracle` (`modules/incentive-pendulum/oracle`)

| Test file | What it covers |
|-----------|------------------|
| [`oracle_test.go`](../../modules/incentive-pendulum/oracle/oracle_test.go) | `WitnessSignatureWindow` ring eviction; `FeedTrust` thresholds; `TrustedHivePrice` filtering. |
| [`movingavg_test.go`](../../modules/incentive-pendulum/oracle/movingavg_test.go) | `MovingAverageRing` mean over partial/full buffer, `Reset`, rejection of non-positive values. |

### Coverage gaps (intentional)

- **Streaming** and **multi-block** \(r\) aggregation: no tests until implemented.
- **On-chain** ledger redirect and **integer** rounding: not in this module.
- **Full Table 1** dollar scenarios: not asserted.

---

## Related

- Historical overview (may overlap): [`../incentive-pendulum.md`](../incentive-pendulum.md).
