# Incentive pendulum (Magi)

**Primary documentation:** [docs/pendulum/README.md](pendulum/README.md) (scope file, math PDF, implementation gaps, **test coverage**).
This document describes the **Magi incentive pendulum** implementation on branch **`pendulum`**: closed-form economics from the March 2026 PDF (“The Incentive Pendulum — Formalized Mathematics”), plus the **sole HIVE oracle** design (witness participation + moving average). **HBD is treated as \$1** for accounting. **No other protocol oracles** are used for pool assets; CLP pools price assets via **open-market** reserves and swap math.

## Code layout

| Path | Purpose |
|------|---------|
| [`modules/incentive-pendulum/pendulum.go`](../modules/incentive-pendulum/pendulum.go) | \(s=V/E\), \(w=P/V\), closed-form split of \(R\) and yields (PDF §6); hard cliff \(s \ge 1\) |
| [`modules/incentive-pendulum/fees.go`](../modules/incentive-pendulum/fees.go) | CLP fee \(x^2Y/(x+X)^2\), pendulum fee fraction (PDF §3), stabilizer \(m(s,r)\) (§5), protocol redirect helper (§9) |
| [`modules/incentive-pendulum/collateral.go`](../modules/incentive-pendulum/collateral.go) | Collateral bands (PDF §11), `EffectiveBondHBD` (stake × sole oracle × fraction) |
| [`modules/incentive-pendulum/slashing.go`](../modules/incentive-pendulum/slashing.go) | Oracle participation slashing schedule (signature deficit + missing feed update + equivocation, capped) |
| [`modules/incentive-pendulum/dexfeed.go`](../modules/incentive-pendulum/dexfeed.go) | **Bolt-on DEX/LP feed**: pool aggregation (`SumPendulumVault`), `QuoteSwapFees`, `PendulumBolt` + `NetworkSnapshot` / `BoltEvaluation` |
| [`modules/incentive-pendulum/oracle/window.go`](../modules/incentive-pendulum/oracle/window.go) | Rolling window of per-block witness signers (default width **100**) |
| [`modules/incentive-pendulum/oracle/feed.go`](../modules/incentive-pendulum/oracle/feed.go) | Feed trust rule (\(\ge 4\) signatures in window + price update), **simple mean** of trusted HIVE-in-HBD quotes |
| [`modules/incentive-pendulum/oracle/properties.go`](../modules/incentive-pendulum/oracle/properties.go) | Running trusted witness group (cap **20**) and automatic HBD APR selection via witness-property **mode** |
| [`modules/incentive-pendulum/oracle/movingavg.go`](../modules/incentive-pendulum/oracle/movingavg.go) | Ring buffer **MA** over last N trusted tick prices |

Run tests:

```bash
go test ./modules/incentive-pendulum/... -count=1
go test ./modules/incentive-pendulum -fuzz=FuzzSplitConservesR -fuzztime=10s   # optional conservation fuzz
```

## Bolt-on integration (DEX / LP)

1. **Per swap**: call `QuoteSwapFees` (or `PendulumBolt.QuoteSwap`) with CLP depths \(X,Y\), input \(x\), **global** \(s=V/E\), and whether the trade **exacerbates** imbalance vs corrective (PDF `push`). Returns user charge, CLP accrual to \(R\), stabilizer surplus, and collateral flags.
2. **Per block / tick**: build `NetworkSnapshot` (total HIVE stake, **sole** `HivePriceHBD` from oracle+MA, \(T\), pool HBD depths + owner metadata). `PendulumBolt.Evaluate` now defaults to **DAO-owner-only** pool aggregation (`owner == hive:vsc.dao`, normalized), then computes `BoltEvaluation` for settlement/dashboards/collateral.
3. **Collateralization**: `CollateralFromSV` / `CollateralReport` encode ideal / safe / warning / cliff bands so policy can throttle or surface risk without embedding DEX logic inside consensus core.

DEX routers keep using **open-market** reserves for asset prices; the pendulum only supplies **fee policy**, **\(s\)**, **split math**, and **HIVE→HBD** for bond valuation.
## Economics (summary)

- **Inputs** (same unit as HBD minors with HBD = \$1): \(E\) effective bond, \(T\) total effective bond, \(V\) vault liquidity, \(P\) pooled HBD, \(R\) CLP fees to distribute, \(u \approx T/E\) (often 1.5).
- **\(s = V/E\)**, **\(w = P/V\)** (if \(V=0\), \(w=0\)).
- **\(s \ge 1\)**: all \(R\) to nodes; `poolYield = 0` (PDF §6.1).
- **\(s < 1\)**: `denom = u s + w(1-s)`; `finalNodeShare`, `finalPoolShare`, `nodeYield`, `poolYield` per PDF §6.2.
- **Fees**: protocol 8 bps + CLP; stabilizer multiplies total fee; pendulum pool receives the CLP layer per PDF §2–§3.

## Sole HIVE oracle

1. Each block, record which **witnesses signed** the block.
2. Keep the last **100** blocks; count signatures per witness.
3. On tick (`block_height % 100 == 0`), a witness contributes to the aggregate **only if** they **updated their HIVE price feed** in the same **100**-block window and **`signatures_in_last_100 >= 4`**. This is driven in-process by [`oracle/FeedTracker`](../modules/incentive-pendulum/oracle/tracker.go) from [`StateEngine.ProcessBlock`](../modules/state-processing/state_engine.go) (Hive L1 `witness` + tx ops); read snapshots via `StateEngine.PendulumFeedTracker().LastTick()`.
4. **Aggregate** = simple arithmetic mean of trusted quotes (HIVE priced in HBD). **Do not** extend this machinery to other assets.
5. Build a **running witness group** from trusted feed participants (rank by signatures, cap at **20**), ingest their witness properties, and set HBD APR to the **mode** (`hbd_interest_rate`) from that group.

### Contract wasm API (`system.get_env` / `system.get_env_key`)

On each `vsc.call_contract` execution (and GraphQL contract simulation), the host merges the latest pendulum tick snapshot into the JSON env:

| Key | Meaning |
|-----|--------|
| `pendulum.hbd_interest_rate_bps` | Mode HBD rate from trusted top-20 feed witnesses (same integer units as Hive `hbd_interest_rate` in witness props; hundredths of a percent, i.e. 2000 ≈ 20% APR) |
| `pendulum.hbd_interest_rate_ok` | Whether a mode was computable from current props |
| `pendulum.trusted_hive_mean_hbd` | Trusted mean HIVE price in HBD at last tick |
| `pendulum.trusted_hive_mean_ok` | Whether the mean was defined |
| `pendulum.hive_ma_hbd` | Moving average of trusted means over recent ticks |
| `pendulum.hive_ma_ok` | Whether the MA is defined |
| `pendulum.tick_block_height` | Hive block height of the last tick (`height % 100 == 0`) |
| `pendulum.trusted_witness_group` | JSON array of witness account names (top trusted by signature count, cap 20) |
| `pendulum.witness_slash_bps` | JSON object `{witness: bps}` from tick evidence (signature deficit + missing feed update, capped) |

## Pool eligibility policy

### Current (implemented)

- Global pendulum \(V,P,s\) includes only pools owned by DAO account (`hive:vsc.dao`, normalized) when evaluating `NetworkSnapshot.Pools`.
- This is a conservative trust boundary while the broader reserve-ingestion and settlement path is finalized.

### Planned upgrade (DAO-governed user-pool enablement)

- Introduce a DAO-voted **code-hash allowlist** keyed by `(runtime, code_hash)`.
- Allow user pools into global pendulum only when:
  - pool auth/update path is valid, and
  - the pool contract’s `(runtime, code_hash)` is active in DAO allowlist.
- Keep deterministic exclusion reasons for observability/audit (`owner_mismatch`, `hash_not_approved`, `stale_update`, ...).

## Not yet wired on-chain

The following remain **out of scope** for full product integration:

- Custom JSON ops (`vsc.clp_swap`, `vsc.witness_hive_price`) and state DB migrations
- Pendulum reserve ledger bucket and epoch settlement wiring in [`state_engine.go`](../modules/state-processing/state_engine.go) (economics split / \(R\) — distinct from the sole-HIVE + APR **feed tracker** now updated each Hive block)
- GraphQL fields for \(s\), \(u\), \(w\), oracle MA, and explorer surfaces
- Streaming swaps and protocol-fee redirect settlement

## References

- On-disk spec copy (workspace root): `Magi.pdf` / `Magi_extracted.txt` (parent folder when working from a full tree).
- Product scope note: HackMD “MAGI INCENTIVE PENDULUM SCOPE” (bounded min/max splits there **differ** from the PDF; this code follows **PDF** closed-form rules, including **0% LP** at \(s \ge 1\)).
