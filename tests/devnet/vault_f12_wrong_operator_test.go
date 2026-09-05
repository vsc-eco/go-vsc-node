package devnet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestVaultF12WrongOperator is failure-state F12 of the BTC vault-rotation-v2 suite:
// "wrong operator identity".
//
// It proves the operator authorization boundary live: a stranger cannot drive a
// rotation, an appointed operator can, clearing the operator revokes it, and garbage
// operator input is rejected.
//
// WHY THIS IS A FAILURE STATE
// The scoped vault operator (contract/main.go checkOperator plus SetVaultOperator) is
// the only identity besides the owner that can move retiring funds toward the committed
// successor. Everything the guard admits is meant to be self-validating, so the guard
// itself is the whole security boundary. Two ways it can fail in production: it admits
// an account it should not (a stranger drives, or a revoked key still drives), or it
// refuses an account it should admit (the appointed operator cannot complete a rotation
// and the drain stalls with funds parked on a retiring generation). This test exercises
// both directions against a running fleet, on real coins, with a real TSS-signed sweep.
//
// IDENTITIES ON THIS DEVNET
// Node identities are hive:<prefix><node>, and node 1 deploys the contract, so node 1 is
// the contract OWNER. Node 3 is the third party: a stranger first, then the appointed
// operator, then a stranger again after revocation. Every contract call is issued as the
// witness account of the node it is sent from (CallContractWithIntents signs the vsc.call
// with required_auths [<prefix><node>]), so "call from node 3" really is a different
// on-chain caller, not a relabelled owner call.
//
// WHAT IT PROVES
//  1. F12-NOOP: with gen-0 retiring and still holding a sweepable UTXO, migrateVault from
//     node 3 (neither owner nor operator) is refused and the pending-spend list does not
//     grow. The case requires gen-0 to actually hold a UTXO, so a refusal cannot be the
//     benign "nothing to migrate" masquerading as an authorization refusal. The positive
//     control for this case is F12-OP: the SAME call, from the SAME node, succeeds once
//     the appointment lands, so the refusal is about identity and nothing else.
//  2. F12-OP: after the owner appoints node 3, migrateVault from node 3 builds a real
//     migration sweep, the retiring generation signs every input, and the sweep is
//     broadcast, mined, relayed and settled. An appointed operator can complete a full
//     rotation without the owner key.
//  3. F12-CLEARED: gen-0 is re-funded with a late deposit (so there is genuinely work to
//     do again), the owner clears the operator with an empty input, and migrateVault from
//     node 3 is refused once more with the pending-spend list unchanged, while the OWNER
//     can still build a sweep on the same state. That owner control is what separates
//     "revocation worked" from "the sweep path is simply broken here".
//  4. F12-BADOP: setVaultOperator with a value that is neither a hive: account nor a did:
//     identity is rejected, and the stored operator stays cleared, so a typo cannot leave
//     the vault with an unusable or unexpected operator entry.
//  5. F12-IDENT: byte-identical vault contract state across all 5 nodes at the end, so
//     none of the authorization decisions above split the fleet.
//
// A precondition that cannot be established (v2 not really on, rotation incomplete) is a
// t.Fatalf, never a t.Skip, so this test can never pass vacuously.
//
// RUN:
//
//	VAULT_F12_RUN=1 DEVNET_KEEP=1 go test -v -run TestVaultF12WrongOperator -timeout 95m ./tests/devnet/
func TestVaultF12WrongOperator(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F12_RUN") == "" {
		t.Skip("set VAULT_F12_RUN=1")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 85*time.Minute)
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		wasm = "/home/clauderfly/utxo-s1/btc-mapping-contract/bin/dev.wasm"
	}
	if _, err := os.Stat(wasm); err != nil {
		t.Fatalf("wasm: %v", err)
	}

	// hpin is AFTER genesis (~block 190) so gen-0 is minted on the v2-off path (no
	// fresh-genesis deadlock) and v2 is ON for the rotation and every operator check.
	const hpin uint64 = 400

	cfg := tssTestConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 85*time.Minute)

	c := &vfCase{t: t}

	// SETUP: deploy, seed headers, wire the oracle, mint + register gen-0, fund it.
	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "vault F12 wrong operator")
	cid := env.cid

	// v2 must really be in force before any v2 assertion, otherwise the whole test is
	// vacuous (flag inert, registry absent).
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	owner := "hive:" + d.witnessAccount(1)
	stranger := "hive:" + d.witnessAccount(3)
	t.Logf("owner=%s (magi-1), third party=%s (magi-3)", owner, stranger)

	primary1, rotated := vfRotate(t, d, ctx, cid, 1)
	if !rotated {
		t.Fatalf("PRECONDITION FAILED: gen-0 to gen-1 rotation did not complete (primary1=%q). Without a Retiring gen-0 and an Active gen-1 there is no migrateVault work for an operator to be authorized (or refused) for", primary1)
	}
	t.Logf("rotation done: gen-1 active, primary1=%s, gen-0 retiring", primary1)

	// The migration sweep pays its miner fee out of FeeSupply, so seed it. Without this
	// every migrateVault would be refused for a fee reason and F12 would be measuring
	// F11's failure state instead of the authorization guard.
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)

	// ---- 1. F12-NOOP: a stranger cannot drive the rotation ----
	// gen-0 must still hold a sweepable UTXO, otherwise a refusal could be the benign
	// "nothing to migrate" and the case would prove nothing about identity.
	gen0Before := vfGenUtxoCountOn(d, ctx, 2, cid, 0)
	spendsBefore := txSpendIds(t, d, ctx, cid)
	strangerCall := vstatus(t, d, ctx, 3, cid, "migrateVault", "")
	spendsAfterStranger := txSpendIds(t, d, ctx, cid)
	noopOK := !isOK(strangerCall) &&
		f12SameSpends(spendsBefore, spendsAfterStranger) &&
		gen0Before >= 1
	c.rec("F12-NOOP", "migrateVault from a non-owner, non-operator is refused and builds no sweep", noopOK,
		fmt.Sprintf("status=%s (want not CONFIRMED/INCLUDED), pendingSpends %d -> %d, gen-0 held %d UTXO(s) so there WAS work to refuse",
			strangerCall, len(spendsBefore), len(spendsAfterStranger), gen0Before))
	t.Logf("vaultop before any appointment (magi-2 view) = %q", f12OperatorOn(d, ctx, 2, cid))

	// ---- 2. F12-OP: the owner appoints node 3, and node 3 drives the whole sweep ----
	appoint := vstatus(t, d, ctx, 1, cid, "setVaultOperator", stranger)
	if !isOK(appoint) {
		t.Errorf("the owner could NOT appoint magi-3 as vault operator (status=%s); the F12-OP case below can no longer prove the operator path works", appoint)
	}
	t.Logf("vaultop after appointment (magi-2 view) = %q", f12OperatorOn(d, ctx, 2, cid))

	opTxid, opSd, opBuild := vfBuildSweep(t, d, ctx, 3, cid)
	c.rec("F12-OP", "the appointed operator (magi-3, not the owner) builds a real migration sweep", opTxid != "" && opSd != nil,
		fmt.Sprintf("appoint=%s migrateVault(from magi-3)=%s sweepTxid=%q signingData=%v", appoint, opBuild, opTxid, opSd != nil))

	if opSd != nil {
		raw, signed := vfAwaitSweepSignatures(t, d, ctx, cid+"-main", opSd)
		if !signed {
			t.Errorf("the retiring generation (%s-main) never signed every input of the operator-built sweep %s, so the operator-driven rotation could not complete", cid, opTxid)
		} else {
			bcTxid, h, err := vfBroadcastAndMine(t, d, ctx, raw)
			if err != nil {
				t.Errorf("operator-built sweep %s failed to broadcast/mine: %v (bcTxid=%q)", opTxid, err, bcTxid)
			} else {
				st := vfRelayAndConfirm(t, d, ctx, 1, cid, bcTxid, h)
				t.Logf("operator-built sweep settled: bcTxid=%s btcHeight=%d confirmSpend=%s", bcTxid, h, st)
			}
		}
	}
	gen0AfterOp := vfGenUtxoCountOn(d, ctx, 2, cid, 0)
	t.Logf("gen-0 UTXO count after the operator-driven sweep: %d -> %d (0 means the operator completed the drain)", gen0Before, gen0AfterOp)

	// ---- 3. F12-CLEARED: revocation puts node 3 back outside the boundary ----
	// gen-0 is (expected to be) empty now, and a refusal on an empty generation would be
	// a "nothing to migrate" refusal, not an authorization refusal. So re-fund gen-0 with
	// a LATE DEPOSIT to the retiring generation's own address first (a retiring gen stays
	// matchable until it is purged, NR-4), giving the next migrateVault genuine work.
	// fundVaultViaSPV fatals if the deposit cannot be mapped; that is a real precondition
	// failure for this step, not something to skip past.
	lateDeposit := int64(30_000_000)
	fundVaultViaSPV(t, d, ctx, cid, env.primary0, backupPubKeyG, owner, lateDeposit, contractLastHeight(t, d, ctx, cid))
	gen0Refunded := vfGenUtxoCountOn(d, ctx, 2, cid, 0)
	t.Logf("late deposit of %d sats to the RETIRING gen-0 address: gen-0 now holds %d UTXO(s)", lateDeposit, gen0Refunded)

	cleared := vstatus(t, d, ctx, 1, cid, "setVaultOperator", "")
	vaultopAfterClear := f12OperatorOn(d, ctx, 2, cid)
	t.Logf("vaultop after clearing (magi-2 view) = %q (empty means revoked)", vaultopAfterClear)

	spendsBeforeRevoked := txSpendIds(t, d, ctx, cid)
	revokedCall := vstatus(t, d, ctx, 3, cid, "migrateVault", "")
	spendsAfterRevoked := txSpendIds(t, d, ctx, cid)

	// Positive control: the OWNER must still be able to build a sweep on exactly this
	// state. Without it, "magi-3 was refused" could just mean the sweep path is dead here
	// (empty fee reserve, nothing sweepable, an already-pending sweep) rather than that
	// revocation worked.
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
	ctlTxid, ctlSd, ctlBuild := vfBuildSweep(t, d, ctx, 1, cid)
	ownerControlOK := ctlTxid != "" && ctlSd != nil

	clearedOK := isOK(cleared) &&
		!isOK(revokedCall) &&
		f12SameSpends(spendsBeforeRevoked, spendsAfterRevoked) &&
		len(vaultopAfterClear) == 0 &&
		gen0Refunded >= 1 &&
		ownerControlOK
	c.rec("F12-CLEARED", "clearing the operator revokes magi-3, while the owner can still sweep the same state", clearedOK,
		fmt.Sprintf("clear=%s storedOperator=%q migrateVault(from magi-3)=%s (want not CONFIRMED/INCLUDED), pendingSpends %d -> %d, gen-0 held %d UTXO(s), ownerControl(build from magi-1)=%v status=%s txid=%q",
			cleared, vaultopAfterClear, revokedCall, len(spendsBeforeRevoked), len(spendsAfterRevoked), gen0Refunded, ownerControlOK, ctlBuild, ctlTxid))

	// ---- 4. F12-BADOP: garbage operator input is rejected ----
	// The contract accepts only a hive: account or a did: identity, so a typo cannot
	// silently install an operator value nothing can ever match.
	badOp := vstatus(t, d, ctx, 1, cid, "setVaultOperator", "garbage")
	vaultopAfterBad := f12OperatorOn(d, ctx, 2, cid)
	c.rec("F12-BADOP", "setVaultOperator rejects an identity that is neither hive: nor did:, and stores nothing", !isOK(badOp) && len(vaultopAfterBad) == 0,
		fmt.Sprintf("status=%s (want not CONFIRMED/INCLUDED), storedOperator=%q (want empty)", badOp, vaultopAfterBad))

	// ---- 5. no authorization decision above may split the fleet ----
	vfAssertContractIdentical(c, d, ctx, cid, vfAllNodes(5), 4*time.Minute, "F12-IDENT")

	c.summary("F12")
	t.Logf("F12 COMPLETE CONTRACT=%s operatorSweepTxid=%s ownerControlSweepTxid=%s", cid, opTxid, ctlTxid)
}

// f12SameSpends reports whether two pending-spend id lists hold exactly the same
// members. Used to prove a refused migrateVault left no half-built sweep behind.
func f12SameSpends(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !contains(b, x) {
			return false
		}
	}
	return true
}

// f12OperatorOn reads the stored vault operator ("vaultop") from one node. An empty
// string means no operator is appointed; "READ_ERR" means the node could not be read.
func f12OperatorOn(d *Devnet, ctx context.Context, node int, cid string) string {
	st, err := getStateHex(d, ctx, node, cid, []string{"vaultop"})
	if err != nil {
		return "READ_ERR"
	}
	return string(st["vaultop"])
}
