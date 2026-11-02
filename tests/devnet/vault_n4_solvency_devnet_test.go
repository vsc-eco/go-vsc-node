//go:build evm_devnet

package devnet

// 11-02 parity batch, devnet proof of the BTC solvency watch (N4, 0.13.0).
//
// F27's phantom made real: a deposit the contract credited is reorged off
// Bitcoin by the depositor's own conflicting spend, so the registry lists a
// confirmed UTXO that Bitcoin no longer holds. That is what the watch exists
// to see: at the next checkpoint (every BTC_SOLVENCY_CHECK_EVERY Hive blocks)
// every witness finds the UTXO missing on its own bitcoind (gettxout), votes
// "vsc.btc_solvency" insolvent, 2/3 of the weight halts BTC vault keysigns on
// every node (BtcSolvencyHalted). A withdrawal requested during the halt is
// not signed; the governance hatch (vsc.tss_halt {"keyId":"btc_solvency",
// "active":false}) suspends the watch and clears the halt, and the held
// withdrawal signs. (THORChain halts a chain on an insolvent attestation
// quorum the same way; StopSolvencyCheck is its hatch.)
//
// Tagged evm_devnet only to share the gateway-multisig helpers with the BOTH
// tests; it runs no EVM.

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
	"go.mongodb.org/mongo-driver/bson"
)

// n4SolvencyHalted counts the nodes whose consensus state has the BTC
// solvency halt set (and returns the checkpoint votes seen on node 1).
func n4SolvencyHalted(t *testing.T, ctx context.Context, d *Devnet, nodes []int) (int, string) {
	t.Helper()
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)
	n, votes := 0, ""
	for _, node := range nodes {
		var doc struct {
			S struct {
				Halted   bool `bson:"halted"`
				WatchOff bool `bson:"watch_off"`
				Votes    struct {
					Checkpoint uint64   `bson:"checkpoint"`
					Insolvent  []string `bson:"insolvent"`
					Solvent    []string `bson:"solvent"`
				} `bson:"votes"`
			} `bson:"btc_solvency"`
		}
		if err := client.Database(d.nodeDbName(node)).Collection("chain_consensus_state").
			FindOne(ctx, bson.M{"_id": "singleton"}).Decode(&doc); err != nil {
			continue
		}
		if node == nodes[0] {
			votes = fmt.Sprintf("checkpoint=%d insolvent=%v solvent=%v watch_off=%v",
				doc.S.Votes.Checkpoint, doc.S.Votes.Insolvent, doc.S.Votes.Solvent, doc.S.WatchOff)
		}
		if doc.S.Halted {
			n++
		}
	}
	return n, votes
}

// n4ReorgOutDeposit is F27 steps 1-2: credit a second deposit, then mine the
// depositor's conflicting spend on a competing branch past the old tip and
// relay the reorg. Returns the reorged-out deposit's txid.
func n4ReorgOutDeposit(t *testing.T, d *Devnet, ctx context.Context, env *vfEnv, depositor string, sats int64) string {
	t.Helper()
	cid := env.cid
	hBefore, _ := d.BitcoinHeight(ctx)
	fundVaultViaSPV(t, d, ctx, cid, env.primary0, backupPubKeyG, depositor, sats, contractLastHeight(t, d, ctx, cid))
	hD := hBefore + 1
	hashD, _ := d.bitcoinCli(ctx, "getblockhash", fmt.Sprint(hD))
	blockJSON, _ := d.bitcoinCli(ctx, "getblock", hashD, "1")
	var blk struct {
		Tx []string `json:"tx"`
	}
	json.Unmarshal([]byte(blockJSON), &blk)
	if len(blk.Tx) != 2 {
		t.Fatalf("PRECONDITION: deposit block %d has %d txs (want coinbase + deposit)", hD, len(blk.Tx))
	}
	txD := blk.Tx[1]
	devnetSleep(10 * time.Second)
	if has, _, _ := f25RegistryEntryForTxid(d, ctx, 2, cid, txD); !has || balanceSats(t, d, ctx, cid, depositor) != sats {
		t.Fatalf("PRECONDITION: the deposit %s was not credited", txD)
	}

	rawDJSON, _ := d.bitcoinCli(ctx, "getrawtransaction", txD, "1")
	var dtx struct {
		Vin []struct {
			Txid string `json:"txid"`
			Vout uint32 `json:"vout"`
		} `json:"vin"`
	}
	json.Unmarshal([]byte(rawDJSON), &dtx)
	if len(dtx.Vin) == 0 {
		t.Fatalf("PRECONDITION: deposit %s has no inputs", txD)
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
	inValue, inScript := 0.0, ""
	for _, o := range ptx.Vout {
		if o.N == dtx.Vin[0].Vout {
			inValue, inScript = o.Value, o.ScriptPubKey.Hex
		}
	}
	if inValue <= 0.0002 || inScript == "" {
		t.Fatalf("PRECONDITION: cannot read the deposit's input (%f, %q)", inValue, inScript)
	}
	back, _ := d.bitcoinCli(ctx, "getnewaddress")
	rawC, err := d.bitcoinCli(ctx, "createrawtransaction",
		fmt.Sprintf(`[{"txid":"%s","vout":%d}]`, dtx.Vin[0].Txid, dtx.Vin[0].Vout),
		fmt.Sprintf(`{"%s":%.8f}`, back, inValue-0.0001))
	if err != nil {
		t.Fatalf("PRECONDITION: createrawtransaction: %v", err)
	}
	prevtxs := fmt.Sprintf(`[{"txid":"%s","vout":%d,"scriptPubKey":"%s","amount":%.8f}]`, dtx.Vin[0].Txid, dtx.Vin[0].Vout, inScript, inValue)
	signedJSON, err := d.bitcoinCli(ctx, "signrawtransactionwithwallet", rawC, prevtxs)
	if err != nil {
		t.Fatalf("PRECONDITION: signrawtransactionwithwallet: %v", err)
	}
	var signed struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	json.Unmarshal([]byte(signedJSON), &signed)
	if !signed.Complete {
		t.Fatalf("PRECONDITION: the conflicting spend did not sign: %s", signedJSON)
	}
	var mtx wire.MsgTx
	txC := ""
	if raw, err := hex.DecodeString(signed.Hex); err == nil && mtx.Deserialize(bytes.NewReader(raw)) == nil {
		txC = mtx.TxHash().String()
	}
	oldTip, _ := d.BitcoinHeight(ctx)
	if _, err := d.bitcoinCli(ctx, "invalidateblock", hashD); err != nil {
		t.Fatalf("PRECONDITION: invalidateblock %s: %v", hashD, err)
	}
	minerAddr, _ := d.bitcoinCli(ctx, "getnewaddress")
	if _, err := d.bitcoinCli(ctx, "generateblock", minerAddr, fmt.Sprintf(`["%s"]`, signed.Hex)); err != nil {
		t.Logf("generateblock with the conflict failed (%v); RBF broadcast instead", err)
		d.bitcoinCli(ctx, "sendrawtransaction", signed.Hex)
		d.MineBlocks(ctx, 1)
	}
	newTip, _ := d.MineBlocks(ctx, 1)
	for i := 0; newTip <= oldTip && i < 8; i++ {
		newTip, _ = d.MineBlocks(ctx, 1)
	}
	if newTip <= oldTip {
		t.Fatalf("PRECONDITION: competing branch stuck at %d (old tip %d)", newTip, oldTip)
	}
	_, dIn := f9TxConfirmed(d, ctx, txD)
	_, cIn := f9TxConfirmed(d, ctx, txC)
	if dIn || !cIn {
		t.Fatalf("PRECONDITION: reorg did not take (deposit confirmed=%v conflict confirmed=%v)", dIn, cIn)
	}
	// Relay the reorg so the contract's header tip is the new chain (the watch
	// abstains while its bitcoind is behind the contract's relayed height).
	last := contractLastHeight(t, d, ctx, cid)
	if repl := f9ReplacementHeaders(t, d, ctx, 2, cid, last); len(repl) > 0 {
		vstatus(t, d, ctx, 1, cid, "replaceBlocks", strings.Join(repl, ""))
	}
	for hh := last + 1; hh <= newTip; hh++ {
		hx, herr := btcBlockHeaderHex(ctx, d, hh)
		if herr != nil {
			break
		}
		vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":10}`, hx))
	}
	if f25TxOutExists(d, ctx, txD, 0) {
		t.Fatalf("PRECONDITION: Bitcoin still holds the deposit output")
	}
	t.Logf("deposit %s (%d sats) reorged off Bitcoin by %s; the registry still lists it", txD, sats, txC)
	return txD
}

func TestVaultN4_PhantomDepositHaltsBtcSigning(t *testing.T) {
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
	// 0.13 from epoch 1: the watch is on.
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = 13
	d, _ := startDevnetNoKey(t, cfg, 110*time.Minute)
	nodes := vfAllNodes(d.cfg.Nodes)

	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "vault N4 solvency watch")
	cid := env.cid
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)
	if n, v := n4SolvencyHalted(t, ctx, d, nodes); n != 0 {
		t.Fatalf("PRECONDITION: solvency halt set before any phantom (%d nodes; %s)", n, v)
	}

	// 1-2. A 5,000,000 sat deposit is credited, then reorged off Bitcoin.
	n4ReorgOutDeposit(t, d, ctx, env, "hive:"+d.witnessAccount(2), 5_000_000)
	start := devnetNow()

	// 3. The next checkpoint's votes halt BTC keysigns on every node.
	var n int
	var votes string
	for deadline := devnetNow().Add(35 * time.Minute); devnetNow().Before(deadline); devnetSleep(20 * time.Second) {
		if n, votes = n4SolvencyHalted(t, ctx, d, nodes); n == len(nodes) {
			break
		}
	}
	if n != len(nodes) {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("N4: the phantom deposit did not halt BTC signing on every node (%d/%d; %s)", n, len(nodes), votes)
	}
	t.Logf("N4: BTC solvency halt on all %d nodes after %s (%s)", n, devnetNow().Sub(start).Round(time.Second), votes)

	// 4. A withdrawal requested during the halt is not signed.
	spendsBefore := txSpendIds(t, d, ctx, cid)
	dest, _ := d.bitcoinCli(ctx, "getnewaddress")
	um := vstatus(t, d, ctx, 2, cid, "unmap", fmt.Sprintf(`{"amount":"%d","to":"%s"}`, 1_000_000, dest))
	if !isOK(um) {
		t.Fatalf("N4: unmap during the halt: %s", um)
	}
	txU := ""
	for i := 0; i < 20 && txU == ""; i++ {
		devnetSleep(3 * time.Second)
		for _, id := range txSpendIds(t, d, ctx, cid) {
			if !contains(spendsBefore, id) {
				txU = id
			}
		}
	}
	sd := waitSigningData(t, d, ctx, cid, txU)
	if sd == nil {
		t.Fatalf("N4: the withdrawal %q produced no signing request", txU)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if _, signed := f25AwaitSignatures(t, d, ctx, cid+"-main", sd); signed {
			t.Fatalf("N4: the BTC withdrawal was signed during the solvency halt")
		}
	}
	t.Log("N4: no signature for the withdrawal during the halt (~4 min)")

	// 5. The hatch: suspend the watch, which clears the halt.
	payload, _ := json.Marshal(map[string]any{"active": false, "keyId": "btc_solvency"})
	for deadline := devnetNow().Add(15 * time.Minute); devnetNow().Before(deadline); devnetSleep(15 * time.Second) {
		if _, err := broadcastGatewayMultisig(t, d, "vsc.tss_halt", []string{"vsc.gateway"}, string(payload), devnetGatewayKeys(t, d)); err != nil {
			t.Logf("hatch broadcast: %s", firstLine(err.Error()))
			continue
		}
		if n, votes = n4SolvencyHalted(t, ctx, d, nodes); n == 0 {
			break
		}
	}
	if n != 0 {
		t.Fatalf("N4: the hatch did not clear the halt (%d still halted; %s)", n, votes)
	}
	t.Logf("N4: hatch cleared the halt (%s)", votes)

	// 6. The held withdrawal now signs.
	signed := false
	for attempt := 1; attempt <= 5 && !signed; attempt++ {
		_, signed = f25AwaitSignatures(t, d, ctx, cid+"-main", sd)
	}
	if !signed {
		t.Fatal("N4: the withdrawal held by the halt never signed after the hatch")
	}
	t.Logf("N4 COMPLETE: phantom -> halt on all nodes -> hatch -> held withdrawal signed (%s)", devnetNow().Sub(start).Round(time.Second))
}
