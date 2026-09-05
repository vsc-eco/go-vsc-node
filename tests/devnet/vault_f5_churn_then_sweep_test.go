package devnet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// vault_f5_churn_then_sweep_test.go, F5 "committee churn then sweep" (V-A).
//
// WHAT THIS TEST CAN AND CANNOT PROVE, STATED PLAINLY.
//
// True election churn of a FUNDED RETIRING member, meaning a committee member that
// holds shares of a retiring BTC vault key being voted out of the election while
// that generation still holds coins, is PREVENTED BY DESIGN by the #11 bond lock.
// TxConsensusUnstake refuses any consensus_unstake whose sender
// IsBondLockedRetiringMember reports as a member of a fund-holding
// retiring/draining/inactive generation (modules/state-processing/transactions.go,
// the bond-lock branch; predicate in modules/state-processing/bond_lock.go, core in
// modules/vaultrotation/eligibility.go). There is therefore NO in-band way to make
// such a member leave the next election while the vault is not yet purged, and no
// devnet test can produce that state without patching the node. Stopping a node
// does not change the election either, so "stop it and see" is not churn.
//
// So F5 proves the three things that ARE reachable and that together stand in for
// the churn scenario:
//
//	F5-LOCK    the lock really holds. A consensus_unstake from a funded retiring
//	           member creates NO pending unstake action, neither for the large
//	           amount that would drop the member under the committee floor and out
//	           of the next election, nor for the small amount that is reused at
//	           release. The committee cannot shrink under the retiring key.
//	F5-SWEEP   signing liveness with ONE retiring member DOWN. With magi-4 stopped
//	           (4 of 5 up, which is exactly the TSS threshold and exactly the BLS
//	           block quorum) the gen-0 migration sweep still signs, broadcasts and
//	           settles, and gen-0 drains to zero UTXOs.
//	F5-RELEASE the lock RELEASES once the generation is finished. The identical op
//	           with the identical amount that was refused above is accepted.
//	F5-IDENT   all five nodes, magi-4 back up and caught up, hold byte-identical
//	           vault contract state.
//
// F5-RELEASE is also the mandatory control for F5-LOCK: it rules out "the unstake
// was refused for some unrelated reason" (bad amount, POA exit-halt, dead op path),
// because the same op with the same amount from the same account is accepted later.
//
// One deliberate deviation from the written spec, driven by reading the code.
// ComputeRetiringSignerSet counts a generation as fund-holding while its status is
// Retiring, Draining OR Inactive (eligibility.go L4-C1), and only PURGED is left
// out ("the bond-lock / V-A consumers must RELEASE a purged gen's members"). A
// drain plus one retireVault only reaches Inactive, so the release is measured in
// two stages: first at Inactive (the spec's step), and, if the bond is still held
// there, again after the purge grace window has been mined and relayed and gen-0
// has actually reached Purged. F5-RELEASE records which stage released it.
//
//	VAULT_F5_RUN=1 BTC_MAPPING_WASM_PATH=... go test -v -run TestVaultF5ChurnThenSweep -timeout 50m ./tests/devnet/
func TestVaultF5ChurnThenSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F5_RUN") == "" {
		t.Skip("set VAULT_F5_RUN=1")
	}
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		t.Fatal("BTC_MAPPING_WASM_PATH must point at the btc-mapping-contract regtest wasm")
	}
	if _, err := os.Stat(wasm); err != nil {
		t.Fatalf("wasm: %v", err)
	}

	const hpin = 400 // v2 activation height, AFTER genesis so gen-0 mints on the v2-off path
	cfg := tssTestConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 45*time.Minute)

	c := &vfCase{t: t}

	// The churning member. Every devnet node takes part in gen-0's keygen, so magi-4
	// is in gen-0's retiring committee once gen-1 supersedes it. Node 4 is also the
	// node stopped for the sweep, so the same member is the subject of both halves.
	const churnNode = 4
	member := "hive:" + d.witnessAccount(churnNode)

	// Setup, matching F2 through vfRotate plus fundFeeReserve: gen-0 minted and
	// funded before hpin, v2 gates on, gen-1 rotated in so gen-0 is Retiring and
	// still funded, fee reserve seeded on the active generation.
	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "F5 churn then sweep")
	cid := env.cid
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)
	t.Logf("F5 churn member=%s (magi-%d) CONTRACT=%s", member, churnNode, cid)

	primary1, ok := vfRotate(t, d, ctx, cid, 1)
	if !ok {
		t.Fatalf("PRECONDITION FAILED: gen-1 never activated, so gen-0 never became retiring and there is no locked bond to test")
	}
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)

	// The bond lock only fires for a FUND-HOLDING retiring generation, so both halves
	// of that precondition are read from chain truth before anything is asserted.
	// activateKey confirms slightly before the gen-0 flip is committed, so poll.
	gen0Status := -1
	for i := 0; i < 8; i++ {
		gen0Status = vaultStatusOf(t, d, ctx, cid, 0)
		if gen0Status >= 2 { // Retiring or later
			break
		}
		time.Sleep(10 * time.Second)
	}
	funded := genUtxoCount(t, d, ctx, cid, 0)
	if gen0Status < 2 || funded <= 0 {
		t.Fatalf("PRECONDITION FAILED: gen-0 status=%s utxos=%d, the bond lock only fires for a fund-holding retiring generation",
			statusStr(gen0Status), funded)
	}
	t.Logf("precondition: gen-0 status=%s holding %d UTXO(s), gen-1 active", statusStr(gen0Status), funded)

	// ---- 1. F5-LOCK: churn by unstake is REFUSED while gen-0 is retiring and funded ----
	head, err := getHeadBlock(d.HiveRPCEndpoint())
	if err != nil {
		t.Fatalf("hive head before the locked unstake: %v", err)
	}
	// 1999.000 of the 2000.000 devnet stake: accepted, this would leave magi-4 under
	// the committee floor and drop it out of the NEXT election, which is the churn
	// the bond lock exists to stop.
	if _, err := d.ConsensusUnstake(churnNode, "1999.000"); err != nil {
		t.Fatalf("consensus_unstake 1999.000 (locked) broadcast: %v", err)
	}
	time.Sleep(3 * time.Second)
	// Same-amount control for F5-RELEASE: the small amount that IS accepted after the
	// generation is finished must be refused now, so the pair isolates the bond lock
	// from any amount-dependent refusal.
	if _, err := d.ConsensusUnstake(churnNode, "1.000"); err != nil {
		t.Fatalf("consensus_unstake 1.000 (locked) broadcast: %v", err)
	}
	waitForBlock(t, d.HiveRPCEndpoint(), head+8, 3*time.Minute)
	time.Sleep(5 * time.Second)
	lockedPending := pendingConsensusUnstake(t, d, ctx, 2, member)
	c.rec("F5-LOCK", "consensus_unstake REFUSED while the member's generation is retiring and funded (no election churn possible)",
		lockedPending == 0,
		fmt.Sprintf("member=%s amounts=1999.000+1.000 gen-0=%s utxos=%d pending consensus_unstake=%d (want 0)",
			member, statusStr(gen0Status), funded, lockedPending))

	// ---- 2. F5-SWEEP: drain gen-0 with the retiring member DOWN ----
	// Stopping magi-4 leaves 4 of 5 nodes up: the BLS block quorum (4 of 5) still
	// forms and the TSS signing threshold (4 live parties of 5) is exactly met, so a
	// refusal here would be the readiness set, not a lost quorum.
	vfStopNodes(t, d, ctx, []int{churnNode})
	before := genUtxoCount(t, d, ctx, cid, 0)
	tranches := 0
	for i := 0; i < 3; i++ {
		remaining := genUtxoCount(t, d, ctx, cid, 0)
		if remaining == 0 {
			break
		}
		if i > 0 {
			// every settled tranche consumes the fee reserve
			fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
		}
		t.Logf("tranche %d: gen-0 still holds %d UTXO(s), sweeping with magi-%d down", i+1, remaining, churnNode)
		migrateAndSettle(t, d, ctx, cid, cid+"-main", primary1, backupPubKeyG)
		tranches++
		if after := genUtxoCount(t, d, ctx, cid, 0); after >= remaining {
			t.Errorf("drain tranche %d made NO progress (%d -> %d UTXO(s)) with magi-%d down", i+1, remaining, after, churnNode)
			break
		}
	}
	left := genUtxoCount(t, d, ctx, cid, 0)
	c.rec("F5-SWEEP", "gen-0 migration sweep signs, broadcasts and settles with one retiring member DOWN",
		before > 0 && left == 0,
		fmt.Sprintf("magi-%d stopped (4 of 5 up), %d tranche(s), gen-0 %d -> %d UTXO(s)", churnNode, tranches, before, left))
	vfStartNodes(t, d, ctx, []int{churnNode})

	// ---- 3. F5-RELEASE: the same unstake is accepted once gen-0 is finished ----
	if left != 0 {
		t.Errorf("F5-RELEASE cannot be read as a release: gen-0 still holds %d UTXO(s), so the bond is legitimately still locked", left)
	}
	// Stage A, the spec's step: drain then retireVault, which lands gen-0 on Inactive.
	vstatus(t, d, ctx, 1, cid, "retireVault", "")
	statusA := vaultStatusOf(t, d, ctx, cid, 0)
	t.Logf("gen-0 status after the post-drain retireVault: %s", statusStr(statusA))
	headA, err := getHeadBlock(d.HiveRPCEndpoint())
	if err != nil {
		t.Fatalf("hive head before the stage-A release unstake: %v", err)
	}
	if _, err := d.ConsensusUnstake(churnNode, "1.000"); err != nil {
		t.Fatalf("consensus_unstake 1.000 (stage A release) broadcast: %v", err)
	}
	waitForBlock(t, d.HiveRPCEndpoint(), headA+8, 3*time.Minute)
	time.Sleep(5 * time.Second)
	pendingA := pendingConsensusUnstake(t, d, ctx, 2, member)
	t.Logf("stage A (gen-0 %s): pending consensus_unstake=%d", statusStr(statusA), pendingA)

	releasedAt := statusStr(statusA)
	pending := pendingA
	statusB := statusA
	if pendingA == 0 {
		// Stage B: eligibility.go counts Inactive as fund-holding, so the bond is
		// expected to hold until gen-0 is PURGED. Mine and relay the purge grace
		// window (VaultPurgeGraceBlocks is 144 BTC blocks), retire again, retry.
		t.Logf("bond still held at %s, driving gen-0 to Purged (eligibility.go counts Inactive as fund-holding)", statusStr(statusA))
		last := contractLastHeight(t, d, ctx, cid)
		h, err := d.MineBlocks(ctx, 150)
		if err != nil {
			t.Fatalf("mining the purge grace window: %v", err)
		}
		const relayBatch = 25
		for start := last + 1; start <= h; start += relayBatch {
			var hexBatch string
			for hh := start; hh < start+relayBatch && hh <= h; hh++ {
				hx, _ := btcBlockHeaderHex(ctx, d, hh)
				hexBatch += hx
			}
			if s := vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":10}`, hexBatch)); !isOK(s) {
				t.Logf("purge-window addBlocks from %d status=%s", start, s)
			}
		}
		vstatus(t, d, ctx, 1, cid, "retireVault", "")
		statusB = vaultStatusOf(t, d, ctx, cid, 0)
		t.Logf("gen-0 status after the purge-grace retireVault: %s", statusStr(statusB))
		headB, err := getHeadBlock(d.HiveRPCEndpoint())
		if err != nil {
			t.Fatalf("hive head before the stage-B release unstake: %v", err)
		}
		if _, err := d.ConsensusUnstake(churnNode, "1.000"); err != nil {
			t.Fatalf("consensus_unstake 1.000 (stage B release) broadcast: %v", err)
		}
		waitForBlock(t, d.HiveRPCEndpoint(), headB+8, 3*time.Minute)
		time.Sleep(5 * time.Second)
		pending = pendingConsensusUnstake(t, d, ctx, 2, member)
		releasedAt = statusStr(statusB)
	}
	c.rec("F5-RELEASE", "the SAME consensus_unstake is ACCEPTED once the member's generation is finished (bond released)",
		pending > 0,
		fmt.Sprintf("member=%s amount=1.000 pending: at %s=%d, at %s=%d (want >0); the identical op was refused while gen-0 was retiring and funded",
			member, statusStr(statusA), pendingA, releasedAt, pending))

	// ---- 4. F5-IDENT: every node, including the one that was down, agrees ----
	if h2, err := d.getLastProcessedBlock(ctx, 2); err == nil {
		if !vfWaitProcessed(t, d, ctx, churnNode, h2, 8*time.Minute) {
			t.Logf("magi-%d did not reach magi-2's processed height %d before the identity check", churnNode, h2)
		}
	}
	vfAssertContractIdentical(c, d, ctx, cid, vfAllNodes(5), 4*time.Minute, "F5-IDENT")

	t.Logf("F5 gen-0 final status=%s, member=%s", statusStr(statusB), member)
	c.summary("F5")
}
