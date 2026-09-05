package devnet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"vsc-node/lib/btcvault"
)

// TestVaultF4FullRestartPendingSweep proves that pending migration state (the "ms-"
// record and the "d-" signing data) survives a full network restart and that the sweep
// still signs, broadcasts and settles afterwards. This is the EW-RESTART scenario from
// the July plan that was never built: every earlier vault test restarted the fleet only
// while the contract had nothing in flight, so nothing ever proved that an unsigned,
// unbroadcast migration sweep is durable across a cold fleet.
//
// Shape of the run:
//  1. vfSetup mints and funds gen-0 while v2 is still off (hpin is after genesis), then
//     vfWaitV2On plus vfPreconditionV2Active prove the rotation gates are really live.
//  2. vfRotate mints and activates gen-1, so gen-0 is Retiring, and fundFeeReserve seeds
//     FeeSupply so migrateVault can pay the sweep fee.
//  3. vfBuildSweep issues migrateVault and captures the new pending spend plus its
//     signing data. Signatures are deliberately NOT awaited: the sweep must be caught in
//     the exact half-built state (contract record live, TSS signatures absent).
//  4. d.RestartAllMagiNodes stops and starts every magi container. All five nodes replay
//     from their own Mongo, so the sweep only survives if it was committed to contract
//     state rather than held in node memory.
//  5. F4-RECORD checks the "ms-" record and the decodable "d-" signing data on all five
//     nodes after the restart, F4-SIGN checks the retiring key still signs the request at
//     a later sign interval, F4-SETTLE checks the broadcast sweep confirms and drains
//     gen-0, and F4-IDENT checks all five nodes hold byte-identical vault state.
//
// VAULT_F4_RUN=1 DEVNET_KEEP=1 go test -v -run TestVaultF4FullRestartPendingSweep -timeout 95m ./tests/devnet/
func TestVaultF4FullRestartPendingSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F4_RUN") == "" {
		t.Skip("set VAULT_F4_RUN=1")
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

	// v2 activation AFTER genesis (~block 190) so gen-0 mints on the v2-off path and
	// the rotation itself runs with the v2 gates live.
	const hpin = 400
	cfg := tssTestConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 85*time.Minute)

	c := &vfCase{t: t}

	// Setup: gen-0 minted, registered and funded with 50,000,000 sats.
	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "vault F4 full restart pending sweep")
	cid := env.cid
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	// Rotate to gen-1 so gen-0 is Retiring and a migration sweep is legal.
	primary1, rotated := vfRotate(t, d, ctx, cid, 1)
	if !rotated {
		t.Fatalf("PRECONDITION FAILED: gen-0 to gen-1 rotation did not complete (primary1=%q, gen-0 status=%d), there is no retiring generation to sweep", primary1, vfVaultStatusOn(d, ctx, 2, cid, 0))
	}
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)

	// Step 1: build the sweep and STOP. No signatures are awaited, so the restart lands
	// on a sweep that exists only as committed contract state.
	txid, sd, mstatus := vfBuildSweep(t, d, ctx, 1, cid)
	if txid == "" || sd == nil {
		t.Fatalf("PRECONDITION FAILED: no pending migration sweep to restart on (txid=%q sd=%v migrateVault status=%s)", txid, sd, mstatus)
	}
	gen0Before := vfGenUtxoCountOn(d, ctx, 2, cid, 0)
	t.Logf("pending sweep txid=%s inputs=%d gen-0 utxos=%d (signatures deliberately NOT awaited)", txid, len(sd.UnsignedSigHashes), gen0Before)

	// Step 2: full network restart.
	processedBefore, err := d.getLastProcessedBlock(ctx, 2)
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: cannot read magi-2 processed height before the restart: %v", err)
	}
	t.Logf("restarting ALL %d magi nodes with sweep %s still pending (magi-2 processed=%d)", cfg.Nodes, txid, processedBefore)
	if err := d.RestartAllMagiNodes(ctx); err != nil {
		t.Fatalf("PRECONDITION FAILED: full network restart: %v", err)
	}
	if !vfWaitProcessed(t, d, ctx, 2, processedBefore+5, 8*time.Minute) {
		t.Errorf("magi-2 never processed past %d within 8m after the full restart (the fleet did not resume)", processedBefore+5)
	}

	// Step 3, F4-RECORD: the sweep record and its signing data are still there, on every
	// node. Retried for up to 3 minutes so a slow GQL warm-up on one node cannot be
	// mistaken for lost state.
	nodes := vfAllNodes(cfg.Nodes)
	recOK := false
	recDetail := ""
	recDeadline := time.Now().Add(3 * time.Minute)
	for {
		allOK := true
		recDetail = ""
		for _, n := range nodes {
			msLive := vfSweepRecordOn(d, ctx, n, cid, txid)
			sdBytes := 0
			sdDecodes := false
			if stx, err := getStateHex(d, ctx, n, cid, []string{"d-" + txid}); err == nil {
				raw := stx["d-"+txid]
				sdBytes = len(raw)
				if len(raw) > 0 {
					if _, err := btcvault.DecodeSigningData(raw); err == nil {
						sdDecodes = true
					}
				}
			}
			if !msLive || !sdDecodes {
				allOK = false
			}
			recDetail += fmt.Sprintf(" magi-%d(ms=%v d=%v/%dB)", n, msLive, sdDecodes, sdBytes)
		}
		if allOK {
			recOK = true
			break
		}
		if time.Now().After(recDeadline) {
			break
		}
		time.Sleep(10 * time.Second)
	}
	sdAfter := waitSigningData(t, d, ctx, cid, txid)
	if sdAfter == nil {
		recOK = false
	}
	recDetail += fmt.Sprintf(" waitSigningData(magi-2)=%v", sdAfter != nil)
	c.rec("F4-RECORD", "pending migration state (ms- record + d- signing data) survives a full network restart", recOK, "txid="+txid+recDetail)

	// Step 4, F4-SIGN: the retiring key still signs the request that was raised before
	// the restart. Retried so a missed sign interval (10 blocks) is not read as a refusal.
	rawHex := ""
	signed := false
	for attempt := 1; attempt <= 3 && !signed; attempt++ {
		rawHex, signed = vfAwaitSweepSignatures(t, d, ctx, cid+"-main", sd)
		if !signed {
			t.Logf("sweep %s not fully signed on attempt %d, waiting for the next sign interval", txid, attempt)
		}
	}
	c.rec("F4-SIGN", "restarted committee signs the sweep whose request predates the restart", signed,
		fmt.Sprintf("txid=%s inputs=%d witnessed_tx=%d bytes", txid, len(sd.UnsignedSigHashes), len(rawHex)/2))

	// Step 5, F4-SETTLE: broadcast, mine, relay the header, confirmSpend, gen-0 drains.
	if !signed {
		c.rec("F4-SETTLE", "restart-surviving sweep confirms on chain and drains gen-0", false,
			"not reached: the sweep never collected a full witness after the restart")
	} else {
		bcTxid, h, err := vfBroadcastAndMine(t, d, ctx, rawHex)
		if err != nil {
			c.rec("F4-SETTLE", "restart-surviving sweep confirms on chain and drains gen-0", false,
				fmt.Sprintf("broadcast rejected: %v", err))
		} else {
			cs := vfRelayAndConfirm(t, d, ctx, 1, cid, bcTxid, h)
			gen0After := -1
			msGone := false
			settleDeadline := time.Now().Add(3 * time.Minute)
			for {
				gen0After = vfGenUtxoCountOn(d, ctx, 2, cid, 0)
				msGone = !vfSweepRecordOn(d, ctx, 2, cid, txid)
				if gen0After == 0 {
					break
				}
				if time.Now().After(settleDeadline) {
					break
				}
				time.Sleep(10 * time.Second)
			}
			c.rec("F4-SETTLE", "restart-surviving sweep confirms on chain and drains gen-0", isOK(cs) && gen0After == 0,
				fmt.Sprintf("confirmSpend=%s gen0_utxos=%d (was %d) ms_record_cleared=%v bcTxid=%s height=%d",
					cs, gen0After, gen0Before, msGone, bcTxid, h))
		}
	}

	// Step 6: no node forked over the restart.
	vfAssertContractIdentical(c, d, ctx, cid, nodes, 4*time.Minute, "F4-IDENT")
	c.summary("F4")
	t.Logf("F4 COMPLETE CONTRACT=%s sweep=%s", cid, txid)
}
