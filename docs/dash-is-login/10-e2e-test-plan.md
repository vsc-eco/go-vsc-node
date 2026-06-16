# Workstream 10 — End-to-End Test Plan

Once workstreams 1-9 are all wired live (i.e. the TODO markers in each
scaffold's commit message are resolved), this plan validates the full
Dash IS-login flow on devnet and testnet.

## Pre-conditions

Before running this plan, confirm:

- [ ] go-vsc-node feat/dash-is-login built + node running on devnet/testnet
- [ ] dash-mapping-contract built + deployed; admin init done
- [ ] dash-forwarder-contract built + deployed; admin init pointing at mapping
- [ ] system-config.TrustedForwarders contains the dash-forwarder-contract id
- [ ] dash-mapping-contract.allowedTargets contains the magi-dex router
- [ ] cmd/is-service running with bridge pubkeys, signer secret, correct network
- [ ] vsc-dashd running on the same network, ZMQ reachable
- [ ] At least one Dash test wallet with funds on the same network

## Test groups

### Group A — DashDID + signing primitives (lib-level)

These are already covered by unit tests landed in workstream 1, 4a, 2.
Sanity-check with `go test ./lib/dids/... ./modules/wasm/sdk/... ./modules/contract/execution-context/... ./modules/common/system-config/... ./modules/islock-attestation/...`.

### Group B — Address derivation parity

- [ ] `go test ./cmd/is-service/ -run TestDepositAddress_Parity` passes
      against current dash-mapping-contract codebase.
- [ ] When the contract's createP2WSHAddressWithBackup is modified,
      parity test catches drift in CI.

### Group C — IS Service HTTP API

Driven by integration tests against a real running is-service binary:

- [ ] `POST /session/start { op: "auth" }` returns 201 with a valid
      `tdash1...` deposit address.
- [ ] `POST /session/start { op: "call", args: {...} }` returns 201
      with the correct instruction encoded.
- [ ] `POST /session/start` with `op=call;amount=100000` (below floor)
      returns 400.
- [ ] Two concurrent starts with the same client-supplied sid: second
      returns 409.
- [ ] `GET /session/:sid/status` after start returns state
      `WAITING_FOR_IS`.
- [ ] `POST /session/:sid/cancel` returns 204; subsequent status shows
      `EXPIRED`.
- [ ] Rate-limit: 11 starts from same IP in 1 minute returns 429 on the
      11th.
- [ ] `addressSignature` verifies against the configured signer's HMAC
      key.

### Group D — Live IS-lock observation on devnet

- [ ] Start an is-service session for `op=auth`. Capture the deposit
      address.
- [ ] Pay 0.0001 DASH from a test wallet via InstantSend to that
      address.
- [ ] Within 30 seconds, status transitions to `IS_OBSERVED` (ZMQ
      subscriber working).
- [ ] Validator p2p subscribers also see the IS-lock (check validator
      logs for the matching txid in their IsLockMemory).

### Group E — Attestation collection

- [ ] IS service broadcasts an attestation request on the
      `/islock-attestation/v1` topic.
- [ ] N-of-M validators respond with BLS-signed responses within 5
      seconds.
- [ ] IS service collects responses and aggregates them.
- [ ] Status transitions to `ATTESTING` while collecting.

### Group F — On-chain finality (op=auth happy path)

- [ ] IS service submits `mapInstantSendV2(rawTxHex, instruction, epoch,
      attestations)` L2 tx.
- [ ] dash-mapping-contract verifies BLS aggregate against active
      validator set — passes.
- [ ] DashDID's internal balance credited with the dust amount.
- [ ] Session token issued; status transitions to `ON_CHAIN`.
- [ ] Frontend can authenticate to Altera with the session token.

### Group G — On-chain finality (op=call swap happy path)

- [ ] Same flow as Group F but with `op=call;contract=<dex-router>;method=swap;...`.
- [ ] mapping-contract writes forwardQueue[txid] = PENDING_FORWARD.
- [ ] mapping-contract calls dash-forwarder-contract.execute(txid).
- [ ] dash-forwarder-contract reads forwardQueue, parses instruction,
      verifies target is in allowedTargets.
- [ ] dash-forwarder-contract calls dex-router.execute via call_as
      with effectiveCaller=DashDID(sender).
- [ ] dex-router.executeDirectSwap reads `env.EffectiveCallerOrCaller()`
      → DashDID; uses it as the `From` for the mapped-asset transferFrom.
- [ ] DashDID's mapped-DASH balance debited by swap amount.
- [ ] HBD output credited to DashDID's internal HBD ledger.
- [ ] RC reimbursement HBD deducted from DashDID's internal HBD;
      sent native HBD to IS service's submitter account.
- [ ] forwardQueue[txid].status = FORWARDED.

### Group H — Failure paths

- [ ] DashDID with zero internal HBD attempts NFT transfer → contract
      rejects with `INSUFFICIENT_RC_BUDGET`; user sees "swap first"
      prompt in the frontend.
- [ ] Bad attestation bundle (wrong signature) → contract rejects.
- [ ] Multi-input Dash tx (inputs from different addresses) →
      contract rejects per §5.2.5 strict rule.
- [ ] op=call with target NOT in allowedTargets → forwarder.execute
      returns false → mapping marks FORWARD_FAILED; DashDID keeps
      mapped-DASH credit from step 1.
- [ ] Forwarder invoked by a non-mapping caller → returns
      ABORT:ErrNoPermission.

### Group I — Trust + governance

- [ ] Remove dash-forwarder-contract from system-config.TrustedForwarders
      → next call_as fails with permission denied.
- [ ] Re-add via governance procedure → call_as works again.
- [ ] Try to add a malicious contract to allowedTargets without the
      7-day timelock → proposal contract rejects the immediate
      ratification call.

### Group J — DoS / rate limit

- [ ] Single DashDID does 31 mapInstantSendV2 in 10 minutes → 31st is
      accepted (credit) but forward is skipped. forwardQueue[txid] has
      no entry; user gets DASH credit but no swap.
- [ ] Block-level RC cap test: at the configured threshold, additional
      mapInstantSendV2 in the same block fail-soft (next-block retry
      works).

### Group K — Slow-path recovery

- [ ] Validators offline → IS service times out attestation
      collection after 30s → state transitions to
      `ATTESTATION_TIMEOUT`.
- [ ] User keeps the deposit address QR for 1 hour, then submits via
      the slow-path `map(tx, blockProof, instruction)` → DASH lands
      on internal balance after PoW confirmation depth. Funds are
      never lost.

## Test infrastructure needed

- Devnet docker-compose with: 3 vsc validators, 1 vsc-dashd, 1
  is-service, deployed contracts, frontend stub.
- Shell scripts in `magi/testnet/scripts/dash-is-login/` for:
  - `e2e-login.sh` — drives Group F end-to-end.
  - `e2e-swap.sh` — drives Group G.
  - `e2e-failures.sh` — drives Group H.

## Estimated effort

Once all workstreams are wired live (TODOs resolved): **3-5 days** of
focused devnet + testnet runs, plus another 2-3 days for the testnet
shake-out with real Dash wallets paying live InstantSends.

## Sign-off criteria

Feature ready for mainnet rollout when:

- All Group A-K test cases pass on devnet.
- Group F + G pass on testnet with real DashPay payments.
- Frontend XSS audit complete (Group H frontend tests).
- ops on-call playbook reviewed (validator key rotation, forwarder
  contract upgrade procedure, allowedTargets governance).
