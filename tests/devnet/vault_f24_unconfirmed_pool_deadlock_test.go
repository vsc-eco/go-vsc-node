package devnet

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"vsc-node/lib/btcvault"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// TestVaultF24UnconfirmedPoolDeadlock is failure-state F24 of the BTC
// vault-rotation-v2 suite: the rotation deadlock observed on the LIVE TESTNET on
// 2026-09-05.
//
// THE FINDING IN ONE PARAGRAPH
// Every UTXO the live testnet vault holds today is change from a legacy withdrawal
// whose confirmSpend was never called, so each one sits in the vault's UTXO registry
// in a state the migration builder refuses to select, while the Bitcoin block header
// that would let anyone confirm it has long since been pruned from the contract.
// getMigrationInputs (btc-mapping-contract/contract/mapping/migration.go) selects
// only inputs that are BOTH in the confirmed pool (id >= UtxoConfirmedPoolStart,
// 1024) AND not reserved by an in-flight unmap, so migrateVault keeps returning
// "nothing to migrate for generation N" with a successful transaction status and no
// pending sweep. Both gates that are supposed to protect the rotation, however, count
// ANY registry UTXO of a superseded generation: AnyFundedSupersededGen (the NN#3
// createKey gate, same file) and the node side #11 bond lock
// (modules/vaultrotation/eligibility.go plus modules/state-processing/bond_lock.go,
// which is keyed on the generation's STATUS being Retiring, Draining or Inactive).
// The result is a permanent deadlock with no attacker: the retiring generation can
// never drain, no successor key can ever be minted, and every committee member's
// consensus bond stays locked, with nothing inside the contract able to move the
// stuck UTXO. Fix candidates: (a) let the migration builder pick up AGED
// unconfirmed-pool or reserved inputs, with a phantom-input escape that rebuilds the
// spend from the registry record when the parent's header is no longer provable;
// (b) exclude such structurally unmigratable inputs from NN#3 and from the bond lock,
// so an undrainable residue can freeze neither rotation nor the committee;
// (c) an operator-callable expiry that returns the inputs of a spend that can no
// longer be confirmed to the selectable pool (and re-arms the change accounting).
//
// HOW THIS DEVNET TEST REPRODUCES IT (and where it deviates from the testnet state)
// On a FRESH v2 deploy nothing ever allocates an unconfirmed-pool id: every credited
// output (deposit, unmap change, migration sweep output, fee-reserve top-up) is
// indexed with allocateConfirmedId, and allocateUnconfirmedId has no caller at all.
// The literal testnet shape (registry entries with id < 1024) is therefore only
// reachable through the v1 to v2 upgrade path, which is F21's subject. This test
// reproduces the IDENTICAL deadlock on a fresh v2 deploy through the sibling clause of
// the very same selection filter: an unmap that is signed, broadcast and mined but
// never confirmed leaves its input REGISTERED and RESERVED under Guard 1
// (delete-at-confirm), so getMigrationInputs skips it exactly as it skips an
// unconfirmed-pool input, while NN#3 and the bond lock still count it. F24-SETUP
// therefore accepts EITHER form (id < 1024, or reserved) and records which one it saw,
// and every later step is unchanged.
//
// WHAT THIS TEST PROVES
//  1. F24-SETUP: after a broadcast but unconfirmed unmap, generation 0 holds exactly
//     one registry UTXO and ZERO migration-selectable UTXOs.
//  2. F24-NOTHING: migrateVault SUCCEEDS three times in a row and changes nothing at
//     all (no new pending spend, byte-identical UTXO registry, vault registry and
//     migration sweep index, generation 0 still Retiring and still holding 1 UTXO).
//     There is no contract-output log reader in tests/devnet (nothing here reads a
//     contract call's return string or logs), so the "nothing to migrate" return value
//     is proven by that state comparison rather than by reading the string.
//  3. F24-NN3: createKey is REFUSED while the superseded generation holds that UTXO.
//  4. F24-BONDLOCK: a committee member's consensus_unstake creates no pending unstake.
//     Nothing inside the contract can move the UTXO, so this is the deadlock.
//  5. F24-ESCAPE-CONFIRM / F24-PROMOTED: the ONLY escape is the missing confirmSpend.
//     On devnet the header still exists, so relaying it and confirming the unmap with
//     the change vout settles it and leaves generation 0 holding one selectable UTXO.
//  6. F24-DRAINED / F24-NN3-RELEASED / F24-BOND-RELEASED: with the UTXO selectable
//     again the rotation completes, createKey is admitted again, and the bond releases.
//  7. F24-PRUNED: INFO, why step 5 is impossible on the live testnet.
//  8. F24-IDENT: the vault state is byte-identical across all 5 nodes at the end.
//
// A precondition that cannot be established is a t.Fatalf, never a t.Skip, so this
// test can never pass vacuously.
//
// RUN:
//
//	VAULT_F24_RUN=1 DEVNET_KEEP=1 go test -v -run TestVaultF24UnconfirmedPoolDeadlock -timeout 95m ./tests/devnet/
func TestVaultF24UnconfirmedPoolDeadlock(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F24_RUN") == "" {
		t.Skip("set VAULT_F24_RUN=1")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), vfTestBudget(85*time.Minute))
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		t.Fatal("BTC_MAPPING_WASM_PATH must point at the btc-mapping-contract v2 regtest wasm")
	}
	if _, err := os.Stat(wasm); err != nil {
		t.Fatalf("wasm: %v", err)
	}

	// hpin is AFTER genesis (around block 190) so generation 0 is minted on the v2-off
	// path (no fresh-genesis deadlock) and v2 is ON for the rotation under test.
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

	// SETUP: deploy, seed headers, wire the oracle, mint + register gen-0, fund it with
	// a single 50,000,000 sat tagged deposit for the owner.
	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "vault F24 unconfirmed pool deadlock")
	cid := env.cid

	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	// ---------------------------------------------------------------------------
	// 0. Build the stuck spend. This is unmapAndSettle (vault_stage3_test.go) with the
	//    tail cut off: unmap, wait for the signing data, wait for the generation 0
	//    signatures, assemble the witness, broadcast, mine ONE block, and then STOP.
	//    No addBlocks for that height and no confirmSpend, which is exactly the state
	//    the live testnet is in: the withdrawal is on Bitcoin, the contract never
	//    learned that it confirmed.
	// ---------------------------------------------------------------------------
	const unmapSats = 10_000_000
	dest := mustNewBtcAddr(t, d, ctx)
	spendsBefore := txSpendIds(t, d, ctx, cid)
	if s := vstatus(t, d, ctx, 1, cid, "unmap",
		fmt.Sprintf(`{"amount":"%d","to":"%s"}`, unmapSats, dest)); !isOK(s) {
		t.Fatalf("PRECONDITION FAILED: unmap of %d sats to %s was rejected (status=%s). Without a broadcast, unconfirmed withdrawal there is no stuck UTXO and F24 has no subject", unmapSats, dest, s)
	}
	var unmapTxid string
	for i := 0; i < 20 && unmapTxid == ""; i++ {
		time.Sleep(3 * time.Second)
		for _, id := range txSpendIds(t, d, ctx, cid) {
			if !contains(spendsBefore, id) {
				unmapTxid = id
				break
			}
		}
	}
	if unmapTxid == "" {
		t.Fatalf("PRECONDITION FAILED: no new pending spend appeared after the unmap (pending spends still %v)", spendsBefore)
	}
	sd := waitSigningData(t, d, ctx, cid, unmapTxid)
	if sd == nil {
		t.Fatalf("PRECONDITION FAILED: no signing data (key d-%s) for the unmap", unmapTxid)
	}
	var mtx wire.MsgTx
	if err := mtx.Deserialize(bytes.NewReader(sd.Tx)); err != nil {
		t.Fatalf("PRECONDITION FAILED: cannot deserialize the unmap's unsigned tx: %v", err)
	}
	for _, uh := range sd.UnsignedSigHashes {
		sig := waitSignature(t, d, ctx, cid+"-main", uh.SigHash)
		if sig == nil {
			t.Fatalf("PRECONDITION FAILED: generation 0 never signed input %d of the unmap, so it can never be broadcast", uh.Index)
		}
		signature := append(append([]byte{}, sig...), byte(txscript.SigHashAll))
		mtx.TxIn[uh.Index].Witness = wire.TxWitness{signature, []byte{0x01}, uh.WitnessScript}
	}
	var buf bytes.Buffer
	if err := mtx.BtcEncode(&buf, wire.ProtocolVersion, wire.WitnessEncoding); err != nil {
		t.Fatalf("PRECONDITION FAILED: encoding the witnessed unmap: %v", err)
	}
	bcTxid, err := d.bitcoinCli(ctx, "sendrawtransaction", hex.EncodeToString(buf.Bytes()))
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: regtest rejected the signed unmap: %v", err)
	}
	unmapHeight, err := d.MineBlocks(ctx, 1)
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: mining the unmap's block: %v", err)
	}
	t.Logf("unmap broadcast and mined but deliberately NOT relayed or confirmed: pendingSpend=%s bitcoinTxid=%s height=%d dest=%s",
		unmapTxid, bcTxid, unmapHeight, dest)

	// The change output pays the generation 0 UNTAGGED vault address (HandleUnmap
	// derives it with createP2WSHAddressWithBackup and a nil tag, which is what
	// untaggedVaultAddr rebuilds); the other output pays the user's destination.
	changeVout := -1
	changeAddrStr, cerr := untaggedVaultAddr(env.primary0, backupPubKeyG)
	if cerr != nil {
		t.Errorf("cannot derive the generation 0 untagged change address: %v", cerr)
	} else if addr, aerr := btcutil.DecodeAddress(changeAddrStr, &chaincfg.RegressionNetParams); aerr != nil {
		t.Errorf("cannot decode the change address %s: %v", changeAddrStr, aerr)
	} else if script, serr := txscript.PayToAddrScript(addr); serr != nil {
		t.Errorf("cannot build the change script for %s: %v", changeAddrStr, serr)
	} else {
		for i, out := range mtx.TxOut {
			if bytes.Equal(out.PkScript, script) {
				changeVout = i
				break
			}
		}
	}
	t.Logf("unmap outputs=%d, vault change vout=%d (change address %s)", len(mtx.TxOut), changeVout, changeAddrStr)

	// ---- F24-SETUP ----
	reg0 := vf24ReadRegistry(t, d, ctx, 2, cid)
	gen0Entries := vf24EntriesOfGen(reg0, 0)
	gen0Selectable := vf24SelectableOfGen(reg0, 0)
	setupOK := len(gen0Entries) == 1 && len(gen0Selectable) == 0
	c.rec("F24-SETUP", "after a broadcast but unconfirmed withdrawal generation 0 holds exactly one registry UTXO and none of it is migration-selectable",
		setupOK,
		fmt.Sprintf("registry=[%s] gen0Entries=%d gen0Selectable=%d (selectable means id>=%d and not reserved, the two clauses getMigrationInputs filters on); deadlock form: %s; NOTE the live testnet form is an unconfirmed-pool id (<%d), which a FRESH v2 deploy can never produce because allocateUnconfirmedId has no caller, so this run reproduces the sibling reserved-input form of the same filter",
			vf24Line(reg0), len(gen0Entries), len(gen0Selectable), vf24ConfirmedPoolStart, vf24DeadlockForm(gen0Entries), vf24ConfirmedPoolStart))
	if !setupOK {
		t.Fatalf("PRECONDITION FAILED: generation 0 does not hold exactly one unmigratable UTXO (registry=[%s]). Every later F24 assertion would be vacuous", vf24Line(reg0))
	}

	// ---------------------------------------------------------------------------
	// 1. Rotate to generation 1 so generation 0 is superseded (Retiring) and funded
	//    ONLY by the stuck change of that unmap, then fund the fee reserve so an
	//    empty reserve can never be confused with the deadlock under test (that is
	//    F11's subject, not this one).
	// ---------------------------------------------------------------------------
	primary1, rotated := vfRotate(t, d, ctx, cid, 1)
	if !rotated {
		t.Fatalf("PRECONDITION FAILED: the generation 0 to generation 1 rotation did not complete (primary1=%q). Without a Retiring generation 0 there is nothing for migrateVault to refuse", primary1)
	}
	// fundFeeReserve relays every header from the contract's last height, which
	// INCLUDES the unmap's block. Relaying a header settles nothing: only confirmSpend
	// can move the stuck output, which is the whole point of step 5.
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)

	gen0Stat := -1
	for i := 0; i < 8; i++ {
		gen0Stat = vfVaultStatusOn(d, ctx, 2, cid, 0)
		if gen0Stat >= int(btcvault.VaultStatusRetiring) {
			break
		}
		time.Sleep(10 * time.Second)
	}
	if gen0Stat != int(btcvault.VaultStatusRetiring) {
		t.Fatalf("PRECONDITION FAILED: generation 0 status is %d, want %d (Retiring) after the rotation. The NN#3 and bond-lock gates under test only apply to a superseded generation", gen0Stat, int(btcvault.VaultStatusRetiring))
	}
	t.Logf("rotation done: generation 1 active (primary1=%s), generation 0 Retiring and funded only by the stuck unmap change", primary1)

	// ---------------------------------------------------------------------------
	// 2. F24-NOTHING: migrateVault succeeds and does absolutely nothing, three times.
	// ---------------------------------------------------------------------------
	// ★ TAKE THE BASELINE ONLY ONCE THE READING NODE HAS SETTLED.
	//
	// Every snapshot here is read from magi-2 while vstatus confirms on magi-1, so a
	// baseline captured immediately after the rotation can be a node-behind. The whole case
	// is a BYTE-EQUALITY comparison against that baseline, so a stale one makes
	// rByteEqual=false for reasons that have nothing to do with migrateVault - which is
	// exactly how this failed in the campaign: status CONFIRMED, sameSpends=true,
	// vByteEqual=true, mslByteEqual=true, gen-0 still Retiring with 1 UTXO on all three
	// tries, and ONLY the UTXO registry bytes differing. The deadlock was reproduced
	// perfectly; the baseline was not settled.
	//
	// Wait for two consecutive identical reads before freezing it. Bounded, and it logs if
	// it never settles rather than silently proceeding with a moving baseline.
	base := vf24Snapshot(t, d, ctx, cid)
	settled := false
	for i := 0; i < 24; i++ {
		time.Sleep(5 * time.Second)
		next := vf24Snapshot(t, d, ctx, cid)
		if bytes.Equal(base.utxoReg, next.utxoReg) && bytes.Equal(base.vaultReg, next.vaultReg) &&
			bytes.Equal(base.sweepIdx, next.sweepIdx) && vf24SameIds(base.spends, next.spends) {
			base = next
			settled = true
			break
		}
		base = next
	}
	if !settled {
		t.Logf("F24-NOTHING: the reading node never produced two identical consecutive snapshots; "+
			"the byte-equality baseline may be stale (contract=%s)", cid)
	}
	nothingOK := true
	nothingDetail := ""
	for i := 1; i <= 3; i++ {
		if i > 1 {
			time.Sleep(10 * time.Second)
		}
		s := vstatus(t, d, ctx, 1, cid, "migrateVault", "")
		snap := vf24Snapshot(t, d, ctx, cid)
		sameSpends := vf24SameIds(base.spends, snap.spends)
		sameUtxo := bytes.Equal(base.utxoReg, snap.utxoReg)
		sameVault := bytes.Equal(base.vaultReg, snap.vaultReg)
		sameSweeps := bytes.Equal(base.sweepIdx, snap.sweepIdx)
		stillRetiring := snap.gen0Stat == int(btcvault.VaultStatusRetiring)
		stillOne := snap.gen0Cnt == 1
		if !isOK(s) || !sameSpends || !sameUtxo || !sameVault || !sameSweeps || !stillRetiring || !stillOne {
			nothingOK = false
		}
		nothingDetail += fmt.Sprintf(" try%d(status=%s pendingSpends=%d sameSpends=%v rByteEqual=%v vByteEqual=%v mslByteEqual=%v gen0Status=%d gen0Utxos=%d)",
			i, s, len(snap.spends), sameSpends, sameUtxo, sameVault, sameSweeps, snap.gen0Stat, snap.gen0Cnt)
	}
	c.rec("F24-NOTHING", "migrateVault reports success but builds no sweep at all: the pending spend list, the UTXO registry, the vault registry and the migration sweep index are byte-unchanged and generation 0 is still Retiring with 1 UTXO",
		nothingOK,
		fmt.Sprintf("baseline pendingSpends=%d gen0Utxos=%d gen0Status=%d;%s | the contract returns the string \"nothing to migrate for generation 0\", which no helper in tests/devnet can read (there is no contract-output log or Results reader here), so the byte-level state comparison above is the proof",
			len(base.spends), base.gen0Cnt, base.gen0Stat, nothingDetail))

	// ---------------------------------------------------------------------------
	// 3. F24-NN3: no new key while the superseded generation still "holds funds".
	// ---------------------------------------------------------------------------
	nn3Status := vstatus(t, d, ctx, 1, cid, "createKey", "")
	gen2Status := vfVaultStatusOn(d, ctx, 2, cid, 2)
	c.rec("F24-NN3", "createKey is refused while the superseded generation still counts a registry UTXO that migration can never select (NN#3, AnyFundedSupersededGen)",
		!isOK(nn3Status) && gen2Status < 0,
		fmt.Sprintf("createKey status=%s (want FAILED or REVERTED), generation 2 vault status=%d (want -1, absent), gen0Utxos=%d",
			nn3Status, gen2Status, vfGenUtxoCountOn(d, ctx, 2, cid, 0)))

	// ---------------------------------------------------------------------------
	// 4. F24-BONDLOCK: and the committee cannot leave either. This is the deadlock.
	// ---------------------------------------------------------------------------
	const unstakeNode = 3
	member := "hive:" + d.witnessAccount(unstakeNode)
	head, herr := getHeadBlock(d.HiveRPCEndpoint())
	if herr != nil {
		t.Errorf("reading the Hive head block before the locked unstake: %v", herr)
	}
	_ = head
	// Terminal-status read (H-25): FAILED + pending 0 is a refusal; a fixed wait cannot
	// tell a refusal from an op that has not been applied yet.
	lockedStatus, lockedPending := vfUnstakeVerdict(t, d, ctx, unstakeNode, 3*time.Minute)
	c.rec("F24-BONDLOCK", "the committee member's consensus bond stays locked while the stuck UTXO keeps its generation superseded and fund-holding, so nothing in the contract can move the funds and nobody can leave",
		lockedStatus == "FAILED" && lockedPending == 0,
		fmt.Sprintf("member=%s unstake status=%s (want FAILED = refused) pending consensus_unstake=%d (want 0), gen0Status=%d gen0Utxos=%d",
			member, lockedStatus, lockedPending, vfVaultStatusOn(d, ctx, 2, cid, 0), vfGenUtxoCountOn(d, ctx, 2, cid, 0)))

	// ---------------------------------------------------------------------------
	// 5. THE ESCAPE. On devnet the unmap's header still exists, so the missing
	//    confirmSpend can still be issued. Relay whatever is not relayed yet, then
	//    confirm the unmap with the CHANGE vout in indices.
	// ---------------------------------------------------------------------------
	// VR2-06: the settle waits MinConfirmationDepth, so the unmap's own block has to
	// be buried before confirmSpend is accepted. Relaying only up to unmapHeight
	// leaves it at depth 0 and the escape would be refused for the wrong reason —
	// which would make this test claim the deadlock is inescapable when it is not.
	// Mirrors constants.MinConfirmationDepth (regtest).
	if _, mErr := d.MineBlocks(ctx, vfDepositMaturityBlocks); mErr != nil {
		t.Logf("mine settle-maturity blocks: %v", mErr)
	}
	relayTo := unmapHeight + uint64(vfDepositMaturityBlocks)

	last := contractLastHeight(t, d, ctx, cid)
	for hh := last + 1; hh <= relayTo; hh++ {
		hx, _ := btcBlockHeaderHex(ctx, d, hh)
		if s := vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":10}`, hx)); !isOK(s) {
			t.Logf("addBlocks %d status=%s", hh, s)
		}
	}
	bhash, _ := d.bitcoinCli(ctx, "getblockhash", fmt.Sprint(unmapHeight))
	blockJSON, _ := d.bitcoinCli(ctx, "getblock", bhash, "1")
	var blk struct {
		Tx []string `json:"tx"`
	}
	if jerr := json.Unmarshal([]byte(blockJSON), &blk); jerr != nil {
		t.Errorf("parsing the unmap block %d: %v", unmapHeight, jerr)
	}
	if len(blk.Tx) != 2 {
		t.Errorf("the unmap block %d holds %d txs (want 2, coinbase plus the unmap): the 2-tx merkle proof below is only valid for a 2-tx block", unmapHeight, len(blk.Tx))
	}
	rawTx, _ := d.bitcoinCli(ctx, "getrawtransaction", bcTxid)
	proof := reverseHexBytes(blk.Tx[0])
	indices := "0,1"
	if changeVout >= 0 {
		indices = strconv.Itoa(changeVout)
	} else {
		t.Logf("the vault change output could not be identified, confirming with every vout index instead (settleUnmap identifies the change by address, so the settle is unaffected)")
	}
	confirmStatus := vstatus(t, d, ctx, 1, cid, "confirmSpend", fmt.Sprintf(
		`{"tx_data":{"block_height":%d,"raw_tx_hex":"%s","merkle_proof_hex":"%s","tx_index":1},"indices":[%s]}`,
		unmapHeight, rawTx, proof, indices))
	c.rec("F24-ESCAPE-CONFIRM", "the only escape is the missing confirmSpend: relaying the header and confirming the unmap with the change vout is accepted",
		isOK(confirmStatus),
		fmt.Sprintf("confirmSpend status=%s at height %d with indices=[%s] for txid=%s", confirmStatus, unmapHeight, indices, bcTxid))

	// ---- F24-PROMOTED: the change is now a selectable generation 0 UTXO. ----
	promoted := false
	promotedDetail := ""
	promoteDeadline := time.Now().Add(3 * time.Minute)
	for {
		reg := vf24ReadRegistry(t, d, ctx, 2, cid)
		entries := vf24EntriesOfGen(reg, 0)
		selectable := vf24SelectableOfGen(reg, 0)
		promotedDetail = fmt.Sprintf("registry=[%s] gen0Entries=%d gen0Selectable=%d", vf24Line(reg), len(entries), len(selectable))
		// The unmap builder splits its change into several outputs (four on this
		// contract; F8 and F21 registry dumps), so the promotion yields SEVERAL selectable
		// gen-0 entries, all belonging to the unmap transaction. Run 1 expected exactly one.
		allFromUnmap := len(selectable) >= 1
		for _, e := range selectable {
			if e.txid != bcTxid {
				allFromUnmap = false
			}
		}
		if len(entries) >= 1 && len(entries) == len(selectable) && allFromUnmap {
			promoted = true
			break
		}
		if time.Now().After(promoteDeadline) {
			break
		}
		time.Sleep(15 * time.Second)
	}
	c.rec("F24-PROMOTED", "the confirmed change becomes a migration-selectable generation 0 UTXO (confirmed pool id, no longer reserved) belonging to the unmap transaction",
		promoted,
		fmt.Sprintf("%s (wanted every gen-0 entry selectable and belonging to txid=%s; the change is split across several vouts, first change vout=%d)", promotedDetail, bcTxid, changeVout))

	// ---- F24-DRAINED: the rotation can now finish. ----
	drained := false
	tranches := 0
	drainDetail := ""
	for i := 0; i < 3 && !drained; i++ {
		if vfGenUtxoCountOn(d, ctx, 2, cid, 0) == 0 {
			drained = true
			break
		}
		migrateAndSettle(t, d, ctx, cid, cid+"-main", primary1, backupPubKeyG)
		tranches++
		settleDeadline := time.Now().Add(3 * time.Minute)
		for {
			g0 := vfGenUtxoCountOn(d, ctx, 2, cid, 0)
			g1 := vfGenUtxoCountOn(d, ctx, 2, cid, 1)
			drainDetail = fmt.Sprintf("after tranche %d: gen0Utxos=%d gen1Utxos=%d gen0Status=%d",
				tranches, g0, g1, vfVaultStatusOn(d, ctx, 2, cid, 0))
			if g0 == 0 {
				drained = true
				break
			}
			if time.Now().After(settleDeadline) {
				break
			}
			time.Sleep(15 * time.Second)
		}
		if !drained {
			fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
		}
	}
	c.rec("F24-DRAINED", "with the stuck output made selectable the migration sweep builds, signs, broadcasts and settles, and generation 0 drains to 0 UTXOs",
		drained,
		fmt.Sprintf("%d tranche(s), %s", tranches, drainDetail))

	// ---- F24-NN3-RELEASED: createKey is admitted again. ----
	releaseStatus := vstatus(t, d, ctx, 1, cid, "createKey", "")
	newGenStatus := -1
	for i := 0; i < 8; i++ {
		newGenStatus = vfVaultStatusOn(d, ctx, 2, cid, 2)
		if newGenStatus >= 0 {
			break
		}
		time.Sleep(10 * time.Second)
	}
	c.rec("F24-NN3-RELEASED", "once the superseded generation really holds nothing, createKey is admitted again and mints generation 2 as Pending",
		isOK(releaseStatus) && newGenStatus == int(btcvault.VaultStatusPending),
		fmt.Sprintf("createKey status=%s, generation 2 vault status=%d (want %d Pending), gen0Utxos=%d",
			releaseStatus, newGenStatus, int(btcvault.VaultStatusPending), vfGenUtxoCountOn(d, ctx, 2, cid, 0)))
	// Drop the pending generation again so the end state stays a single active vault.
	discardStatus := vstatus(t, d, ctx, 1, cid, "discardPendingKey", "")
	t.Logf("discardPendingKey status=%s, generation 2 vault status now %d", discardStatus, vfVaultStatusOn(d, ctx, 2, cid, 2))

	// ---- F24-BOND-RELEASED ----
	// The bond lock is keyed on the generation's STATUS, not on whether it holds funds
	// (modules/vaultrotation/eligibility.go locks Retiring, Draining AND Inactive; only
	// Purged releases), so releasing it needs the generation to leave those states.
	// Stage 1 retires the drained generation to Inactive and retries the unstake;
	// if the bond is still held, stage 2 completes the purge (the grace window plus
	// batched header relay, the vault_drain_complete_test.go recipe) and retries again.
	// The case records WHICH status finally released the bond.
	retire1 := vstatus(t, d, ctx, 1, cid, "retireVault", "")
	statAfterRetire := vaultStatusOf(t, d, ctx, cid, 0)
	head2, herr2 := getHeadBlock(d.HiveRPCEndpoint())
	if herr2 != nil {
		t.Errorf("reading the Hive head block before the release unstake: %v", herr2)
	}
	_ = head2
	releasedStatus, releasedPending := vfUnstakeVerdict(t, d, ctx, unstakeNode, 3*time.Minute)
	releaseStage := fmt.Sprintf("stage1(retireVault status=%s, gen0Status=%d, unstake status=%s, pending=%d)", retire1, statAfterRetire, releasedStatus, releasedPending)

	if releasedPending == 0 {
		enough := true
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < 10*time.Minute {
			enough = false
		}
		if !enough {
			releaseStage += "; stage2(purge) SKIPPED: less than 10 minutes of context budget left"
		} else {
			lastH := contractLastHeight(t, d, ctx, cid)
			h, merr := d.MineBlocks(ctx, 150) // VaultPurgeGraceBlocks is 144
			if merr != nil {
				t.Errorf("mining the purge grace window: %v", merr)
			}
			const relayBatch = 25
			for start := lastH + 1; start <= h; start += relayBatch {
				var hexBatch string
				for hh := start; hh < start+relayBatch && hh <= h; hh++ {
					hx, _ := btcBlockHeaderHex(ctx, d, hh)
					hexBatch += hx
				}
				if s := vstatus(t, d, ctx, 1, cid, "addBlocks",
					fmt.Sprintf(`{"blocks":"%s","latest_fee":10}`, hexBatch)); !isOK(s) {
					t.Logf("purge relay batch from %d status=%s", start, s)
				}
			}
			retire2 := vstatus(t, d, ctx, 1, cid, "retireVault", "")
			statAfterPurge := vaultStatusOf(t, d, ctx, cid, 0)
			head3, _ := getHeadBlock(d.HiveRPCEndpoint())
			_ = head3
			var releasedStatus2 string
			releasedStatus2, releasedPending = vfUnstakeVerdict(t, d, ctx, unstakeNode, 3*time.Minute)
			releaseStage += fmt.Sprintf("; stage2(retireVault status=%s, gen0Status=%d after mining and relaying 150 blocks for the 144-block purge grace window in batches of %d, unstake status=%s, pending=%d)",
				retire2, statAfterPurge, relayBatch, releasedStatus2, releasedPending)
		}
	}
	c.rec("F24-BOND-RELEASED", "with the generation drained and out of the bond-locked statuses the same consensus_unstake is accepted",
		releasedPending > 0,
		fmt.Sprintf("member=%s pending consensus_unstake=%d (want >0); %s | the lock is status-keyed (Retiring, Draining and Inactive are all locked, only Purged releases), see modules/vaultrotation/eligibility.go",
			member, releasedPending, releaseStage))

	// ---- F24-PRUNED (INFO) ----
	c.rec("F24-PRUNED", "why the escape above does not exist on the live testnet", true,
		fmt.Sprintf("INFO (not a pass): step 5 worked here only because the unmap's block header (height %d) is still inside the contract's header retention window on this devnet. On the live testnet the equivalent legacy withdrawals were mined thousands of blocks ago and their headers have been pruned (retention 1,080 on the deployed contract, 4,608 on v2), so verifyTransaction can no longer validate any SPV proof for them and confirmSpend is impossible. No other contract entry point can promote, reindex, expire or migrate such a UTXO, so the deadlock proved by F24-NOTHING, F24-NN3 and F24-BONDLOCK is PERMANENT there: the retiring generation can never drain, no successor key can be minted, and the committee's bonds stay locked",
			unmapHeight))

	// ---- F24-IDENT ----
	vfAssertContractIdentical(c, d, ctx, cid, vfAllNodes(5), 4*time.Minute, "F24-IDENT")

	c.summary("F24")
	t.Logf("F24 COMPLETE CONTRACT=%s (stuck unmap %s at height %d, escape confirmSpend=%s, drained=%v)",
		cid, unmapTxid, unmapHeight, confirmStatus, drained)
}

// vf24ConfirmedPoolStart mirrors btc-mapping-contract/contract/constants/constants.go
// UtxoConfirmedPoolStart: ids 0..1023 are the unconfirmed pool (change pending
// confirmation), 1024..65535 the confirmed pool. getMigrationInputs skips everything
// below this boundary, which is one half of the F24 deadlock.
const vf24ConfirmedPoolStart uint16 = 1024

// vf24Utxo is one decoded entry of the contract's UTXO registry ("r") joined with its
// per-UTXO record ("u-<hex id>") and its reservation marker ("ru-<decimal id>", set at
// unmap build and cleared at settleUnmap).
type vf24Utxo struct {
	id       uint16
	amount   int64
	gen      uint32
	txid     string
	vout     uint32
	reserved bool
}

// selectable reports whether getMigrationInputs could pick this UTXO: it must live in
// the confirmed pool AND not be reserved by an in-flight unmap. Both clauses are read
// from btc-mapping-contract/contract/mapping/migration.go getMigrationInputs.
func (u vf24Utxo) selectable() bool {
	return u.id >= vf24ConfirmedPoolStart && !u.reserved
}

// vf24ReadRegistry decodes "r" from one node (8 bytes per entry: 2-byte big-endian id
// plus a 6-byte big-endian amount) and joins every entry with its "u-" record
// (32-byte txid, 4-byte vout, 8-byte amount, script, tag, trailing 4-byte generation)
// and its "ru-" reservation marker.
func vf24ReadRegistry(t *testing.T, d *Devnet, ctx context.Context, node int, cid string) []vf24Utxo {
	t.Helper()
	st, err := getStateHex(d, ctx, node, cid, []string{"r"})
	if err != nil {
		t.Logf("vf24ReadRegistry: reading r on magi-%d failed: %v", node, err)
		return nil
	}
	reg := st["r"]
	var out []vf24Utxo
	var keys []string
	for off := 0; off+8 <= len(reg); off += 8 {
		id := binary.BigEndian.Uint16(reg[off : off+2])
		var amt [8]byte
		copy(amt[2:], reg[off+2:off+8])
		out = append(out, vf24Utxo{id: id, amount: int64(binary.BigEndian.Uint64(amt[:]))})
		keys = append(keys, "u-"+strconv.FormatUint(uint64(id), 16), "ru-"+strconv.FormatUint(uint64(id), 10))
	}
	if len(out) == 0 {
		return nil
	}
	vals := map[string][]byte{}
	// getStateByKeys accepts at most 100 keys per query.
	for start := 0; start < len(keys); start += 100 {
		end := start + 100
		if end > len(keys) {
			end = len(keys)
		}
		chunk, cerr := getStateHex(d, ctx, node, cid, keys[start:end])
		if cerr != nil {
			t.Logf("vf24ReadRegistry: reading utxo records on magi-%d failed: %v", node, cerr)
			continue
		}
		for k, v := range chunk {
			vals[k] = v
		}
	}
	for i := range out {
		raw := vals["u-"+strconv.FormatUint(uint64(out[i].id), 16)]
		if len(raw) >= 36 {
			out[i].txid = hex.EncodeToString(raw[0:32])
			out[i].vout = binary.BigEndian.Uint32(raw[32:36])
		}
		if len(raw) >= 4 {
			out[i].gen = binary.BigEndian.Uint32(raw[len(raw)-4:])
		}
		out[i].reserved = len(vals["ru-"+strconv.FormatUint(uint64(out[i].id), 10)]) > 0
	}
	return out
}

// vf24EntriesOfGen returns every registry entry belonging to a generation, whether or
// not migration could ever select it. This is the set NN#3 and the bond lock count.
func vf24EntriesOfGen(us []vf24Utxo, gen uint32) []vf24Utxo {
	var out []vf24Utxo
	for _, u := range us {
		if u.gen == gen {
			out = append(out, u)
		}
	}
	return out
}

// vf24SelectableOfGen returns the entries of a generation that getMigrationInputs
// could actually sweep.
func vf24SelectableOfGen(us []vf24Utxo, gen uint32) []vf24Utxo {
	var out []vf24Utxo
	for _, u := range us {
		if u.gen == gen && u.selectable() {
			out = append(out, u)
		}
	}
	return out
}

// vf24Line renders a registry for the log.
func vf24Line(us []vf24Utxo) string {
	if len(us) == 0 {
		return "empty"
	}
	out := ""
	for i, u := range us {
		if i > 0 {
			out += " "
		}
		pool := "confirmed"
		if u.id < vf24ConfirmedPoolStart {
			pool = "UNCONFIRMED"
		}
		txid := u.txid
		if len(txid) > 12 {
			txid = txid[:12]
		}
		out += fmt.Sprintf("id=%d(%s gen=%d amount=%d reserved=%v selectable=%v tx=%s:%d)",
			u.id, pool, u.gen, u.amount, u.reserved, u.selectable(), txid, u.vout)
	}
	return out
}

// vf24DeadlockForm names which clause of getMigrationInputs is holding the entries
// back, so the report can say whether this run reproduced the testnet's
// unconfirmed-pool form or the sibling reserved-input form.
func vf24DeadlockForm(us []vf24Utxo) string {
	unconfirmed, reserved, free := 0, 0, 0
	for _, u := range us {
		switch {
		case u.id < vf24ConfirmedPoolStart:
			unconfirmed++
		case u.reserved:
			reserved++
		default:
			free++
		}
	}
	return fmt.Sprintf("%d unconfirmed-pool (the live testnet form), %d reserved by an in-flight unmap (the fresh-deploy form), %d selectable",
		unconfirmed, reserved, free)
}

// vf24State is every piece of contract state a successful HandleMigrateVault would
// have to touch. A "nothing to migrate" call must leave all of it untouched.
type vf24State struct {
	spends   []string // "p" pending spend txids
	vaultReg []byte   // "v" vault registry
	utxoReg  []byte   // "r" UTXO registry
	sweepIdx []byte   // "msl" migration sweep index
	gen0Cnt  int
	gen0Stat int
}

// vf24Snapshot takes that snapshot from magi-2 (the node txSpendIds always reads, so
// every field comes from one replica).
func vf24Snapshot(t *testing.T, d *Devnet, ctx context.Context, cid string) vf24State {
	t.Helper()
	st, err := getStateHex(d, ctx, 2, cid, []string{"v", "r", "msl"})
	if err != nil {
		t.Logf("vf24Snapshot: state read on magi-2 failed: %v", err)
	}
	return vf24State{
		spends:   txSpendIds(t, d, ctx, cid),
		vaultReg: st["v"],
		utxoReg:  st["r"],
		sweepIdx: st["msl"],
		gen0Cnt:  vfGenUtxoCountOn(d, ctx, 2, cid, 0),
		gen0Stat: vfVaultStatusOn(d, ctx, 2, cid, 0),
	}
}

// vf24SameIds reports whether two pending spend id lists hold the same multiset.
func vf24SameIds(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := map[string]int{}
	for _, x := range a {
		counts[x]++
	}
	for _, x := range b {
		counts[x]--
		if counts[x] < 0 {
			return false
		}
	}
	return true
}
