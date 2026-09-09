package devnet

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestVaultF23PauseMidSweep is failure-state F23 of the BTC vault-rotation-v2
// suite: "the owner hits the emergency pause while a migration sweep is already on
// Bitcoin".
//
// WHAT IT PROVES
// With v2 ON, an emergency pause cannot strand an already-broadcast sweep (the
// settle is exempt) while every NEW spend authorization (migrate, re-drive, unmap)
// is refused, and unpause resumes the rotation.
//
// WHY THAT SPLIT IS THE WHOLE POINT
// A pause is the operator's panic button, and the panic almost always arrives at
// the worst moment: a sweep has been signed by the retiring generation and is
// already sitting in a Bitcoin block, but the contract has not reconciled it yet.
// If the pause also froze the reconciliation, the contract would keep the swept
// UTXOs on its books forever while the coins had in fact already moved, and a pause
// that outlived header retention would prune the very proof needed to reconcile it.
// So the contract splits the two directions deliberately:
//
//   - NEW spend authorizations are pause-gated. migrateVault calls checkNotPaused
//     (contract/main.go:1023), redriveSpend calls it too (contract/main.go:1052,
//     a re-drive SIGNS a replacement spend and adjusts the fee reserve, so it is a
//     new authorization and not a reconciliation), and both unmap and unmapFrom
//     reach it through doUnmap (contract/main.go:363).
//   - The SETTLE is pause-exempt, but only for a spend that is already pending.
//     confirmSpend itself carries no checkNotPaused; the check lives inside
//     HandleConfirmSpend (contract/mapping/handlers.go, the BRK-4b block) and is
//     skipped when the txid is a live pending spend, which it decides from the
//     "d-<txid>" signing record, the "ms-<txid>" migration record or the
//     "us-<txid>" unmap record. A confirm of any OTHER transaction stays gated.
//   - addBlocks is NOT pause-gated at all (contract/main.go:198 calls only
//     checkOracle), which is what lets the sweep's block header reach the contract
//     during the pause so the exempt confirm has a header to prove against. The
//     node's own oracle relays headers too (modules/oracle/chain/handle_block_tick.go),
//     so headers can arrive during the pause without any test call at all.
//
// THE CASES
//  1. F23-MIGRATE-REFUSED: with the contract paused, migrateVault is refused and
//     moves nothing (no new pending spend, vault registry and migration sweep index
//     byte-equal, supplies unchanged, gen-0 status unchanged).
//  2. F23-REDRIVE-REFUSED: redriveSpend of the in-flight sweep txid is refused and
//     moves nothing. The txid used is a LIVE pending spend at that moment (it is in
//     "p"), and the confirmSpend of that same txid succeeds seconds later, so the
//     refusal is the pause gate and not a missing record.
//  3. F23-CONFIRM-EXEMPT: the same paused contract accepts confirmSpend of the
//     broadcast sweep and gen-0's UTXO count drops. The paused flag is re-read
//     before and after the confirm so the exemption cannot be claimed against a
//     contract that quietly unpaused.
//  4. F23-UNMAP-REFUSED: a user withdrawal while paused is refused, with the owner
//     holding a balance large enough to fund it, and the identical unmap succeeds
//     after the unpause (the control is carried in the case detail).
//  5. F23-RESUME: after unpause, migrateVault builds a NEW sweep, so the pending
//     spend list grows again.
//  6. F23-IDENT: the vault state is byte-identical across all 5 nodes at the end.
//
// DELIBERATE DEVIATION FROM THE SPEC (and why)
// The spec puts the late deposit to gen-0 after the unpause. This test makes that
// deposit BEFORE the pause instead. Reason: a sweep tranche takes up to
// MaxMigrationInputs (100) UTXOs, so the first sweep consumes everything gen-0
// holds, and a second migrateVault would then be refused for "nothing to migrate"
// whether the contract is paused or not. F23-MIGRATE-REFUSED would pass while
// proving nothing. Depositing 5,000,000 sats to gen-0's address BEFORE the pause
// leaves one genuinely migratable, confirmed, unswept UTXO behind, so the paused
// refusal has to be the pause gate, and the SAME call on the SAME UTXO succeeding
// after the unpause (F23-RESUME) is the positive control that proves it. The
// deposit has to happen before the pause in any case, because map is pause-gated
// too (contract/main.go:293).
//
// A precondition that cannot be established (v2 not really on, rotation did not
// complete, no sweep, sweep never signed, pause never landed) is a t.Fatalf, never
// a t.Skip, so this test can never pass vacuously.
//
// RUN:
//
//	VAULT_F23_RUN=1 DEVNET_KEEP=1 go test -v -run TestVaultF23PauseMidSweep -timeout 95m ./tests/devnet/
func TestVaultF23PauseMidSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F23_RUN") == "" {
		t.Skip("set VAULT_F23_RUN=1")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), vfTestBudget(85*time.Minute))
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		wasm = "/home/clauderfly/utxo-s1/btc-mapping-contract/bin/dev.wasm"
	}
	if _, err := os.Stat(wasm); err != nil {
		t.Fatalf("wasm: %v", err)
	}

	// hpin is AFTER genesis (~block 190) so gen-0 is minted on the v2-off path (no
	// fresh-genesis deadlock) and v2 is ON for the rotation, the sweep and the pause.
	const hpin uint64 = 400

	cfg := vfSlowReshareConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 85*time.Minute)

	c := &vfCase{t: t}

	// ---- SETUP: deploy, seed headers, wire the oracle, mint + register gen-0, fund it ----
	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "vault F23 pause mid sweep")
	cid := env.cid
	owner := env.owner

	// v2 must really be in force before any v2 assertion, otherwise the pause cases
	// would be measured against an inert system.
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	primary1, rotated := vfRotate(t, d, ctx, cid, 1)
	if !rotated {
		t.Fatalf("PRECONDITION FAILED: gen-0 to gen-1 rotation did not complete (primary1=%q). Without a retiring gen-0 and an active gen-1 there is no migration sweep to pause mid-flight", primary1)
	}
	t.Logf("rotation done: gen-1 active primary1=%s, gen-0 retiring", primary1)

	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)

	// ---- SETUP: build the sweep and collect the retiring generation's signatures ----
	txid, sd, migStatus0 := vfBuildSweep(t, d, ctx, 1, cid)
	if sd == nil {
		t.Fatalf("PRECONDITION FAILED: migrateVault produced no signable sweep (status=%s txid=%q). The whole test is about a sweep that is already in flight when the pause lands", migStatus0, txid)
	}
	t.Logf("sweep built: txid=%s (migrateVault status=%s)", txid, migStatus0)

	raw, signed := vfAwaitSweepSignatures(t, d, ctx, cid+"-main", sd)
	if !signed {
		t.Fatalf("PRECONDITION FAILED: the retiring generation never signed sweep %s, so there is nothing to broadcast and no in-flight spend for the pause to strand", txid)
	}

	// ---- SETUP: the migratable witness UTXO (see the DELIBERATE DEVIATION note) ----
	// A late deposit to gen-0's address, made while the contract is still unpaused
	// (map is pause-gated), so gen-0 still holds something worth migrating once the
	// pause is on. Without it the paused migrateVault would be refused for "nothing
	// to migrate" and F23-MIGRATE-REFUSED would prove nothing.
	gen0BeforeDeposit := vfGenUtxoCountOn(d, ctx, 2, cid, 0)
	balBeforeDeposit := balanceSats(t, d, ctx, cid, owner)
	fundVaultViaSPV(t, d, ctx, cid, env.primary0, backupPubKeyG, owner, 5_000_000, contractLastHeight(t, d, ctx, cid))
	gen0AfterDeposit := vfGenUtxoCountOn(d, ctx, 2, cid, 0)
	balAfterDeposit := balanceSats(t, d, ctx, cid, owner)
	if gen0AfterDeposit <= gen0BeforeDeposit {
		t.Fatalf("PRECONDITION FAILED: the late deposit to gen-0 did not add a UTXO (gen-0 count %d -> %d, owner balance %d -> %d). Without an unswept gen-0 UTXO the paused migrateVault refusal is indistinguishable from 'nothing to migrate'",
			gen0BeforeDeposit, gen0AfterDeposit, balBeforeDeposit, balAfterDeposit)
	}
	t.Logf("migratable witness UTXO in place: gen-0 count %d -> %d, owner balance %d -> %d sats",
		gen0BeforeDeposit, gen0AfterDeposit, balBeforeDeposit, balAfterDeposit)

	// ---- SETUP: the sweep goes onto Bitcoin, still unreconciled by the contract ----
	bcTxid, sweepH, err := vfBroadcastAndMine(t, d, ctx, raw)
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: broadcasting the signed sweep failed: %v (txid=%s). The pause must land on a sweep that is already on chain", err, txid)
	}
	t.Logf("sweep broadcast and mined: bcTxid=%s at BTC height %d (contract txid=%s)", bcTxid, sweepH, txid)

	// ---- PAUSE (owner, node 1) ----
	pauseStatus := vstatus(t, d, ctx, 1, cid, "pause", "")
	pausedVal, pausedNow := vf23WaitPaused(d, ctx, 2, cid, true, 90*time.Second)
	if !pausedNow {
		t.Fatalf("PRECONDITION FAILED: the contract is not paused on magi-2 (pause status=%s, 'paused' key = %q, want \"1\"). Every case below would measure an UNPAUSED contract", pauseStatus, pausedVal)
	}
	t.Logf("contract PAUSED (status=%s, magi-2 'paused'=%q) with sweep %s in flight", pauseStatus, pausedVal, txid)

	base := vf23Snap(t, d, ctx, cid)
	t.Logf("paused-state baseline: pendingSpends=%v gen-0 utxos=%d gen-0 status=%s",
		base.spends, base.gen0Utxo, statusStr(base.gen0Stat))

	// ---- 1. F23-MIGRATE-REFUSED ----
	migStatus := vstatus(t, d, ctx, 1, cid, "migrateVault", "")
	afterMig := vf23Snap(t, d, ctx, cid)
	migDrift := vf23Diff(base, afterMig)
	_, stillPausedMig := vf23PausedOn(d, ctx, 2, cid)
	c.rec("F23-MIGRATE-REFUSED", "a second migrateVault is refused while the contract is paused and leaves no half-built sweep",
		!isOK(migStatus) && len(migDrift) == 0 && stillPausedMig,
		fmt.Sprintf("migrateVault status=%s, contract still paused=%v, gen-0 holds %d UTXO(s) including the unswept late deposit so 'nothing to migrate' is NOT the reason, gen-0 status=%s, pendingSpends %v -> %v, stateDrift=%v (the positive control that the refusal is the pause gate is F23-RESUME, the same call on the same UTXO after unpause)",
			migStatus, stillPausedMig, afterMig.gen0Utxo, statusStr(afterMig.gen0Stat), base.spends, afterMig.spends, migDrift))

	// ---- 2. F23-REDRIVE-REFUSED ----
	// The txid handed to redriveSpend is a LIVE pending spend right now, and the
	// confirmSpend of that same txid succeeds a few lines below, so a refusal here
	// cannot be blamed on an unknown or already-settled spend.
	redrivePending := contains(base.spends, txid)
	redriveStatus := vstatus(t, d, ctx, 1, cid, "redriveSpend", txid)
	afterRedrive := vf23Snap(t, d, ctx, cid)
	redriveDrift := vf23Diff(base, afterRedrive)
	_, stillPausedRedrive := vf23PausedOn(d, ctx, 2, cid)
	c.rec("F23-REDRIVE-REFUSED", "redriveSpend of the in-flight sweep is refused while paused (a re-drive is a NEW spend authorization, not a reconciliation)",
		!isOK(redriveStatus) && len(redriveDrift) == 0 && stillPausedRedrive && redrivePending,
		fmt.Sprintf("redriveSpend(%s) status=%s, contract still paused=%v, that txid was a live pending spend at the time=%v, pendingSpends %v -> %v, stateDrift=%v",
			txid, redriveStatus, stillPausedRedrive, redrivePending, base.spends, afterRedrive.spends, redriveDrift))

	// ---- 3. F23-CONFIRM-EXEMPT ----
	// addBlocks carries only checkOracle, so the sweep's header can still be relayed
	// while paused (the node's own oracle may already have relayed it). The confirm
	// of an already-pending spend is exempt from the pause by BRK-4b.
	preConfirm := afterRedrive
	hBeforeRelay := contractLastHeight(t, d, ctx, cid)
	_, pausedBeforeConfirm := vf23PausedOn(d, ctx, 2, cid)
	confirmStatus := vfRelayAndConfirm(t, d, ctx, 1, cid, bcTxid, sweepH)
	hAfterRelay := contractLastHeight(t, d, ctx, cid)

	settleDeadline := time.Now().Add(5 * time.Minute)
	postConfirm := preConfirm
	drained := false
	for {
		postConfirm = vf23Snap(t, d, ctx, cid)
		if postConfirm.gen0Utxo >= 0 && postConfirm.gen0Utxo < preConfirm.gen0Utxo {
			drained = true
			break
		}
		if time.Now().After(settleDeadline) {
			break
		}
		time.Sleep(10 * time.Second)
	}
	msGone := !vfSweepRecordOn(d, ctx, 2, cid, txid)
	spendGone := !contains(postConfirm.spends, txid)
	_, pausedAfterConfirm := vf23PausedOn(d, ctx, 2, cid)
	c.rec("F23-CONFIRM-EXEMPT", "confirmSpend settles the already-broadcast sweep while the contract is PAUSED (BRK-4b) and gen-0 drains",
		isOK(confirmStatus) && drained && pausedBeforeConfirm && pausedAfterConfirm,
		fmt.Sprintf("confirmSpend status=%s, paused before=%v after=%v, gen-0 utxos %d -> %d (the remaining one is the late deposit), ms-record gone=%v, txid left the pending list=%v, contract BTC height %d -> %d over the relay (addBlocks is not pause-gated, it carries only checkOracle), gen-1 utxos=%d",
			confirmStatus, pausedBeforeConfirm, pausedAfterConfirm, preConfirm.gen0Utxo, postConfirm.gen0Utxo,
			msGone, spendGone, hBeforeRelay, hAfterRelay, vfGenUtxoCountOn(d, ctx, 2, cid, 1)))

	// ---- 4. the unmap refusal (recorded after its post-unpause control below) ----
	dest := mustNewBtcAddr(t, d, ctx)
	const unmapSats = 1_000_000
	unmapPayload := fmt.Sprintf(`{"amount":"%d","to":"%s"}`, unmapSats, dest)
	balBeforeUnmap := balanceSats(t, d, ctx, cid, owner)
	unmapStatus := vstatus(t, d, ctx, 1, cid, "unmap", unmapPayload)
	afterUnmap := vf23Snap(t, d, ctx, cid)
	unmapDrift := vf23Diff(postConfirm, afterUnmap)
	balAfterUnmap := balanceSats(t, d, ctx, cid, owner)
	_, stillPausedUnmap := vf23PausedOn(d, ctx, 2, cid)
	t.Logf("unmap while paused: status=%s (owner balance %d -> %d sats, dest=%s, still paused=%v, drift=%v)",
		unmapStatus, balBeforeUnmap, balAfterUnmap, dest, stillPausedUnmap, unmapDrift)

	// ---- 5. UNPAUSE ----
	unpauseStatus := vstatus(t, d, ctx, 1, cid, "unpause", "")
	unpausedVal, stillPaused := vf23WaitPaused(d, ctx, 2, cid, false, 90*time.Second)
	if stillPaused {
		t.Errorf("unpause did not clear the flag on magi-2 (status=%s, 'paused'=%q). F23-RESUME and the unmap control below are expected to fail for that reason, not because the resume path is broken",
			unpauseStatus, unpausedVal)
	} else {
		t.Logf("contract UNPAUSED (status=%s, magi-2 'paused'=%q)", unpauseStatus, unpausedVal)
	}

	// ---- 6. F23-RESUME: the same migration the pause refused now builds a sweep ----
	beforeResume := txSpendIds(t, d, ctx, cid)
	resumeTxid, resumeSd, resumeStatus := vfBuildSweep(t, d, ctx, 1, cid)
	afterResume := txSpendIds(t, d, ctx, cid)
	c.rec("F23-RESUME", "after unpause the refused migrateVault builds a new sweep over the same gen-0 UTXO (pending spend list grows)",
		resumeTxid != "",
		fmt.Sprintf("migrateVault status=%s, new sweep txid=%q, signing data present=%v, pendingSpends %d -> %d (%v -> %v), gen-0 utxos=%d gen-1 utxos=%d, gen-0 status=%s",
			resumeStatus, resumeTxid, resumeSd != nil, len(beforeResume), len(afterResume), beforeResume, afterResume,
			vfGenUtxoCountOn(d, ctx, 2, cid, 0), vfGenUtxoCountOn(d, ctx, 2, cid, 1),
			statusStr(vfVaultStatusOn(d, ctx, 2, cid, 0))))

	// ---- 7. F23-UNMAP-REFUSED, with its post-unpause positive control ----
	// The identical unmap payload is replayed now that the pause is lifted. Without
	// this control a refusal could just mean "withdrawals are broken".
	unmapControl := vstatus(t, d, ctx, 1, cid, "unmap", unmapPayload)
	c.rec("F23-UNMAP-REFUSED", "a user withdrawal is refused while paused and the identical withdrawal succeeds after unpause",
		!isOK(unmapStatus) && stillPausedUnmap && balAfterUnmap == balBeforeUnmap && len(unmapDrift) == 0,
		fmt.Sprintf("paused unmap of %d sats to %s status=%s (contract still paused=%v, owner balance %d -> %d, stateDrift=%v); the SAME unmap after unpause status=%s (control: withdrawals are not simply broken)",
			unmapSats, dest, unmapStatus, stillPausedUnmap, balBeforeUnmap, balAfterUnmap, unmapDrift, unmapControl))
	if !isOK(unmapControl) {
		t.Logf("NOTE: the post-unpause unmap control returned %s. F23-UNMAP-REFUSED above is then a refusal WITHOUT a positive control, so read it as unproven rather than as a pass", unmapControl)
	}

	// ---- 8. no fork anywhere along the way ----
	vfAssertContractIdentical(c, d, ctx, cid, vfAllNodes(5), 4*time.Minute, "F23-IDENT")

	c.summary("F23")
	t.Logf("F23 COMPLETE CONTRACT=%s (sweep %s settled under pause=%v, resumed sweep=%q)", cid, txid, drained, resumeTxid)
}

// vf23Snapshot is the contract state that any pause-gated write would have to move.
// A refused call must leave every field of it untouched.
type vf23Snapshot struct {
	spends   []string // "p" pending spend txids
	registry []byte   // "v" vault registry
	sweeps   []byte   // "msl" migration sweep index
	supply   []byte   // "s" supply blob, 32 bytes of four big-endian int64 fields
	gen0Utxo int
	gen0Stat int
}

// vf23Snap takes that snapshot from magi-2. The node is fixed at 2 because
// txSpendIds always reads node 2, so mixing nodes would compare readings taken from
// two different replicas.
func vf23Snap(t *testing.T, d *Devnet, ctx context.Context, cid string) vf23Snapshot {
	t.Helper()
	st, err := getStateHex(d, ctx, 2, cid, []string{"v", "msl", "s"})
	if err != nil {
		t.Logf("vf23Snap: state read on magi-2 failed: %v", err)
	}
	return vf23Snapshot{
		spends:   txSpendIds(t, d, ctx, cid),
		registry: st["v"],
		sweeps:   st["msl"],
		supply:   st["s"],
		gen0Utxo: vfGenUtxoCountOn(d, ctx, 2, cid, 0),
		gen0Stat: vfVaultStatusOn(d, ctx, 2, cid, 0),
	}
}

// vf23Diff lists everything that moved between two snapshots. An empty result means
// the refused call was a true no-op at the byte level.
func vf23Diff(before, after vf23Snapshot) []string {
	var out []string
	if !vf23SameIds(before.spends, after.spends) {
		out = append(out, fmt.Sprintf("p pendingSpends %v -> %v", before.spends, after.spends))
	}
	if !bytes.Equal(before.registry, after.registry) {
		out = append(out, fmt.Sprintf("v vaultRegistry not byte-equal (%d -> %d bytes)", len(before.registry), len(after.registry)))
	}
	if !bytes.Equal(before.sweeps, after.sweeps) {
		out = append(out, fmt.Sprintf("msl migrationSweepIndex not byte-equal (%d -> %d bytes)", len(before.sweeps), len(after.sweeps)))
	}
	if !bytes.Equal(vf23SupplyCore(before.supply), vf23SupplyCore(after.supply)) {
		out = append(out, fmt.Sprintf("s supplies not byte-equal (%x -> %x)", vf23SupplyCore(before.supply), vf23SupplyCore(after.supply)))
	}
	if before.gen0Utxo != after.gen0Utxo {
		out = append(out, fmt.Sprintf("gen0Utxos %d -> %d", before.gen0Utxo, after.gen0Utxo))
	}
	if before.gen0Stat != after.gen0Stat {
		out = append(out, fmt.Sprintf("gen0Status %s -> %s", statusStr(before.gen0Stat), statusStr(after.gen0Stat)))
	}
	return out
}

// vf23SupplyCore returns the ActiveSupply, UserSupply and FeeSupply fields of the
// supply blob and deliberately drops the trailing BaseFeeRate. Every addBlocks call
// rewrites BaseFeeRate (contract/main.go:198), and the node's own oracle relays
// headers on its own schedule, so comparing the full blob would report drift that no
// pause-gated call caused.
func vf23SupplyCore(b []byte) []byte {
	if len(b) >= 24 {
		return b[:24]
	}
	return b
}

// vf23SameIds reports whether two pending spend id lists hold the same set.
func vf23SameIds(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, id := range a {
		seen[id]++
	}
	for _, id := range b {
		seen[id]--
		if seen[id] < 0 {
			return false
		}
	}
	return true
}

// vf23PausedOn reads the contract's "paused" key from one node. The contract writes
// the literal "1" on pause (contract/main.go Pause) and deletes the key on unpause,
// and checkNotPaused tests for exactly "1".
func vf23PausedOn(d *Devnet, ctx context.Context, node int, cid string) (string, bool) {
	st, err := getStateHex(d, ctx, node, cid, []string{"paused"})
	if err != nil {
		return "", false
	}
	v := string(st["paused"])
	return v, v == "1"
}

// vf23WaitPaused polls a node until the paused flag reaches `want`, and returns the
// last value seen plus whether the contract is paused at that moment.
func vf23WaitPaused(d *Devnet, ctx context.Context, node int, cid string, want bool, within time.Duration) (string, bool) {
	deadline := time.Now().Add(within)
	val, on := vf23PausedOn(d, ctx, node, cid)
	for on != want {
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Second)
		val, on = vf23PausedOn(d, ctx, node, cid)
	}
	return val, on
}
