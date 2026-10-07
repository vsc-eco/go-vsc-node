package devnet

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common/consensusversion"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Devnet scenarios for the review fixes to the POA 0.9.0 batch. Each runs at
// the 0.9.0 floor; the same file on a tree without the fixes shows the finding.

// reviewLatestEpoch is the highest election epoch a node has stored.
func reviewLatestEpoch(ctx context.Context, d *Devnet, node int) uint64 {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return 0
	}
	defer client.Disconnect(ctx)
	var e struct {
		Epoch uint64 `bson:"epoch"`
	}
	if err := client.Database(d.nodeDbName(node)).Collection("elections").FindOne(ctx, bson.M{},
		options.FindOne().SetSort(bson.M{"epoch": -1})).Decode(&e); err != nil {
		return 0
	}
	return e.Epoch
}

// TestPoaCorruptSeatRowHoldsNotHalts (#252 review, decode errors): a seat row
// that does not decode is the same on every node. The exit-halt used to count
// the decode error as a transient read and retry forever, so block processing
// stopped on every node for as long as the row stayed bad. Now the bond is
// held and the unstake refused, and blocks keep coming. The row is restored
// before the next election so nothing else reads it.
func TestPoaCorruptSeatRowHoldsNotHalts(t *testing.T) {
	d, ctx, floor := poaFixDevnet(t, 0, false, 45*time.Minute)
	const x = 2
	acct := d.witnessAccount(x)
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)
	var orig struct {
		LastSeatedHeight uint64 `bson:"last_seated_height"`
	}
	if err := client.Database(d.nodeDbName(1)).Collection("poa_seats").FindOne(ctx, bson.M{"account": acct}).Decode(&orig); err != nil {
		t.Fatalf("seat of %s: %v", acct, err)
	}

	// Start right after an election lands, so the window closes before the next one.
	ep0 := reviewLatestEpoch(ctx, d, 1)
	for tries := 0; ; tries++ {
		if ep := reviewLatestEpoch(ctx, d, 1); ep > ep0 {
			break
		}
		if tries > 100 {
			t.Fatalf("no election after %d", ep0)
		}
		time.Sleep(2 * time.Second)
	}
	setSeat := func(v any) {
		for n := 1; n <= 5; n++ {
			if _, err := client.Database(d.nodeDbName(n)).Collection("poa_seats").UpdateOne(ctx,
				bson.M{"account": acct}, bson.M{"$set": bson.M{"last_seated_height": v}}); err != nil {
				t.Fatalf("seat row on magi-%d: %v", n, err)
			}
		}
	}
	before := make([]int, 6)
	for n := 1; n <= 5; n++ {
		before[n], _ = d.pfMaxSlotHeight(ctx, n)
	}
	setSeat("not-a-height")
	corruptedAt := time.Now()
	txId, err := d.ledgerOp("vsc.consensus_unstake", acct, acct, "1.000", "hive", "")
	if err != nil {
		setSeat(orig.LastSeatedHeight)
		t.Fatalf("unstake broadcast: %v", err)
	}
	t.Logf("seat row of %s corrupted on all nodes; unstake %s sent", acct, txId)
	// Keep the row bad until the unstake is decided and every node has stored
	// a later L2 block (or 45 s), then restore it before the next election's
	// anchor (20 blocks after the last one).
	status := ""
	after := make([]int, 6)
	for time.Since(corruptedAt) < 45*time.Second {
		time.Sleep(2 * time.Second)
		if s, err := d.FindTransactionStatus(ctx, 1, txId); err == nil && s != "" {
			status = s
		}
		moved := 0
		for n := 1; n <= 5; n++ {
			after[n], _ = d.pfMaxSlotHeight(ctx, n)
			if after[n] > before[n] {
				moved++
			}
		}
		if (status == "FAILED" || status == "CONFIRMED") && moved == 5 {
			break
		}
	}
	setSeat(orig.LastSeatedHeight)
	t.Logf("row restored after %s; unstake status during the window: %q", time.Since(corruptedAt).Round(time.Second), status)

	stalled, held := 0, 0
	for n := 1; n <= 5; n++ {
		out, _ := exec.CommandContext(ctx, "bash", "-c", fmt.Sprintf("docker logs %s 2>&1 | grep -cE 'halting slot until DB recovers.*poaExitHalt|poaExitHalt.*halting slot'", d.containerName(n))).CombinedOutput()
		s := strings.TrimSpace(string(out))
		out2, _ := exec.CommandContext(ctx, "bash", "-c", fmt.Sprintf("docker logs %s 2>&1 | grep -c 'did not decode; HOLDING'", d.containerName(n))).CombinedOutput()
		h := strings.TrimSpace(string(out2))
		t.Logf("magi-%d: L2 slot %d -> %d in the window, retry-loop log lines %s, decode-hold log lines %s", n, before[n], after[n], s, h)
		if s != "0" {
			stalled++
		}
		if h != "0" {
			held++
		}
	}
	if stalled > 0 {
		t.Errorf("DECODE STALL (floor %d): %d of 5 nodes retried the corrupt seat row forever (block processing stopped while it was bad)", floor, stalled)
	}
	if held == 0 {
		t.Errorf("no node logged the decode hold: the unstake did not reach the exit-halt read")
	}
	if status != "FAILED" {
		t.Errorf("the unstake was not refused while the row was bad (status %q, want FAILED)", status)
	}
	advanced := 0
	for n := 1; n <= 5; n++ {
		if after[n] > before[n] {
			advanced++
		}
	}
	if advanced < 5 {
		t.Errorf("only %d of 5 nodes stored a new L2 block while the row was bad", advanced)
	}
}

// mongoQueryCount is the Mongo server's cumulative query + getmore counter
// (all node databases share one server on the devnet).
func mongoQueryCount(t *testing.T, d *Devnet, ctx context.Context) int64 {
	t.Helper()
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)
	var st struct {
		Opcounters struct {
			Query   int64 `bson:"query"`
			Getmore int64 `bson:"getmore"`
		} `bson:"opcounters"`
	}
	if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "serverStatus", Value: 1}}).Decode(&st); err != nil {
		t.Fatalf("serverStatus: %v", err)
	}
	return st.Opcounters.Query + st.Opcounters.Getmore
}

// logCount counts log lines matching an extended regex on one node.
func logCount(ctx context.Context, d *Devnet, node int, re string) string {
	out, _ := exec.CommandContext(ctx, "bash", "-c", fmt.Sprintf("docker logs %s 2>&1 | grep -cE '%s'", d.containerName(node), re)).CombinedOutput()
	return strings.TrimSpace(string(out))
}

// TestPoa1LockOnDevnet (#252 review, POA-1): the POA-1 bond lock on a live
// devnet with a v2 BTC vault registry.
//
//   - LOCK: a party of the Active generation with no seat unstakes: refused, with
//     the corrected refusal text ("not yet purged", not "still holds funds").
//
//   - LOAD: 800 keygen/reshare rows on the generation (distinct epochs, as on a
//     long-lived testnet key) and 40 unstakes from accounts without a seat to
//     accounts that hold nothing. Before the fix every unstake re-read every row and one
//     election per epoch on every node; now once per block. Measured as Mongo
//     query counts against an idle baseline of the same length.
//
//   - DECODE: one of those rows stops decoding on every node; the next such
//     unstake is refused and blocks keep coming (before: every node retried
//     the read until the row was fixed).
//
//     BTC_MAPPING_WASM_PATH=... go test -v -run TestPoa1LockOnDevnet -timeout 110m ./tests/devnet/
func TestPoa1LockOnDevnet(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		t.Skip("set BTC_MAPPING_WASM_PATH to the btc-mapping-contract regtest wasm")
	}
	requireDocker(t)
	const hpin = 400
	cfg := vfSlowReshareConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.LogLevel = "error,tss=trace,se=debug"
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.VaultRotationV2ActivationHeight = hpin
	cp.ConsensusVersionFloorMajor = 0
	cp.ConsensusVersionFloorConsensus = poaDevnetFloor()
	cp.ConsensusVersionFloorEpoch = 1
	d, ctx := startDevnetNoKey(t, cfg, 105*time.Minute)

	seedH, _ := d.MineBlocks(ctx, 101)
	hdr1, _ := btcBlockHeaderHex(ctx, d, seedH)
	cid, err := d.DeployContract(ctx, ContractDeployOpts{WasmPath: wasm, Name: "btc-mapping-contract", Description: "poa-1", DeployerNode: 1, GQLNode: 2})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	vstatus(t, d, ctx, 1, cid, "seedBlocks", fmt.Sprintf(`{"block_header":"%s","block_height":%d}`, hdr1, seedH))
	d.WriteOracleConfigs(ctx)
	d.SetOracleContractIDs(map[string]string{"BTC": cid})
	d.RestartAllMagiNodes(ctx)
	time.Sleep(10 * time.Second)

	// v2 first, then a genesis gen-0: the registry holds a single Active
	// generation, so the retiring-member bond lock (#11) cannot apply and POA-1
	// is the only lock a key-share holder without a seat meets.
	vfWaitV2On(t, d, ctx, uint64(hpin))
	vstatus(t, d, ctx, 1, cid, "createKey", "")
	kd0, err := d.WaitForTssKey(ctx, 2, bson.M{"id": cid + "-main", "status": "active"}, 8*time.Minute)
	if err != nil {
		t.Fatalf("gen0 keygen: %v", err)
	}
	vfWaitPreparams(t, d, ctx, 12*time.Minute)
	// At the POA floor (0.9) v2 is in force and registration waits for the key's
	// BRK-2 check-signature, which lands a little after the key is active.
	if s := vfRegisterGenesis(t, d, ctx, 1, cid, fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, kd0.PublicKey, backupPubKeyG)); !isOK(s) {
		t.Fatalf("gen0 register: %s", s)
	}
	st0 := -1
	for i := 0; i < 12 && st0 != 1; i++ {
		if st0 = vaultStatusOf(t, d, ctx, cid, 0); st0 != 1 {
			time.Sleep(10 * time.Second)
		}
	}
	t.Logf("registry: gen0 %s", statusStr(st0))
	if st0 != 1 {
		t.Fatalf("PRECONDITION FAILED: gen0 is not Active (%s)", statusStr(st0))
	}

	// ── LOCK ──
	const x = 3
	xAcct := d.witnessAccount(x)
	var keep []string
	for n := 1; n <= cfg.Nodes; n++ {
		if n != x {
			keep = append(keep, d.witnessAccount(n))
		}
	}
	trimRegistryEverywhere(t, d, ctx, keep, cfg.Nodes)
	lockTx, err := d.ConsensusUnstake(x, "1.000")
	if err != nil {
		t.Fatalf("unstake broadcast: %v", err)
	}
	lockStatus := ""
	for i := 0; i < 36 && lockStatus != "FAILED" && lockStatus != "CONFIRMED"; i++ {
		time.Sleep(5 * time.Second)
		lockStatus, _ = d.FindTransactionStatus(ctx, 1, lockTx)
	}
	newText := logCount(ctx, d, 1, "not yet purged")
	oldText := logCount(ctx, d, 1, "still holds funds")
	if out, err := exec.CommandContext(ctx, "bash", "-c", fmt.Sprintf("docker logs %s 2>&1 | grep -E 'bond is locked' | tail -3", d.containerName(1))).CombinedOutput(); err == nil {
		t.Logf("LOCK: refusal lines on magi-1:\n%s", string(out))
	}
	t.Logf("LOCK: %s (no seat, party of gen0) unstake %s -> %s; refusal text: new=%s old=%s", xAcct, lockTx, lockStatus, newText, oldText)
	if lockStatus != "FAILED" {
		t.Errorf("LOCK: the unstake of a key-share holder was not refused (status %q)", lockStatus)
	}
	if newText == "0" {
		t.Errorf("LOCK: the refusal does not carry the corrected text (\"not yet purged\"); old text lines: %s", oldText)
	}

	// ── LOAD ──
	genKey := cid + "-main"
	var kg struct {
		BlockHeight uint64 `bson:"block_height"`
	}
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)
	if err := client.Database(d.nodeDbName(1)).Collection("tss_commitments").FindOne(ctx,
		bson.M{"key_id": genKey, "type": "keygen"}, options.FindOne().SetSort(bson.M{"block_height": 1})).Decode(&kg); err != nil {
		t.Fatalf("gen0 keygen row: %v", err)
	}
	rowsPerType := min(400, int(kg.BlockHeight)-1)
	var docs []any
	for h := 1; h <= rowsPerType; h++ {
		for i, typ := range []string{"keygen", "reshare"} {
			docs = append(docs, bson.M{"key_id": genKey, "type": typ, "block_height": uint64(h),
				"epoch": uint64(900000 + 2*h + i), "commitment": "AQ", "tx_id": fmt.Sprintf("devnet-load-%d-%d", h, i)})
		}
	}
	for n := 1; n <= cfg.Nodes; n++ {
		if _, err := client.Database(d.nodeDbName(n)).Collection("tss_commitments").InsertMany(ctx, docs); err != nil {
			t.Fatalf("load rows on magi-%d: %v", n, err)
		}
	}
	rows := len(docs)
	perScan := int64(rows * cfg.Nodes) // one read per distinct epoch, on every node
	t.Logf("LOAD: %d rows (distinct epochs, no elections) on %s below its keygen at %d, on all %d nodes", rows, genKey, kg.BlockHeight, cfg.Nodes)

	const window = 60 * time.Second
	q0 := mongoQueryCount(t, d, ctx)
	time.Sleep(window)
	baseline := mongoQueryCount(t, d, ctx) - q0

	const unstakes = 40
	q1 := mongoQueryCount(t, d, ctx)
	start := time.Now()
	var txs []string
	// Signers hold no seat (a seat's own exit-halt would refuse first): initminer
	// and x, five ops each per block (Hive's custom_json limit per account).
	signers := []string{"initminer", xAcct}
	for i := 0; i < unstakes; i++ {
		from := signers[i%len(signers)]
		to := fmt.Sprintf("nobody%d", i%8)
		tx, err := d.ledgerOp("vsc.consensus_unstake", from, to, "0.001", "hive", "")
		if err != nil {
			t.Logf("load unstake %d: %v", i, err)
			continue
		}
		txs = append(txs, tx)
		if i%(5*len(signers)) == 5*len(signers)-1 {
			time.Sleep(3 * time.Second)
		}
	}
	done := 0
	for time.Since(start) < window && done < len(txs) {
		time.Sleep(3 * time.Second)
		done = 0
		for _, tx := range txs {
			if s, _ := d.FindTransactionStatus(ctx, 1, tx); s == "FAILED" || s == "CONFIRMED" {
				done++
			}
		}
	}
	for time.Since(start) < window {
		time.Sleep(time.Second)
	}
	loaded := mongoQueryCount(t, d, ctx) - q1
	extra := loaded - baseline
	t.Logf("LOAD: %d unstakes sent, %d processed within %s; Mongo reads in %s: %d (idle baseline %d, extra %d = %.1f full scans; one scan = %d reads)",
		len(txs), done, window, window, loaded, baseline, extra, float64(extra)/float64(perScan), perScan)
	if done < len(txs) {
		t.Errorf("LOAD: only %d of %d unstakes were processed within %s", done, len(txs), window)
	}
	if extra > 10*perScan {
		t.Errorf("LOAD: %d unstakes cost %.1f full scans: the party set is re-read per unstake, not once per block", len(txs), float64(extra)/float64(perScan))
	}

	// ── DECODE ──
	coll := func(n int) *mongo.Collection { return client.Database(d.nodeDbName(n)).Collection("tss_commitments") }
	badFilter := bson.M{"key_id": genKey, "tx_id": "devnet-load-1-0"}
	for n := 1; n <= cfg.Nodes; n++ {
		if _, err := coll(n).UpdateOne(ctx, badFilter, bson.M{"$set": bson.M{"epoch": "not-a-number"}}); err != nil {
			t.Fatalf("corrupt row on magi-%d: %v", n, err)
		}
	}
	before := make([]int, cfg.Nodes+1)
	for n := 1; n <= cfg.Nodes; n++ {
		before[n], _ = d.pfMaxSlotHeight(ctx, n)
	}
	decTx, err := d.ledgerOp("vsc.consensus_unstake", "initminer", "nobody99", "0.001", "hive", "")
	if err != nil {
		t.Fatalf("decode unstake: %v", err)
	}
	decStatus := ""
	decStart := time.Now()
	for time.Since(decStart) < 40*time.Second && decStatus != "FAILED" {
		time.Sleep(2 * time.Second)
		decStatus, _ = d.FindTransactionStatus(ctx, 1, decTx)
	}
	after := make([]int, cfg.Nodes+1)
	for n := 1; n <= cfg.Nodes; n++ {
		after[n], _ = d.pfMaxSlotHeight(ctx, n)
	}
	for n := 1; n <= cfg.Nodes; n++ {
		coll(n).UpdateOne(ctx, badFilter, bson.M{"$set": bson.M{"epoch": uint64(900002)}})
	}
	stalled, held, advanced := 0, 0, 0
	for n := 1; n <= cfg.Nodes; n++ {
		s := logCount(ctx, d, n, "halting slot until DB recovers.*heldShareOfFundedVault|heldShareOfFundedVault.*halting slot")
		h := logCount(ctx, d, n, "poa-1: a stored row did not decode")
		t.Logf("DECODE magi-%d: L2 slot %d -> %d, retry-loop lines %s, decode-hold lines %s", n, before[n], after[n], s, h)
		if s != "0" {
			stalled++
		}
		if h != "0" {
			held++
		}
		if after[n] > before[n] {
			advanced++
		}
	}
	t.Logf("DECODE: unstake %s -> %q", decTx, decStatus)
	if stalled > 0 {
		t.Errorf("DECODE: %d of %d nodes retried the undecodable row forever", stalled, cfg.Nodes)
	}
	if held == 0 || decStatus != "FAILED" {
		t.Errorf("DECODE: the unstake was not held and refused (status %q, nodes logging the hold: %d)", decStatus, held)
	}
	if advanced < cfg.Nodes {
		t.Errorf("DECODE: only %d of %d nodes stored a new L2 block while the row was bad", advanced, cfg.Nodes)
	}
}

// TestPoaFloorRiseCountsTopUps (#252 review, readiness after a top-up epoch):
// needs a build that announces 0.10 (currentConsensus = 10 in
// modules/common/consensusversion/version.go), so every node is ready for a
// floor rise to 0.10. Epoch E1 is a top-up committee: seat magi.test1 plus
// magi.test2 and magi.test3 (previous-committee members, no seat). Then
// magi.test4 and magi.test5 get their seats back, so the next election is
// gated to {1, 4, 5}, and a rise to 0.10 is proposed for E1+1. Before the fix
// the outgoing-committee check read versions from the seats-only list, so the
// two top-ups counted as not ready and the rise waited until E1+2.
func TestPoaFloorRiseCountsTopUps(t *testing.T) {
	if consensusversion.RunningVersion().Consensus < 10 {
		t.Skip("needs a build announcing consensus 0.10 (bump currentConsensus in version.go)")
	}
	d, ctx, _ := poaFixDevnet(t, 0, false, 60*time.Minute)
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)
	accts := make([]string, 6)
	for n := 1; n <= 5; n++ {
		accts[n] = d.witnessAccount(n)
	}
	saved := make(map[string]bson.M)
	for _, a := range []string{accts[4], accts[5]} {
		var row bson.M
		if err := client.Database(d.nodeDbName(1)).Collection("poa_seats").FindOne(ctx, bson.M{"account": a}).Decode(&row); err != nil {
			t.Fatalf("seat of %s: %v", a, err)
		}
		delete(row, "_id")
		saved[a] = row
	}
	waitLanded := func(after uint64) uint64 {
		for tries := 0; tries < 200; tries++ {
			if ep := reviewLatestEpoch(ctx, d, 1); ep > after {
				return ep
			}
			time.Sleep(2 * time.Second)
		}
		t.Fatalf("no election after epoch %d", after)
		return 0
	}
	line := func() string {
		l, _, _, _ := d.ConsensusInfo(ctx, 1)
		return l
	}

	ea := waitLanded(reviewLatestEpoch(ctx, d, 1))
	trimRegistryEverywhere(t, d, ctx, []string{accts[1]}, 5)
	t.Logf("epoch %d landed; registry trimmed to %s", ea, accts[1])
	e1 := waitLanded(ea)
	m1, _ := d.GetElectionMembers(ctx, 1, e1)
	t.Logf("E1 = epoch %d members %v (line %s)", e1, bareAccounts(m1), line())
	if got := bareAccounts(m1); len(got) != 3 || !sliceHasAll(got, accts[1], accts[2], accts[3]) {
		// The top-up may land one epoch later if the trim missed E1's anchor.
		e1 = waitLanded(e1)
		m1, _ = d.GetElectionMembers(ctx, 1, e1)
		if got := bareAccounts(m1); len(got) != 3 || !sliceHasAll(got, accts[1], accts[2], accts[3]) {
			t.Fatalf("PRECONDITION FAILED: no top-up committee {1,2,3}: epoch %d members %v", e1, got)
		}
	}
	for n := 1; n <= 5; n++ {
		for _, a := range []string{accts[4], accts[5]} {
			if _, err := client.Database(d.nodeDbName(n)).Collection("poa_seats").InsertOne(ctx, saved[a]); err != nil {
				t.Fatalf("re-seat %s on magi-%d: %v", a, n, err)
			}
		}
	}
	payload := map[string]interface{}{"major": 0, "consensus": 10, "activation_epoch": e1 + 1}
	if _, err := d.BroadcastCustomJSON("vsc.propose_consensus_version", []string{accts[1]}, payload, d.cfg.InitminerWIF); err != nil {
		t.Fatalf("propose 0.10: %v", err)
	}
	t.Logf("seats back for %s and %s; proposed 0.10 for epoch %d", accts[4], accts[5], e1+1)

	e2 := waitLanded(e1)
	time.Sleep(5 * time.Second)
	m2, _ := d.GetElectionMembers(ctx, 1, e2)
	l2 := line()
	t.Logf("E2 = epoch %d members %v, active line %s", e2, bareAccounts(m2), l2)
	e3 := waitLanded(e2)
	time.Sleep(5 * time.Second)
	l3 := line()
	t.Logf("E3 = epoch %d, active line %s", e3, l3)
	if l2 != "0.10" {
		t.Errorf("the floor did not rise at the first election after the top-up epoch (line %s at epoch %d, %s at %d): the top-up members of the outgoing committee counted as not ready", l2, e2, l3, e3)
	}
	if l3 != "0.10" {
		t.Errorf("INCONCLUSIVE: the floor never rose (line %s at epoch %d)", l3, e3)
	}
}

func sliceHasAll(xs []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, x := range xs {
			if x == w {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
