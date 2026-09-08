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

	"github.com/btcsuite/btcd/wire"
)

// TestVaultF27DepositReorgedOut is failure-state F27 of the BTC vault suite: a deposit
// that the contract has already credited is removed from Bitcoin by a reorg carrying the
// depositor's own conflicting spend.
//
// Two thresholds now stack. The node oracle relays headers `validityThreshold` blocks
// behind the tip (2 on mainnet, 0 on devnet — modules/oracle/chain/bitcoin.go:55,59), and
// since VR2-07 the CONTRACT enforces its own maturity gate on top
// (constants.MinConfirmationDepth: 4 on mainnet, 2 on testnet and regtest), so a credited
// deposit is buried ~6 deep on mainnet and 2 deep on devnet before it can be credited at
// all. This test therefore reorgs DEEPER than the gate: fundVaultViaSPV mines the
// maturity margin on top of the deposit's block, so invalidating that block removes the
// margin blocks with it, and the competing branch is mined past the old tip.
//
// That the gate cannot be reached by a shallower reorg is exactly the point — the gate
// widens the margin, it does not remove the limitation. Deposits confirmed against
// later-orphaned blocks still cannot be un-mapped (the contract's own docs record this as
// an accepted SPV limitation), and this test measures the consequence when a reorg does go
// deep enough: it is the SPV trust assumption made concrete, not a probability claim.
//
// SEQUENCE
//
//  1. The owner's 50,000,000 sat deposit backs the vault (vfSetup). A second account
//     deposits 5,000,000 sats and the maturity margin is mined; map credits it
//     (F27-CREDIT).
//
//  2. invalidateblock the deposit's block — which drops the maturity-margin blocks mined
//     on top of it, so this is a reorg deeper than the contract's own gate; mine a
//     competing block that contains the depositor's conflicting spend of the same input
//     back to their own wallet (generateblock; RBF fallback), then mine PAST the old tip;
//     relay the reorg with replaceBlocks + addBlocks (F27-REORG, instrument).
//
//  3. F27-PHANTOM-CREDIT: the second account's L2 balance is unchanged and the registry
//     still lists the deposit output, which Bitcoin's UTXO set no longer holds.
//
//  4. F27-THEFT: the second account withdraws 4,500,000 sats. The input selector takes
//     the FIRST registry entry large enough (contract/mapping/unmapping.go:205), which
//     is the owner's real 50,000,000 sat UTXO, so the spend is valid, gen-0 signs it and
//     Bitcoin accepts it: coins that back the owner's balance leave the vault against a
//     credit that was never funded.
//
//  5. F27-INSOLVENT: after the withdrawal settles, L2 balances exceed the coins the
//     vault actually holds by the phantom amount.
//
//  6. F27-IDENT.
//
//     VAULT_F27_RUN=1 go test -v -run TestVaultF27DepositReorgedOut -timeout 95m ./tests/devnet/
func TestVaultF27DepositReorgedOut(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F27_RUN") == "" {
		t.Skip("set VAULT_F27_RUN=1")
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

	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "vault F27 deposit reorged out")
	cid := env.cid
	owner := env.owner
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	finish := func() {
		vfAssertContractIdentical(c, d, ctx, cid, vfAllNodes(5), 4*time.Minute, "F27-IDENT")
		c.summary("F27")
		t.Logf("F27 COMPLETE CONTRACT=%s", cid)
	}

	// ---- 1. a second depositor is credited ----
	const phantomSats = 5_000_000
	depositor := "hive:" + d.witnessAccount(2)
	balOwner0 := balanceSats(t, d, ctx, cid, owner)
	hBefore, _ := d.BitcoinHeight(ctx)
	fundVaultViaSPV(t, d, ctx, cid, env.primary0, backupPubKeyG, depositor, phantomSats, contractLastHeight(t, d, ctx, cid))
	hD := hBefore + 1
	hashD, _ := d.bitcoinCli(ctx, "getblockhash", fmt.Sprint(hD))
	blockJSON, _ := d.bitcoinCli(ctx, "getblock", hashD, "1")
	var blk struct {
		Tx []string `json:"tx"`
	}
	json.Unmarshal([]byte(blockJSON), &blk)
	if len(blk.Tx) != 2 {
		t.Fatalf("PRECONDITION FAILED: deposit block %d has %d txs (want coinbase + deposit)", hD, len(blk.Tx))
	}
	txD := blk.Tx[1]
	time.Sleep(10 * time.Second)
	balDep0 := balanceSats(t, d, ctx, cid, depositor)
	hasD, idD, amtD := f25RegistryEntryForTxid(d, ctx, 2, cid, txD)
	c.rec("F27-CREDIT", "the second deposit is credited once it clears the contract maturity gate (MinConfirmationDepth=2 on regtest)",
		balDep0 == phantomSats && hasD,
		fmt.Sprintf("deposit txid=%s block=%d balance(%s)=%d registry id=%d amount=%d", txD, hD, depositor, balDep0, idD, amtD))
	if balDep0 != phantomSats || !hasD {
		finish()
		return
	}

	// ---- 2. the depositor double-spends the deposit on a competing branch ----
	rawDJSON, _ := d.bitcoinCli(ctx, "getrawtransaction", txD, "1")
	var dtx struct {
		Vin []struct {
			Txid string `json:"txid"`
			Vout uint32 `json:"vout"`
		} `json:"vin"`
	}
	json.Unmarshal([]byte(rawDJSON), &dtx)
	if len(dtx.Vin) == 0 {
		t.Fatalf("PRECONDITION FAILED: deposit %s has no inputs to double-spend", txD)
	}
	prevJSON, _ := d.bitcoinCli(ctx, "getrawtransaction", dtx.Vin[0].Txid, "1")
	var ptx struct {
		Vout []struct {
			Value        float64 `json:"value"`
			N            uint32  `json:"n"`
			ScriptPubKey struct {
				Hex string `json:"hex"`
			} `json:"scriptPubKey"`
		} `json:"vout"`
	}
	json.Unmarshal([]byte(prevJSON), &ptx)
	inValue := 0.0
	inScript := ""
	for _, o := range ptx.Vout {
		if o.N == dtx.Vin[0].Vout {
			inValue = o.Value
			inScript = o.ScriptPubKey.Hex
		}
	}
	if inValue <= 0.0002 || inScript == "" {
		t.Fatalf("PRECONDITION FAILED: cannot read the deposit's input value/script (%f, %q)", inValue, inScript)
	}
	back, _ := d.bitcoinCli(ctx, "getnewaddress")
	rawC, err := d.bitcoinCli(ctx, "createrawtransaction",
		fmt.Sprintf(`[{"txid":"%s","vout":%d}]`, dtx.Vin[0].Txid, dtx.Vin[0].Vout),
		fmt.Sprintf(`{"%s":%.8f}`, back, inValue-0.0001))
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: createrawtransaction (conflict): %v", err)
	}
	// The input is already SPENT by the mined deposit from the wallet's point of view, so
	// the signer cannot look it up in its UTXO set (run 1: complete=false). Hand it the
	// previous output explicitly (prevtxs); the wallet still holds the key.
	prevtxs := fmt.Sprintf(`[{"txid":"%s","vout":%d,"scriptPubKey":"%s","amount":%.8f}]`, dtx.Vin[0].Txid, dtx.Vin[0].Vout, inScript, inValue)
	signedJSON, err := d.bitcoinCli(ctx, "signrawtransactionwithwallet", rawC, prevtxs)
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: signrawtransactionwithwallet (conflict): %v", err)
	}
	var signed struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	json.Unmarshal([]byte(signedJSON), &signed)
	if !signed.Complete {
		t.Fatalf("PRECONDITION FAILED: the conflicting spend did not sign completely: %s", signedJSON)
	}
	var ctx2 wire.MsgTx
	txC := ""
	if raw, err := hex.DecodeString(signed.Hex); err == nil && ctx2.Deserialize(bytes.NewReader(raw)) == nil {
		txC = ctx2.TxHash().String()
	}

	// Record the tip BEFORE invalidating. fundVaultViaSPV mined the contract's maturity
	// margin on top of the deposit's block, so invalidating hD drops those margin blocks
	// too — the competing branch has to be mined past this height, or the contract would
	// still hold headers at heights the new chain never reaches and could never be brought
	// to a canonical view (replaceBlocks would have nothing to replace them with).
	oldTip, err := d.BitcoinHeight(ctx)
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: read tip before the reorg: %v", err)
	}
	if _, err := d.bitcoinCli(ctx, "invalidateblock", hashD); err != nil {
		t.Fatalf("PRECONDITION FAILED: invalidateblock %s: %v", hashD, err)
	}
	minerAddr, _ := d.bitcoinCli(ctx, "getnewaddress")
	how := "generateblock"
	if _, err := d.bitcoinCli(ctx, "generateblock", minerAddr, fmt.Sprintf(`["%s"]`, signed.Hex)); err != nil {
		how = "sendrawtransaction+generatetoaddress"
		t.Logf("generateblock with the conflict failed (%v); falling back to an RBF broadcast", err)
		if _, err2 := d.bitcoinCli(ctx, "sendrawtransaction", signed.Hex); err2 != nil {
			t.Logf("RBF broadcast of the conflict rejected: %v", err2)
		}
		d.MineBlocks(ctx, 1)
	}
	// Extend the competing branch past the old tip (see oldTip above). Bounded so a branch
	// that refuses to grow fails the test rather than spinning.
	newTip, _ := d.MineBlocks(ctx, 1)
	for i := 0; newTip <= oldTip && i < 8; i++ {
		newTip, _ = d.MineBlocks(ctx, 1)
	}
	if newTip <= oldTip {
		t.Fatalf("PRECONDITION FAILED: competing branch stuck at %d, needs to pass the pre-reorg tip %d", newTip, oldTip)
	}
	_, dInChain := f9TxConfirmed(d, ctx, txD)
	cHash, cInChain := f9TxConfirmed(d, ctx, txC)
	cHeight := uint64(0)
	if cInChain {
		cHeight, _ = f9BlockHeightOf(d, ctx, cHash)
	}
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
	canonD, _ := btcBlockHeaderHex(ctx, d, hD)
	storedD := f9StoredHeaderHex(d, ctx, 2, cid, hD)
	c.rec("F27-REORG", "instrument: the deposit is gone from Bitcoin, the depositor's conflicting spend is mined at the same height, the contract holds the canonical header",
		!dInChain && cInChain && cHeight == hD && strings.EqualFold(canonD, storedD),
		fmt.Sprintf("method=%s deposit_confirmed=%v conflict=%s confirmed=%v height=%d (deposit was %d) replaceBlocks=%s depth=%d contract_header_canonical=%v",
			how, dInChain, txC, cInChain, cHeight, hD, replStatus, len(repl), strings.EqualFold(canonD, storedD)))
	if dInChain || !cInChain {
		finish()
		return
	}

	// ---- 3. the credit survives without backing ----
	time.Sleep(10 * time.Second)
	balDep1 := balanceSats(t, d, ctx, cid, depositor)
	hasD1, _, _ := f25RegistryEntryForTxid(d, ctx, 2, cid, txD)
	dUnspent := f25TxOutExists(d, ctx, txD, 0)
	phantom := f25PhantomSats(t, d, ctx, 2, cid)
	c.rec("F27-PHANTOM-CREDIT", "the L2 balance and the registry entry survive the reorg although Bitcoin no longer holds the deposit output",
		balDep1 == phantomSats && hasD1 && !dUnspent && phantom >= phantomSats,
		fmt.Sprintf("balance(%s)=%d registry_has_deposit=%v bitcoin_has_output=%v phantom_sats=%d", depositor, balDep1, hasD1, dUnspent, phantom))

	// ---- 4. the unfunded credit is withdrawn against the owner's coins ----
	dest, _ := d.bitcoinCli(ctx, "getnewaddress")
	const withdrawSats = 4_500_000
	spendsBefore := txSpendIds(t, d, ctx, cid)
	um := vstatus(t, d, ctx, 2, cid, "unmap", fmt.Sprintf(`{"amount":"%d","to":"%s"}`, withdrawSats, dest))
	txU := ""
	for i := 0; i < 20 && isOK(um) && txU == ""; i++ {
		time.Sleep(3 * time.Second)
		for _, id := range txSpendIds(t, d, ctx, cid) {
			if !contains(spendsBefore, id) {
				txU = id
			}
		}
	}
	theftDetail := fmt.Sprintf("unmap(%d by %s)=%s txid=%s", withdrawSats, depositor, um, txU)
	theft := false
	settled := false
	if txU != "" {
		sdU := waitSigningData(t, d, ctx, cid, txU)
		usesReal, usesPhantom := false, false
		if sdU != nil {
			var mtx wire.MsgTx
			if mtx.Deserialize(bytes.NewReader(sdU.Tx)) == nil {
				for _, in := range mtx.TxIn {
					if in.PreviousOutPoint.Hash.String() == txD {
						usesPhantom = true
					} else {
						usesReal = true
					}
				}
			}
		}
		rawU, signedU := "", false
		if sdU != nil {
			for attempt := 1; attempt <= 3 && !signedU; attempt++ {
				rawU, signedU = f25AwaitSignatures(t, d, ctx, cid+"-main", sdU)
			}
		}
		btcVerdict := "(not broadcast: unsigned)"
		if signedU {
			bcU, berr := d.bitcoinCli(ctx, "sendrawtransaction", rawU)
			if berr != nil {
				btcVerdict = strings.TrimSpace(berr.Error())
			} else {
				btcVerdict = "ACCEPTED " + bcU
				h, _ := d.MineBlocks(ctx, 1)
				changeVout := vfChangeVout(d, ctx, bcU, dest)
				if changeVout < 0 {
					changeVout = 1
				}
				cs := vfRelayAndConfirmIndex(t, d, ctx, 1, cid, bcU, h, changeVout)
				settled = isOK(cs)
				btcVerdict += fmt.Sprintf(" mined@%d confirmSpend=%s", h, cs)
			}
		}
		theft = usesReal && !usesPhantom && signedU && strings.HasPrefix(btcVerdict, "ACCEPTED")
		theftDetail += fmt.Sprintf(" inputs: real=%v phantom=%v signed_by_gen0=%v bitcoin=%q", usesReal, usesPhantom, signedU, btcVerdict)
	}
	c.rec("F27-THEFT", "the withdrawal of the unfunded credit is built from the owner's real UTXO (first-fit selection), signed by gen-0 and accepted by Bitcoin",
		theft, theftDetail)

	// ---- 5. the books no longer balance ----
	time.Sleep(10 * time.Second)
	balOwner1 := balanceSats(t, d, ctx, cid, owner)
	balDep2 := balanceSats(t, d, ctx, cid, depositor)
	regSum := f25RegistrySum(d, ctx, 2, cid)
	phantom2 := f25PhantomSats(t, d, ctx, 2, cid)
	real := regSum - phantom2
	l2 := balOwner1 + balDep2
	c.rec("F27-INSOLVENT", "after the withdrawal settles, L2 balances exceed the coins the vault holds by the phantom deposit",
		settled && phantom2 >= phantomSats && l2 > real && balOwner1 == balOwner0,
		fmt.Sprintf("settled=%v owner %d -> %d, depositor %d -> %d, L2 total=%d, registry sum=%d, phantom=%d, real backing=%d (L2 - real = %d)",
			settled, balOwner0, balOwner1, balDep0, balDep2, l2, regSum, phantom2, real, l2-real))

	finish()
}
