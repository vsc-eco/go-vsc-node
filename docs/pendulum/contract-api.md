# W8 — Pool Contract Integration Spec

This is the **delta** required of an existing HBD-paired CLP pool contract to integrate with the Magi incentive pendulum. Pre-existing contract surfaces (CPMM math, LP token accounting, deposit/withdraw flows) stay as they are. The only required new behaviors are:

1. Per-swap call into the new `system.pendulum_apply_swap_fees` SDK method.
2. A new internal accumulator + claim entry point for the network's 25% protocol cut.
3. A whitelist gate (network-side; nothing the contract has to do beyond using its assigned contract ID).

Pool contracts that do not opt in keep working unchanged — they just don't take fees on the pendulum path.

---

## Network preconditions

Before a contract integrates:

- The contract's ID must appear in [`SystemConfig.PendulumPoolWhitelist()`](../../modules/common/system-config/system-config.go). Mainnet defaults to empty; testnet ships with the deployed pool IDs.
- The pool must be HBD-paired — exactly one of `(asset_in, asset_out)` is `"hbd"`. Non-HBD-paired pools are rejected at the SDK call boundary with `INVALID_ARGUMENT` for the testnet rollout.
- Whatever asset the contract holds in liquidity is keyed in its ledger account `contract:<contract_id>` (the `LedgerSystem` convention used by the existing `SendBalance` / `PullBalance` SDK methods). The W7 geometry reader uses the contract's `hbd` balance directly as the pool's HBD-side reserve `P_hbd`, so the contract does **not** need to publish any per-pool reserve state key.

---

## SDK method: `system.pendulum_apply_swap_fees`

### Call shape (JSON in / JSON out)

The SDK function takes a single JSON-encoded string and returns a single JSON-encoded string. All numeric amounts are decimal strings (wasm guests typically encode large integers as strings to avoid float precision loss).

**Input** (`PendulumSwapFeeInput`):

| Field | Type | Notes |
|---|---|---|
| `asset_in` | string | `"hbd"` or the non-HBD asset (e.g., `"hive"`). Case-insensitive; normalized lowercase server-side. |
| `asset_out` | string | The other asset. Must differ from `asset_in`; one side **must** be `"hbd"`. |
| `x` | string-int | User input amount, in base units of `asset_in`. Must be `> 0`. |
| `x_reserve` | string-int | Pool reserve of `asset_in` **before** the swap. Must be `> 0`. |
| `y_reserve` | string-int | Pool reserve of `asset_out` **before** the swap. Must be `> 0`. |
| `base_clp` | string-int | THORChain CLP fee = `floor(x²·y_reserve / (x + x_reserve)²)`. Computed by the contract; must be `≥ 0`. |
| `exacerbates` | bool | Contract's hint for the stabilizer push direction (PDF §5). `true` if the swap moves `s` further from 0.5; otherwise `false` (the SDK lowers `push` to 0.7×). |

**Output** (`PendulumSwapFeeOutput`):

| Field | Type | Meaning |
|---|---|---|
| `user_output` | string-int | Amount the contract pays the user (in `asset_out`). Equals `floor(x · y_reserve / (x + x_reserve)) − base_clp`. |
| `new_x_reserve` | string-int | Reserve to write back for `asset_in`. Already accounts for the user's input, the LP-retained protocol fee, the network's protocol cut, **and** any node-side conversion in/out of this side. |
| `new_y_reserve` | string-int | Reserve to write back for `asset_out`. Same accounting on the output side. |
| `node_bucket_credited_hbd` | string-int | HBD already accrued by the SDK to `pendulum:nodes` (informational; the contract has nothing to do with this). |
| `multiplier_q8` | string-int | Stabilizer multiplier `m(s, r)` in SQ64 (10⁸ scale). Useful for receipt / event log. |
| `s_after_q8` | string-int | The geometry ratio `s = V/E` from the snapshot the SDK consumed. SQ64. Useful for receipt / UX. |
| `network_credit_native.asset_in_amount` | string-int | The 25% network cut on the protocol fee, in `asset_in` base units. The contract **must** add this to its internal network-share accumulator. |
| `network_credit_native.asset_out_amount` | string-int | Same for the CLP cut, in `asset_out` base units. |

### Error codes

All SDK errors come back wrapped with the standard `contracts.SDK_ERROR` marker so the wasm runtime maps them to its `sdk-error` channel. Inner messages the contract may see:

| Inner message | Meaning |
|---|---|
| `INVALID_ARGUMENT_JSON` | Top-level JSON parse failed. Contract bug. |
| `INVALID_ARGUMENT` | A required string field couldn't parse to int64, an asset string was empty, both assets matched, or neither side was `"hbd"`. |
| `contract not whitelisted` | Caller's contract ID isn't in `PendulumPoolWhitelist()`. The SDK takes the caller ID from `eCtx.ContractId()` — there is no `pool_id` argument; spoofing isn't possible. |
| `pendulum snapshot unavailable` | No oracle snapshot for the current block, or the snapshot's geometry block didn't compute (`GeometryOK=false` / `T==0` / `E==0`). Happens early in node lifetime before the first oracle tick + bond read; should be rare on a steady-state network. |
| `insufficient reserves for pendulum split` | Defensive guard: the math drove a side negative or pushed an int64 past its range. Should not fire on sane inputs; treat as contract-side bug. |
| `pendulum accrual failed: <ledger msg>` | The SDK couldn't credit `pendulum:nodes` (e.g., transient ledger session error). Surface to caller; the swap should not be applied. |

The contract's swap handler **must abort the swap** on any error from this SDK call. Do not write reserves, do not pay the user.

---

## Per-swap flow inside the contract

Reference pseudocode for an HBD-paired pool's swap handler. Every step is something the contract owns; the SDK call is the only outsourced piece.

```
fn swap(asset_in, asset_out, x):
    assert pool_pair == (asset_in, asset_out) or (asset_out, asset_in)
    assert x > 0

    # 1. Read own reserves.
    (X, Y) = (reserves[asset_in], reserves[asset_out])
    assert X > 0 and Y > 0

    # 2. Compute base CLP fee — UNCHANGED. Same THORChain formula
    #    the contract already implements.
    base_clp = (x * x * Y) / ((x + X) * (x + X))     # integer floor div

    # 3. Compute exacerbates hint.
    #    Optional but recommended: the SDK uses this only to pick the
    #    stabilizer push factor (1.0 vs 0.7). If unsure, pass `true`
    #    (charges the more conservative protocol fee).
    exacerbates = projects_s_further_from_half((X, Y), x, asset_in)

    # 4. Call the SDK once.
    out = system.pendulum_apply_swap_fees(json.encode({
        asset_in:    asset_in,
        asset_out:   asset_out,
        x:           str(x),
        x_reserve:   str(X),
        y_reserve:   str(Y),
        base_clp:    str(base_clp),
        exacerbates: exacerbates,
    }))
    (user_output, new_X, new_Y, network_in, network_out, ...) = json.decode(out)

    # 5. Apply final reserves.  No additional swap math required.
    reserves[asset_in]  = new_X
    reserves[asset_out] = new_Y

    # 6. Update internal network-share accumulator. Native asset on each side;
    #    no in-pool conversion at this point.
    network_share[asset_in]  += network_in
    network_share[asset_out] += network_out

    # 7. Pay the user.
    transfer(user, asset_out, user_output)

    # 8. (Optional) emit a receipt event for indexers.
    emit("pendulum_swap", {
        x, base_clp, multiplier_q8, s_after_q8,
        node_bucket_credited_hbd,
        network_credit_native: {asset_in: network_in, asset_out: network_out},
    })
```

**Things the contract must NOT do:**

- **Do not run a "secondary" swap inside the contract** to convert the node-runner share back to HBD. That conversion is already baked into `(new_X, new_Y)` via a closed-form CPMM hop the SDK applied internally. Running another swap would double-charge the pool.
- **Do not maintain its own per-pool node bucket.** Node-runner accrual goes to the global `pendulum:nodes` ledger account; the contract is not authorized to read or write that account.
- **Do not** apply the LP-retained portion of the fees to LP token NAV in any explicit step. LP NAV grows passively because the pool's reserves grew while LP-token supply stayed constant — same mechanism as today's CLP fee retention.

---

## Network-share claim flow

The 25% network cut accumulates **inside the pool** in the asset of each side (native), tracked in the contract's own `network_share[<asset>]` map. Default (subject to design question §3 in the testnet plan) is to keep the cut native and let the claimer take a basket; the contract does **not** convert at claim time.

A new entry point on the contract:

```
fn claim_network_share():
    require(authorized(caller))   # DAO key or designated treasury account
    for asset in supported_assets:
        amt = network_share[asset]
        if amt > 0:
            transfer(treasury, asset, amt)
            network_share[asset] = 0
    emit("pendulum_network_claim", { amounts: ... })
```

Authorization model is up to the contract author; recommended pattern is a single hard-coded `caller_active_auth` matching the DAO multisig account. The contract should refuse claims from unauthorized callers without modifying state.

---

## LP-token migration at upgrade

For an existing pool gaining pendulum integration on the upgrade height:

- **No LP token redenomination.** Pre-upgrade swaps already folded their CLP fees into pool reserves and therefore into LP NAV; that's realized.
- **Snapshot reserves** at upgrade activation height for transparency, not for any math.
- **Post-upgrade swaps** all go through `system.pendulum_apply_swap_fees`. The SDK applies the 25/75 cut and the pendulum split from the very first post-upgrade swap; nothing transitional is needed.
- The contract's `network_share[<asset>]` accumulator starts at zero on upgrade. That's the right starting state — pre-upgrade pools had no separate network cut.

---

## Determinism and replay invariants the contract must preserve

These are not optional. Violating any one breaks consensus.

1. **Pure inputs only.** Every input to `system.pendulum_apply_swap_fees` (`x`, `x_reserve`, `y_reserve`, `base_clp`, `exacerbates`) must be a deterministic function of the contract's pre-swap state and the swap call args. Don't read wall-clock time, don't read external feeds, don't read a counter that could differ between nodes. Contract storage is the only sanctioned source.
2. **No floats anywhere on the swap path.** `base_clp` must be computed with integer floor-div, not `f64`. The SDK assumes integer base units throughout.
3. **Apply reserves byte-for-byte.** Don't round, don't clamp, don't post-process `(new_X, new_Y)`. The values returned are already the correct integer reserves.
4. **Order of side-effects matters.** Apply reserves and the network accumulator **before** transferring `user_output`. This matches what the SDK has already accounted for internally and avoids any window where the pool is observably double-spent.

The SDK itself is consensus-deterministic: it reads the snapshot at `block_height`, runs integer / SQ64 math through `lib/intmath`, and calls into the ledger session that the rest of the state engine uses. Two honest nodes running the same block produce the same `(new_X, new_Y, user_output, network_credit_native, node_bucket_credited_hbd)` byte-for-byte.

---

## Eligibility checklist before testnet enrollment

- [ ] Contract ID added to `SystemConfig.PendulumPoolWhitelist()` in the network's defaults (testnet).
- [ ] Pool is HBD-paired.
- [ ] Swap handler computes integer `base_clp` and calls `system.pendulum_apply_swap_fees` once per swap, before paying the user.
- [ ] Final reserves come from the SDK return; no secondary in-contract swap.
- [ ] `network_share[<asset>]` accumulator added to contract storage; updated from `network_credit_native` on every swap.
- [ ] `claim_network_share()` entry point gated on the agreed authorized caller.
- [ ] No floating-point arithmetic on any code path that touches reserves or fees.
- [ ] (Recommended) Receipt event emitted with `multiplier_q8`, `s_after_q8`, and the per-side fee breakdown for indexer consumers.

## Open items the spec defers to the testnet plan

- **Slash destination** (B7) — affects what happens to `HIVE_CONSENSUS` debited from slashed witnesses; does not change the contract surface.
- **LP minimum-floor cliff** (B12) — the SDK's `MinFractionBps` knob is currently 0; a future bump caps LP dilution at PDF equilibrium. The contract surface is unchanged; only the SDK output `(new_X, new_Y)` shifts. No contract changes needed when the knob moves.
- **Network-share asset of record** (§3) — current design keeps the cut native and lets the claimer take a basket; if this changes to "convert to HBD at claim time," the only affected code is `claim_network_share()` itself.
