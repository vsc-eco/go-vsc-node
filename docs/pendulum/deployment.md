# Pendulum deployment checklist

Items that must be coordinated when the pendulum incentive system is first
deployed to a network (testnet, mainnet, …). Track here as we go; review and
sequence before the deployment branch is cut.

## Wasm contract ABI

- [ ] **`system.get_env_key` return type for pendulum oracle keys changed
      from float-formatted strings to integer-formatted strings.**
      Affected keys: `pendulum.trusted_hive_mean_hbd`, `pendulum.hive_ma_hbd`
      (and any future `pendulum.*` numeric key).
      Contracts that previously parsed the returned string as a float will
      need to parse it as an integer in the asset's base units (HIVE/HBD: raw
      amount, precision 3 — i.e. `1.234 HBD` arrives as the string `"1234"`).
      No on-chain migration; contracts must re-link against the updated SDK
      before the pendulum is enabled on the target network.
