package devnet

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"

	"vsc-node/lib/btcvault"
)

// TestVaultF25ReorgSwapsRedrivenSweep is failure-state F25 of the BTC vault-rotation-v2
// suite, the branch F9 could not exercise: a settled migration sweep is DROPPED by a
// Bitcoin reorg and replaced by its own re-driven (RBF) sibling.
//
// F9 recorded "dropping the sweep needs a conflicting spend of the same vault inputs,
// which no devnet harness can manufacture". That is wrong: redriveSpend manufactures
// exactly that spend (same reserved inputs, successor output, higher fee), and the
// committee signs it because output scoping admits a successor-paying sweep. On mainnet
// the same two transactions exist whenever an operator fee-bumps a slow sweep; if the
// ORIGINAL is mined and a one-block reorg then mines the REPLACEMENT instead (miners
// prefer the higher fee), the contract ends up in the state this test measures.
//
// SEQUENCE
//  1. Rotate (gen-0 Retiring, gen-1 Active), fund the fee reserve, build sweep O, wait
//     for its full witness, keep it unbroadcast.
//  2. Age the record past RedriveStaleBlocks (12) by relaying regtest headers, then
//     redriveSpend(O) -> replacement R, fully signed too. O and R form one spend group.
//  3. Broadcast and mine O, relay its header, confirmSpend(O): the contract credits
//     gen-1 with O:0, deletes gen-0's inputs and clears the WHOLE group (O and R).
//  4. Reorg: invalidateblock(O's block), mine a competing block that contains R
//     (generateblock; fallback RBF broadcast + mine), mine one more on top, relay the
//     reorg with replaceBlocks + addBlocks exactly like F9.
//  5. Measure:
//     F25-REORG (instrument): O is no longer in any block, R is confirmed, the contract
//     holds the canonical header at that height.
//     F25-PHANTOM: gen-1's registry still lists O:0, a transaction Bitcoin no longer
//     has. The vault's books now count coins that do not exist at that outpoint; the
//     real coins sit at R:0, which the contract does not know.
//     F25-NO-RECONCILE: confirmSpend(R) is refused (its record was cleared with the
//     group), so there is no in-band way to point the books at the real output.
//     F25-INFLATION (INFO if refused): topUpFeeReserve with R's proof re-credits R:0 as
//     a fresh active-gen UTXO while the phantom stays, so the registry total exceeds
//     the coins Bitcoin actually holds by O:0's amount.
//     F25-STUCK-WITHDRAWAL: a withdrawal large enough to need the phantom input is
//     built and TSS-signed by gen-1, and Bitcoin rejects it (inputs missing or spent).
//     F25-IDENT: all five nodes agree (the corruption is consensus state, not a fork).
//
// U-10 "reorg-reversible migration" is documented as unbuilt in the contract; this
// test turns that note into a measured fund-freeze.
//
//	VAULT_F25_RUN=1 go test -v -run TestVaultF25ReorgSwapsRedrivenSweep -timeout 95m ./tests/devnet/
func TestVaultF25ReorgSwapsRedrivenSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F25_RUN") == "" {
		t.Skip("set VAULT_F25_RUN=1")
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

	const hpin = 400
	cfg := vfSlowReshareConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 85*time.Minute)

	c := &vfCase{t: t}

	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "vault F25 reorg swaps redriven sweep")
	cid := env.cid
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	finish := func() {
		vfAssertContractIdentical(c, d, ctx, cid, vfAllNodes(5), 4*time.Minute, "F25-IDENT")
		c.summary("F25")
		t.Logf("F25 COMPLETE CONTRACT=%s", cid)
	}

	// ---- 1. rotate, fund the reserve, build + sign the original sweep O ----
	primary1, rotated := vfRotate(t, d, ctx, cid, 1)
	if !rotated {
		t.Fatalf("PRECONDITION FAILED: gen-0 to gen-1 rotation did not complete (primary1=%q, gen-0 status=%d)", primary1, vfVaultStatusOn(d, ctx, 2, cid, 0))
	}
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)

	buildH := contractLastHeight(t, d, ctx, cid)
	txO, sdO, ms := vfBuildSweep(t, d, ctx, 1, cid)
	if txO == "" || sdO == nil {
		t.Fatalf("PRECONDITION FAILED: no migration sweep built (txid=%q migrateVault status=%s)", txO, ms)
	}
	rawO, signedO := "", false
	for attempt := 1; attempt <= 3 && !signedO; attempt++ {
		rawO, signedO = vfAwaitSweepSignatures(t, d, ctx, cid+"-main", sdO)
	}
	if !signedO {
		t.Fatalf("PRECONDITION FAILED: sweep %s never collected a full witness", txO)
	}
	t.Logf("original sweep O=%s signed (build height %d), kept unbroadcast", txO, buildH)

	// ---- 2. age it, redrive it, sign the replacement R ----
	if _, err := d.MineBlocks(ctx, 14); err != nil {
		t.Fatalf("PRECONDITION FAILED: mining the staleness blocks: %v", err)
	}
	tip, _ := d.BitcoinHeight(ctx)
	f10RelayTo(t, d, ctx, 1, cid, tip, 10)
	before := txSpendIds(t, d, ctx, cid)
	rd := ""
	for attempt := 1; attempt <= 3; attempt++ {
		rd = vstatus(t, d, ctx, 1, cid, "redriveSpend", txO)
		if isOK(rd) {
			break
		}
		t.Logf("redriveSpend attempt %d status=%s (contract last=%d, build=%d); aging 6 more blocks", attempt, rd, contractLastHeight(t, d, ctx, cid), buildH)
		d.MineBlocks(ctx, 6)
		tip, _ = d.BitcoinHeight(ctx)
		f10RelayTo(t, d, ctx, 1, cid, tip, 10)
	}
	if !isOK(rd) {
		t.Fatalf("PRECONDITION FAILED: redriveSpend never accepted (last status=%s); without a replacement there is no conflicting spend to mine", rd)
	}
	txR := ""
	for i := 0; i < 20 && txR == ""; i++ {
		time.Sleep(3 * time.Second)
		for _, id := range txSpendIds(t, d, ctx, cid) {
			if !contains(before, id) {
				txR = id
			}
		}
	}
	if txR == "" {
		t.Fatalf("PRECONDITION FAILED: redrive accepted but no replacement txid appeared in the spends registry")
	}
	sdR := waitSigningData(t, d, ctx, cid, txR)
	if sdR == nil {
		t.Fatalf("PRECONDITION FAILED: no signing data for replacement %s", txR)
	}
	rawR, signedR := "", false
	for attempt := 1; attempt <= 3 && !signedR; attempt++ {
		rawR, signedR = vfAwaitSweepSignatures(t, d, ctx, cid+"-main", sdR)
	}
	if !signedR {
		t.Fatalf("PRECONDITION FAILED: replacement %s never collected a full witness (the committee refused to sign the redrive?)", txR)
	}
	t.Logf("replacement R=%s signed; spend group is {O, R}", txR)

	// ---- 3. mine O and settle it ----
	bcO, hA, err := vfBroadcastAndMine(t, d, ctx, rawO)
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: broadcast of O rejected: %v", err)
	}
	f10RelayTo(t, d, ctx, 1, cid, hA, 10)
	csO := vfConfirmSpendOnly(t, d, ctx, 1, cid, bcO, hA, 0)
	if !isOK(csO) {
		t.Fatalf("PRECONDITION FAILED: confirmSpend(O) status=%s; the settled-then-dropped state cannot be built", csO)
	}
	time.Sleep(10 * time.Second)
	gen1Settled := vfGenUtxoCountOn(d, ctx, 2, cid, 1)
	hasO, idO, amtO := f25RegistryEntryForTxid(d, ctx, 2, cid, txO)
	msOGone := !vfSweepRecordOn(d, ctx, 2, cid, txO)
	msRGone := !vfSweepRecordOn(d, ctx, 2, cid, txR)
	t.Logf("after settle: gen-1 utxos=%d, registry has O:0=%v (id=%d amount=%d), ms records cleared O=%v R=%v", gen1Settled, hasO, idO, amtO, msOGone, msRGone)
	if !hasO {
		t.Fatalf("PRECONDITION FAILED: the settled sweep output O:0 is not in the registry, nothing can become a phantom")
	}
	hashA, _ := d.bitcoinCli(ctx, "getblockhash", fmt.Sprint(hA))

	// ---- 4. the reorg: drop O's block, mine R in its place, extend ----
	if _, err := d.bitcoinCli(ctx, "invalidateblock", hashA); err != nil {
		t.Fatalf("PRECONDITION FAILED: invalidateblock %s: %v", hashA, err)
	}
	minerAddr, _ := d.bitcoinCli(ctx, "getnewaddress")
	how := "generateblock"
	if _, err := d.bitcoinCli(ctx, "generateblock", minerAddr, fmt.Sprintf(`["%s"]`, rawR)); err != nil {
		how = "sendrawtransaction+generatetoaddress"
		t.Logf("generateblock with R failed (%v); falling back to an RBF broadcast of R", err)
		if _, err2 := d.bitcoinCli(ctx, "sendrawtransaction", rawR); err2 != nil {
			t.Logf("RBF broadcast of R rejected: %v", err2)
		}
		d.MineBlocks(ctx, 1)
	}
	newTip, _ := d.MineBlocks(ctx, 1)
	_, oInChain := f9TxBlockHash(d, ctx, bcO)
	rHash, rInChain := f9TxBlockHash(d, ctx, txR)
	rHeight := uint64(0)
	if rInChain {
		rHeight, _ = f9BlockHeightOf(d, ctx, rHash)
	}
	if !rInChain || oInChain {
		t.Errorf("PRECONDITION FAILED: the competing branch does not carry R instead of O (R confirmed=%v at %d, O confirmed=%v, method=%s)", rInChain, rHeight, oInChain, how)
		finish()
		return
	}

	// Relay the reorg to the contract (replaceBlocks for the divergent header, then addBlocks).
	last := contractLastHeight(t, d, ctx, cid)
	repl := f9ReplacementHeaders(t, d, ctx, 2, cid, last)
	replStatus := "(nothing to replace)"
	if len(repl) > 0 {
		replStatus = vstatus(t, d, ctx, 1, cid, "replaceBlocks", strings.Join(repl, ""))
	}
	for hh := last + 1; hh <= newTip; hh++ {
		hx, herr := btcBlockHeaderHex(ctx, d, hh)
		if herr != nil {
			break
		}
		vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":10}`, hx))
	}
	canonA, _ := btcBlockHeaderHex(ctx, d, hA)
	storedA := f9StoredHeaderHex(d, ctx, 2, cid, hA)
	c.rec("F25-REORG", "instrument: the original sweep is dropped from Bitcoin, its redriven replacement is mined at the same height, and the contract holds the canonical header",
		!oInChain && rInChain && rHeight == hA && strings.EqualFold(canonA, storedA),
		fmt.Sprintf("method=%s O_confirmed=%v R_confirmed=%v R_height=%d (O was %d) replaceBlocks=%s depth=%d contract_header_canonical=%v", how, oInChain, rInChain, rHeight, hA, replStatus, len(repl), strings.EqualFold(canonA, storedA)))

	// ---- 5. measure the damage ----
	time.Sleep(10 * time.Second)
	hasO2, idO2, amtO2 := f25RegistryEntryForTxid(d, ctx, 2, cid, txO)
	hasR, _, _ := f25RegistryEntryForTxid(d, ctx, 2, cid, txR)
	oUnspent := f25TxOutExists(d, ctx, txO, 0)
	rUnspent := f25TxOutExists(d, ctx, txR, 0)
	c.rec("F25-PHANTOM", "after the reorg gen-1's registry still lists O:0 (an output Bitcoin no longer has) and does not know R:0 (where the coins really are)",
		hasO2 && !hasR && !oUnspent && rUnspent,
		fmt.Sprintf("registry has O:0=%v (id=%d, %d sats) R:0=%v | bitcoin gettxout O:0 exists=%v R:0 exists=%v | gen-1 utxos=%d", hasO2, idO2, amtO2, hasR, oUnspent, rUnspent, vfGenUtxoCountOn(d, ctx, 2, cid, 1)))

	csR := vfConfirmSpendOnly(t, d, ctx, 1, cid, txR, rHeight, 0)
	c.rec("F25-NO-RECONCILE", "confirmSpend(R) is refused because settling O cleared the whole spend group, so nothing in-band can re-point the books at R:0",
		!isOK(csR), fmt.Sprintf("confirmSpend(R) status=%s (want FAILED/REVERTED)", csR))

	// topUpFeeReserve with R's proof: the only op that credits an untagged vault output.
	phantomBefore := f25PhantomSats(t, d, ctx, 2, cid)
	regSumBefore := f25RegistrySum(d, ctx, 2, cid)
	tu := f25TopUpWithProof(t, d, ctx, 1, cid, txR, rHeight)
	time.Sleep(10 * time.Second)
	hasRAfter, _, _ := f25RegistryEntryForTxid(d, ctx, 2, cid, txR)
	regSumAfter := f25RegistrySum(d, ctx, 2, cid)
	phantomAfter := f25PhantomSats(t, d, ctx, 2, cid)
	inflationDetail := fmt.Sprintf("topUpFeeReserve(R)=%s registry_has_R=%v registry_sum %d -> %d phantom_sats %d -> %d", tu, hasRAfter, regSumBefore, regSumAfter, phantomBefore, phantomAfter)
	if isOK(tu) {
		c.rec("F25-INFLATION", "re-crediting R:0 through topUpFeeReserve leaves the phantom in place: the registry counts more sats than Bitcoin holds",
			hasRAfter && phantomAfter > 0 && regSumAfter > regSumBefore, inflationDetail)
	} else {
		c.rec("F25-INFLATION", "INFO: topUpFeeReserve refused R:0, so the real coins stay unknown to the contract (phantom persists either way)",
			phantomAfter > 0, inflationDetail)
	}

	// A withdrawal that needs the phantom input: built, signed, rejected by Bitcoin.
	dest, _ := d.bitcoinCli(ctx, "getnewaddress")
	spendsBefore := txSpendIds(t, d, ctx, cid)
	withdrawSats := amtO2 - 5_000_000
	if withdrawSats < 1_000_000 {
		withdrawSats = amtO2
	}
	um := vstatus(t, d, ctx, 1, cid, "unmap", fmt.Sprintf(`{"amount":"%d","to":"%s"}`, withdrawSats, dest))
	txU := ""
	for i := 0; i < 20 && isOK(um) && txU == ""; i++ {
		time.Sleep(3 * time.Second)
		for _, id := range txSpendIds(t, d, ctx, cid) {
			if !contains(spendsBefore, id) {
				txU = id
			}
		}
	}
	stuckDetail := fmt.Sprintf("unmap(%d sats)=%s txid=%s", withdrawSats, um, txU)
	stuck := false
	if txU != "" {
		sdU := waitSigningData(t, d, ctx, cid, txU)
		usesPhantom := false
		if sdU != nil {
			var mtx wire.MsgTx
			if mtx.Deserialize(bytes.NewReader(sdU.Tx)) == nil {
				for _, in := range mtx.TxIn {
					if in.PreviousOutPoint.Hash.String() == txO {
						usesPhantom = true
					}
				}
			}
		}
		rawU, signedU := "", false
		if sdU != nil {
			for attempt := 1; attempt <= 3 && !signedU; attempt++ {
				rawU, signedU = f25AwaitSignatures(t, d, ctx, cid+"-mainv1", sdU)
			}
		}
		btcErr := "(not broadcast: unsigned)"
		if signedU {
			if _, berr := d.bitcoinCli(ctx, "sendrawtransaction", rawU); berr != nil {
				btcErr = strings.TrimSpace(berr.Error())
			} else {
				btcErr = "ACCEPTED (unexpected: the phantom input should not exist)"
			}
		}
		stuck = usesPhantom && signedU && !strings.Contains(btcErr, "ACCEPTED")
		stuckDetail += fmt.Sprintf(" uses_phantom_input=%v signed_by_gen1=%v bitcoin=%q", usesPhantom, signedU, btcErr)
	}
	c.rec("F25-STUCK-WITHDRAWAL", "a withdrawal that selects the phantom input is built and TSS-signed by gen-1, then rejected by Bitcoin: those sats can never be withdrawn",
		stuck, stuckDetail)

	finish()
}

// f25RegistryEntryForTxid scans the vault UTXO registry on node n for an entry whose
// stored txid equals txid (MarshalUtxo stores the display-hex txid bytes first).
func f25RegistryEntryForTxid(d *Devnet, ctx context.Context, node int, cid, txid string) (bool, int, int64) {
	want, err := hex.DecodeString(txid)
	if err != nil || len(want) != 32 {
		return false, -1, 0
	}
	st, err := getStateHex(d, ctx, node, cid, []string{"r"})
	if err != nil {
		return false, -1, 0
	}
	reg := st["r"]
	for off := 0; off+8 <= len(reg); off += 8 {
		id := int(reg[off])<<8 | int(reg[off+1])
		amt := int64(0)
		for _, b := range reg[off+2 : off+8] {
			amt = amt<<8 | int64(b)
		}
		key := "u-" + fmt.Sprintf("%x", id)
		us, err := getStateHex(d, ctx, node, cid, []string{key})
		if err != nil {
			continue
		}
		raw := us[key]
		if len(raw) >= 32 && bytes.Equal(raw[:32], want) {
			return true, id, amt
		}
	}
	return false, -1, 0
}

// f25RegistrySum totals the registry amounts on node n.
func f25RegistrySum(d *Devnet, ctx context.Context, node int, cid string) int64 {
	st, err := getStateHex(d, ctx, node, cid, []string{"r"})
	if err != nil {
		return -1
	}
	reg := st["r"]
	var sum int64
	for off := 0; off+8 <= len(reg); off += 8 {
		amt := int64(0)
		for _, b := range reg[off+2 : off+8] {
			amt = amt<<8 | int64(b)
		}
		sum += amt
	}
	return sum
}

// f25PhantomSats totals registry entries whose outpoint Bitcoin does not hold.
func f25PhantomSats(t *testing.T, d *Devnet, ctx context.Context, node int, cid string) int64 {
	t.Helper()
	st, err := getStateHex(d, ctx, node, cid, []string{"r"})
	if err != nil {
		return -1
	}
	reg := st["r"]
	var phantom int64
	for off := 0; off+8 <= len(reg); off += 8 {
		id := int(reg[off])<<8 | int(reg[off+1])
		amt := int64(0)
		for _, b := range reg[off+2 : off+8] {
			amt = amt<<8 | int64(b)
		}
		key := "u-" + fmt.Sprintf("%x", id)
		us, err := getStateHex(d, ctx, node, cid, []string{key})
		if err != nil || len(us[key]) < 36 {
			continue
		}
		raw := us[key]
		txid := hex.EncodeToString(raw[:32])
		vout := uint32(raw[32])<<24 | uint32(raw[33])<<16 | uint32(raw[34])<<8 | uint32(raw[35])
		if !f25TxOutExists(d, ctx, txid, vout) {
			phantom += amt
			t.Logf("phantom registry entry id=%d %s:%d %d sats (bitcoin gettxout: absent)", id, txid, vout, amt)
		}
	}
	return phantom
}

// f25TxOutExists reports whether bitcoind's UTXO set holds txid:vout.
func f25TxOutExists(d *Devnet, ctx context.Context, txid string, vout uint32) bool {
	out, err := d.bitcoinCli(ctx, "gettxout", txid, fmt.Sprint(vout))
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != "" && strings.TrimSpace(out) != "null"
}

// f25TopUpWithProof issues topUpFeeReserve for txid mined at height h (tx index 1 in a
// coinbase+1 block, the same proof shape the settle helpers use).
func f25TopUpWithProof(t *testing.T, d *Devnet, ctx context.Context, callNode int, cid, txid string, h uint64) string {
	t.Helper()
	bhash, _ := d.bitcoinCli(ctx, "getblockhash", fmt.Sprint(h))
	blockJSON, _ := d.bitcoinCli(ctx, "getblock", bhash, "1")
	var blk struct {
		Tx []string `json:"tx"`
	}
	json.Unmarshal([]byte(blockJSON), &blk)
	if len(blk.Tx) != 2 || blk.Tx[1] != txid {
		t.Logf("top-up block %d has %d txs (want coinbase + %s), proof may not match", h, len(blk.Tx), txid)
	}
	rawTx, _ := d.bitcoinCli(ctx, "getrawtransaction", txid)
	proof := ""
	if len(blk.Tx) > 0 {
		proof = reverseHexBytes(blk.Tx[0])
	}
	return vstatus(t, d, ctx, callNode, cid, "topUpFeeReserve", fmt.Sprintf(
		`{"tx_data":{"block_height":%d,"raw_tx_hex":"%s","merkle_proof_hex":"%s","tx_index":1}}`, h, rawTx, proof))
}

// f25AwaitSignatures is vfAwaitSweepSignatures for an arbitrary signing key id.
func f25AwaitSignatures(t *testing.T, d *Devnet, ctx context.Context, keyId string, sd *btcvault.SigningData) (string, bool) {
	t.Helper()
	var mtx wire.MsgTx
	if err := mtx.Deserialize(bytes.NewReader(sd.Tx)); err != nil {
		return "", false
	}
	for _, uh := range sd.UnsignedSigHashes {
		sig := waitSignature(t, d, ctx, keyId, uh.SigHash)
		if sig == nil {
			return "", false
		}
		signature := append(append([]byte{}, sig...), byte(txscript.SigHashAll))
		mtx.TxIn[uh.Index].Witness = wire.TxWitness{signature, []byte{0x01}, uh.WitnessScript}
	}
	var buf bytes.Buffer
	mtx.BtcEncode(&buf, wire.ProtocolVersion, wire.WitnessEncoding)
	return hex.EncodeToString(buf.Bytes()), true
}
