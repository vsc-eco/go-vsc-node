# MAGI INCENTIVE PENDULUM SCOPE


### Objective

#### Implement a simple algorithm that distributes rewards between

- Liquidity Providers (LP)
- Nodes/Hive stakers

#### Distribution is based on the ratio between

- LP value/Stake value
- The system targets optimal capital efficiency at 1.5× overcollateralization.

#### LP Value Measurement
- Single-Side LP Tracking (HBD Side). We track HBD value via internal market.

For simplicity, the system tracks only one side of the liquidity pool, specifically the HBD side, and assumes equal value exists on the opposing asset side.

### Rationale

- LP pools are normally balanced by AMM mechanics.
- Single-side tracking simplifies implementation.
- Reduces oracle complexity and computation requirements.

### Known Tradeoffs
- Asset Price Imbalance.
 
If one asset rapidly changes USD value:
- The assumed 1:1 valuation may temporarily diverge.
- This can affect measured overcollateralization. Economic security is established after 1:1 LP/Colateral ratio is reached.
#### Mitigation
- Stablecoin pairs reduce this risk significantly.
- Overcollateralization operates within a range, providing tolerance.
- Arbitrage activity is expected to rebalance pools naturally.

### Pool Eligibility Rules
- Overcollateralization logic applies only to protocol-approved pools.

#### Reward Distribution Logic
1. Step 1 — Calculate Ratio
2. Step 2 — Determine Reward Split
3. Step 3 — Apply Constraints (Min / Max Limits)

#### Reward distribution must remain bounded:
Maximum allocation:
- 90% rewards → LP
- 10% rewards → Stake
Minimum allocation:
- Both LP and Stake must always receive rewards.
- Neither side can reach 0%.

This prevents incentive collapse on either side.
