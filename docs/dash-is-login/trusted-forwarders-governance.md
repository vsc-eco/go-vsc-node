# TrustedForwarders governance contract — design spec

**Status:** draft, not implemented.
**Last updated:** 2026-06-14.
**Branch:** `tibfox/feat/dash-is-login` (this file is a spec, no code yet).

## Why this exists

Today the trusted-forwarders allow-list (the contracts allowed to invoke
`contracts.call_as` and override the effective caller) lives in each
witness's local sysconfig override file (`-sysconfig` flag → JSON file →
`SystemConfig.TrustedForwarders()`). The execution-context reads it at
tx execution time:

```go
// modules/state-processing/transactions.go:160
contract_execution_context.WithTrustedForwarders(
    se.SystemConfig().TrustedForwarders())
```

This is a **consensus-critical** gate (a wrongly-trusted forwarder can
`call_as` any DID and drain that DID's HBD/DASH). Storing it in
per-witness operator files has three concrete problems:

1. **Consensus drift seam.** If one witness operator updates their
   sysconfig and another doesn't, the two witnesses produce divergent
   contract outputs for any tx that invokes `call_as`. That is the
   shape of a chain fork. The current devnet smoke test
   `TestIsLoginOpCallSmoke` had to plumb a `SysConfigOverrides`-based
   sync mechanism just to keep all five magi nodes in lockstep.

2. **Coordinated-restart window.** Adding a new forwarder requires every
   witness to update their file and restart. The interval between "first
   operator restarts" and "last operator restarts" is a window in which
   the network is partly accepting the new forwarder and partly
   rejecting it. Adversary watching for that window has a real seam.

3. **No audit trail.** There's no on-chain record of who added which
   forwarder to which witness when. A misconfigured operator looks
   identical to a malicious one until you inspect their actual file.

## Goals

* **No consensus drift.** Every witness must read the same allow-list
  for the same block height.
* **Auditable history.** Every add/remove is an on-chain transaction
  with a deterministic activation block.
* **Time-locked changes.** A new forwarder must clear a 48-hour window
  before it can `call_as`, mirroring the dash-mapping-contract's
  existing `addAllowedTarget` + `commitAllowedTarget` flow.
* **Emergency revocation.** Operators retain a local kill-switch: if
  the governance contract is compromised (e.g. multisig key leak), the
  sysconfig override can still REMOVE entries — never ADD them.
* **Graceful activation.** Pre-activation binaries must keep working;
  the new path is gated on a consensus version bump.

## Non-goals

* Replacing the existing sysconfig override entirely. Operators still
  want a local revoke-only path for emergencies.
* Changing the per-call ERC-2771 semantics (`CallAs` does not propagate
  trustedForwarders into nested calls — that frame-boundary stays).
* Designing the governance contract's *governance*. Whether it's a 2-of-3
  multisig, validator-vote, DAO, or single key is out of scope here.
  The spec assumes "admin" is whatever governance ratifies; this doc
  defines the contract surface, not the social process.

## Threat model

The trusted-forwarder list is the privilege gate for `contracts.call_as`,
which lets the calling contract spoof the `effectiveCaller` for outbound
calls. Concretely:

* **Trusted forwarder X added.** X can call `contracts.call_as(target,
  method, args, DID, opts)` and the target sees `EffectiveCaller=DID`.
  If `target` makes a balance decision based on `EffectiveCaller` (e.g.
  the DEX router checking allowance), X can drain that DID's funds.
* **Allow-list size.** The list is currently a flat `[]string` of
  `"contract:vsc1B..."` ids; no per-entry capability scoping. Adding
  one entry adds full `call_as` power.
* **Activation timing.** A 48h timelock between "propose" and "activate"
  is the same window currently used for `addAllowedTarget` on the
  mapping contract — operators have time to detect a bad proposal and
  veto via sysconfig kill-switch.
* **Removal timing.** Symmetric 48h timelock for removal, with a
  separate "emergency revoke" path that takes effect immediately at the
  cost of a higher governance threshold (left as an implementation
  parameter — multisig admin's choice).

## Contract surface

```text
contract: governance-trusted-forwarders
  state keys:
    f-active-<id>     => activation_height (uint64) — active iff present
    f-pending-add-<id>     => unlock_height (uint64)
    f-pending-remove-<id>  => unlock_height (uint64)
    cfg-timelock           => uint64 (default: 48 * 60 * 60 / 3 ≈ 57600 blocks at 3s)
    cfg-emergency-revoke-allowed => bool (default true)

  actions:
    proposeForwarder(contractId: string) [admin]
      — writes f-pending-add-<id> = blockHeight + cfg-timelock
      — rejects if entry already in f-active-<id>
      — rejects if pending-remove already exists

    cancelProposeForwarder(contractId: string) [admin]
      — removes f-pending-add-<id> if present

    activateForwarder(contractId: string) [anyone]
      — promotes f-pending-add-<id> → f-active-<id> = blockHeight
        once unlock_height has passed
      — emits log: "forwarder activated: contract:<id> at <height>"

    proposeRemoveForwarder(contractId: string) [admin]
      — writes f-pending-remove-<id> = blockHeight + cfg-timelock
      — rejects if not currently active
      — rejects if pending-add already exists

    cancelProposeRemoveForwarder(contractId: string) [admin]
      — removes f-pending-remove-<id> if present

    activateRemoveForwarder(contractId: string) [anyone]
      — deletes f-active-<id> once unlock_height has passed

    emergencyRevoke(contractId: string) [admin, gated on cfg-emergency-revoke-allowed]
      — deletes f-active-<id> immediately
      — emits log: "emergency revoke: contract:<id>"
      — does NOT need timelock — explicitly for compromise response

    listActive() [view]
      — returns []string of currently-active forwarder ids
      — read by magi's execution-context wiring at tx execution time
```

## Magi-side integration

### Where the read happens

The existing wiring at `modules/state-processing/transactions.go:160` is:

```go
contract_execution_context.WithTrustedForwarders(
    se.SystemConfig().TrustedForwarders())
```

The new wiring becomes:

```go
// Pseudocode — actual impl needs to handle the (state, blockHeight) accessor
// surface the state engine exposes.
list := mergeAllowlist(
    se.SystemConfig().TrustedForwarders(),         // sysconfig override
    governance.ReadActiveForwarders(se, blockHeight), // new path
)
contract_execution_context.WithTrustedForwarders(list)
```

where `mergeAllowlist` is **intersection-skewed**:

* If the consensus version is below the activation floor → use sysconfig
  only (legacy behaviour, byte-identical).
* If the consensus version is at or above the activation floor → take
  the **union of sysconfig and governance state**.
* Sysconfig retains the **subtraction power**: an operator can add a
  `revokedForwarders` list (new sysconfig field) that subtracts entries
  from the union. This is the local kill-switch.

The subtraction-only rule for sysconfig means a compromised governance
contract cannot bypass operator vigilance, but a compromised operator
file cannot inject a new forwarder either (they can only revoke).

### Consensus-version gating

Add `TrustedForwardersFromContractVersion` to
`modules/common/consensusversion/`. The state-processing wiring uses
`se.ActiveConsensusVersion(blockHeight).MeetsConsensusMin(...)` (same
pattern as the existing `TryCatchICCVersion` gate at
`transactions.go:158`).

Pre-activation: governance contract state is fully maintained but NOT
read by the execution context. Operators can write their first
`proposeForwarder` weeks before activation height so the timelock has
already cleared when the consensus version flips.

### Reading from a contract during tx execution

This is the part that needs the most care. The execution context runs
inside a contract call frame; reading from ANOTHER contract's state
during that frame has implications:

1. **No cycles.** The governance contract must not itself be in the
   trusted-forwarders list (otherwise a malicious admin could swap the
   list mid-tx). Enforce in `proposeForwarder` — reject if `contractId ==
   self.contractId`.

2. **Cache per block.** Reading from the contract's state on every
   `WithTrustedForwarders` call is expensive (O(active-count) state
   reads per tx). Cache the active list per block in the state engine.
   Invalidate when any of the governance contract's f-active-* keys
   change in this block.

3. **Determinism.** The "read at tx start" must use the state as-of the
   start of the current block, not the in-progress block. Same semantics
   as `se.SystemConfig().TrustedForwarders()` today (which is a
   compile-time snapshot loaded at startup).

### sysconfig schema bump

`SysConfigOverrides` gains:

```go
type SysConfigOverrides struct {
    // ... existing fields ...
    TrustedForwarders *[]string `json:"trustedForwarders,omitempty"`
    // NEW: revokedForwarders subtracts from the union of sysconfig +
    // governance contract. Use this for operator-side emergency
    // response if the governance contract is compromised — entries
    // here will NOT be honored by this node's execution context even
    // if the governance contract lists them as active. The operator
    // can publish a Hive post / governance-emergency-revoke proposal
    // to coordinate cross-witness revocation.
    RevokedForwarders *[]string `json:"revokedForwarders,omitempty"`
}
```

## Rollout

1. **Deploy governance contract on testnet.** Pre-populate with the
   current production forwarder ids via `proposeForwarder` immediately
   after deploy + `activateForwarder` after timelock. Owner = the
   real multisig / admin account, NOT a single key.

2. **Bump consensus version target.** New
   `TrustedForwardersFromContractVersion` constant, target activation
   height ~2 weeks after the binary lands.

3. **Witnesses upgrade binary.** Pre-activation, every binary reads
   sysconfig only (no behavioural change).

4. **At activation height.** All witnesses simultaneously start reading
   from the governance contract. The list at activation height is
   already populated (step 1) so the network sees no discontinuity.

5. **Deprecate sysconfig add-path.** A future release can refuse to
   parse `trustedForwarders` from sysconfig entirely (forcing all
   adds through governance). Operators keep `revokedForwarders` as
   the local subtract-only kill-switch.

## Open questions

* **Activation atomicity.** What happens if a witness binary doesn't
  upgrade by the activation height? It keeps reading sysconfig only,
  so its `IsTrustedForwarder` returns false for any forwarder that's
  only in the governance contract. Result: divergent contract outputs
  on the first post-activation `call_as`. **Mitigation:** announce
  activation height widely + monitor witness binary versions via
  `Witness.git_commit` field; refuse to activate if N-out-of-M
  witnesses haven't upgraded. Requires a per-witness "ack version"
  mechanism, which itself needs design.

* **Governance contract upgrades.** If the governance contract is later
  found to have a bug, can it be replaced? The execution-context wiring
  hardcodes the contract id. Options: (a) hardcode + accept that
  changing it needs a binary release; (b) make it a sysconfig pointer
  (which puts us back in operator-coordination land); (c) make it itself
  governance-resolvable (chicken-and-egg).

* **Per-entry capability scoping.** Today every trusted forwarder gets
  full `call_as` power. A v2 could attach a "scopes" array per entry
  (e.g. "only for target=mapping-contract"). Out of scope for v1.

* **Witness ack mechanism.** The current `Witness` GraphQL surface has
  `git_commit` and `version_id` fields. Can monitoring code derive
  "has upgraded to ≥ vX.Y" from those? Probably yes via tag-based
  versioning in the binary build.

## Acceptance criteria

A `TestTrustedForwardersGovernanceE2E` devnet test exists and passes,
covering:

1. Deploy governance contract.
2. Pre-activation: `proposeForwarder(X)` + `activateForwarder(X)`
   succeeds but contract output for a `call_as` from X still aborts
   ("not in TrustedForwarders" — pre-activation reads sysconfig only).
3. Activate consensus version: re-run the same `call_as`, succeeds.
4. `proposeRemoveForwarder(X)` + `activateRemoveForwarder(X)` →
   subsequent `call_as` from X aborts.
5. `emergencyRevoke(Y)` (a different forwarder, currently active) →
   takes effect in the next block without timelock.
6. Sysconfig `revokedForwarders: [Z]` (Z is in governance contract's
   active list) → that witness sees Z as NOT trusted, but other
   witnesses still see Z as trusted. **This is a deliberate fork
   case** for emergency response; the test asserts the divergence is
   observable on the local node and recorded in logs.

## Estimated work

* Contract code + tests: 4-6 hours.
* Magi wiring + consensus version gate: 6-10 hours (includes the
  per-block cache + the careful "read other contract's state from
  inside execution context" plumbing).
* Devnet E2E test: 3-4 hours.
* Documentation + runbook: 1 hour.
* **Total: 14-21 hours of focused work.**

Not implementing now — surfacing the design for review first.
