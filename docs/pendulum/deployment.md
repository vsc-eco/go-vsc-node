# Pendulum deployment checklist

Items that must be coordinated when the pendulum incentive system is first
deployed to a network (testnet, mainnet, …). Track here as we go; review and
sequence before the deployment branch is cut.

## Wasm contract ABI

- [ ] **`system.get_env_key` pendulum oracle keys are now integer-typed.**
      Float values are no longer accepted by the wasm host serializer at all
      (a float in the env map surfaces as `ENV_VAR_ERROR` rather than a
      formatted string).
      Affected keys and types:
      - `pendulum.trusted_hive_price_bps` — int64, HBD-per-HIVE price in
        basis points (10000 = 1.0). Replaces `pendulum.trusted_hive_mean_hbd`.
      - `pendulum.hive_moving_avg_bps` — int64, basis-point MA. Replaces
        `pendulum.hive_ma_hbd`.
      - `pendulum.hbd_interest_rate_bps` — int, unchanged.
      - `pendulum.tick_block_height` — uint64, unchanged.
      - All boolean `*_ok` flags — unchanged.
      Contracts that previously parsed `pendulum.trusted_hive_mean_hbd` /
      `pendulum.hive_ma_hbd` as float strings must switch to integer parsing
      (the new key names also force a compile/link break, so silent drift
      isn't possible).

- [ ] **`system.pendulum_apply_swap_fees` SDK return shape:** the JSON keys
      `multiplier_q8` and `s_after_q8` were renamed to `multiplier_bps` and
      `s_after_bps`. Both are integer strings (basis points). Pool contracts
      that read these for logging or display must update their JSON parsing.

## Persisted-collection schema

- [ ] **`pendulum_oracle_snapshots` field renames** (no backward-compatible
      mapping; the collection should be empty before the rollout):
      - `trusted_hive_mean_sq64` → `trusted_hive_price_bps`
      - `hive_moving_avg_sq64` → `hive_moving_avg_bps`
      - `geometry_s_sq64` → `geometry_s_bps`
      Numeric values are also rescaled (10⁸ → 10⁴). Drop the collection
      before enabling the new code on a network that ever ran an earlier
      pendulum build.

## GraphQL ABI

- [ ] **`PendulumOracleSnapshot` field renames** (matching the schema rename):
      `trusted_hive_mean_sq64` → `trusted_hive_price_bps`,
      `hive_moving_avg_sq64` → `hive_moving_avg_bps`,
      `geometry_s_sq64` → `geometry_s_bps`. The standalone
      `PendulumGeometry.s_sq64` is now `PendulumGeometry.s_bps`. Any
      explorer / dashboard client must update query strings.
