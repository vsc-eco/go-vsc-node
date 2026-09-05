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

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"go.mongodb.org/mongo-driver/bson"
)

// TestVaultF21UpgradePath is THE TESTNET REHEARSAL of the vault-rotation-v2 cutover.
//
// It rehearses the exact production path, in the exact production order, on a real
// 5-node devnet with a real regtest bitcoind:
//
//  1. a FUNDED V1 contract exists first (v1 wasm deployed, gen-0 key generated and
//     registered, two real SPV deposits credited, no vault registry at all), with the
//     node-side rotation flag still OFF (hpin=600), which is precisely what testnet
//     looks like today;
//  2. a user withdrawal is issued and TSS-signed against that V1 contract and is left
//     IN FLIGHT, unbroadcast, straddling the upgrade;
//  3. the contract CODE UPDATE to the v2 wasm is queued and rides out the 30-block
//     devnet timelock, again with the rotation flag still off;
//  4. migrate() runs and FOLDS the legacy single-slot key into generation 0 of the new
//     vault registry (mv 1 -> 2, v = one Active gen-0, va=0, vn=1), leaving every
//     legacy UTXO and every balance untouched;
//  5. the LEGACY withdrawal, built by v1 code, is broadcast and SETTLES under v2 code
//     (the spend leaves the pending list, the owner stays debited);
//  6. only then does the rotation flag switch on, gen-0 rotates to gen-1 and gen-0 is
//     swept dry, with user balances untouched by the sweep;
//  7. every node must hold byte-identical contract state, and a node whose database is
//     dropped must REPLAY the code update plus the fold plus the sweep from Hive and
//     land on the same bytes.
//
// A FAIL anywhere in this test is a FAIL of the testnet upgrade plan itself.
//
//	VAULT_F21_RUN=1 BTC_MAPPING_V1_WASM_PATH=/home/clauderfly/utxo-v1/btc-mapping-contract/bin/dev.wasm \
//	BTC_MAPPING_WASM_PATH=/home/clauderfly/utxo-v2/btc-mapping-contract/bin/dev.wasm \
//	DEVNET_KEEP=1 go test -v -run TestVaultF21UpgradePath -timeout 80m ./tests/devnet/
func TestVaultF21UpgradePath(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F21_RUN") == "" {
		t.Skip("set VAULT_F21_RUN=1")
	}
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Minute)
	defer cancel()

	// The V1 wasm has no default: rehearsing the upgrade against the v2 wasm twice
	// would be a vacuous test, so an unset or missing path is fatal, never a skip.
	v1wasm := os.Getenv("BTC_MAPPING_V1_WASM_PATH")
	if v1wasm == "" {
		t.Fatalf("PRECONDITION FAILED: BTC_MAPPING_V1_WASM_PATH is unset. F21 must deploy the REAL v1 contract first, " +
			"otherwise there is no upgrade to rehearse (expected e.g. /home/clauderfly/utxo-v1/btc-mapping-contract/bin/dev.wasm)")
	}
	if _, err := os.Stat(v1wasm); err != nil {
		t.Fatalf("PRECONDITION FAILED: v1 wasm %s: %v", v1wasm, err)
	}
	v2wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if v2wasm == "" {
		v2wasm = "/home/clauderfly/utxo-s1/btc-mapping-contract/bin/dev.wasm"
	}
	if _, err := os.Stat(v2wasm); err != nil {
		t.Fatalf("PRECONDITION FAILED: v2 wasm %s: %v", v2wasm, err)
	}

	// hpin=600 is LATE on purpose: the whole v1 phase AND the code update happen with
	// the node-side rotation gates OFF, exactly like testnet today. The gates only come
	// on after the fold has already populated the registry.
	const hpin uint64 = 600
	cfg := tssTestConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 70*time.Minute)

	c := &vfCase{t: t}

	// ---------------------------------------------------------------------------
	// Step 1: a FUNDED V1 contract. vfSetup cannot be used, it deploys the v2 wasm;
	// its deploy/seed/oracle/genesis steps are inlined here against the v1 wasm.
	// ---------------------------------------------------------------------------
	seedH, err := d.MineBlocks(ctx, 101)
	if err != nil {
		t.Fatalf("mine: %v", err)
	}
	hdr1, err := btcBlockHeaderHex(ctx, d, seedH)
	if err != nil {
		t.Fatalf("hdr: %v", err)
	}
	cid, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: v1wasm, Name: "btc-mapping-contract", Description: "f21 upgrade path (v1)", DeployerNode: 1, GQLNode: 2,
	})
	if err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	t.Logf("CONTRACT=%s hpin=%d v1wasm=%s v2wasm=%s", cid, hpin, v1wasm, v2wasm)
	if s := vstatus(t, d, ctx, 1, cid, "seedBlocks", fmt.Sprintf(`{"block_header":"%s","block_height":%d}`, hdr1, seedH)); !isOK(s) {
		t.Fatalf("seedBlocks: %s", s)
	}
	d.WriteOracleConfigs(ctx)
	d.SetOracleContractIDs(map[string]string{"BTC": cid})
	d.RestartAllMagiNodes(ctx)
	time.Sleep(10 * time.Second)

	// v1 genesis: createKey mints the single legacy TSS key "main" (v1 has no vault
	// registry at all), registerPublicKey stores it in the flat pubkey/backupkey slots.
	vstatus(t, d, ctx, 1, cid, "createKey", "")
	kd0, err := d.WaitForTssKey(ctx, 2, bson.M{"id": cid + "-main", "status": "active"}, 10*time.Minute)
	if err != nil {
		all, _ := d.GetTssKeys(ctx, 2, bson.M{})
		for _, k := range all {
			t.Logf("  tss_key id=%s status=%s", k.Id, k.Status)
		}
		t.Fatalf("PRECONDITION FAILED: v1 gen-0 keygen never completed: %v", err)
	}
	primary0 := kd0.PublicKey
	if s := vstatus(t, d, ctx, 1, cid, "registerPublicKey",
		fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, primary0, backupPubKeyG)); !isOK(s) {
		t.Fatalf("PRECONDITION FAILED: v1 registerPublicKey: %s", s)
	}
	t.Logf("v1 gen-0 registered: primary=%s backup=%s", primary0, backupPubKeyG)

	owner := "hive:" + fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, 1)
	owner2 := "hive:" + fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, 2)
	fundVaultViaSPV(t, d, ctx, cid, primary0, backupPubKeyG, owner, 30_000_000, seedH)
	fundVaultViaSPV(t, d, ctx, cid, primary0, backupPubKeyG, owner2, 20_000_000, contractLastHeight(t, d, ctx, cid))
	// The map tx is CONFIRMED on the calling node before the READ node (magi-2) has
	// applied it; poll for the credit instead of reading once (first run recorded
	// owner2=0 sats and utxos=1 from a read that raced the settle).
	if !balanceCredited(t, d, ctx, cid, owner2) {
		t.Logf("owner2 credit not visible on magi-2 yet after the poll window")
	}
	for i := 0; i < 12 && f21GenUtxoCountOn(d, ctx, 2, cid, 0) < 2; i++ {
		time.Sleep(5 * time.Second)
	}
	balOwnerFunded := balanceSats(t, d, ctx, cid, owner)
	balOwner2Funded := balanceSats(t, d, ctx, cid, owner2)
	pre, err := getStateHex(d, ctx, 2, cid, []string{"mv", "v"})
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: cannot read v1 contract state from magi-2: %v", err)
	}
	utxosBeforeFold := f21GenUtxoCountOn(d, ctx, 2, cid, 0)
	c.rec("F21-V1-FUNDED", "the v1 contract is funded on two accounts and has no vault registry",
		balOwnerFunded > 0 && balOwner2Funded > 0 && len(pre["v"]) == 0,
		fmt.Sprintf("%s=%d sats, %s=%d sats, mv=%q, v=%d bytes, utxos=%d",
			owner, balOwnerFunded, owner2, balOwner2Funded, string(pre["mv"]), len(pre["v"]), utxosBeforeFold))
	if balOwnerFunded == 0 || balOwner2Funded == 0 {
		t.Fatalf("PRECONDITION FAILED: the v1 deposits did not credit, there is no funded contract to upgrade")
	}

	// ---------------------------------------------------------------------------
	// Step 2: a LEGACY withdrawal, issued and signed by v1, left in flight across the
	// upgrade. v1 debits the caller at BUILD time, so the debit is visible now and the
	// settle later only has to clear the pending spend.
	// ---------------------------------------------------------------------------
	const unmapSats = 1_000_000
	unmapTxid, unmapDest, unmapRaw := f21IssueLegacyUnmap(t, d, ctx, cid, unmapSats)
	balOwnerAfterUnmap := balanceSats(t, d, ctx, cid, owner)
	c.rec("F21-V1-UNMAP-SIGNED", "a v1 withdrawal is fully TSS-signed by the legacy gen-0 key and left unbroadcast",
		unmapRaw != "" && balOwnerAfterUnmap < balOwnerFunded,
		fmt.Sprintf("txid=%s dest=%s rawlen=%d, owner %d to %d sats", unmapTxid, unmapDest, len(unmapRaw), balOwnerFunded, balOwnerAfterUnmap))

	// ---------------------------------------------------------------------------
	// Step 3: the contract code update, still with the rotation flag OFF.
	// ---------------------------------------------------------------------------
	activeV1, err := d.ActiveContract(ctx, 2, cid)
	if err != nil || activeV1 == nil {
		t.Fatalf("PRECONDITION FAILED: cannot read the active contract before the update (err=%v)", err)
	}
	codeV1 := activeV1.Code
	if err := d.UpdateContract(ctx, ContractUpdateOpts{
		ContractId: cid, WasmPath: v2wasm, Name: "btc-mapping-contract", DeployerNode: 1, GQLNode: 2,
	}); err != nil {
		t.Fatalf("PRECONDITION FAILED: queueing the v2 code update: %v", err)
	}
	// The expected v2 code id is the CID the deployer put on chain: read it back off the
	// queued (timelocked) update rather than recomputing it locally.
	pending := f21WaitPendingUpdate(t, d, ctx, 2, cid, 5*time.Minute)
	codeV2 := pending.Code
	if codeV2 == codeV1 {
		t.Fatalf("PRECONDITION FAILED: the queued code %s equals the active code, the v1 and v2 wasm are the same build", codeV2)
	}
	t.Logf("queued v2 update: code=%s creation_height=%d activation_height=%d (timelock %d blocks)",
		codeV2, pending.CreationHeight, pending.ActivationHeight, pending.ActivationHeight-pending.CreationHeight)
	if err := d.WaitForBlockProcessing(ctx, 2, pending.ActivationHeight+1, 10*time.Minute); err != nil {
		t.Fatalf("PRECONDITION FAILED: chain never reached the update activation height %d: %v", pending.ActivationHeight, err)
	}
	actV2, errV2 := d.WaitForActiveCode(ctx, 2, cid, codeV2, 8*time.Minute)
	updated := errV2 == nil
	if updated {
		// The caller node must be running the new code too before migrate is issued.
		if _, err := d.WaitForActiveCode(ctx, 1, cid, codeV2, 6*time.Minute); err != nil {
			t.Logf("magi-1 has not surfaced the new active code yet: %v", err)
		}
	}
	c.rec("F21-UPDATED", "the v2 code becomes the active contract code after the devnet timelock", updated,
		fmt.Sprintf("v1 code=%s, v2 code=%s, active=%v (err=%v)", codeV1, codeV2, actV2, errV2))
	if !updated {
		c.summary("F21")
		t.Fatalf("PRECONDITION FAILED: the contract never ran v2 code, nothing after this point would be a rehearsal")
	}

	// ---------------------------------------------------------------------------
	// Step 4: migrate() folds the legacy key into generation 0.
	// ---------------------------------------------------------------------------
	balOwnerPreFold := balanceSats(t, d, ctx, cid, owner)
	balOwner2PreFold := balanceSats(t, d, ctx, cid, owner2)
	utxosPreFold := f21GenUtxoCountOn(d, ctx, 2, cid, 0)
	migStatus := ""
	for i := 0; i < 3; i++ {
		migStatus = vstatus(t, d, ctx, 1, cid, "migrate", "")
		if isOK(migStatus) {
			break
		}
		t.Logf("migrate not accepted yet (status=%s), retry %d", migStatus, i)
		time.Sleep(15 * time.Second)
	}
	time.Sleep(10 * time.Second)
	post, err := getStateHex(d, ctx, 2, cid, []string{"mv", "va", "vn"})
	if err != nil {
		t.Fatalf("cannot read contract state after migrate: %v", err)
	}
	va, vaOK := btcvault.ReadUint32BE(post["va"])
	vn, vnOK := btcvault.ReadUint32BE(post["vn"])
	vaults := vfVaultRegistryOn(d, ctx, 2, cid)
	foldOK := string(post["mv"]) == "2" && len(vaults) == 1 && vaOK && va == 0 && vnOK && vn == 1
	genDetail := "registry EMPTY"
	if len(vaults) == 1 {
		v0 := vaults[0]
		genDetail = fmt.Sprintf("gen=%d status=%d predecessor=%d primary=%s backup=%s",
			v0.Generation, int(v0.Status), v0.Predecessor, hex.EncodeToString(v0.Primary), hex.EncodeToString(v0.Backup))
		foldOK = foldOK &&
			v0.Generation == 0 &&
			v0.Status == btcvault.VaultStatusActive &&
			strings.EqualFold(hex.EncodeToString(v0.Primary), primary0) &&
			strings.EqualFold(hex.EncodeToString(v0.Backup), backupPubKeyG)
	}
	utxosPostFold := f21GenUtxoCountOn(d, ctx, 2, cid, 0)
	balOwnerPostFold := balanceSats(t, d, ctx, cid, owner)
	balOwner2PostFold := balanceSats(t, d, ctx, cid, owner2)
	foldOK = foldOK &&
		utxosPostFold == utxosPreFold &&
		balOwnerPostFold == balOwnerPreFold &&
		balOwner2PostFold == balOwner2PreFold
	c.rec("F21-FOLD", "migrate folds the legacy key into an Active gen-0 without touching funds", foldOK,
		fmt.Sprintf("migrate status=%s, mv=%q, va=%d(ok=%v) vn=%d(ok=%v), %d vault(s) [%s], gen-0 utxos %d to %d, %s %d to %d, %s %d to %d",
			migStatus, string(post["mv"]), va, vaOK, vn, vnOK, len(vaults), genDetail,
			utxosPreFold, utxosPostFold, owner, balOwnerPreFold, balOwnerPostFold, owner2, balOwner2PreFold, balOwner2PostFold))

	// ---------------------------------------------------------------------------
	// Step 5: the legacy, v1-built withdrawal settles under v2 code.
	// ---------------------------------------------------------------------------
	if unmapRaw == "" {
		c.rec("F21-LEGACY-UNMAP-SETTLES", "the v1-built withdrawal settles under v2 code", false,
			"no fully signed legacy withdrawal was available to broadcast (see F21-V1-UNMAP-SIGNED)")
	} else {
		bcTxid, h, berr := vfBroadcastAndMine(t, d, ctx, unmapRaw)
		if berr != nil {
			c.rec("F21-LEGACY-UNMAP-SETTLES", "the v1-built withdrawal settles under v2 code", false,
				fmt.Sprintf("regtest refused the legacy withdrawal: %v", berr))
		} else {
			cs := vfRelayAndConfirm(t, d, ctx, 1, cid, bcTxid, h)
			gone := f21WaitSpendGone(t, d, ctx, cid, unmapTxid, 4*time.Minute)
			balOwnerSettled := balanceSats(t, d, ctx, cid, owner)
			c.rec("F21-LEGACY-UNMAP-SETTLES", "the v1-built withdrawal settles under v2 code", gone && balOwnerSettled < balOwnerFunded,
				fmt.Sprintf("bcTxid=%s height=%d confirmSpend=%s, pending spend gone=%v, owner %d (funded) to %d sats",
					bcTxid, h, cs, gone, balOwnerFunded, balOwnerSettled))
		}
	}

	// ---------------------------------------------------------------------------
	// Step 6: only now do the node-side rotation gates come on, and only because the
	// fold already populated the registry.
	// ---------------------------------------------------------------------------
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	balOwnerPreSweep := balanceSats(t, d, ctx, cid, owner)
	balOwner2PreSweep := balanceSats(t, d, ctx, cid, owner2)

	primary1, rotated := vfRotate(t, d, ctx, cid, 1)
	c.rec("F21-ROTATE", "the folded gen-0 rotates to gen-1 under v2", rotated,
		fmt.Sprintf("gen-1 primary=%s, gen-0 status=%d, gen-1 status=%d",
			primary1, vfVaultStatusOn(d, ctx, 2, cid, 0), vfVaultStatusOn(d, ctx, 2, cid, 1)))

	if rotated {
		fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
		left := f21GenUtxoCountOn(d, ctx, 2, cid, 0)
		tranches := 0
		// The first tranche is capped at MigrationCanaryValue and always carries exactly
		// one input, so a multi-UTXO legacy generation needs several tranches to drain.
		for i := 0; i < 5 && left > 0; i++ {
			migrateAndSettle(t, d, ctx, cid, cid+"-main", primary1, backupPubKeyG)
			tranches++
			next := f21WaitGenDrained(d, ctx, 2, cid, 0, 3*time.Minute)
			if next >= left {
				left = next
				t.Logf("tranche %d did not reduce the gen-0 UTXO count (still %d), stopping the sweep loop", tranches, left)
				break
			}
			left = next
		}
		c.rec("F21-SWEEP", "the legacy gen-0 UTXOs sweep into gen-1 and gen-0 drains to zero", left == 0,
			fmt.Sprintf("%d tranche(s), gen-0 utxos left=%d, gen-1 utxos=%d", tranches, left, f21GenUtxoCountOn(d, ctx, 2, cid, 1)))
	} else {
		c.rec("F21-SWEEP", "the legacy gen-0 UTXOs sweep into gen-1 and gen-0 drains to zero", false,
			"gen-1 never activated, so there is no successor to sweep into")
	}

	balOwnerPostSweep := balanceSats(t, d, ctx, cid, owner)
	balOwner2PostSweep := balanceSats(t, d, ctx, cid, owner2)
	c.rec("F21-BALANCES", "the migration sweep moves custody only, never user balances",
		balOwnerPostSweep == balOwnerPreSweep && balOwner2PostSweep == balOwner2PreSweep,
		fmt.Sprintf("%s %d to %d sats, %s %d to %d sats",
			owner, balOwnerPreSweep, balOwnerPostSweep, owner2, balOwner2PreSweep, balOwner2PostSweep))

	// ---------------------------------------------------------------------------
	// Step 7: no node may have taken a different view of the upgrade, and a node that
	// replays from an empty database must reach the same bytes, which means replaying
	// the code update, the fold and the sweep.
	// ---------------------------------------------------------------------------
	vfAssertContractIdentical(c, d, ctx, cid, vfAllNodes(5), 4*time.Minute, "F21-IDENT")

	vfStopNodes(t, d, ctx, []int{5})
	vfDropNodeDb(t, d, ctx, 5)
	vfStartNodes(t, d, ctx, []int{5})
	target, err := d.getLastProcessedBlock(ctx, 1)
	if err != nil {
		t.Fatalf("cannot read magi-1 processed height for the re-index target: %v", err)
	}
	if !vfWaitProcessed(t, d, ctx, 5, target, 12*time.Minute) {
		bh, berr := d.getLastProcessedBlock(ctx, 5)
		c.rec("F21-REINDEX", "a wiped node replays the code update, the fold and the sweep to the same state", false,
			fmt.Sprintf("magi-5 only reached %d of %d (err=%v)", bh, target, berr))
	} else {
		vfAssertContractIdentical(c, d, ctx, cid, []int{1, 5}, 5*time.Minute, "F21-REINDEX")
	}

	c.summary("F21")
	t.Logf("F21 COMPLETE CONTRACT=%s hpin=%d v1code=%s v2code=%s", cid, hpin, codeV1, codeV2)
}

// f21IssueLegacyUnmap issues a v1 unmap, waits for the contract's signing data and for
// the legacy gen-0 key to sign every input, and returns the fully witnessed raw tx
// WITHOUT broadcasting it. This is the first half of unmapAndSettle: the withdrawal has
// to be left in flight so it straddles the code update. Returns an empty raw hex if the
// signatures never landed (the caller records that as the failure).
func f21IssueLegacyUnmap(t *testing.T, d *Devnet, ctx context.Context, cid string, sats int64) (txid, dest, rawHex string) {
	t.Helper()
	dest, err := d.bitcoinCli(ctx, "getnewaddress")
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: getnewaddress: %v", err)
	}
	before := txSpendIds(t, d, ctx, cid)
	if s := vstatus(t, d, ctx, 1, cid, "unmap", fmt.Sprintf(`{"amount":"%d","to":"%s"}`, sats, dest)); !isOK(s) {
		t.Fatalf("PRECONDITION FAILED: the v1 unmap was rejected (status=%s), there is no legacy withdrawal to carry across the upgrade", s)
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
		t.Fatalf("PRECONDITION FAILED: no pending spend appeared after the v1 unmap")
	}
	t.Logf("legacy v1 unmap pending spend txid=%s dest=%s", txid, dest)

	sd := waitSigningData(t, d, ctx, cid, txid)
	if sd == nil {
		t.Fatalf("PRECONDITION FAILED: no signing data for the v1 unmap %s", txid)
	}
	var mtx wire.MsgTx
	if err := mtx.Deserialize(bytes.NewReader(sd.Tx)); err != nil {
		t.Fatalf("PRECONDITION FAILED: deserialising the v1 unmap tx: %v", err)
	}
	for _, uh := range sd.UnsignedSigHashes {
		sig := waitSignature(t, d, ctx, cid+"-main", uh.SigHash)
		if sig == nil {
			t.Logf("the legacy gen-0 key did not sign input %d of the v1 unmap", uh.Index)
			return txid, dest, ""
		}
		signature := append(append([]byte{}, sig...), byte(txscript.SigHashAll))
		mtx.TxIn[uh.Index].Witness = wire.TxWitness{signature, []byte{0x01}, uh.WitnessScript}
	}
	var buf bytes.Buffer
	if err := mtx.BtcEncode(&buf, wire.ProtocolVersion, wire.WitnessEncoding); err != nil {
		t.Logf("encoding the signed v1 unmap: %v", err)
		return txid, dest, ""
	}
	return txid, dest, hex.EncodeToString(buf.Bytes())
}

// f21WaitPendingUpdate polls until the queued (timelocked) code update surfaces for the
// contract, which is how the expected v2 code id is obtained: it is the CID the deployer
// actually put on chain, not one recomputed by the test.
func f21WaitPendingUpdate(t *testing.T, d *Devnet, ctx context.Context, node int, cid string, within time.Duration) ContractGQL {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		rows, err := d.PendingUpdates(ctx, node, cid)
		if err == nil && len(rows) > 0 {
			return rows[0]
		}
		if time.Now().After(deadline) {
			d.dumpContracts(ctx, t, node)
			t.Fatalf("PRECONDITION FAILED: no pending code update appeared for %s (err=%v)", cid, err)
		}
		time.Sleep(3 * time.Second)
	}
}

// f21UtxoGen decodes a UTXO blob's generation exactly as the contract's UnmarshalUtxo
// does: a pre-S1 blob written by the v1 contract ends after the tag and reads as
// generation 0, an S1 blob carries exactly 4 trailing big-endian bytes, and anything
// else is malformed. The suite's genUtxoCount / vfGenUtxoCountOn read the last 4 bytes
// blindly, which is correct only for S1 blobs: on a freshly upgraded contract that is
// the tail of the deposit tag, so they would miscount every legacy UTXO as a random
// generation. F21 is the one test whose UTXOs are legacy, so it needs this parser.
func f21UtxoGen(raw []byte) (uint32, bool) {
	const minLen = 32 + 4 + 8 + 1 + 1
	if len(raw) < minLen {
		return 0, false
	}
	off := 32 + 4 + 8
	pkLen := int(raw[off])
	off++
	if off+pkLen > len(raw) {
		return 0, false
	}
	off += pkLen
	if off >= len(raw) {
		return 0, false
	}
	tagLen := int(raw[off])
	off++
	if off+tagLen > len(raw) {
		return 0, false
	}
	off += tagLen
	switch len(raw) - off {
	case 0:
		return 0, true // pre-S1 blob, generation 0
	case 4:
		return btcvault.ReadUint32BE(raw[off:])
	}
	return 0, false
}

// f21GenUtxoCountOn counts the UTXOs a generation holds on one node, decoding each blob
// with the contract's own tail rule (see f21UtxoGen). Returns -1 if the node cannot be read.
func f21GenUtxoCountOn(d *Devnet, ctx context.Context, node int, cid string, gen uint32) int {
	st, err := getStateHex(d, ctx, node, cid, []string{"r"})
	if err != nil {
		return -1
	}
	reg := st["r"]
	n := 0
	for off := 0; off+8 <= len(reg); off += 8 {
		id := uint16(reg[off])<<8 | uint16(reg[off+1])
		key := "u-" + fmt.Sprintf("%x", id)
		us, err := getStateHex(d, ctx, node, cid, []string{key})
		if err != nil {
			continue
		}
		g, ok := f21UtxoGen(us[key])
		if !ok {
			continue
		}
		if g == gen {
			n++
		}
	}
	return n
}

// f21WaitGenDrained polls the committed UTXO registry until a generation holds nothing,
// and returns the final count. Chain state, never a transaction status.
func f21WaitGenDrained(d *Devnet, ctx context.Context, node int, cid string, gen uint32, within time.Duration) int {
	deadline := time.Now().Add(within)
	n := f21GenUtxoCountOn(d, ctx, node, cid, gen)
	for n != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Second)
		n = f21GenUtxoCountOn(d, ctx, node, cid, gen)
	}
	return n
}

// f21WaitSpendGone polls the pending-spend list until a txid is no longer in it, which is
// how a settled withdrawal is recognised from chain state.
func f21WaitSpendGone(t *testing.T, d *Devnet, ctx context.Context, cid, txid string, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if !contains(txSpendIds(t, d, ctx, cid), txid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Second)
	}
}
