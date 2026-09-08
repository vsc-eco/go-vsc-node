package devnet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"vsc-node/cmd/mapping-bot/chain"

	"github.com/btcsuite/btcd/chaincfg"
)

// TestVaultF19LateDepositInactive is failure-state F19 of the BTC vault-rotation-v2
// suite. It proves match-until-purged and revert-on-late-deposit live: money that
// arrives at an emptied vault is credited, re-swept, and never destroyed by the purge.
//
// WHAT IT REPRODUCES
// gen-0 is rotated out to gen-1, drained to zero UTXOs and retired to INACTIVE, which
// records the purge-grace anchor (InactiveHeight). Only THEN does a late depositor,
// an exchange or a wallet still pointing at the retired address, send BTC to gen-0 and
// prove it with a valid SPV proof. The generation is empty, superseded and one grace
// window away from being purged out of existence. This is the exact window in which a
// naive implementation loses the coins.
//
// WHY THE OUTCOME IS WHAT IT IS (read from the contract, not assumed)
// Two mechanisms in contract/mapping have to hold hands for the money to survive:
//
//  1. MATCH-UNTIL-PURGED. isFundHoldingStatus (contract/mapping/vault_lifecycle.go)
//     counts INACTIVE as fund-holding, and depositAddressGenerations
//     (contract/mapping/init.go) derives a matchable deposit address for every
//     fund-holding generation. So the emptied gen-0 address is still in
//     MappingState.AddressRegistry and indexOutputs still matches it: the deposit
//     credits the depositor and tags a fresh UTXO to generation 0.
//
//  2. REVERT-ON-LATE-DEPOSIT. ReconcileRetiringVaults (the owner-only retireVault op)
//     scans the live UTXO registry and, for an INACTIVE generation that is
//     registry-NON-empty, takes the revert branch: status back to DRAINING and
//     InactiveHeight reset to 0. The reset matters as much as the status flip, because
//     canPurgeGen fails closed on a zero anchor, so the re-funded generation must serve
//     a FRESH full 144-block grace window before it can ever be purged again.
//
// The revert also re-arms the sweep: HandleMigrateVault only acts on RETIRING or
// DRAINING generations, so without the revert the late deposit would sit in a vault no
// migrateVault would ever touch. With it, the normal drain machinery picks the UTXO up
// and moves it to the successor. The depositor's VSC balance does not move during that
// sweep: the coins change VAULTS, not OWNERS.
//
// The ordering is the safety property. The purge branch requires registry-EMPTY, and
// the revert branch is checked FIRST on the same INACTIVE status, so a generation
// holding a late deposit can never be purged out from under it.
//
// CASES
//   - F19-INACTIVE     gen-0 really reached Inactive (4) in the committed vault
//     registry after the full drain, so the late deposit really does
//     land on an EMPTIED, superseded generation.
//   - F19-LATE-CREDIT  a valid SPV-proven deposit to that INACTIVE generation credits
//     the depositor and re-tags a UTXO to gen-0 (match-until-purged).
//   - F19-REVERT       retireVault flips the re-funded generation INACTIVE to DRAINING
//     AND clears InactiveHeight to 0, so the purge-grace clock restarts
//     from scratch rather than carrying over the old anchor.
//   - F19-RESWEEP      the re-engaged sweep drains gen-0 back to zero UTXOs while the
//     depositor's balance stays exactly where the credit left it.
//   - F19-PURGED       after a second retire plus the full 144-block grace window the
//     generation reaches Purged (5), so the cycle really closes.
//   - F19-IDENT        vault contract state byte-identical across all 5 nodes.
//
// The optional step-4 observation (a deposit to the now PURGED address credits
// nothing) is recorded as an INFO log line rather than a case, to keep the case id set
// exactly as specified. It needs no separate positive control: F19-LATE-CREDIT above
// already drove the identical deposit-and-map instrument to a CREDIT earlier in this
// same run, so a later non-credit cannot be a broken merkle proof, an unrelayed header
// or a dead map path.
//
// DEVIATIONS FROM THE SPEC (with reasons)
//   - The late deposits go through f19DepositAndMap, a local copy of fundVaultViaSPV
//     with the fatals on the map path removed, instead of fundVaultViaSPV itself.
//     fundVaultViaSPV t.Fatalf's when map is refused, which would abort the run at the
//     single most interesting failure (match-until-purged broken) instead of recording
//     F19-LATE-CREDIT as a FAIL and continuing to gather the revert and purge evidence.
//     The optional purged-address deposit in step 4 EXPECTS a refusal, so it cannot use
//     fundVaultViaSPV at all. Everything that would make a non-credit meaningless
//     rather than real (address derivation, the send, a deposit block that is not
//     exactly [coinbase, deposit]) stays fatal.
//   - BTC_MAPPING_WASM_PATH is required rather than defaulted. The S5 revert branch
//     under test does not exist in the pre-v2 contract, so silently falling back to a
//     stale wasm would produce a vacuous run.
//
// A precondition that cannot be established (rotation, full drain, Inactive) is a
// t.Fatalf, never a t.Skip. After that everything is t.Errorf via the recorder so the
// run keeps gathering evidence and still reaches the cross-node identity check.
//
// RUN:
//
//	VAULT_F19_RUN=1 DEVNET_KEEP=1 go test -v -run TestVaultF19LateDepositInactive -timeout 95m ./tests/devnet/
func TestVaultF19LateDepositInactive(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F19_RUN") == "" {
		t.Skip("set VAULT_F19_RUN=1")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 85*time.Minute)
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		t.Fatal("BTC_MAPPING_WASM_PATH must point at the btc-mapping-contract regtest wasm built from the v2 contract; the S5 revert branch under test does not exist without it")
	}
	if _, err := os.Stat(wasm); err != nil {
		t.Fatalf("wasm: %v", err)
	}

	// hpin is AFTER genesis (~block 190) so gen-0 is minted on the v2-off path (no
	// fresh-genesis deadlock) and v2 is ON for the rotation, the drain, the revert and
	// the purge.
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
	// one tagged deposit for the owner.
	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "F19 late deposit to an inactive generation")
	cid := env.cid
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	// ROTATE gen-0 to gen-1. Every case below needs a superseded generation, so a
	// failure here is a precondition failure, not a finding.
	primary1, rotated := vfRotate(t, d, ctx, cid, 1)
	if !rotated {
		t.Fatalf("PRECONDITION FAILED: gen-0 never rotated to gen-1, so there is no superseded generation to drain, empty and re-fund")
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

	// DRAIN gen-0 completely. Every tranche is settled end to end and the REGISTRY, not
	// a transaction status, decides when the generation is empty.
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
	for i := 0; i < 6; i++ {
		remaining := genUtxoCount(t, d, ctx, cid, 0)
		if remaining == 0 {
			break
		}
		t.Logf("drain tranche %d: gen-0 still holds %d UTXO(s), sweeping", i+1, remaining)
		migrateAndSettle(t, d, ctx, cid, cid+"-main", primary1, backupPubKeyG)
		vfDumpRegistry(t, d, ctx, 2, cid, fmt.Sprintf("after tranche %d", i+1))
		if after := vfWaitGenBelow(t, d, ctx, cid, 0, remaining, 3*time.Minute); after >= remaining {
			t.Fatalf("PRECONDITION FAILED: drain tranche %d made no progress (%d to %d UTXOs), gen-0 can never be emptied so the late-deposit window cannot be reached", i+1, remaining, after)
		}
		fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
	}
	if left := genUtxoCount(t, d, ctx, cid, 0); left != 0 {
		t.Fatalf("PRECONDITION FAILED: gen-0 still holds %d UTXO(s) after 6 tranches, the late deposit must land on an EMPTIED generation", left)
	}

	// RETIRE: draining to INACTIVE. This is the call that records InactiveHeight, the
	// anchor the purge grace window is measured from and the value the revert must clear.
	vstatus(t, d, ctx, 1, cid, "retireVault", "")
	inactiveSt := vaultStatusOf(t, d, ctx, cid, 0)
	anchor, anchorFound := f19InactiveHeightOf(d, ctx, 2, cid, 0)
	c.rec("F19-INACTIVE", "gen-0 reached Inactive in the committed vault registry after a full drain", inactiveSt == 4,
		fmt.Sprintf("gen-0 status=%s (node 2), InactiveHeight=%d (present=%v), contract btc height=%d",
			statusStr(inactiveSt), anchor, anchorFound, contractLastHeight(t, d, ctx, cid)))
	if inactiveSt != 4 {
		t.Fatalf("PRECONDITION FAILED: gen-0 is %s, not Inactive, so a deposit to it would not exercise the emptied-generation window at all", statusStr(inactiveSt))
	}
	if anchorFound && anchor == 0 {
		t.Errorf("F19-INACTIVE anomaly: gen-0 is Inactive but InactiveHeight is 0, so canPurgeGen fails closed forever and the purge leg of this test cannot be reached")
	}

	// === STEP 1: the LATE DEPOSIT to the emptied, INACTIVE generation ===
	owner2 := "hive:" + d.witnessAccount(2)
	balBefore := balanceSats(t, d, ctx, cid, owner2)
	const lateSats int64 = 5_000_000
	lateAddr, lateMapStatus := f19DepositAndMap(t, d, ctx, cid, env.primary0, backupPubKeyG, owner2, lateSats)
	t.Logf("late deposit of %d sats to INACTIVE gen-0 address %s, map status=%s", lateSats, lateAddr, lateMapStatus)

	// Poll rather than read once: a credit that lands a moment later is still a credit,
	// and a single immediate read would understate the crediting path.
	balAfter := balBefore
	gen0Utxos := -1
	for i := 0; i < 6; i++ {
		balAfter = balanceSats(t, d, ctx, cid, owner2)
		gen0Utxos = genUtxoCount(t, d, ctx, cid, 0)
		if balAfter > balBefore && gen0Utxos == 1 {
			break
		}
		time.Sleep(10 * time.Second)
	}
	c.rec("F19-LATE-CREDIT", "a deposit to an EMPTIED, INACTIVE generation is still matched and credited (match-until-purged)",
		balAfter > balBefore && gen0Utxos == 1,
		fmt.Sprintf("map=%s addr=%s balance %s: %d -> %d sats (sent %d), gen-0 utxos=%d, gen-0 status=%s",
			lateMapStatus, lateAddr, owner2, balBefore, balAfter, lateSats, gen0Utxos, statusStr(vaultStatusOf(t, d, ctx, cid, 0))))

	// === STEP 2: retireVault must REVERT the re-funded generation, not purge it ===
	vstatus(t, d, ctx, 1, cid, "retireVault", "")
	revertSt := -1
	revertAnchor := uint32(0)
	revertFound := false
	for i := 0; i < 6; i++ {
		revertSt = vaultStatusOf(t, d, ctx, cid, 0)
		revertAnchor, revertFound = f19InactiveHeightOf(d, ctx, 2, cid, 0)
		if revertSt == 3 && revertFound && revertAnchor == 0 {
			break
		}
		time.Sleep(10 * time.Second)
	}
	c.rec("F19-REVERT", "retireVault reverts the re-funded generation Inactive to Draining and resets the purge-grace anchor to 0",
		revertSt == 3 && revertFound && revertAnchor == 0,
		fmt.Sprintf("gen-0 status=%s (want Draining), InactiveHeight=%d (want 0, present=%v), was anchored at %d",
			statusStr(revertSt), revertAnchor, revertFound, anchor))

	// === STEP 3: the re-engaged sweep moves the late deposit to the successor ===
	balBeforeSweep := balanceSats(t, d, ctx, cid, owner2)
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
	for i := 0; i < 3; i++ {
		remaining := genUtxoCount(t, d, ctx, cid, 0)
		if remaining == 0 {
			break
		}
		t.Logf("resweep tranche %d: gen-0 holds %d UTXO(s) again after the late deposit", i+1, remaining)
		migrateAndSettle(t, d, ctx, cid, cid+"-main", primary1, backupPubKeyG)
		if genUtxoCount(t, d, ctx, cid, 0) == 0 {
			break
		}
		fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
	}
	sweptLeft := genUtxoCount(t, d, ctx, cid, 0)
	balAfterSweep := balanceSats(t, d, ctx, cid, owner2)
	c.rec("F19-RESWEEP", "the reverted generation is swept back to empty and the depositor's balance is untouched (funds moved vaults, not owners)",
		sweptLeft == 0 && balAfterSweep == balBeforeSweep,
		fmt.Sprintf("gen-0 utxos=%d (want 0), %s balance %d -> %d sats (want unchanged), gen-0 status=%s",
			sweptLeft, owner2, balBeforeSweep, balAfterSweep, statusStr(vaultStatusOf(t, d, ctx, cid, 0))))

	// === STEP 4: retire again, serve the FULL grace window, then purge ===
	vstatus(t, d, ctx, 1, cid, "retireVault", "")
	secondInactive := vaultStatusOf(t, d, ctx, cid, 0)
	secondAnchor, secondFound := f19InactiveHeightOf(d, ctx, 2, cid, 0)
	if secondInactive != 4 {
		t.Errorf("gen-0 is %s after the post-resweep retireVault, expected Inactive; the purge case below cannot be established from this state", statusStr(secondInactive))
	}
	t.Logf("post-resweep retire: gen-0 status=%s, fresh InactiveHeight=%d (present=%v)", statusStr(secondInactive), secondAnchor, secondFound)

	graceFrom := contractLastHeight(t, d, ctx, cid)
	tip, err := d.MineBlocks(ctx, 150)
	if err != nil {
		t.Fatalf("mining the purge grace window: %v", err)
	}
	f19RelayHeaders(t, d, ctx, cid, tip)
	vstatus(t, d, ctx, 1, cid, "retireVault", "")
	purgedSt := vaultStatusOf(t, d, ctx, cid, 0)
	c.rec("F19-PURGED", "after the fresh 144-block grace window the emptied generation finally reaches Purged", purgedSt == 5,
		fmt.Sprintf("gen-0 status=%s, anchor=%d, contract btc height %d -> %d (grace needs 144 blocks)",
			statusStr(purgedSt), secondAnchor, graceFrom, contractLastHeight(t, d, ctx, cid)))

	// Optional closing observation (INFO, not a case): once PURGED the address leaves
	// the matchable set, so the same deposit flow credits nothing. F19-LATE-CREDIT above
	// is this observation's positive control: the identical instrument already produced a
	// CREDIT earlier in this run, so a non-credit here is the purge and not a harness fault.
	if purgedSt == 5 {
		owner3 := "hive:" + d.witnessAccount(3)
		purgedBefore := balanceSats(t, d, ctx, cid, owner3)
		purgedAddr, purgedMapStatus := f19DepositAndMap(t, d, ctx, cid, env.primary0, backupPubKeyG, owner3, lateSats)
		purgedAfter := purgedBefore
		for i := 0; i < 6; i++ {
			time.Sleep(10 * time.Second)
			purgedAfter = balanceSats(t, d, ctx, cid, owner3)
			if purgedAfter != purgedBefore {
				break
			}
		}
		t.Logf("INFO (not a pass): deposit of %d sats to the PURGED gen-0 address %s, map=%s, %s balance %d -> %d sats, credited=%v, gen-0 utxos=%d, gen-0 status=%s",
			lateSats, purgedAddr, purgedMapStatus, owner3, purgedBefore, purgedAfter, purgedAfter != purgedBefore,
			genUtxoCount(t, d, ctx, cid, 0), statusStr(vaultStatusOf(t, d, ctx, cid, 0)))
	}

	// === STEP 5: every node must agree on the whole credit/revert/resweep/purge history ===
	vfAssertContractIdentical(c, d, ctx, cid, vfAllNodes(5), 4*time.Minute, "F19-IDENT")
	c.summary("F19")
	t.Logf("F19 COMPLETE CONTRACT=%s late-deposit-address=%s", cid, lateAddr)
}

// f19InactiveHeightOf reads a generation's InactiveHeight, the purge-grace anchor, out
// of the committed vault registry on one node. The second return reports whether the
// generation is in the registry at all, so "anchor is 0" can be told apart from
// "generation is missing".
func f19InactiveHeightOf(d *Devnet, ctx context.Context, node int, cid string, gen uint32) (uint32, bool) {
	for _, v := range vfVaultRegistryOn(d, ctx, node, cid) {
		if v.Generation == gen {
			return v.InactiveHeight, true
		}
	}
	return 0, false
}

// f19RelayBatch is the number of concatenated 80-byte headers sent per addBlocks call.
// Relaying the 144-block purge grace window one header at a time would be ~150 confirmed
// contract calls, which does not fit the test budget.
const f19RelayBatch = 25

// f19RelayHeaders relays every BTC header the contract has not seen yet, up to and
// including height upTo, in batches.
func f19RelayHeaders(t *testing.T, d *Devnet, ctx context.Context, cid string, upTo uint64) {
	t.Helper()
	last := contractLastHeight(t, d, ctx, cid)
	for start := last + 1; start <= upTo; start += f19RelayBatch {
		var hexBatch string
		for hh := start; hh < start+f19RelayBatch && hh <= upTo; hh++ {
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

// f19DepositAndMap is fundVaultViaSPV with every fatal on the map path removed.
// fundVaultViaSPV t.Fatalf's when map is refused; for F19 the map result is the
// MEASUREMENT (a credit in step 1, an expected non-credit against the purged address in
// step 4), so aborting on it would destroy the evidence. Its steps are replicated here
// and the map status is returned to the caller instead.
//
// Everything that would make the measurement MEANINGLESS rather than real stays fatal:
// a bad address derivation, a failed send, and above all a deposit block that does not
// hold exactly [coinbase, deposit]. The merkle proof used here is the two-transaction
// proof (sibling = coinbase, tx_index = 1), so a third transaction in the block would
// produce an invalid proof and a map failure that says nothing about the vault state.
// The mempool is flushed into a throwaway block first to keep that block clean.
//
// Returns the derived deposit address and the map call's terminal status.
func f19DepositAndMap(t *testing.T, d *Devnet, ctx context.Context, cid, primaryHex, backupHex, recipient string, sats int64) (string, string) {
	t.Helper()
	instruction := "deposit_to=" + recipient
	gen := &chain.BTCAddressGenerator{Params: &chaincfg.RegressionNetParams, BackupCSVBlocks: 2}
	addr, _, err := gen.GenerateDepositAddress(primaryHex, backupHex, instruction)
	if err != nil {
		t.Fatalf("deriving deposit address for %s: %v", recipient, err)
	}

	// Flush anything already in the mempool into its own block so the deposit lands in a
	// block with exactly the coinbase beside it.
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
		t.Fatalf("INSTRUMENT BROKEN: deposit block %d holds %v, want exactly [coinbase, %s]; the two-transaction merkle proof would be invalid and the map result would prove nothing about the vault state",
			h, blk.Tx, depTxid)
	}
	rawTx, err := d.bitcoinCli(ctx, "getrawtransaction", depTxid)
	if err != nil {
		t.Fatalf("getrawtransaction %s: %v", depTxid, err)
	}
	proofHex := reverseHexBytes(blk.Tx[0])

	// VR2-07: the contract refuses a deposit fewer than MinConfirmationDepth below
	// its own tip, so relaying only up to the deposit's own block leaves it at
	// depth 0. In production the oracle keeps relaying and a deposit matures on its
	// own; the harness has to model that rather than mapping the instant the block
	// lands. Mirrors constants.MinConfirmationDepth for regtest.
	if _, err := d.MineBlocks(ctx, vfDepositMaturityBlocks); err != nil {
		t.Fatalf("mine maturity blocks: %v", err)
	}
	f19RelayHeaders(t, d, ctx, cid, h+uint64(vfDepositMaturityBlocks))

	mapPayload := fmt.Sprintf(
		`{"tx_data":{"block_height":%d,"raw_tx_hex":"%s","merkle_proof_hex":"%s","tx_index":1},"instructions":["%s"]}`,
		h, rawTx, proofHex, instruction)
	return addr, vstatus(t, d, ctx, 1, cid, "map", mapPayload)
}
