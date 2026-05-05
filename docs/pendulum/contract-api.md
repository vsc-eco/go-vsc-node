# W8 — Pool Contract Integration Spec

This is the **delta** required of an existing HBD-paired CLP pool contract to integrate with the Magi incentive pendulum. Pre-existing contract surfaces (CPMM math, LP token accounting, deposit/withdraw flows) stay as they are. The only required new behaviors are:

1. Per-swap call into the new `system.pendulum_apply_swap_fees` SDK method.

Pool contracts that do not opt in keep working unchanged — they just don't take fees on the pendulum path.

---

## Network preconditions

Before a contract integrates:

- The contract's ID must appear in [`SystemConfig.PendulumPoolWhitelist()`](../../modules/common/system-config/system-config.go). Either whitelist or contract owner based.
- The pool must be HBD-paired — exactly one of `(asset_in, asset_out)` is `"hbd"`. Non-HBD-paired pools are rejected at the SDK call boundary with `INVALID_ARGUMENT` for the testnet rollout.
- Whatever asset the contract holds in liquidity is keyed in its ledger account `contract:<contract_id>` (the `LedgerSystem` convention used by the existing `SendBalance` / `PullBalance` SDK methods). The geometry reader uses the contract's `hbd` balance directly as the pool's HBD-side reserve `P_hbd`, so the contract does **not** need to publish any per-pool reserve state key.

---

## SDK method: `system.pendulum_apply_swap_fees`

### Call shape (JSON in / JSON out)

The SDK function takes a single JSON-encoded string and returns a single JSON-encoded string. All numeric amounts are decimal strings (wasm guests typically encode large integers as strings to avoid float precision loss).

The contract supplies only the swap inputs. The SDK derives `gross_out`, both base fees, the stabilizer surplus, the network cut, and the pendulum split internally — all on the **output side** (matching the existing pre-pendulum contract math, where both `base_fee` and `clp_fee` are subtracted from `gross_out`).

**Input** (`PendulumSwapFeeInput`):

| Field       | Type       | Notes                                                                                                |
| ----------- | ---------- | ---------------------------------------------------------------------------------------------------- |
| `asset_in`  | string     | `"hbd"` or the non-HBD asset (e.g., `"hive"`). Case-insensitive; normalized lowercase server-side.   |
| `asset_out` | string     | The other asset. Must differ from `asset_in`; one side **must** be `"hbd"`.                          |
| `x`         | string-int | User input amount, in base units of `asset_in`. Must be `> 0`.                                       |
| `x_reserve` | string-int | Pool reserve of `asset_in` **before** the swap. Must be `> 0`.                                       |
| `y_reserve` | string-int | Pool reserve of `asset_out` **before** the swap. Must be `> 0`.                                      |

The stabilizer push direction is **not** an input. The SDK derives it from the snapshot's `s` and the swap direction (HBD-in raises `s = 2P/E`; HBD-out lowers it). A trade is "exacerbating" iff it moves `s` away from 0.5; otherwise it's corrective (push = 0.7×). At exactly `s = 0.5` any nonzero swap exacerbates by definition.

**Output** (`PendulumSwapFeeOutput`):

| Field                       | Type       | Meaning                                                                                                                                                                                                                |
| --------------------------- | ---------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `user_output`               | string-int | Amount the contract pays the user (in `asset_out`). Equals `floor(x · y_reserve / (x + x_reserve)) − total_clp − total_protocol`, where both fees are computed in output-asset units.                                  |
| `new_x_reserve`             | string-int | Reserve to write back for `asset_in`. For an HBD-output swap this is just `x_reserve + x`; for a non-HBD-output swap it also accounts for the secondary-hop HBD withdrawn for the node bucket.                         |
| `new_y_reserve`             | string-int | Reserve to write back for `asset_out`. Captures the user's draw and the node-share leaving the pool (or the virtual addition when the secondary hop happens on this side).                                             |
| `node_bucket_credited_hbd`  | string-int | HBD already accrued by the SDK to `pendulum:nodes` (informational; the contract has nothing to do with this).                                                                                                          |
| `multiplier_q8`             | string-int | Stabilizer multiplier `m(s, r)` in SQ64 (10⁸ scale). Useful for receipt / event log.                                                                                                                                   |
| `s_after_q8`                | string-int | The geometry ratio `s = V/E` from the snapshot the SDK consumed. SQ64. Useful for receipt / UX.                                                                                                                        |
| `network_credit_output`     | string-int | The 25% network cut on `(total_clp + total_protocol)`, in `asset_out` base units. The contract **must** add this to its single output-asset network-share accumulator (one bucket per asset, indexed by output side).  |

### Error codes

All SDK errors come back wrapped with the standard `contracts.SDK_ERROR` marker so the wasm runtime maps them to its `sdk-error` channel. Inner messages the contract may see:

| Inner message                              | Meaning                                                                                                                                                                                                                                              |
| ------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `INVALID_ARGUMENT_JSON`                    | Top-level JSON parse failed. Contract bug.                                                                                                                                                                                                           |
| `INVALID_ARGUMENT`                         | A required string field couldn't parse to int64, an asset string was empty, both assets matched, or neither side was `"hbd"`.                                                                                                                        |
| `contract not whitelisted`                 | Caller's contract ID isn't in `PendulumPoolWhitelist()`. The SDK takes the caller ID from `eCtx.ContractId()` — there is no `pool_id` argument; spoofing isn't possible.                                                                             |
| `pendulum snapshot unavailable`            | No oracle snapshot for the current block, or the snapshot's geometry block didn't compute (`GeometryOK=false` / `T==0` / `E==0`). Happens early in node lifetime before the first oracle tick + bond read; should be rare on a steady-state network. |
| `insufficient reserves for pendulum split` | Defensive guard: the math drove a side negative or pushed an int64 past its range. Should not fire on sane inputs; treat as contract-side bug.                                                                                                       |
| `pendulum accrual failed: <ledger msg>`    | The SDK couldn't credit `pendulum:nodes` (e.g., transient ledger session error). Surface to caller; the swap should not be applied.                                                                                                                  |

**Errors auto-abort the contract.** Any `result.Err` from an SDK call — including this one, just like `hive.transfer`, `hive.withdraw`, `hive.draw`, and the RC system — is intercepted by the wasm runtime at the import boundary, pushed to the contract's result channel as the final output (with `error_code = "sdk_error"` and the inner message above), and translated to `wasmedge.Result_Fail`, which unwinds the executing wasm function. Control never returns to the contract code after the SDK call site, so reserves are not written and no payment to the user happens. The contract does **not** need (and cannot) handle this — there is no error result for the contract to inspect on the failure path.

---

## Per-swap flow inside the contract

Reference pseudocode for an HBD-paired pool's swap handler. Every step is something the contract owns; the SDK call is the only outsourced piece.

```
func swap(asset_in, asset_out, x):
    assert pool_pair == (asset_in, asset_out) or (asset_out, asset_in)
    assert x > 0

    # 1. Read own reserves.
    (X, Y) = (reserves[asset_in], reserves[asset_out])
    assert X > 0 and Y > 0

    # 2. Call the SDK once. The SDK derives gross_out, base_clp,
    #    base_protocol, the stabilizer push direction, and the surplus
    #    from (x, X, Y) and the snapshot. The contract no longer
    #    pre-computes any fee component or hint.
    #    On any SDK error (whitelist, snapshot, accrual, etc.) the wasm
    #    runtime aborts the contract before the next line runs — the
    #    contract does not handle errors here.
    out = system.pendulum_apply_swap_fees(json.encode({
        asset_in:  asset_in,
        asset_out: asset_out,
        x:         str(x),
        x_reserve: str(X),
        y_reserve: str(Y),
    }))
    (user_output, new_X, new_Y, network_credit_output, ...) = json.decode(out)

    # 3. Apply final reserves. No additional swap math required.
    reserves[asset_in]  = new_X
    reserves[asset_out] = new_Y

    # 4. Update internal network-share accumulator. One value, in the
    #    output asset. Contracts holding two assets typically keep two
    #    buckets indexed by which side was the swap output.
    network_share[asset_out] += network_credit_output

    # 5. Pay the user.
    transfer(user, asset_out, user_output)

    # 6. (Optional) emit a receipt event for indexers.
    emit("pendulum_swap", {
        x, multiplier_q8, s_after_q8,
        node_bucket_credited_hbd,
        network_credit_output,
    })
```

**Things the contract must NOT do:**

- **Do not pre-compute or pre-deduct fees from `x`, `X`, or `Y`.** Pass the raw swap inputs; the SDK handles the entire fee schedule internally. Pre-deducting would double-charge the user.
- **Do not run a "secondary" swap inside the contract** to convert the node-runner share back to HBD. That conversion is already baked into `(new_X, new_Y)` via a closed-form CPMM hop the SDK applied internally. Running another swap would double-charge the pool.
- **Do not maintain its own per-pool node bucket.** Node-runner accrual goes to the global `pendulum:nodes` ledger account; the contract is not authorized to read or write that account.
- **Do not** apply the LP-retained portion of the fees to LP token NAV in any explicit step. LP NAV grows passively because the pool's reserves grew while LP-token supply stayed constant — same mechanism as today's CLP fee retention.

---

## Network-share claim flow

The 25% network cut accumulates **inside the pool** in the **output asset of each swap** (one value per swap, in output units). Because both fee components live on the output side, the contract needs at most one accumulator per asset (e.g., one HBD bucket and one ASSET1 bucket for an HBD-paired pool — credited depending on which way each swap went). Fees should accrue as they do now, and the claim mechanism will remain within the `claim_fees` function.

---

## LP-token migration at upgrade

For an existing pool gaining pendulum integration on the upgrade height:

- **No LP token redenomination.** Pre-upgrade swaps already folded their CLP fees into pool reserves and therefore into LP NAV; that's realized.
- **Snapshot reserves** at upgrade activation height for transparency, not for any math.
- **Post-upgrade swaps** all go through `system.pendulum_apply_swap_fees`. The SDK applies the 25/75 cut and the pendulum split from the very first post-upgrade swap; nothing transitional is needed.

---

## Determinism and replay invariants the contract must preserve

These are not optional. Violating any one breaks consensus.

1. **Pure inputs only.** Every input to `system.pendulum_apply_swap_fees` (`x`, `x_reserve`, `y_reserve`) must be a deterministic function of the contract's pre-swap state and the swap call args. Don't read wall-clock time, don't read external feeds, don't read a counter that could differ between nodes. Contract storage is the only sanctioned source.
2. **No floats anywhere on the swap path.** The SDK assumes integer base units throughout; the contract must not introduce floats anywhere in the inputs it computes.
3. **Apply reserves byte-for-byte.** Don't round, don't clamp, don't post-process `(new_X, new_Y)`. The values returned are already the correct integer reserves.
4. **Order of side-effects matters.** Apply reserves and the network accumulator **before** transferring `user_output`. This matches what the SDK has already accounted for internally and avoids any window where the pool is observably double-spent.

The SDK itself is consensus-deterministic: it reads the snapshot at `block_height`, runs integer / SQ64 math through `lib/intmath`, and calls into the ledger session that the rest of the state engine uses. Two honest nodes running the same block produce the same `(new_X, new_Y, user_output, network_credit_output, node_bucket_credited_hbd)` byte-for-byte.

---

## Eligibility checklist before testnet enrollment

- [ ] Contract ID added to `SystemConfig.PendulumPoolWhitelist()` in the network's defaults (testnet).
- [ ] Pool is HBD-paired.
- [ ] Swap handler calls `system.pendulum_apply_swap_fees` once per swap, before paying the user, and passes raw `(x, X, Y)` (no pre-deducted fees).
- [ ] Final reserves come from the SDK return; no secondary in-contract swap.
- [ ] No floating-point arithmetic on any code path that touches reserves or fees.
- [ ] (Recommended) Receipt event emitted with `multiplier_q8`, `s_after_q8`, and `network_credit_output` for indexer consumers.

## Open items the spec defers to the testnet plan

- **LP minimum-floor cliff** (B12) — the SDK's `MinFractionBps` knob is currently 0; a future bump caps LP dilution at equilibrium. The contract surface is unchanged; only the SDK output `(new_X, new_Y)` shifts. No contract changes needed when the knob moves.
- **Network-share asset of record** current design keeps the network cut native (output asset of each swap) and lets the claimer take a basket.
