package devnet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestVaultF6EpochBoundaryKeygen is the F6 failure-state test of the BTC
// vault-rotation-v2 suite.
//
// WHAT IT REPRODUCES
// On the test config ElectionInterval is 20 blocks and TSS RotateInterval is 20
// blocks, so every keygen session starts on a block that is ALSO an election
// boundary: the block that advances the election epoch is the same block on
// which the TSS tick reads FindNewKeys and fires the keygen action
// (modules/tss/tss.go, the bh%rotateInterval==0 branch). The test deliberately
// lands the gen-1 createKey call as close to that shared boundary as it can,
// so the contract write that mints the key row and the tick that consumes it
// race inside the same block window. If the two nodes disagree about which
// election epoch is current at that instant they build different party lists,
// which is exactly the SSID-mismatch / divergent-commitment failure mode.
//
// WHAT IT PROVES
//  1. F6-KEY: the keygen that fires on the boundary completes and the gen-1 key
//     reaches status active.
//  2. F6-SSID: no node logged an SSID mismatch, so every node built the same
//     party list from the same election epoch across the epoch advance.
//  3. F6-SAMEKEY: all 5 nodes hold the identical gen-1 key (same PublicKey,
//     same Epoch), so the boundary produced ONE key, not a per-node fork.
//  4. F6-EPOCHS: the on-chain keygen commitment read from all 5 nodes carries
//     the identical epoch and block_height, so every node attributes the
//     session to the same epoch and the same boundary block. The block_height
//     is reported against the 20-block rotate interval so the run shows the
//     session really sat on a boundary.
//  5. The key then activates normally (BRK-2 check-signature) and gen-0 drains
//     into gen-1 with a signed migration sweep, so a boundary keygen produces
//     usable key material and not just a matching commitment.
//  6. F6-IDENT: the vault contract state is byte-identical on all 5 nodes at
//     the end.
//
// RUN
//
//	VAULT_F6_RUN=1 go test -v -run TestVaultF6EpochBoundaryKeygen -timeout 95m ./tests/devnet/
//
// Set DEVNET_KEEP=1 to leave the devnet running for post-mortem inspection, and
// BTC_MAPPING_WASM_PATH to point at a different contract build.
func TestVaultF6EpochBoundaryKeygen(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F6_RUN") == "" {
		t.Skip("set VAULT_F6_RUN=1")
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

	// hpin AFTER genesis (~block 190) so gen-0 is minted on the v2-off genesis
	// path (no fresh-genesis deadlock) and v2 is live for the rotation.
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

	// Setup: deploy, seed headers, wire the oracle, mint + register gen-0, fund it.
	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "vault F6 epoch boundary keygen")
	cid := env.cid
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	keyId1 := cid + "-mainv1"
	hive := d.HiveRPCEndpoint()

	// ---- step 1: line the gen-1 createKey up with a rotate/election boundary ----
	// The keygen action only fires on a block that is a multiple of the rotate
	// interval, and with ElectionInterval == RotateInterval == 20 that block is
	// also the election boundary. createKey has to be CONFIRMED before that block
	// or the key row is not there when the tick reads FindNewKeys, so the target
	// boundary is picked at least 8 Hive blocks (about 24s) ahead and the call is
	// issued 3 blocks before it.
	head, err := getHeadBlock(hive)
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: cannot read the Hive head block (%v), so the createKey cannot be aligned with the election boundary and the test would prove nothing", err)
	}
	boundary := nextReshareBoundary(head)
	for boundary < head+8 {
		boundary += testRotateInterval
	}
	submitAt := boundary - 3
	t.Logf("Hive head=%d, target rotate/election boundary=%d (multiple of %d), issuing createKey at block %d",
		head, boundary, testRotateInterval, submitAt)
	waitForBlock(t, hive, submitAt, 6*time.Minute)

	headSubmit, errSubmit := getHeadBlock(hive)
	t.Logf("SUBMIT createKey (gen-1) at Hive head=%d (err=%v), boundary=%d, %d blocks before the boundary",
		headSubmit, errSubmit, boundary, boundary-headSubmit)
	s := vstatus(t, d, ctx, 1, cid, "createKey", "")
	headConfirm, errConfirm := getHeadBlock(hive)
	t.Logf("CONFIRM createKey status=%s at Hive head=%d (err=%v), boundary=%d, delta=%d (negative means the call confirmed BEFORE the boundary, so the keygen fires on it)",
		s, headConfirm, errConfirm, boundary, headConfirm-boundary)
	if !isOK(s) {
		t.Fatalf("PRECONDITION FAILED: createKey (gen-1) status=%s, the keygen request never reached the chain so no keygen ever fires on the epoch boundary", s)
	}

	// Steps 2 to 5 run in a closure so an unrecoverable failure can stop the
	// sequence while F6-IDENT and the summary still run below.
	func() {
		// ---- step 2: F6-KEY, the boundary keygen completes ----
		kd1, err := d.WaitForTssKey(ctx, 2, bson.M{"id": keyId1, "status": "active"}, 10*time.Minute)
		if err != nil {
			all, _ := d.GetTssKeys(ctx, 2, bson.M{})
			for _, k := range all {
				t.Logf("  tss_key id=%s status=%s epoch=%d", k.Id, k.Status, k.Epoch)
			}
			c.rec("F6-KEY", "gen-1 keygen fired on the election boundary completes", false,
				fmt.Sprintf("WaitForTssKey(%s, active) on magi-2: %v (boundary=%d, createKey confirmed at head=%d)",
					keyId1, err, boundary, headConfirm))
			return
		}
		primary1 := kd1.PublicKey
		c.rec("F6-KEY", "gen-1 keygen fired on the election boundary completes", true,
			fmt.Sprintf("keyId=%s epoch=%d pubkey=%s (target boundary=%d)", keyId1, kd1.Epoch, primary1, boundary))

		// ---- step 3: F6-SSID, the epoch advance did not poison the session ----
		// assertNoSSIDMismatch is the suite instrument and reports on its own; the
		// scan below only builds the recorded detail line.
		assertNoSSIDMismatch(t, d, ctx, 5)
		mismatchNodes := ""
		for _, n := range vfAllNodes(5) {
			if _, found := vfLogsContainAny(d, ctx, n, "ssid mismatch"); found {
				mismatchNodes += fmt.Sprintf(" magi-%d", n)
			}
		}
		c.rec("F6-SSID", "no node logged an SSID mismatch across the election epoch advance",
			mismatchNodes == "", fmt.Sprintf("nodes logging \"ssid mismatch\":%q (empty is the pass)", mismatchNodes))

		// ---- step 4: F6-SAMEKEY, one key on every node ----
		sameKey := true
		keyDetail := ""
		for _, n := range vfAllNodes(5) {
			docs, kerr := d.GetTssKeys(ctx, n, bson.M{"id": keyId1})
			if kerr != nil || len(docs) == 0 {
				sameKey = false
				keyDetail += fmt.Sprintf(" magi-%d=UNREADABLE(err=%v,docs=%d)", n, kerr, len(docs))
				continue
			}
			keyDetail += fmt.Sprintf(" magi-%d=(pk=%s,epoch=%d,status=%s)", n, docs[0].PublicKey, docs[0].Epoch, docs[0].Status)
			if docs[0].PublicKey != primary1 || docs[0].Epoch != kd1.Epoch {
				sameKey = false
			}
		}
		c.rec("F6-SAMEKEY", "the boundary keygen produced ONE key: identical PublicKey and Epoch on all 5 nodes",
			sameKey, fmt.Sprintf("reference pk=%s epoch=%d;%s", primary1, kd1.Epoch, keyDetail))

		// ---- step 5: F6-EPOCHS, one commitment, same epoch and block height ----
		type commitObs struct {
			ok     bool
			epoch  uint64
			height uint64
			count  int
			note   string
		}
		obs := map[int]commitObs{}
		commitDeadline := time.Now().Add(4 * time.Minute)
		for {
			allSeen := true
			for _, n := range vfAllNodes(5) {
				docs, cerr := d.GetCommitments(ctx, n, bson.M{"key_id": keyId1, "type": "keygen"})
				if cerr != nil || len(docs) == 0 {
					allSeen = false
					obs[n] = commitObs{note: fmt.Sprintf("err=%v docs=%d", cerr, len(docs))}
					continue
				}
				obs[n] = commitObs{ok: true, epoch: docs[0].Epoch, height: docs[0].BlockHeight, count: len(docs)}
			}
			if allSeen || time.Now().After(commitDeadline) {
				break
			}
			time.Sleep(10 * time.Second)
		}
		var ref commitObs
		haveRef := false
		for _, n := range vfAllNodes(5) {
			if obs[n].ok {
				ref = obs[n]
				haveRef = true
				break
			}
		}
		epochsAgree := haveRef
		commitDetail := ""
		for _, n := range vfAllNodes(5) {
			o := obs[n]
			if !o.ok {
				epochsAgree = false
				commitDetail += fmt.Sprintf(" magi-%d=NONE(%s)", n, o.note)
				continue
			}
			commitDetail += fmt.Sprintf(" magi-%d=(epoch=%d,block_height=%d,rows=%d)", n, o.epoch, o.height, o.count)
			if o.epoch != ref.epoch || o.height != ref.height {
				epochsAgree = false
			}
		}
		onBoundary := haveRef && ref.height%testRotateInterval == 0
		c.rec("F6-EPOCHS", "the keygen commitment carries the same epoch and block_height on all 5 nodes",
			epochsAgree, fmt.Sprintf("reference epoch=%d block_height=%d (on a %d-block rotate/election boundary: %v; target boundary was %d);%s",
				ref.epoch, ref.height, testRotateInterval, onBoundary, boundary, commitDetail))
		if haveRef {
			if members, merr := d.GetElectionMembers(ctx, 2, ref.epoch); merr == nil {
				t.Logf("election epoch %d (the keygen session's epoch) has %d members on magi-2", ref.epoch, len(members))
			} else {
				t.Logf("could not read the members of election epoch %d on magi-2: %v", ref.epoch, merr)
			}
		}

		// ---- step 6: the boundary key is usable, activate then drain gen-0 ----
		vfWaitPreparams(t, d, ctx, 12*time.Minute) // VR2-09: let the post-DKG pre-parameter regeneration finish first
		if !vfRegisterAndActivate(t, d, ctx, cid, primary1, 20) {
			t.Errorf("gen-1 never activated after the boundary keygen (BRK-2 check-signature not admitted): gen-1 status on magi-2=%d, gen-0 status=%d",
				vfVaultStatusOn(d, ctx, 2, cid, 1), vfVaultStatusOn(d, ctx, 2, cid, 0))
			return
		}
		t.Logf("gen-1 activated after the boundary keygen: gen-1 status on magi-2=%d (1=Active), gen-0 status=%d (2=Retiring)",
			vfVaultStatusOn(d, ctx, 2, cid, 1), vfVaultStatusOn(d, ctx, 2, cid, 0))

		gen0Before := genUtxoCount(t, d, ctx, cid, 0)
		if gen0Before <= 0 {
			t.Errorf("gen-0 held %d UTXOs before the sweep, so the drain proves nothing about the boundary key material", gen0Before)
			return
		}
		fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
		migrateAndSettle(t, d, ctx, cid, cid+"-main", primary1, backupPubKeyG)
		gen0After := genUtxoCount(t, d, ctx, cid, 0)
		if gen0After < 0 || gen0After >= gen0Before {
			t.Errorf("gen-0 did NOT drain into the boundary-keygen gen-1: UTXO count %d -> %d, gen-1 count=%d",
				gen0Before, gen0After, genUtxoCount(t, d, ctx, cid, 1))
		} else {
			t.Logf("gen-0 drained into the boundary-keygen gen-1: UTXO count %d -> %d, gen-1 count=%d",
				gen0Before, gen0After, genUtxoCount(t, d, ctx, cid, 1))
		}
	}()

	// ---- step 7: F6-IDENT, the boundary keygen forked nothing ----
	vfAssertContractIdentical(c, d, ctx, cid, vfAllNodes(5), 4*time.Minute, "F6-IDENT")
	c.summary("F6")
}
