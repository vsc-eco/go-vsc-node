package devnet

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/btcvault"
	"vsc-node/modules/common/params"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// rhUpgradeNode moves one node from the old build to this tree's build: its compose
// entry is rewritten and only its container is recreated. The data directory is a
// bind mount, so the node keeps its database and keys, exactly as an operator
// replacing the binary would.
func (d *Devnet) rhUpgradeNode(ctx context.Context, node int) error {
	keep := make([]int, 0, len(d.cfg.OldCodeNodes))
	for _, n := range d.cfg.OldCodeNodes {
		if n != node {
			keep = append(keep, n)
		}
	}
	d.cfg.OldCodeNodes = keep
	if err := writeNodesOverride(d.cfg, d.devnetDir, d.projectName, d.imageName, d.overrideFile); err != nil {
		return err
	}
	return d.compose(ctx, "up", "-d", "--no-deps", "--force-recreate", fmt.Sprintf("magi-%d", node))
}

// rhLatestElection returns the highest stored election epoch and its protocol
// (consensus) version on one node.
func rhLatestElection(ctx context.Context, d *Devnet, node int) (uint64, uint64, error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer client.Disconnect(ctx)
	var e struct {
		Epoch           uint64 `bson:"epoch"`
		ProtocolVersion uint64 `bson:"protocol_version"`
		BlockHeight     uint64 `bson:"block_height"`
	}
	err = client.Database(d.nodeDbName(node)).Collection("elections").
		FindOne(ctx, bson.M{}, options.FindOne().SetSort(bson.M{"epoch": -1})).Decode(&e)
	return e.Epoch, e.ProtocolVersion, err
}

// rhElection returns one epoch's protocol version and block height on one node.
func rhElection(ctx context.Context, d *Devnet, node int, epoch uint64) (uint64, uint64, error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer client.Disconnect(ctx)
	var e struct {
		ProtocolVersion uint64 `bson:"protocol_version"`
		BlockHeight     uint64 `bson:"block_height"`
	}
	err = client.Database(d.nodeDbName(node)).Collection("elections").
		FindOne(ctx, bson.M{"epoch": epoch}).Decode(&e)
	return e.ProtocolVersion, e.BlockHeight, err
}

// rhBlamesSince lists the blame commitments stored on magi-1 above a height, with the
// accounts each one names.
func rhBlamesSince(t *testing.T, ctx context.Context, d *Devnet, height uint64) []string {
	docs, err := d.GetCommitments(ctx, 1, bson.M{"type": "blame", "block_height": bson.M{"$gt": height}})
	if err != nil {
		t.Logf("reading blame commitments: %v", err)
		return nil
	}
	out := []string{}
	for _, c := range docs {
		members, _ := d.GetElectionMembers(ctx, 1, c.Epoch)
		bits := decodeBitset(t, c.Commitment)
		named := []string{}
		for i, m := range members {
			if bits.Bit(i) == 1 {
				named = append(named, strings.TrimPrefix(m, "hive:"))
			}
		}
		out = append(out, fmt.Sprintf("%s@%d%v err=%v", c.KeyId, c.BlockHeight, named, c.Metadata["error"]))
	}
	return out
}

// rhIssueUnmap is f21IssueLegacyUnmap with the signing key as a parameter: after the
// rotation a withdrawal is signed by the active generation's key, not the legacy one.
func rhIssueUnmap(t *testing.T, d *Devnet, ctx context.Context, cid, keyId string, sats int64) (txid, dest, rawHex string) {
	t.Helper()
	dest, err := d.bitcoinCli(ctx, "getnewaddress")
	if err != nil {
		t.Errorf("getnewaddress: %v", err)
		return "", "", ""
	}
	before := txSpendIds(t, d, ctx, cid)
	if s := vstatus(t, d, ctx, 1, cid, "unmap", fmt.Sprintf(`{"amount":"%d","to":"%s"}`, sats, dest)); !isOK(s) {
		t.Errorf("unmap rejected (status=%s)", s)
		return "", dest, ""
	}
	for i := 0; i < 20 && txid == ""; i++ {
		time.Sleep(3 * time.Second)
		for _, id := range txSpendIds(t, d, ctx, cid) {
			if !contains(before, id) {
				txid = id
				break
			}
		}
	}
	if txid == "" {
		t.Errorf("no pending spend appeared after the unmap")
		return "", dest, ""
	}
	sd := waitSigningData(t, d, ctx, cid, txid)
	if sd == nil {
		t.Errorf("no signing data for %s", txid)
		return txid, dest, ""
	}
	var mtx wire.MsgTx
	if err := mtx.Deserialize(bytes.NewReader(sd.Tx)); err != nil {
		t.Errorf("deserialising %s: %v", txid, err)
		return txid, dest, ""
	}
	for _, uh := range sd.UnsignedSigHashes {
		sig := waitSignature(t, d, ctx, keyId, uh.SigHash)
		if sig == nil {
			t.Logf("%s did not sign input %d of %s", keyId, uh.Index, txid)
			return txid, dest, ""
		}
		signature := append(append([]byte{}, sig...), byte(txscript.SigHashAll))
		mtx.TxIn[uh.Index].Witness = wire.TxWitness{signature, []byte{0x01}, uh.WitnessScript}
	}
	var buf bytes.Buffer
	if err := mtx.BtcEncode(&buf, wire.ProtocolVersion, wire.WitnessEncoding); err != nil {
		t.Logf("encoding %s: %v", txid, err)
		return txid, dest, ""
	}
	return txid, dest, hex.EncodeToString(buf.Bytes())
}

// TestMainnetRehearsal rehearses the mainnet rollout end to end on a 5-node devnet with a
// real regtest bitcoind, in the order mainnet will go through it:
//
//  1. every node runs MAINNET'S BUILD (REHEARSAL_OLD, aa112bc1) at the devnet's default
//     consensus line; the v1 BTC mapping contract is deployed, its legacy key generated
//     and registered, two SPV deposits credited, and a v1 withdrawal signed and left in
//     flight (mainnet today: v1 vault, one active TSS key);
//
//  2. the nodes are upgraded ONE AT A TIME to this tree's build, with the fleet mixed in
//     between; blocks, reshares and signing must carry on and no blame may name anyone;
//
//  3. still below the line, the contract code is updated to v2 (BTC_MAPPING_WASM_PATH),
//     migrate() folds the legacy key into generation 0 and the in-flight v1 withdrawal
//     settles under v2 code;
//
//  4. the floor is raised to 0.9.0 the way operators will do it, with
//     vsc.propose_consensus_version: POA bootstraps from the committee in force before
//     the switch (every seat on every node identical) and the vault rotation rules
//     (0.8.0) come on with it;
//
//  5. key regen + migration: gen-0 rotates to gen-1, gen-0 is swept dry, balances are
//     untouched by the sweep, and a new withdrawal is paid from gen-1;
//
//  6. every node holds byte-identical contract state, and a node with a wiped database
//     replays the whole history (old build blocks included) to the same bytes.
//
//     REHEARSAL_OLD=/home/clauderfly/gvn-mainnet-aa112bc1 \
//     BTC_MAPPING_V1_WASM_PATH=/home/clauderfly/utxo-v1/btc-mapping-contract/bin/dev.wasm \
//     BTC_MAPPING_WASM_PATH=/home/clauderfly/vault-batch2/btc-mapping-contract/bin/dev.wasm \
//     go test -v -run '^TestMainnetRehearsal$' -timeout 240m ./tests/devnet/
func TestMainnetRehearsal(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	oldSrc := os.Getenv("REHEARSAL_OLD")
	v1wasm := os.Getenv("BTC_MAPPING_V1_WASM_PATH")
	v2wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if oldSrc == "" || v1wasm == "" || v2wasm == "" {
		t.Skip("set REHEARSAL_OLD (mainnet's tree), BTC_MAPPING_V1_WASM_PATH and BTC_MAPPING_WASM_PATH")
	}
	for _, p := range []string{oldSrc, v1wasm, v2wasm} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("PRECONDITION FAILED: %s: %v", p, err)
		}
	}
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), vfTestBudget(220*time.Minute))
	defer cancel()

	cfg := vfSlowReshareConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	// Mainnet: no height pin, the vault rotation rules come on with the 0.8.0 line.
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = 0
	cfg.OldCodeSourceDir = oldSrc
	cfg.OldCodeNodes = vfAllNodes(cfg.Nodes)
	cfg.OldCodeSysconfig = true
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 220*time.Minute)
	c := &vfCase{t: t}
	nodes := vfAllNodes(cfg.Nodes)

	for _, n := range nodes {
		if err := d.waitForElectionEpoch(ctx, n, 1, 20*time.Minute); err != nil {
			t.Fatalf("PRECONDITION FAILED: magi-%d never reached epoch 1 on the old build: %v", n, err)
		}
	}
	ep0, ver0, _ := rhLatestElection(ctx, d, 1)
	t.Logf("old build on every node: epoch %d at consensus line 0.%d", ep0, ver0)
	if ver0 >= 8 {
		t.Fatalf("PRECONDITION FAILED: the devnet starts at 0.%d, already at or past the vault rotation line; nothing to rehearse", ver0)
	}

	// -------------------------------------------------------------------------
	// Step 1 (old build): a funded v1 contract and a v1 withdrawal in flight.
	// Same steps as TestVaultF21UpgradePath step 1-2.
	// -------------------------------------------------------------------------
	seedH, err := d.MineBlocks(ctx, 101)
	if err != nil {
		t.Fatalf("mine: %v", err)
	}
	hdr1, err := btcBlockHeaderHex(ctx, d, seedH)
	if err != nil {
		t.Fatalf("hdr: %v", err)
	}
	cid, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: v1wasm, Name: "btc-mapping-contract", Description: "mainnet rehearsal (v1)", DeployerNode: 1, GQLNode: 2,
	})
	if err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	t.Logf("CONTRACT=%s v1wasm=%s v2wasm=%s old=%s", cid, v1wasm, v2wasm, oldSrc)
	if s := vstatus(t, d, ctx, 1, cid, "seedBlocks", fmt.Sprintf(`{"block_header":"%s","block_height":%d}`, hdr1, seedH)); !isOK(s) {
		t.Fatalf("seedBlocks: %s", s)
	}
	d.WriteOracleConfigs(ctx)
	d.SetOracleContractIDs(map[string]string{"BTC": cid})
	d.RestartAllMagiNodes(ctx)
	time.Sleep(10 * time.Second)

	vstatus(t, d, ctx, 1, cid, "createKey", "")
	kd0, err := d.WaitForTssKey(ctx, 2, bson.M{"id": cid + "-main", "status": "active"}, 12*time.Minute)
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: v1 gen-0 keygen never completed on the old build: %v", err)
	}
	primary0 := kd0.PublicKey
	if s := vstatus(t, d, ctx, 1, cid, "registerPublicKey",
		fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, primary0, backupPubKeyG)); !isOK(s) {
		t.Fatalf("PRECONDITION FAILED: v1 registerPublicKey: %s", s)
	}
	owner := "hive:" + fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, 1)
	owner2 := "hive:" + fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, 2)
	fundVaultViaSPV(t, d, ctx, cid, primary0, backupPubKeyG, owner, 30_000_000, seedH)
	fundVaultViaSPV(t, d, ctx, cid, primary0, backupPubKeyG, owner2, 20_000_000, contractLastHeight(t, d, ctx, cid))
	for i := 0; i < 24 && (balanceSats(t, d, ctx, cid, owner2) <= 0 || f21GenUtxoCountOn(d, ctx, 2, cid, 0) < 2); i++ {
		time.Sleep(5 * time.Second)
	}
	balOwnerFunded := balanceSats(t, d, ctx, cid, owner)
	balOwner2Funded := balanceSats(t, d, ctx, cid, owner2)
	c.rec("RH-V1-FUNDED", "on the old build the v1 contract is funded on two accounts",
		balOwnerFunded > 0 && balOwner2Funded > 0,
		fmt.Sprintf("%s=%d sats, %s=%d sats, gen-0 utxos=%d", owner, balOwnerFunded, owner2, balOwner2Funded, f21GenUtxoCountOn(d, ctx, 2, cid, 0)))
	if balOwnerFunded == 0 || balOwner2Funded == 0 {
		c.summary("RH")
		t.Fatalf("PRECONDITION FAILED: the v1 deposits did not credit on the old build")
	}
	const unmapSats = 1_000_000
	unmapTxid, unmapDest, unmapRaw := f21IssueLegacyUnmap(t, d, ctx, cid, unmapSats)
	c.rec("RH-V1-UNMAP-SIGNED", "on the old build a v1 withdrawal is TSS-signed and left in flight", unmapRaw != "",
		fmt.Sprintf("txid=%s dest=%s rawlen=%d", unmapTxid, unmapDest, len(unmapRaw)))

	// -------------------------------------------------------------------------
	// Step 2: rolling upgrade, one node at a time, the fleet mixed in between.
	// -------------------------------------------------------------------------
	upStart, err := d.getLastProcessedBlock(ctx, 1)
	if err != nil {
		t.Fatalf("processed height: %v", err)
	}
	upEpoch, _, _ := rhLatestElection(ctx, d, 1)
	for _, n := range nodes {
		ref := 1
		if n == 1 {
			ref = 2
		}
		if err := d.rhUpgradeNode(ctx, n); err != nil {
			t.Fatalf("upgrading magi-%d: %v", n, err)
		}
		// Let the mixed fleet run for a while: at least one TSS rotate interval.
		time.Sleep(4 * time.Minute)
		h, _ := d.getLastProcessedBlock(ctx, ref)
		if !vfWaitProcessed(t, d, ctx, n, h, 10*time.Minute) {
			got, _ := d.getLastProcessedBlock(ctx, n)
			t.Fatalf("magi-%d did not catch up after its upgrade: %d of %d", n, got, h)
		}
		t.Logf("upgraded magi-%d; now on the new build: %v; head %d", n, nodes[:n], h)
	}
	// One full epoch on the upgraded fleet, so its reshares run without the old build.
	if err := d.waitForElectionEpoch(ctx, 1, upEpoch+2, 30*time.Minute); err != nil {
		t.Fatalf("no election after the upgrade: %v", err)
	}
	time.Sleep(4 * time.Minute)
	upEnd, _ := d.getLastProcessedBlock(ctx, 1)
	reshares, _ := d.GetCommitments(ctx, 1, bson.M{"type": "reshare", "key_id": cid + "-main", "block_height": bson.M{"$gt": upStart}})
	blames := rhBlamesSince(t, ctx, d, upStart)
	c.rec("RH-UPGRADE", "the fleet upgrades one node at a time: the legacy key keeps resharing and nobody is blamed",
		len(reshares) > 0 && len(blames) == 0,
		fmt.Sprintf("blocks %d to %d: %d reshare(s) of %s-main, blames %v", upStart, upEnd, len(reshares), cid, blames))
	vfAssertContractIdentical(c, d, ctx, cid, nodes, 4*time.Minute, "RH-UPGRADE-IDENT")
	_, verUp, _ := rhLatestElection(ctx, d, 1)
	if verUp >= 8 {
		t.Fatalf("PRECONDITION FAILED: the line rose to 0.%d without a proposal; step 3 must run below 0.8.0", verUp)
	}

	// -------------------------------------------------------------------------
	// Step 3 (below the line): contract code update, fold, legacy withdrawal settles.
	// -------------------------------------------------------------------------
	activeV1, err := d.ActiveContract(ctx, 2, cid)
	if err != nil || activeV1 == nil {
		t.Fatalf("PRECONDITION FAILED: cannot read the active contract before the update (err=%v)", err)
	}
	// The deployer needs a storage proof from the nodes and gives up after a short
	// timeout (seen once right after a node restart); retry the whole update.
	queued := false
	for attempt := 1; attempt <= 3 && !queued; attempt++ {
		if err := d.UpdateContract(ctx, ContractUpdateOpts{
			ContractId: cid, WasmPath: v2wasm, Name: "btc-mapping-contract", DeployerNode: 1, GQLNode: 2,
		}); err != nil {
			t.Logf("queueing the v2 code update, attempt %d: %v", attempt, err)
		}
		for i := 0; i < 20 && !queued; i++ {
			if rows, err := d.PendingUpdates(ctx, 2, cid); err == nil && len(rows) > 0 {
				queued = true
				break
			}
			time.Sleep(3 * time.Second)
		}
		if !queued {
			t.Logf("no pending code update after attempt %d", attempt)
			time.Sleep(30 * time.Second)
		}
	}
	pending := f21WaitPendingUpdate(t, d, ctx, 2, cid, 2*time.Minute)
	if pending.Code == activeV1.Code {
		t.Fatalf("PRECONDITION FAILED: the v1 and v2 wasm are the same build")
	}
	if err := d.WaitForBlockProcessing(ctx, 2, pending.ActivationHeight+1, 10*time.Minute); err != nil {
		t.Fatalf("PRECONDITION FAILED: chain never reached the update activation height %d: %v", pending.ActivationHeight, err)
	}
	_, errV2 := d.WaitForActiveCode(ctx, 2, cid, pending.Code, 8*time.Minute)
	if errV2 == nil {
		if _, err := d.WaitForActiveCode(ctx, 1, cid, pending.Code, 6*time.Minute); err != nil {
			t.Logf("magi-1 has not surfaced the new active code yet: %v", err)
		}
	}
	c.rec("RH-UPDATED", "the v2 code becomes active after the timelock", errV2 == nil,
		fmt.Sprintf("v1=%s v2=%s err=%v", activeV1.Code, pending.Code, errV2))
	if errV2 != nil {
		c.summary("RH")
		t.Fatalf("PRECONDITION FAILED: the contract never ran v2 code")
	}
	balOwnerPreFold := balanceSats(t, d, ctx, cid, owner)
	balOwner2PreFold := balanceSats(t, d, ctx, cid, owner2)
	utxosPreFold := f21GenUtxoCountOn(d, ctx, 2, cid, 0)
	migStatus := ""
	for i := 0; i < 3; i++ {
		if migStatus = vstatus(t, d, ctx, 1, cid, "migrate", ""); isOK(migStatus) {
			break
		}
		time.Sleep(15 * time.Second)
	}
	time.Sleep(10 * time.Second)
	post, err := getStateHex(d, ctx, 2, cid, []string{"mv", "va", "vn"})
	if err != nil {
		t.Fatalf("cannot read contract state after migrate: %v", err)
	}
	vaults := vfVaultRegistryOn(d, ctx, 2, cid)
	foldOK := string(post["mv"]) == "2" && len(vaults) == 1 &&
		vaults[0].Generation == 0 && vaults[0].Status == btcvault.VaultStatusActive &&
		strings.EqualFold(hex.EncodeToString(vaults[0].Primary), primary0) &&
		f21GenUtxoCountOn(d, ctx, 2, cid, 0) == utxosPreFold &&
		balanceSats(t, d, ctx, cid, owner) == balOwnerPreFold &&
		balanceSats(t, d, ctx, cid, owner2) == balOwner2PreFold
	c.rec("RH-FOLD", "migrate folds the legacy key into an Active gen-0 without touching funds", foldOK,
		fmt.Sprintf("migrate=%s mv=%q vaults=%d utxos %d", migStatus, string(post["mv"]), len(vaults), utxosPreFold))
	if unmapRaw != "" {
		bcTxid, h, berr := vfBroadcastAndMine(t, d, ctx, unmapRaw)
		if berr != nil {
			c.rec("RH-LEGACY-UNMAP-SETTLES", "the v1-built withdrawal settles under v2 code", false, berr.Error())
		} else {
			changeVout := vfChangeVout(d, ctx, bcTxid, unmapDest)
			cs := vfRelayAndConfirmIndex(t, d, ctx, 1, cid, bcTxid, h, changeVout)
			gone := f21WaitSpendGone(t, d, ctx, cid, unmapTxid, 4*time.Minute)
			c.rec("RH-LEGACY-UNMAP-SETTLES", "the v1-built withdrawal settles under v2 code", gone,
				fmt.Sprintf("bcTxid=%s confirmSpend=%s gone=%v", bcTxid, cs, gone))
		}
	}

	// -------------------------------------------------------------------------
	// Step 4: raise the floor to 0.9.0 with the on-chain proposal.
	// -------------------------------------------------------------------------
	cur, _ := d.currentEpoch(ctx, 1)
	target := cur + 2
	for _, n := range nodes {
		if _, err := d.proposeConsensusVersion(n, 0, 9, target); err != nil {
			t.Logf("propose from magi-%d failed (continuing): %v", n, err)
		}
	}
	t.Logf("proposed 0.9.0 at epoch %d (current %d)", target, cur)
	var riseEpoch, riseHeight uint64
	deadline := time.Now().Add(45 * time.Minute)
	for riseEpoch == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Second)
		last, _, _ := rhLatestElection(ctx, d, 1)
		for e := cur + 1; e <= last; e++ {
			if v, bh, err := rhElection(ctx, d, 1, e); err == nil && v >= 9 {
				riseEpoch, riseHeight = e, bh
				break
			}
		}
	}
	if riseEpoch == 0 {
		_, v, _ := rhLatestElection(ctx, d, 1)
		c.rec("RH-RISE", "the floor rises to 0.9.0 through the proposal", false, fmt.Sprintf("latest election still at 0.%d", v))
		c.summary("RH")
		t.Fatalf("the floor never rose to 0.9.0")
	}
	// Flat weight lands with the first election built under POA; seats are written at the
	// transition. Wait one election past it, on every node.
	for _, n := range nodes {
		if err := d.waitForElectionEpoch(ctx, n, riseEpoch+1, 25*time.Minute); err != nil {
			t.Fatalf("magi-%d never reached epoch %d: %v", n, riseEpoch+1, err)
		}
	}
	prevMembers, _ := d.GetElectionMembers(ctx, 1, riseEpoch-1)
	seats := pfMustSeats(t, d, ctx, 1)
	fp := pfSeatFingerprint(seats)
	seatsSame := true
	for _, n := range nodes[1:] {
		if other := pfSeatFingerprint(pfMustSeats(t, d, ctx, n)); other != fp {
			seatsSame = false
			t.Logf("magi-%d registry differs: %s", n, other)
		}
	}
	seated := map[string]bool{}
	for _, s := range seats {
		seated[s.Account] = true
	}
	allSeated := len(prevMembers) > 0
	for _, m := range prevMembers {
		if !seated[strings.TrimPrefix(m, "hive:")] {
			allSeated = false
		}
	}
	el1, _ := d.GetElectionGQL(ctx, 1, riseEpoch+1)
	flat := el1 != nil && len(el1.Weights) > 0
	if el1 != nil {
		for _, w := range el1.Weights {
			if w != params.PoaSeatWeight {
				flat = false
			}
		}
	}
	c.rec("RH-RISE-POA", "the floor rises to 0.9.0; POA seats the pre-switch committee identically on every node and weights go flat",
		seatsSame && allSeated && flat,
		fmt.Sprintf("0.9.0 at epoch %d (block %d); pre-switch committee %v; registry %s; epoch %d weights %v",
			riseEpoch, riseHeight, prevMembers, fp, riseEpoch+1, func() []uint64 {
				if el1 == nil {
					return nil
				}
				return el1.Weights
			}()))

	// -------------------------------------------------------------------------
	// Step 5: key regen and migration under the 0.8.0 vault rules.
	// -------------------------------------------------------------------------
	for _, n := range nodes {
		vfWaitProcessed(t, d, ctx, n, riseHeight+5, 6*time.Minute)
	}
	balOwnerPreSweep := balanceSats(t, d, ctx, cid, owner)
	balOwner2PreSweep := balanceSats(t, d, ctx, cid, owner2)
	primary1, rotated := vfRotate(t, d, ctx, cid, 1)
	c.rec("RH-ROTATE", "gen-0 rotates to gen-1 after the rise", rotated,
		fmt.Sprintf("gen-1 primary=%s, gen-0 status=%d, gen-1 status=%d",
			primary1, vfVaultStatusOn(d, ctx, 2, cid, 0), vfVaultStatusOn(d, ctx, 2, cid, 1)))
	if !rotated {
		// Explain a keygen that never landed: the key rows, the commitments for it, and
		// each node's recent TSS log lines.
		keyId := cid + "-" + btcvault.VaultKeyName(1)
		for _, n := range nodes {
			ks, _ := d.GetTssKeys(ctx, n, bson.M{"id": keyId})
			for _, k := range ks {
				t.Logf("DIAG magi-%d tss_key %s status=%s", n, k.Id, k.Status)
			}
		}
		cs, _ := d.GetCommitments(ctx, 1, bson.M{"key_id": keyId})
		for _, cm := range cs {
			t.Logf("DIAG commitment %s %s@%d epoch=%d meta=%v", cm.Type, cm.KeyId, cm.BlockHeight, cm.Epoch, cm.Metadata)
		}
		for _, n := range nodes {
			d.dumpTssLogs(ctx, t, n)
		}
	}
	left := -1
	if rotated {
		fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
		left = f21GenUtxoCountOn(d, ctx, 2, cid, 0)
		for i := 0; i < 5 && left > 0; i++ {
			migrateAndSettle(t, d, ctx, cid, cid+"-main", primary1, backupPubKeyG)
			next := f21WaitGenDrained(d, ctx, 2, cid, 0, 3*time.Minute)
			if next >= left {
				left = next
				break
			}
			left = next
		}
	}
	c.rec("RH-SWEEP", "gen-0 sweeps into gen-1 and drains to zero", left == 0,
		fmt.Sprintf("gen-0 utxos left=%d, gen-1 utxos=%d", left, f21GenUtxoCountOn(d, ctx, 2, cid, 1)))
	c.rec("RH-BALANCES", "the sweep moves custody only, never user balances",
		balanceSats(t, d, ctx, cid, owner) == balOwnerPreSweep && balanceSats(t, d, ctx, cid, owner2) == balOwner2PreSweep,
		fmt.Sprintf("%s %d to %d, %s %d to %d", owner, balOwnerPreSweep, balanceSats(t, d, ctx, cid, owner),
			owner2, balOwner2PreSweep, balanceSats(t, d, ctx, cid, owner2)))
	if rotated && left == 0 {
		// The withdrawal is issued by magi-1 (owner) and signed by the gen-1 key.
		before := balanceSats(t, d, ctx, cid, owner)
		gen1Key := cid + "-" + btcvault.VaultKeyName(1)
		wTxid, wDest, wRaw := rhIssueUnmap(t, d, ctx, cid, gen1Key, 500_000)
		settled := false
		detail := fmt.Sprintf("txid=%s signed by %s rawlen=%d", wTxid, gen1Key, len(wRaw))
		if wRaw != "" {
			bcTxid, h, berr := vfBroadcastAndMine(t, d, ctx, wRaw)
			if berr != nil {
				detail += fmt.Sprintf(", regtest refused it: %v", berr)
			} else {
				cs := vfRelayAndConfirmIndex(t, d, ctx, 1, cid, bcTxid, h, vfChangeVout(d, ctx, bcTxid, wDest))
				settled = f21WaitSpendGone(t, d, ctx, cid, wTxid, 4*time.Minute)
				detail += fmt.Sprintf(", broadcast %s, confirmSpend=%s, pending gone=%v", bcTxid, cs, settled)
			}
		}
		after := balanceSats(t, d, ctx, cid, owner)
		c.rec("RH-WITHDRAW-GEN1", "a new withdrawal is signed by gen-1, paid on Bitcoin and settled", settled && after < before,
			fmt.Sprintf("%s; %s %d to %d sats", detail, owner, before, after))
	}
	riseBlames := rhBlamesSince(t, ctx, d, upEnd)
	c.rec("RH-NO-BLAME", "no blame lands from the upgrade through the migration", len(riseBlames) == 0, fmt.Sprintf("%v", riseBlames))

	// -------------------------------------------------------------------------
	// Step 6: identical state everywhere, and a full replay.
	// -------------------------------------------------------------------------
	vfAssertContractIdentical(c, d, ctx, cid, nodes, 4*time.Minute, "RH-IDENT")
	vfStopNodes(t, d, ctx, []int{5})
	vfDropNodeDb(t, d, ctx, 5)
	vfStartNodes(t, d, ctx, []int{5})
	target5, err := d.getLastProcessedBlock(ctx, 1)
	if err != nil {
		t.Fatalf("cannot read magi-1 processed height for the re-index target: %v", err)
	}
	if !vfWaitProcessed(t, d, ctx, 5, target5, 20*time.Minute) {
		bh, _ := d.getLastProcessedBlock(ctx, 5)
		c.rec("RH-REINDEX", "a wiped node replays the old-build blocks, the update, the rise and the sweep to the same state", false,
			fmt.Sprintf("magi-5 only reached %d of %d", bh, target5))
	} else {
		vfAssertContractIdentical(c, d, ctx, cid, []int{1, 5}, 5*time.Minute, "RH-REINDEX")
		s1 := pfSeatFingerprint(pfMustSeats(t, d, ctx, 1))
		s5 := pfSeatFingerprint(pfMustSeats(t, d, ctx, 5))
		c.rec("RH-REINDEX-SEATS", "the replayed node rebuilds the same POA registry", s1 == s5, fmt.Sprintf("magi-1 %s / magi-5 %s", s1, s5))
	}
	c.summary("RH")
}
