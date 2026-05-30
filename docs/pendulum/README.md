# Incentive pendulum — documentation

This folder holds the **scope document**, the **formal math PDF**, and this README (implementation status, divergence notes, and **test coverage** for the `modules/incentive-pendulum` library).

| File | Description |
|------|-------------|
| [`scope.md`](scope.md) | Product scope (HackMD source: “MAGI INCENTIVE PENDULUM SCOPE”). |
| [`Magi.pdf`](Magi.pdf) | Formal mathematics: “The Incentive Pendulum — Formalized Mathematics (Magi Network)”, March 2026. |

**Code:** [`modules/incentive-pendulum`](../../modules/incentive-pendulum) in the `go-vsc-node` tree.

> **The implementation supersedes `Magi.pdf`.** Equilibrium moved from \(s=0.5\) to **\(s=1.0\)** (\(T/V=1.5\), keeping \(E=\tfrac23T\)). Hard cliff is now **\(s \ge 3\)**; equilibrium split **60/40** node/LP; yield ratio **\(2s^2/(3-s)\)**; §9 redirect **direction corrected** (low \(s\)→LPs, high \(s\)→nodes); bands are **derived from `s_eq` + yield-ratio thresholds** in [`params.go`](../../modules/incentive-pendulum/params.go). The library is integer / basis-point throughout: the legacy float `pendulum.go` / `fees.go` / `collateral.go` and the float `dexfeed.go` bolt have been retired in favour of the `*_int.go` files, the `wasm` applier, and the `oracle` geometry computer.

---

## Normative sources

- **PDF (math document):** Fee structure, closed-form reward split, stabilizer, operating bands, edge cases. Denoted below as **from the PDF**.
- **Scope document:** High-level product goals, single-side HBD tracking, optional min/max reward bounds. Denoted below as **from the scope document**.

Where the two differ (e.g. reward floors/ceilings), the implemented library follows **from the PDF** unless a future governance decision adopts **from the scope document** constraints.

---

## Implemented (from the PDF)

| Topic (PDF section) | Implementation |
|---------------------|----------------|
| §1 Variables \(s,u,w,R,r\), E/T/V/P | `SplitInputsInt` in [`pendulum_int.go`](../../modules/incentive-pendulum/pendulum_int.go); E/T/V/P/s computed in [`oracle/geometry.go`](../../modules/incentive-pendulum/oracle/geometry.go) |
| §2 CLP fee \(x^2Y/(x+X)^2\) | `CLPFeeInt` in [`fees_int.go`](../../modules/incentive-pendulum/fees_int.go) |
| §3 CLP/total fee mix, pendulum share of fees | `ApplySwapFees` (baseCLP + baseProtocol legs) in [`wasm/applier.go`](../../modules/incentive-pendulum/wasm/applier.go); `ProtocolFeeRateBps` in `fees_int.go` |
| §5 Stabilizer \(m(s,r)\), cap, push 1.0 / 0.7 | `StabilizerMultiplierBps` in [`fees_int.go`](../../modules/incentive-pendulum/fees_int.go); applied by `ApplySwapFees` |
| §6–§7 Split, yields, \(2s^2/(c-s)\) (\(c=3\)) | `SplitInt`, `YieldRatioBps`, tests |
| §8 Table 2 behaviour | `TestSplitInt_*` in [`pendulum_int_test.go`](../../modules/incentive-pendulum/pendulum_int_test.go); swap scenarios in [`wasm/applier_test.go`](../../modules/incentive-pendulum/wasm/applier_test.go) |
| §9 Redirect *recommendation* (not execution) | `ProtocolFeeRedirectRecommendedBps`, `ProtocolFeeRedirectToNodesBps` in [`collateral_int.go`](../../modules/incentive-pendulum/collateral_int.go) |
| Oracle participation slashing (window evidence) | `SlashParams`, `OracleEvidence`, `SlashBps` in [`slashing.go`](../../modules/incentive-pendulum/slashing.go) |
| §11 Safety bands (cliff, ideal, safe, warning, extreme low/high) | `CollateralReportBps` / `CollateralFromSVBps` in [`collateral_int.go`](../../modules/incentive-pendulum/collateral_int.go) |

---

## Not implemented or only partial (from the PDF)

| Topic | Gap |
|-------|-----|
| §4, §12 **Streaming swaps** | No multi-block stream planner; no **aggregate \(r\)** over a stream for \(m(s,r)\). |
| §9 **Redirect execution** | Booleans only; no ledger moves of protocol-fee revenue to starved side. |
| §9 / §12 **Stabilizer surplus** | `SwapFeeQuote.StabilizerSurplus` is computed; no settlement policy for where surplus goes. |
| **Table 1** (dollar scenarios) | Not reproduced as full numeric regression tests. |
| **Consensus money** | Done: the library is integer / basis-point throughout (`*_int.go`, `big.Int` minor units). On-chain settlement rounding lives in [`settlement`](../../modules/incentive-pendulum/settlement) + the ledger, not in this math layer. |

---

## Implemented (from the scope document)

| Topic | Implementation |
|-------|----------------|
| Split LP vs nodes from LP/stake geometry | `SplitInt` in `pendulum_int.go`; driven by `settlement.CalculateSplitPreviewFixed` and the `wasm` `ApplySwapFees` |
| Single-side HBD, vault aggregation \(V \approx 2\sum P_{\text{HBD}}\) | `GeometryComputer.Compute` (V = 2P) in [`oracle/geometry.go`](../../modules/incentive-pendulum/oracle/geometry.go) |
| Global pool eligibility (current) | `WhitelistGetter` callback on the `wasm` `Applier` ([`wasm/applier.go`](../../modules/incentive-pendulum/wasm/applier.go)); only whitelisted contracts accrue |
| Target **1.5×** overcollateral | Now the **equilibrium**: \(s_{eq}=1.0\) with \(E=\tfrac23T\) gives \(T/V=u/s_{eq}=1.5\). The pendulum drives toward it via the fee split (it is the equal-yield fixed point), rather than being a passed-in input. |

---

## Not implemented (from the scope document)

| Topic | Gap |
|-------|-----|
| **Internal market** for HBD | Out of library; no DEX ops in this repo. |
| **User pools via DAO approval** | Planned: DAO-voted `(runtime, code_hash)` allowlist for eligibility; not yet wired. |
| **Min/max 90% / 10%** and **neither side 0%** | **Intentionally omitted** in code: these rules **conflict** with the closed-form §6.1 (0% LP at \(s \ge c\), \(c=3\)). Implemented behaviour follows the generalized closed form. |

---

## Oracle / participation (design notes, not in the PDF file)

| Topic | Implementation |
|-------|----------------|
| Rolling witness signature window | [`oracle/window.go`](../../modules/incentive-pendulum/oracle/window.go) |
| Trust: **≥4** signatures + feed update | `FeedTrust` in [`oracle/feed.go`](../../modules/incentive-pendulum/oracle/feed.go) |
| Trusted quote mean, MA ring | `TrustedHivePriceBps`, `MovingAverageRing` |
| Trusted running witness group (cap 20) + APR mode from properties | `RunningWitnessGroup`, `HBDAPRModeFromGroup` in [`oracle/properties.go`](../../modules/incentive-pendulum/oracle/properties.go) |
| Hive block ingestion + `height % 100` tick | [`oracle/tracker.go`](../../modules/incentive-pendulum/oracle/tracker.go) updated from [`state_engine.go`](../../modules/state-processing/state_engine.go) (`ProcessBlock`); `StateEngine.PendulumFeedTracker()` |
| Tick slashing evidence | `FeedTickSnapshot.WitnessSlashBps` and env key `pendulum.witness_slash_bps` |
| Custom ops / consensus-enforced oracle tx | **Not** wired |

---

## Test coverage

Run from repository root:

```bash
go test ./modules/incentive-pendulum/... -count=1
```

### Package `pendulum` (`modules/incentive-pendulum`)

| Test file | What it covers |
|-----------|------------------|
| [`pendulum_int_test.go`](../../modules/incentive-pendulum/pendulum_int_test.go) | 60/40 split at equilibrium \(s=1.0\); hard cliff \(s \ge 3\); conservation (`FinalNodeShare + FinalPoolShare = R`); degenerate-vault and invalid-input fallbacks. |
| [`params_test.go`](../../modules/incentive-pendulum/params_test.go) | Cliff derivation \(c = 2s_{eq}^2 + s_{eq}\); the eight derived band edges (independent oracle table); yield-ratio round-trip; faithfulness check (\(s_{eq}=0.5\) reproduces the old \(c=1\) / 0.30–0.70 bands). |
| [`fees_int_test.go`](../../modules/incentive-pendulum/fees_int_test.go) | `CLPFeeInt`; stabilizer **\(m=1\)** at \(s_{eq}=1.0\), grid of \(\lvert s-s_{eq}\rvert\) deviations, cap enforcement, overflow guards. |
| [`slashing_test.go`](../../modules/incentive-pendulum/slashing_test.go) | Slashing schedule invariants: compliant witness = 0 bps, additive penalties (signature deficit/update/equivocation), and cap enforcement. |
| [`collateral_int_test.go`](../../modules/incentive-pendulum/collateral_int_test.go) | Curve-derived band flags (`IdealZone`, `SafeGrowth`, `WarningZone`, `ExtremeLow`, `ExtremeHigh`, `UnderSecured`); corrected redirect direction; `EffectiveBondHBDInt` edge cases. |
| [`integration_test.go`](../../modules/incentive-pendulum/integration_test.go) | `TestOracleIntegration`: signature window → `FeedTrust` → `TrustedHivePriceBps` → `MovingAverageRing` (integer oracle path; the float bolt-evaluate path is retired). |

### Package `oracle` (`modules/incentive-pendulum/oracle`)

| Test file | What it covers |
|-----------|------------------|
| [`oracle_test.go`](../../modules/incentive-pendulum/oracle/oracle_test.go) | `WitnessProductionWindow` ring eviction; `FeedTrust` thresholds; `TrustedHivePriceBps` filtering. |
| [`movingavg_test.go`](../../modules/incentive-pendulum/oracle/movingavg_test.go) | `MovingAverageRing` mean over partial/full buffer, `Reset`, rejection of non-positive values. |

### Coverage gaps (intentional)

- **Streaming** and **multi-block** \(r\) aggregation: no tests until implemented.
- **On-chain** ledger redirect and **integer** rounding: not in this module.
- **Full Table 1** dollar scenarios: not asserted.

---

## Related

- Historical overview (may overlap): [`../incentive-pendulum.md`](../incentive-pendulum.md).
