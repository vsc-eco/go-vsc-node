//go:build evm_devnet

package devnet

// I-40 (testnet 11-05): a mined BTC migration sweep must not read as
// insolvency. On testnet the gen-13 -> gen-14 sweep was mined, the witnesses'
// next solvency checkpoint found its 5 confirmed inputs spent on Bitcoin with
// no "ru-" mark (migration keeps confirmed inputs in its "ms-" record), 2/3
// voted insolvent and BTC vault keysigns halted until the sweep settles.
//
// The harness settles a sweep itself (vfRelayAndConfirm), so earlier devnet
// rotations settled right after mining and no checkpoint fell in between. Here
// the sweep is mined and its settle is held across two checkpoints. With the
// fix no node halts and no witness logs missing coins; on the pre-fix watch
// every node halts (the A/B arm A). The watch must actually run at those
// checkpoints (not abstain), or the run is vacuous.
//
// Tagged evm_devnet to share n4SolvencyHalted with the N4 test.

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"vsc-node/modules/common/params"
)

func TestVaultI40_MinedMigrationSweepDoesNotHaltBtcSigning(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	requireDocker(t)
	ctx, cancel := devnetWithTimeout(context.Background(), vfTestBudget(110*time.Minute))
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if _, err := os.Stat(wasm); err != nil {
		t.Fatalf("BTC_MAPPING_WASM_PATH: %v", err)
	}
	const hpin = 400
	cfg := vfSlowReshareConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	// 0.13 from epoch 1: the watch is on (as on testnet).
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = 13
	// The watch logs at info/warn; devnet logs only errors by default, and a
	// solvent check casts no vote, so without its lines a watch that never ran
	// looks the same as one that saw a solvent book (11-05: arm A's counts were
	// blind). "checkpoint observed" is the positive proof it looked.
	cfg.LogLevel = "error,tss=trace,oracle=info"
	d, _ := startDevnetNoKey(t, cfg, 110*time.Minute)
	nodes := vfAllNodes(d.cfg.Nodes)

	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "I-40 migration sweep vs the solvency watch")
	cid := env.cid
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)
	if n, v := n4SolvencyHalted(t, ctx, d, nodes); n != 0 {
		t.Fatalf("PRECONDITION: solvency halt set before the rotation (%d nodes; %s)", n, v)
	}

	primary1, rotated := vfRotate(t, d, ctx, cid, 1)
	if !rotated {
		t.Fatalf("PRECONDITION: gen-0 -> gen-1 rotation did not complete (primary1=%q)", primary1)
	}
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
	vfDumpRegistry(t, d, ctx, 1, cid, "before the sweep")

	sweepTxid, sd, st := vfBuildSweep(t, d, ctx, 1, cid)
	if sd == nil {
		t.Fatalf("PRECONDITION: migrateVault built no sweep (status=%s txid=%q)", st, sweepTxid)
	}
	// The I-40 shape: the sweep spends confirmed-pool coins that carry no "ru-".
	recs, err := getStateHex(d, ctx, 1, cid, []string{"ms-" + sweepTxid, "msl"})
	if err != nil {
		t.Fatalf("read the sweep record: %v", err)
	}
	rec := recs["ms-"+sweepTxid]
	if len(rec) < 18 {
		t.Fatalf("PRECONDITION: no ms- record for sweep %s (%d bytes)", sweepTxid, len(rec))
	}
	n := int(binary.BigEndian.Uint16(rec[16:]))
	var confirmed []uint16
	for i := 0; i < n && 18+2*i+2 <= len(rec); i++ {
		if id := binary.BigEndian.Uint16(rec[18+2*i:]); id >= 1024 {
			confirmed = append(confirmed, id)
		}
	}
	if len(confirmed) == 0 {
		t.Fatalf("VACUOUS: the sweep spends no confirmed-pool coin (inputs %d); the watch never checks unconfirmed ones", n)
	}
	for _, id := range confirmed {
		k := "ru-" + strconv.FormatUint(uint64(id), 10)
		if r, _ := getStateHex(d, ctx, 1, cid, []string{k}); len(r[k]) > 0 {
			t.Fatalf("VACUOUS: confirmed input %d carries %s; the contract now reserves sweep inputs and I-40 cannot occur", id, k)
		}
	}
	t.Logf("sweep %s spends %d input(s), %d confirmed without ru- marks %v (msl %d bytes)", sweepTxid, n, len(confirmed), confirmed, len(recs["msl"]))

	raw, ok := vfAwaitSweepSignatures(t, d, ctx, cid+"-main", sd)
	if !ok {
		t.Fatal("PRECONDITION: the sweep was not signed")
	}
	const abstain, missingLog, observed = "BTC solvency watch: abstaining", "spent on Bitcoin without a reservation", "BTC solvency watch: checkpoint observed"
	abstain0, missing0, observed0 := map[int]int{}, map[int]int{}, map[int]int{}
	for _, node := range nodes {
		abstain0[node] = vfCountLogs(d, ctx, node, abstain)
		missing0[node] = vfCountLogs(d, ctx, node, missingLog)
		observed0[node] = vfCountLogs(d, ctx, node, observed)
	}
	bcTxid, bh, err := vfBroadcastAndMine(t, d, ctx, raw)
	if err != nil {
		t.Fatalf("broadcast the sweep: %v", err)
	}
	h0, _ := d.getLastProcessedBlock(ctx, 1)
	every := params.BTC_SOLVENCY_CHECK_EVERY
	target := (h0/every+2)*every + 40 // two checkpoints after the mine, plus time for the votes
	t.Logf("sweep %s mined at BTC height %d (Hive %d); holding the settle until Hive %d (two checkpoints)", bcTxid, bh, h0, target)

	maxHalted, lastVotes := 0, ""
	for deadline := devnetNow().Add(40 * time.Minute); devnetNow().Before(deadline); devnetSleep(20 * time.Second) {
		if k, v := n4SolvencyHalted(t, ctx, d, nodes); k > maxHalted {
			maxHalted, lastVotes = k, v
		}
		if h, _ := d.getLastProcessedBlock(ctx, 1); h >= target {
			break
		}
	}
	if h, _ := d.getLastProcessedBlock(ctx, 1); h < target {
		t.Fatalf("Hive reached only %d of %d: the checkpoints were not passed", h, target)
	}
	abstained, missingSeen, looked := 0, 0, 0
	for _, node := range nodes {
		if vfCountLogs(d, ctx, node, abstain)-abstain0[node] >= 2 {
			abstained++
		}
		if vfCountLogs(d, ctx, node, missingLog) > missing0[node] {
			missingSeen++
		}
		if vfCountLogs(d, ctx, node, observed)-observed0[node] >= 2 {
			looked++
		}
	}
	t.Logf("I-40 window: max halted %d/%d (%s); nodes that observed both checkpoints %d, abstained at both %d, logged missing coins %d",
		maxHalted, len(nodes), lastVotes, looked, abstained, missingSeen)
	if abstained*3 > len(nodes) || looked*3 < len(nodes)*2 {
		t.Fatalf("VACUOUS: %d of %d nodes observed both checkpoints (%d abstained); the watch did not look", looked, len(nodes), abstained)
	}
	if maxHalted > 0 || missingSeen > 0 {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("I-40: the mined migration sweep read as insolvency: halted on %d/%d nodes, %d node(s) logged its inputs as missing (%s)",
			maxHalted, len(nodes), missingSeen, lastVotes)
	}

	// Settle the sweep; nothing halts afterwards either.
	cs := vfRelayAndConfirm(t, d, ctx, 1, cid, bcTxid, bh)
	if k, v := n4SolvencyHalted(t, ctx, d, nodes); k != 0 {
		t.Fatalf("halted after the settle (%d nodes; %s)", k, v)
	}
	t.Logf("I-40 COMPLETE: sweep mined, held two checkpoints with no halt, settled (%s)", fmt.Sprint(cs))
}
