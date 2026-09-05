package devnet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"vsc-node/cmd/mapping-bot/chain"

	"github.com/btcsuite/btcd/chaincfg"
	"go.mongodb.org/mongo-driver/bson"
)

// TestVaultF17DepositAfterPurge is failure-state F17 of the BTC vault-rotation-v2
// suite: what happens to a Bitcoin deposit that arrives at a PURGED generation's
// deposit address, after the whole rotation has completed.
//
// WHAT IT REPRODUCES
// gen-0 is rotated out, drained to zero UTXOs, retired to Inactive, left alone for
// the full VaultPurgeGraceBlocks (144 regtest blocks) window, and retired again so
// it reaches PURGED. Only then does a user send BTC to gen-0's old deposit address
// and submit a valid SPV proof through map. This is the realistic late-deposit case:
// an exchange, a wallet or a bookmarked address that still points at the retired
// generation long after the operator finished rotating.
//
// WHY THE OUTCOME IS WHAT IT IS (read from the contract, not assumed)
// The deposit-matchable address set is built by depositAddressGenerations
// (contract/mapping/init.go), which keeps only generations passing
// isFundHoldingStatus (contract/mapping/vault_lifecycle.go): Active, Retiring,
// Draining, Inactive. PURGED is deliberately excluded, so a purged generation's
// address is no longer derived into MappingState.AddressRegistry and indexOutputs
// (contract/mapping/mapping.go) matches nothing in the deposit transaction. map
// therefore does not abort: processUtxos simply iterates an empty slice, so the
// call can settle cleanly while crediting NOTHING. That is the behaviour this test
// pins down, and it is why the case asserts on the BALANCE and the UTXO registry
// rather than on the map transaction's status.
//
// The state machine cannot save the depositor here either. An INACTIVE generation
// that gets re-funded reverts to DRAINING ("match-until-purged", the revert branch
// in ReconcileRetiringVaults), so a deposit landing one block before the purge is
// swept to the successor. Once PURGED there is no UTXO tagged to gen-0 at all, so
// the revert branch can never fire and the coins stay outside the contract's view.
//
// TWO STANDING GAPS THIS DOCUMENTS
//  1. Purge gate leg (d), the independent zero-balance attestation, is a PERMISSIVE
//     STUB: zeroBalanceAttested returns true unconditionally
//     (contract/mapping/vault_lifecycle.go). So the purge fires on emptiness plus
//     grace alone, with no external proof that the L1 address really holds zero.
//  2. The purge destroys NO key material. tss_db.KeyRetirementEnabled is false
//     (modules/db/vsc/tss/interface.go), and nothing deactivates a purged
//     generation's key at purge time (see the reshare-skip comment in
//     modules/vaultrotation/eligibility.go). So the gen-0 TSS key row stays alive on
//     every node.
//
// Put together: the funds are UNCREDITED but NOT DESTROYED. Nobody's VSC balance
// moves, and the BTC sits at a P2WSH whose primary key the committee still holds,
// so an operator can still recover it out of band. The failure mode is a silent
// loss of visibility, not a loss of coins.
//
// CASES
//   - F17-PURGED       gen-0 really reached status Purged (5) in the committed
//     vault registry on node 2, not merely "retireVault confirmed".
//   - F17-NOTCREDITED  a valid SPV-proven deposit to the purged gen-0 address
//     credits nothing: the recipient balance never moves, no UTXO
//     is added to the registry, and gen-0 does not revert to Draining.
//   - F17-CONTROL      positive control (see DEVIATIONS): the identical deposit
//     flow against the ACTIVE gen-1 address DOES credit, so the
//     non-credit above is caused by the purge and not by a broken
//     merkle proof, a stale header relay or a dead map path.
//   - F17-KEY-ALIVE    the gen-0 TSS key row is still present and not retired on
//     node 2 (and is logged for all 5), so the coins are still
//     recoverable by the key holders.
//   - F17-IDENT        vault contract state byte-identical across all 5 nodes.
//
// DEVIATIONS FROM THE SPEC
//   - F17-CONTROL is an extra case the spec does not list. Without it a
//     "not credited" observation is indistinguishable from a harness fault
//     (wrong merkle proof, unrelayed header, map path broken), which would make
//     the headline case a vacuous pass.
//   - F17-KEY-ALIVE passes on "key row present and NOT retired" rather than on the
//     literal string "active". Not-retired is the substantive claim (no key
//     destruction, funds recoverable); the exact status string is carried in the
//     case detail and logged for every node.
//
// A precondition that cannot be established (rotation, full drain, Inactive, Purged,
// or a two-transaction deposit block that the merkle-proof helper can prove) is a
// t.Fatalf, never a t.Skip, so this test can never pass vacuously.
//
// RUN:
//
//	VAULT_F17_RUN=1 DEVNET_KEEP=1 go test -v -run TestVaultF17DepositAfterPurge -timeout 50m ./tests/devnet/
func TestVaultF17DepositAfterPurge(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F17_RUN") == "" {
		t.Skip("set VAULT_F17_RUN=1")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		wasm = "/home/clauderfly/utxo-s1/btc-mapping-contract/bin/dev.wasm"
	}
	if _, err := os.Stat(wasm); err != nil {
		t.Fatalf("wasm: %v", err)
	}

	// hpin is AFTER genesis (~block 190) so gen-0 is minted on the v2-off path (no
	// fresh-genesis deadlock) and v2 is ON for the rotation, the drain and the purge.
	const hpin uint64 = 400

	cfg := tssTestConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 45*time.Minute)

	c := &vfCase{t: t}

	// SETUP: deploy, seed headers, wire the oracle, mint + register gen-0, fund it.
	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "F17 deposit after purge")
	cid := env.cid
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	// ROTATE gen-0 to gen-1. Everything below depends on this, so it is fatal.
	primary1, ok := vfRotate(t, d, ctx, cid, 1)
	if !ok {
		t.Fatalf("PRECONDITION FAILED: gen-0 never rotated to gen-1, so there is no retiring generation to drain and purge")
	}
	gen0Status := -1
	for i := 0; i < 8; i++ {
		gen0Status = vaultStatusOf(t, d, ctx, cid, 0)
		if gen0Status >= 2 {
			break
		}
		time.Sleep(10 * time.Second)
	}
	if gen0Status < 2 {
		t.Fatalf("PRECONDITION FAILED: gen-0 is %s after activateKey, it never entered the retiring path", statusStr(gen0Status))
	}
	t.Logf("gen-1 active, gen-0 status=%s", statusStr(gen0Status))

	// DRAIN gen-0 completely. Every tranche is settled end to end; the registry, not
	// a transaction status, decides when the generation is empty.
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
	for i := 0; i < 6; i++ {
		remaining := genUtxoCount(t, d, ctx, cid, 0)
		if remaining == 0 {
			break
		}
		t.Logf("tranche %d: gen-0 still holds %d UTXO(s), sweeping", i+1, remaining)
		migrateAndSettle(t, d, ctx, cid, cid+"-main", primary1, backupPubKeyG)
		if after := genUtxoCount(t, d, ctx, cid, 0); after >= remaining {
			t.Fatalf("PRECONDITION FAILED: tranche %d made no progress (%d to %d UTXOs), gen-0 cannot be drained so it can never be purged", i+1, remaining, after)
		}
		fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
	}
	if left := genUtxoCount(t, d, ctx, cid, 0); left != 0 {
		t.Fatalf("PRECONDITION FAILED: gen-0 still holds %d UTXO(s) after 6 tranches, purge requires an empty registry", left)
	}

	// RETIRE: draining to Inactive. This is the call that records InactiveHeight,
	// the anchor the purge grace window is measured from.
	vstatus(t, d, ctx, 1, cid, "retireVault", "")
	if st := vaultStatusOf(t, d, ctx, cid, 0); st != 4 {
		t.Fatalf("PRECONDITION FAILED: gen-0 is %s after retireVault, it must be Inactive before the grace window starts", statusStr(st))
	}

	// PURGE: mine past VaultPurgeGraceBlocks (144) and relay the headers in batches,
	// then retire again. addBlocks accepts many concatenated 80-byte headers, so ~25
	// headers per call keeps this to a handful of contract calls.
	h, err := d.MineBlocks(ctx, 150)
	if err != nil {
		t.Fatalf("mining the purge grace window: %v", err)
	}
	f17RelayHeaders(t, d, ctx, cid, h)
	vstatus(t, d, ctx, 1, cid, "retireVault", "")
	purgedStatus := vaultStatusOf(t, d, ctx, cid, 0)
	c.rec("F17-PURGED", "gen-0 reached Purged in the committed vault registry", purgedStatus == 5,
		fmt.Sprintf("gen-0 status=%s (node 2), btc height=%d", statusStr(purgedStatus), h))
	if purgedStatus != 5 {
		t.Fatalf("PRECONDITION FAILED: gen-0 is %s, not Purged, so the late-deposit case cannot be established", statusStr(purgedStatus))
	}

	// LATE DEPOSIT to the PURGED generation's address. fundVaultViaSPV is not used:
	// it t.Fatalf's on a refused map, and a refused or non-crediting map is exactly
	// the expected outcome here, so its steps are replicated inline below.
	owner2 := "hive:" + d.witnessAccount(2)
	balBefore := balanceSats(t, d, ctx, cid, owner2)
	regBefore := f17UtxoRegistryLen(t, d, ctx, cid)
	const lateSats int64 = 25_000_000
	addr, mapStatus := f17DepositAndMap(t, d, ctx, cid, env.primary0, backupPubKeyG, owner2, lateSats)
	t.Logf("late deposit of %d sats to PURGED gen-0 address %s, map status=%s", lateSats, addr, mapStatus)

	// Poll rather than read once: a credit that arrives late is still a credit, and
	// asserting on a single immediate read would understate the crediting path.
	balAfter := balBefore
	for i := 0; i < 6; i++ {
		time.Sleep(10 * time.Second)
		balAfter = balanceSats(t, d, ctx, cid, owner2)
		if balAfter != balBefore {
			break
		}
	}
	regAfter := f17UtxoRegistryLen(t, d, ctx, cid)
	gen0After := vaultStatusOf(t, d, ctx, cid, 0)
	gen0Utxos := genUtxoCount(t, d, ctx, cid, 0)
	notCredited := balAfter == balBefore && regAfter == regBefore && gen0Utxos == 0 && gen0After == 5
	c.rec("F17-NOTCREDITED", "deposit to a purged generation's address credits nothing and does not revive the generation", notCredited,
		fmt.Sprintf("map=%s balance %s: %d -> %d sats (sent %d), utxo registry entries %d -> %d, gen-0 utxos=%d, gen-0 status=%s",
			mapStatus, owner2, balBefore, balAfter, lateSats, regBefore, regAfter, gen0Utxos, statusStr(gen0After)))

	// POSITIVE CONTROL: the same deposit flow against the ACTIVE gen-1 address must
	// credit. Without this, "not credited" above could be a broken merkle proof, an
	// unrelayed header or a dead map path rather than the purge.
	ctlAcct := "hive:" + d.witnessAccount(3)
	ctlBefore := balanceSats(t, d, ctx, cid, ctlAcct)
	const ctlSats int64 = 25_000_000
	ctlAddr, ctlStatus := f17DepositAndMap(t, d, ctx, cid, primary1, backupPubKeyG, ctlAcct, ctlSats)
	ctlAfter := ctlBefore
	for i := 0; i < 6; i++ {
		time.Sleep(10 * time.Second)
		ctlAfter = balanceSats(t, d, ctx, cid, ctlAcct)
		if ctlAfter > ctlBefore {
			break
		}
	}
	c.rec("F17-CONTROL", "the identical deposit flow against the ACTIVE generation DOES credit", ctlAfter > ctlBefore,
		fmt.Sprintf("map=%s addr=%s balance %s: %d -> %d sats (sent %d)", ctlStatus, ctlAddr, ctlAcct, ctlBefore, ctlAfter, ctlSats))

	// KEY ALIVE: the purge stops address matching, it destroys no key material.
	// KeyRetirementEnabled is false and nothing deactivates a purged generation's
	// key, so the row must still be there and must not be retired. That is what
	// makes the stranded coins recoverable by the key holders.
	gen0KeyId := cid + "-main"
	keys, kerr := d.GetTssKeys(ctx, 2, bson.M{"id": gen0KeyId})
	keyStatus, keyPub := "", ""
	var keyEpoch uint64
	if len(keys) > 0 {
		keyStatus = keys[0].Status
		keyPub = keys[0].PublicKey
		keyEpoch = keys[0].Epoch
	}
	alive := kerr == nil && len(keys) > 0 && keyPub != "" && !strings.EqualFold(keyStatus, "retired")
	c.rec("F17-KEY-ALIVE", "the purged generation's TSS key row survives the purge (no key destruction)", alive,
		fmt.Sprintf("id=%s rows=%d status=%q epoch=%d pubkey=%s err=%v", gen0KeyId, len(keys), keyStatus, keyEpoch, keyPub, kerr))
	if !strings.EqualFold(keyStatus, "active") {
		t.Logf("INFO F17-KEY-ALIVE: gen-0 key status is %q, not \"active\" (still not retired, so the shares are intact)", keyStatus)
	}
	for n := 1; n <= 5; n++ {
		ks, err := d.GetTssKeys(ctx, n, bson.M{"id": gen0KeyId})
		if err != nil || len(ks) == 0 {
			t.Logf("INFO F17-KEY-ALIVE: magi-%d has no %s row (err=%v)", n, gen0KeyId, err)
			continue
		}
		t.Logf("INFO F17-KEY-ALIVE: magi-%d %s status=%q epoch=%d pubkey=%s", n, gen0KeyId, ks[0].Status, ks[0].Epoch, ks[0].PublicKey)
	}

	vfAssertContractIdentical(c, d, ctx, cid, vfAllNodes(5), 4*time.Minute, "F17-IDENT")
	c.summary("F17")
	t.Logf("F17 COMPLETE CONTRACT=%s purged-gen-0-address=%s", cid, addr)
}

// f17RelayBatch is the number of concatenated 80-byte headers sent per addBlocks
// call. Relaying the 144-block purge grace window one header at a time would be
// ~150 confirmed contract calls, which does not fit the test budget.
const f17RelayBatch = 25

// f17RelayHeaders relays every BTC header the contract has not seen yet, up to and
// including height upTo, in batches.
func f17RelayHeaders(t *testing.T, d *Devnet, ctx context.Context, cid string, upTo uint64) {
	t.Helper()
	last := contractLastHeight(t, d, ctx, cid)
	for start := last + 1; start <= upTo; start += f17RelayBatch {
		var hexBatch string
		for hh := start; hh < start+f17RelayBatch && hh <= upTo; hh++ {
			hx, err := btcBlockHeaderHex(ctx, d, hh)
			if err != nil {
				t.Fatalf("reading BTC header %d: %v", hh, err)
			}
			hexBatch += hx
		}
		if s := vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":10}`, hexBatch)); !isOK(s) {
			t.Logf("addBlocks batch starting at %d status=%s", start, s)
		}
	}
}

// f17UtxoRegistryLen returns the number of entries in the committed UTXO registry
// ("r", 8 bytes per entry). A late deposit that is not matched adds no entry, so
// this is the registry-level witness that nothing was recorded at all, independent
// of any per-generation accounting.
func f17UtxoRegistryLen(t *testing.T, d *Devnet, ctx context.Context, cid string) int {
	t.Helper()
	st, err := getStateHex(d, ctx, 2, cid, []string{"r"})
	if err != nil {
		t.Logf("f17UtxoRegistryLen: %v", err)
		return -1
	}
	return len(st["r"]) / 8
}

// f17DepositAndMap is fundVaultViaSPV with every fatal on the map path removed.
// fundVaultViaSPV t.Fatalf's when map is refused, and for F17 a refused (or a
// confirmed but non-crediting) map is the EXPECTED result, so its steps are
// replicated here and the map status is returned to the caller instead.
//
// Everything that would make a non-credit MEANINGLESS rather than real stays fatal:
// a bad address derivation, a failed send, and above all a deposit block that does
// not hold exactly [coinbase, deposit]. The merkle proof used here is the two-tx
// proof (sibling = coinbase, tx_index = 1), so a third transaction in the block
// would produce an invalid proof and a map failure that has nothing to do with the
// purge. The mempool is flushed with a throwaway block first to keep that block
// clean.
//
// Returns the derived deposit address and the map call's terminal status.
func f17DepositAndMap(t *testing.T, d *Devnet, ctx context.Context, cid, primaryHex, backupHex, recipient string, sats int64) (string, string) {
	t.Helper()
	instruction := "deposit_to=" + recipient
	gen := &chain.BTCAddressGenerator{Params: &chaincfg.RegressionNetParams, BackupCSVBlocks: 2}
	addr, _, err := gen.GenerateDepositAddress(primaryHex, backupHex, instruction)
	if err != nil {
		t.Fatalf("deriving deposit address for %s: %v", recipient, err)
	}

	// Flush anything already in the mempool into its own block so the deposit lands
	// in a block with exactly the coinbase beside it.
	if _, err := d.MineBlocks(ctx, 1); err != nil {
		t.Fatalf("flushing the mempool before the deposit: %v", err)
	}
	amt := fmt.Sprintf("%d.%08d", sats/1e8, sats%int64(1e8))
	depTxid, err := d.bitcoinCli(ctx, "sendtoaddress", addr, amt)
	if err != nil {
		t.Fatalf("sendtoaddress %s %s: %v", addr, amt, err)
	}
	h, err := d.MineBlocks(ctx, 1)
	if err != nil {
		t.Fatalf("mining the deposit block: %v", err)
	}
	bhash, err := d.bitcoinCli(ctx, "getblockhash", fmt.Sprint(h))
	if err != nil {
		t.Fatalf("getblockhash %d: %v", h, err)
	}
	blockJSON, err := d.bitcoinCli(ctx, "getblock", bhash, "1")
	if err != nil {
		t.Fatalf("getblock %s: %v", bhash, err)
	}
	var blk struct {
		Tx []string `json:"tx"`
	}
	if err := json.Unmarshal([]byte(blockJSON), &blk); err != nil {
		t.Fatalf("parsing block %s: %v", bhash, err)
	}
	if len(blk.Tx) != 2 || blk.Tx[1] != depTxid {
		t.Fatalf("INSTRUMENT BROKEN: deposit block %d holds %v, want exactly [coinbase %s]; the two-tx merkle proof would be invalid and a map failure would prove nothing about the purge",
			h, blk.Tx, depTxid)
	}
	rawTx, err := d.bitcoinCli(ctx, "getrawtransaction", depTxid)
	if err != nil {
		t.Fatalf("getrawtransaction %s: %v", depTxid, err)
	}
	proofHex := reverseHexBytes(blk.Tx[0])

	f17RelayHeaders(t, d, ctx, cid, h)

	mapPayload := fmt.Sprintf(
		`{"tx_data":{"block_height":%d,"raw_tx_hex":"%s","merkle_proof_hex":"%s","tx_index":1},"instructions":["%s"]}`,
		h, rawTx, proofHex, instruction)
	return addr, vstatus(t, d, ctx, 1, cid, "map", mapPayload)
}
