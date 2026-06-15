# TrustedForwarders — design + operator notes

**Status:** implemented (go-vsc-node `21affffd`, utxo-mapping `12fe0a9`).
**Last updated:** 2026-06-15.
**Branch:** `tibfox/feat/dash-is-login`.

## What this is

The Dash IS-login feature lets a Dash payer (who has no L2 key) invoke
an arbitrary L2 contract by paying Dash. The `dash-forwarder-contract`
translates "I observed an IS-locked Dash payment from DID X" into a
call to the target contract with `effectiveCaller=X`. The VM primitive
that lets a contract set `effectiveCaller` to an arbitrary DID is
`contracts.call_as` — gated by a per-tx allow-list called
`TrustedForwarders`. A contract whose id is in the allow-list can spoof
identity for any DID; everything else aborts.

This doc covers (a) how the allow-list is populated, (b) how to change
it on a live network, (c) how operators emergency-revoke.

## The allow-list source: the dash-mapping-contract itself

There is no separate governance contract. The dash-mapping-contract
holds the trusted dash-forwarder-contract id at its `forwarder` state
key (set once via `setForwarderContractId`, locked thereafter). At
every `TxVscCallContract.ExecuteTx`, magi:

1. Reads `sysconfig.DashMappingContractId()` — the operator-named
   dash-mapping-contract id. Empty → no Dash IS-login feature on this
   network → empty allow-list → `call_as` aborts.
2. Reads the `forwarder` state key from that contract via
   `callSession.GetStateStore(mappingId).Get("forwarder")`. Empty /
   missing → mapping deployed but admin hasn't yet wired the forwarder
   → `call_as` still aborts.
3. Returns a single-entry allow-list `["contract:" + forwarderId]`.

The full implementation is in
[`modules/state-processing/trusted_forwarders.go`](../../modules/state-processing/trusted_forwarders.go).
~30 lines of code. One `StateGetObject` call per tx.

### Why "the mapping is the registry"

The forwarder is feature-scoped — it only makes sense in the context
of Dash IS-login, which is owned by the dash-mapping-contract. Having
the mapping name its own forwarder:

* Reuses `vsc.update_contract` as the change mechanism. No new admin
  key, no new governance contract, no parallel timelock to configure.
* The change is **visible in the contract diff** during the
  `ContractUpdateTimelockBlocks` window (network-baked: 48h on
  mainnet). Reviewers see the new forwarder id in the same patch as
  any other code change, in context.
* The `setForwarderContractId` action already exists in the mapping
  and has a lock-on-first-set + pause-required-to-change semantic.
* Future ltc/bch/doge IS-login flows can do the same — each mapping
  owns its own forwarder reference, sysconfig names which mapping is
  live, no per-chain protocol changes to magi.

## Operator deployment (testnet + mainnet)

```
1. Deploy dash-mapping-contract.
2. mapping.setForwarderContractId(forwarderContractId)
   — pins the forwarder. Locked after this call.
3. SetDashMappingContractId(mappingId) in each witness's sysconfig.
   Restart magi.
```

After step 3, every magi node reads `mapping["forwarder"]` on every
`vsc.call` and uses the resulting id as the trusted-forwarders list.
`sdk.ContractCallAs` from the forwarder succeeds.

## Changing the trusted forwarder

Two paths:

### A. Replace the forwarder code

If the existing forwarder has a bug and you want a new binary at the
same contract id, just `vsc.update_contract` on the forwarder. The
mapping's `forwarder` state key still names the same id; behaviour
changes via the forwarder's new code. Standard `ContractUpdateTimelock
Blocks` window applies.

### B. Switch to a different forwarder contract id

Update the dash-mapping-contract via `vsc.update_contract` with a
patch that changes how `forwarder` is set (e.g. an admin migration
action that re-enables `setForwarderContractId` once, sets the new id,
then re-locks). This is the heavy path — only relevant if the existing
forwarder is compromised badly enough that you don't trust its code
even after an update.

In practice (A) is the path you want; (B) exists as the escape hatch.

## Emergency revoke

Two paths:

### Local (per-operator, immediate)

Set `sysconfig.DashMappingContractId=""` on the affected witness +
restart. Magi's `resolveTrustedForwarders` now returns an empty list →
every `call_as` via Dash IS-login aborts on that witness. That witness
falls out of consensus on any tx that exercises `call_as` until the
operator re-enables it.

Coarse: it disables the whole feature, not just one bad forwarder. But
it's the right shape for compromise response because (a) operators can
do it unilaterally without coordinating with the fleet, and (b) the
worst case is the IS-login feature is unavailable for users on that
witness's view — Dash deposits still land in the dashd-watcher and
get IS-locked, the only thing that breaks is the call_as step.

### Coordinated (network-wide, slow)

`vsc.update_contract` on the dash-mapping with a forwarder-cleared
patch. Standard timelock. Reaches all witnesses simultaneously when
the timelock expires. Use when the local kill-switch isn't enough
(e.g. the issue affects users who interacted with the forwarder
before any operator could react).

## Security model

The trusted-forwarders mechanism inherits the same trust shape as
ERC-2771 and every other relayed-transaction system: **the forwarder
is trusted unconditionally for identity derivation**. If the
forwarder has a bug in how it derives DashDID from the raw Dash tx,
or in how it validates the BLS attestation quorum, it can be tricked
into `call_as`'ing as the wrong user. So the security gate is the
forwarder's code correctness + the gate on WHO gets `call_as`
privilege.

The previous proposal (a separate governance contract) introduced a
new admin-key concentration point on top of an unaudited governance
contract. Embedding the registry into the mapping eliminates both:

| | Separate governance contract | dash-mapping owns its forwarder ref (chosen) |
|---|---|---|
| Admin keys to defend | 2 (governance admin + mapping owner) | 1 (mapping owner = `magi.contracts`) |
| New contract to audit | Yes (governance) | No |
| Forwarder-change visibility | One propose tx | Full mapping diff visible during update timelock |
| Per-witness local revoke | Sysconfig.RevokedForwarders list | `DashMappingContractId=""` (coarser, simpler) |
| BFT resistance to single rogue op | Same | Same |

Tradeoff accepted: the local revoke is coarser (whole-feature, not
per-forwarder). For a feature with exactly one trusted forwarder per
chain this isn't a meaningful loss — there's no distinction between
"revoke the bad forwarder" and "disable Dash IS-login on my node."

## Witness-fleet readiness

The trusted-forwarders mechanism doesn't gate on a consensus version
flip (binaries below `currentConsensus=2` can't run any of this code
anyway). What operators DO need to coordinate:

* All witnesses must run a binary that ships this code (the current
  `feat/dash-is-login` tip or whatever shipped tag includes it).
* All witnesses' sysconfig must point at the same
  `DashMappingContractId` — otherwise magi nodes with different
  pointers will compute different allow-lists and diverge on the first
  `call_as` tx.

For the second point, `cmd/check-witness-readiness` exists to surface
which witnesses are running which binary version. Operators should
run it before broadcasting `vsc.update_contract` on the mapping (so
all witnesses are running a binary that knows how to read the new
forwarder reference) and before deploying the IS-login feature
network-wide (so all sysconfigs are pointing at the same mapping).

## Acceptance test

The chain-side wiring is exercised by
[`tests/devnet/is_login_opcall_test.go`](../../tests/devnet/is_login_opcall_test.go).
The test:

1. Deploys dash-mapping + dash-forwarder + a target contract.
2. Calls `mapping.setForwarderContractId(forwarderId)`.
3. `SetDashMappingContractId(mappingId)` in sysconfig + restart magi.
4. Runs a real Dash testnet payment → orchestrator observes →
   attestation → L2 submission of `mapInstantSendV2` → mapping
   dispatches to forwarder via `sdk.ContractCall` → forwarder invokes
   `sdk.ContractCallAs(target, "setString", "key=ophk;value=opval",
   senderDashDID, nil)`.
5. Asserts the target contract's state key `ophk == opval`.

Pass time: ~377s. Last verified: 2026-06-15 against go-vsc-node
`21affffd` + utxo-mapping `12fe0a9`.

## Files

* `modules/state-processing/trusted_forwarders.go` —
  `resolveTrustedForwarders` (the single state-key read).
* `modules/state-processing/transactions.go:160-175` — wiring into
  the execution context via `WithTrustedForwarders` option.
* `modules/common/system-config/system-config.go` —
  `DashMappingContractId()` accessor + `SysConfigOverrides.
  DashMappingContractId` JSON field.
* `modules/contract/execution-context/execution-context.go` —
  `IsTrustedForwarder()` check (the consumer) + the child-context
  propagation fix (so forwarders reached via mapping.mapInstantSendV2
  still see themselves in the allow-list).
* `cmd/is-service/peer_discovery.go` — IS service auto-discovers
  bootstrap peers from `witnessNodes(height)` so operators don't have
  to maintain a peer CSV (unrelated to this doc but lives in the same
  rollout).
* `cmd/check-witness-readiness/main.go` — pre-deploy fleet-version
  audit tool.
