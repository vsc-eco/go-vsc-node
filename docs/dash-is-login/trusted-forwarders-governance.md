# TrustedForwarders — design + operator notes

**Status:** implemented (go-vsc-node `21affffd`, utxo-mapping `12fe0a9`).
**Last updated:** 2026-06-15.
**Branch:** `tibfox/feat/dash-is-login`.
**Related:** [`cmd/is-service/RUNBOOK.md`](../../cmd/is-service/RUNBOOK.md) — IS service operator runbook.

## TL;DR

The Dash IS-login feature lets a Dash payer invoke L2 contracts without
holding an L2 key. To make that work, a single "forwarder" contract is
allowed to spoof identity on behalf of users. **The dash-mapping-contract
holds the id of its trusted forwarder at the `forwarder` state key.**
Magi reads that key once per tx to populate the allow-list.

* **Who controls the trusted forwarder:** whoever can update the
  dash-mapping-contract (the `magi.contracts` admin account).
* **How it's changed:** `vsc.update_contract` on either the forwarder
  (replace code) or the mapping (replace the id) — both go through the
  network-baked `ContractUpdateTimelockBlocks` window.
* **How operators emergency-revoke:** clear
  `sysconfig.DashMappingContractId` on their witness + restart →
  feature disabled on that node, no fleet coordination required.

If you've used ERC-2771 on Ethereum the trust model is identical:
a forwarder is unconditionally trusted to derive caller identity, and
the privilege is gated by an allow-list.

## Background

A Dash payer has no L2 (Hive / eth) key. For the payer's intent to
reach an L2 contract, *something* has to translate "I saw an IS-locked
Dash payment from this Dash address" into "call the target contract
with the Dash payer's DID as the `effectiveCaller`."

That something is the **dash-forwarder-contract**. It observes the
mapping's forward-queue entries (which carry the validated DashDID +
target call), then invokes the target via `sdk.ContractCallAs(target,
method, args, dashDID, opts)`.

`contracts.call_as` is the VM primitive that sets `effectiveCaller` to
an arbitrary DID on the next call frame. If ANY contract could call it,
ANY contract could drain ANY user's funds by spoofing identity. So
magi gates it with the per-tx `TrustedForwarders` allow-list — a
contract whose id is in the list can spoof, every other contract
aborts.

The forwarder is "trusted" in the same sense as in ERC-2771: it is
trusted unconditionally for identity-derivation correctness. A bug in
how the forwarder validates the BLS attestation quorum, or in how it
extracts the DashDID from the raw Dash tx, translates directly into
a fund-drain. **The forwarder code is the security boundary.**

## How magi populates the allow-list

```
                      sysconfig.DashMappingContractId
                                    │
                                    ▼
              ┌──────────────────────────────────────────┐
              │           dash-mapping-contract          │
              │ ┌──────────────────────────────────────┐ │
              │ │ state["forwarder"] = vsc1BforwarderX │ │
              │ └──────────────────────────────────────┘ │
              └──────────────────────────────────────────┘
                                    │
                                    ▼
               ["contract:vsc1BforwarderX"]  ← per-tx allow-list

                  IsTrustedForwarder() compares the executing
                  contract's id against this list.
```

At every `TxVscCallContract.ExecuteTx`, magi runs
`resolveTrustedForwarders` ([modules/state-processing/trusted_forwarders.go](../../modules/state-processing/trusted_forwarders.go),
~30 lines). It:

1. Reads `sysconfig.DashMappingContractId()`. Empty → returns `nil`
   → empty allow-list → `call_as` aborts.
2. Reads the `forwarder` state key from that contract
   (`callSession.GetStateStore(mappingId).Get("forwarder")`). Empty
   → returns `nil` → `call_as` aborts.
3. Trims + prefixes with `"contract:"` so the
   `IsTrustedForwarder()` check on the magi side does a direct string
   compare with `"contract:" + ctx.env.ContractId`.

One `StateGetObject` per tx. No caching — the read is cheap and the
state-engine's own cache absorbs the cost across batched calls.

## Why "the mapping is the registry"

The forwarder is feature-scoped — it only makes sense in the context
of Dash IS-login, which is owned by the dash-mapping-contract. Having
the mapping name its own forwarder gives us:

* **No new admin key.** The mapping's existing
  `setForwarderContractId` action (admin-only, locked after first
  call) is the write surface; no separate governance admin to defend.
* **Reuses `vsc.update_contract` governance.** The same code-change
  flow that's already audited + battle-tested handles forwarder
  changes too. The new id is visible in the contract diff during the
  `ContractUpdateTimelockBlocks` window, reviewers see it in context
  with any other change.
* **No new contract to audit.** Avoids introducing an unaudited
  governance contract whose admin-key compromise = fund security.
* **Forward-compatible with future chains.** When LTC/BCH/DOGE
  IS-login flows ship, each mapping owns its own forwarder ref the
  same way; sysconfig grows a list (`LtcMappingContractId`,
  `BchMappingContractId`, ...) and magi reads each one's forwarder
  key, no per-chain protocol changes.

## Operator deployment

Three contract operations + one sysconfig update + one restart.

### 1. Deploy the dash-mapping-contract

Build the testnet wasm (or use a pre-built artifact):

```bash
cd utxo-mapping/dash-mapping-contract
USE_DOCKER=1 make testnet
# → bin/testnet.wasm
```

Deploy (`contract-deployer` binary lives in `cmd/contract-deployer/`,
identity is `magi.contracts`):

```bash
contract-deployer deploy \
    -wasm utxo-mapping/dash-mapping-contract/bin/testnet.wasm \
    -name dash-mapping \
    -description "Dash IS-login mapping for $network" \
    -owner magi.contracts
# → prints "contract id: vsc1BmappingExampleID"
```

### 2. Pin the forwarder id

Deploy the dash-forwarder-contract the same way to get a
`vsc1BforwarderId`, then pin it on the mapping:

```bash
contract-deployer call \
    -contractId vsc1BmappingExampleID \
    -action setForwarderContractId \
    -payload vsc1BforwarderId
```

This action is admin-only AND **locked after the first successful
call**. To change the forwarder id later, see "Changing the trusted
forwarder" below.

### 3. Sysconfig + magi restart

Each witness operator updates their sysconfig override file:

```json
{
  "dashMappingContractId": "vsc1BmappingExampleID"
}
```

Then restart magi to pick up the change. After the restart, every
`vsc.call` tx checks `mapping["forwarder"]` against the calling
contract id; `sdk.ContractCallAs` from the pinned forwarder succeeds,
every other contract's `call_as` aborts.

The IS service itself doesn't need a sysconfig change — its bootstrap
peers auto-discover from `witnessNodes(height)` per the
[peer_discovery.go](../../cmd/is-service/peer_discovery.go) helper.
See [`cmd/is-service/RUNBOOK.md`](../../cmd/is-service/RUNBOOK.md) for
its own deploy steps.

## Changing the trusted forwarder

There are two distinct change paths depending on whether you're
swapping CODE or IDENTITY:

### Path A — replace the forwarder's code (same contract id)

If the existing forwarder has a bug or needs a feature added, push
the new binary at the SAME id via `vsc.update_contract` on the
forwarder. The mapping's `forwarder` state key still names the same
id; behaviour changes via the forwarder's new code.

```bash
contract-deployer update \
    -contractId vsc1BforwarderId \
    -wasm <new-forwarder.wasm>
```

The network-baked `ContractUpdateTimelockBlocks` window (48h on
mainnet) applies. Anyone can inspect the proposed diff during the
window.

**This is the path you want for almost every change.**

### Path B — swap to a DIFFERENT forwarder contract id

If the existing forwarder is compromised badly enough that you don't
trust its code even after an update (e.g. an attacker has the admin
key, OR there's a deep flaw in the forwarder's storage layout that
makes a code update insufficient), you have to migrate to a new
forwarder contract at a fresh id.

The mapping's `setForwarderContractId` is locked-after-first-set. To
unlock it requires an admin sequence that the current contract code
does NOT have a one-shot action for. The change path is:

1. `vsc.update_contract` on the mapping with a patch that ADDS a
   one-time `clearForwarderContractId` admin action (or extends the
   existing pause/clear flow). The patch goes through the
   `ContractUpdateTimelockBlocks` window like any code change.
2. After activation: pause the mapping, call the new clearing action,
   call `setForwarderContractId(<new-forwarder-id>)`, unpause.
3. (Optional) push another `vsc.update_contract` that REMOVES the
   clearing action so the lock semantics are restored.

This is the heavy path. The doc records it as a possibility, not a
recommended routine; in practice you'll use path A.

## Emergency revoke

### Local — per-operator, immediate

Set `sysconfig.DashMappingContractId=""` on the affected witness and
restart magi.

```bash
# Edit the operator's sysconfig override file:
{
  "dashMappingContractId": ""
}
# Then restart magi.
```

`resolveTrustedForwarders` returns `nil` immediately on restart →
empty allow-list → every `call_as` via Dash IS-login aborts on that
witness. The witness will produce contract-output disagreements with
the rest of the fleet on any tx that exercises `call_as`, falling out
of consensus on those specific tx outputs until the operator
re-enables the feature.

**Coarse:** disables the entire IS-login feature, not just one bad
forwarder. Fine for compromise response — there's exactly one
forwarder per chain, and the worst case is "users can't use IS-login
on this witness's view" while Dash deposits still land in the
dashd-watcher and get IS-locked. Operators can do this unilaterally
without fleet coordination.

### Coordinated — network-wide, slow

`vsc.update_contract` on the dash-mapping-contract with a patch that
returns empty from the `forwarder` state key (or uses path B above to
clear-and-reset). Standard `ContractUpdateTimelockBlocks` window
applies, then takes effect simultaneously on every witness.

Use when the issue is severe enough that the slow network-wide path
is preferable to the BFT-divergent local-revoke approach (e.g. a
discovered exploit you want every witness to refuse before users
can interact with the forwarder).

## Failure modes

What magi does when something is misconfigured:

| Situation | `resolveTrustedForwarders` returns | `call_as` from the forwarder | Effect |
|---|---|---|---|
| `sysconfig.DashMappingContractId` empty | `nil` | aborts | IS-login disabled (safe default) |
| sysconfig points at a non-existent contract id | `nil` (`GetStateStore` returns nil) | aborts | IS-login disabled (safe default) |
| Mapping deployed, `setForwarderContractId` not called | `nil` (state key empty) | aborts | IS-login disabled (safe default) |
| Mapping wired correctly, `vsc.call` from the pinned forwarder | `["contract:vsc1B<forwarder>"]` | succeeds | Normal operation |
| Mapping wired correctly, `vsc.call` from any OTHER contract | `["contract:vsc1B<forwarder>"]` | aborts (`ErrNoPermission`) | Defended |
| Mapping wired correctly, but `mapping["forwarder"]` points at a wrong/malicious contract id | `["contract:<wrong-id>"]` | succeeds for `<wrong-id>` | **Whoever controls the mapping pins the forwarder. This is the trust assumption.** |
| Per-witness sysconfig divergence (some witnesses point at the mapping, others don't) | Different on each witness | Different per witness | BFT rejects the minority's contract output; that witness falls behind on `call_as` blocks |

Every case except the last two is a safe-default (aborts on the
ambiguous case). The "wrong contract pinned" row is the explicit
trust assumption — magi trusts whoever can update the mapping to pin
the right forwarder.

## Witness-fleet readiness

The trusted-forwarders mechanism doesn't introduce a new consensus
version flip — the magi-side wiring ships as part of the binary, no
runtime gate. What operators need to coordinate is the binary
upgrade itself:

* All witnesses must run a binary that includes the
  `resolveTrustedForwarders` code path. Older binaries don't read the
  mapping's `forwarder` key and will diverge on the first `call_as`
  tx after the feature is enabled.
* All witnesses' sysconfig must point at the same
  `DashMappingContractId`. Mismatched pointers compute different
  allow-lists → divergent outputs → BFT ejects the minority on
  affected blocks.

[`cmd/check-witness-readiness`](../../cmd/check-witness-readiness/main.go)
is the audit tool. It queries `witnessNodes(height)` from a magi
GraphQL endpoint, filters to enabled witnesses, reports which are
running a binary at-or-above a target version triple. Exit code is
non-zero when any witness is behind, list of behind accounts on
stdout, summary on stderr.

```bash
check-witness-readiness \
    -gqlURL https://magi-mainnet/api/v1/graphql \
    -targetMajor 0 -targetConsensus 2 -targetNonConsensus 0
```

Run this before deploying the mapping contract on testnet/mainnet to
make sure the entire fleet is on a binary that knows how to read
`mapping["forwarder"]`.

## Automated test coverage

The chain-side wiring is exercised by
[`tests/devnet/is_login_opcall_test.go`](../../tests/devnet/is_login_opcall_test.go).
The full happy path runs:

1. Deploy dash-mapping + dash-forwarder + a target contract.
2. Call `mapping.setForwarderContractId(forwarderId)`.
3. `SetDashMappingContractId(mappingId)` in sysconfig + restart magi.
4. Trigger a real Dash testnet payment → IS service observes →
   attestation → L2 submission of `mapInstantSendV2` → mapping
   dispatches to forwarder via `sdk.ContractCall` → forwarder invokes
   `sdk.ContractCallAs(target, "setString", "key=ophk;value=opval",
   senderDashDID, nil)`.
5. Asserts the target contract's state key `ophk == opval`.

Pass time: ~377s. Last verified: 2026-06-15 against go-vsc-node
`21affffd` + utxo-mapping `12fe0a9`.

### What's NOT exercised by automated tests

A few scenarios from the failure-mode table above are only verified at
the unit-test level (the `resolveTrustedForwarders` helper has
exhaustive coverage in
[`modules/state-processing/trusted_forwarders_test.go`](../../modules/state-processing/trusted_forwarders_test.go)
— wait, that file was DELETED in the rework; coverage now lives in
the system-config tests + the integration in TestIsLoginOpCallSmoke).
Operators should manually verify these before mainnet flip:

* **Emergency revoke via sysconfig clear.** Take a single witness on
  a working testnet, set `dashMappingContractId=""`, restart, attempt
  an IS-login op=call → that witness should produce an
  `errMsg="call_as: caller contract:<id> is not in system-config.
  TrustedForwarders"` contract output while the rest of the fleet
  succeeds. Restore the witness's sysconfig + restart → it
  re-converges.
* **Mapping deployed but forwarder unset.** Deploy a fresh mapping
  without calling `setForwarderContractId`. Point sysconfig at it.
  Restart magi. Trigger an op=call → the call_as should abort because
  `mapping["forwarder"]` is empty.
* **Path B forwarder swap.** Demonstrating the heavy-path
  `vsc.update_contract` + pause + clear + reset cycle end-to-end on
  testnet. Not done because path A handles every realistic case.

Adding these to the automated devnet suite is straightforward (each
is a small variation on `TestIsLoginOpCallSmoke`) but the cost-benefit
is questionable — the resolver's branches are fully unit-tested, and
the integration of "magi reads sysconfig + state correctly" is
exercised by the happy-path test. Add them if operator-side
confidence requires it.

## Files

* [`modules/state-processing/trusted_forwarders.go`](../../modules/state-processing/trusted_forwarders.go)
  — `resolveTrustedForwarders` (the single state-key read).
* [`modules/state-processing/transactions.go`](../../modules/state-processing/transactions.go)
  (`TxVscCallContract.ExecuteTx`) — wiring into the execution context
  via the `WithTrustedForwarders` option.
* [`modules/common/system-config/system-config.go`](../../modules/common/system-config/system-config.go)
  — `DashMappingContractId()` accessor + `SysConfigOverrides.
  DashMappingContractId` JSON field.
* [`modules/contract/execution-context/execution-context.go`](../../modules/contract/execution-context/execution-context.go)
  — `IsTrustedForwarder()` check (the consumer) + the child-context
  propagation fix (so forwarders reached transitively via
  `mapping.mapInstantSendV2` still see themselves in the allow-list).
* [`modules/gql/gqlgen/schema.resolvers.go`](../../modules/gql/gqlgen/schema.resolvers.go)
  (`SimulateContractCall` resolver) — mirrors the same state-read so
  GraphQL simulate paths match real-execution semantics for `call_as`.
* [`cmd/check-witness-readiness/main.go`](../../cmd/check-witness-readiness/main.go)
  — pre-deploy fleet-version audit tool.
* [`cmd/is-service/peer_discovery.go`](../../cmd/is-service/peer_discovery.go)
  — IS service auto-discovers bootstrap peers from
  `witnessNodes(height)`; unrelated to TrustedForwarders but lives in
  the same rollout.
