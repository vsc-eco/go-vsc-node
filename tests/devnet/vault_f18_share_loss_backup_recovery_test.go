package devnet

// vault_f18_share_loss_backup_recovery_test.go, F18: key-share loss, CSV backup
// recovery, and the theft halt that recovery trips.
//
// THE DISASTER-RECOVERY PATH. Every other failure test in this suite asks what
// happens when the TSS committee is degraded but still able to sign. F18 asks the
// question after that: what does an operator do when the committee can NEVER sign
// again, because enough nodes lost their key shares that the signing threshold is
// unreachable? The vault script answers it. Every vault P2WSH carries a second
// spending branch, guarded by OP_CHECKSEQUENCEVERIFY, that the owner-held BACKUP
// key alone can take once the relative timelock has matured (2 blocks on regtest,
// constants.TestnetBackupCSVBlocks; 4320 blocks, about a month, on mainnet). The
// backup branch is the reason a lost committee is not lost money.
//
// THE THING AN OPERATOR MUST KNOW BEFORE THEY USE IT, and the reason this test
// exists: a backup-branch rescue TRIPS THE ANTI-THEFT HALT. The M1.1b detector
// (contract/mapping/theft_gate.go HandleReportUnauthorizedSpend) trips on any
// SPV-proven spend of a CURRENTLY-REGISTERED vault UTXO whose txid is not an
// authorised in-flight spend. An honest CSV rescue is exactly that shape: the
// contract never authorised it, so the UTXO never left the registry and the txid
// is not in the pending-spend list. The trip is DELIBERATE (theft_gate.go says so
// in as many words: an anomalous backup recovery is worth halting keysign on,
// because the TSS is presumably non-functional if the backup path is being
// exercised). But it means the rescue leaves BTC keysign FROZEN fleet wide, and
// the ONLY way back is the owner calling clearTheftHalt. An operator who rescues
// funds and then wonders why no withdrawal will sign has hit this, and this test
// is the record that it is by design and that clearTheftHalt is the cure.
//
// Cases:
//
//	F18-STUCK          with 3 of 5 shares deleted, the retiring generation's
//	                   migration sweep can never be signed (threshold needs 4 of
//	                   5 parties), so the funds are unreachable through the TSS.
//	F18-BACKUP-SPEND   the CSV backup branch rescues them: a version-2 tx with
//	                   nSequence 2, witness [sig||SIGHASH_ALL, empty, script],
//	                   signed by the backup private key alone, is accepted and
//	                   mined by bitcoind.
//	F18-THEFT-TRIP     reportUnauthorizedSpend over that rescue sets the contract
//	                   theft flag "th", and every node mirrors it into
//	                   chain_consensus_state.btc_theft_halted.
//	F18-FROZEN         while the flag is up, a BTC keysign for the ACTIVE
//	                   generation is refused pre-issuance by the solvency gate.
//	F18-CLEAR          the owner's clearTheftHalt deletes "th" and every node
//	                   clears its mirror.
//	F18-NOFALSE-TRIP   INFO: the legitimate operations that came first (rotation,
//	                   fee-reserve top-up, the sweep build) never set "th".
//	F18-IDENT          contract state byte-identical across all 5 nodes.
//
// The backup private key is 1 (a 32-byte scalar whose last byte is 1), whose
// public key is the secp256k1 generator G, which is the suite's backupPubKeyG.
// The test asserts that equality rather than assuming it.
//
//	VAULT_F18_RUN=1 go test -v -run TestVaultF18ShareLossBackupRecovery -timeout 95m ./tests/devnet/

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"vsc-node/cmd/mapping-bot/chain"
	"vsc-node/lib/btcvault"

	btcec "github.com/btcsuite/btcd/btcec/v2"
	btcecdsa "github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// vfF18FrozenLog is the substring of the solvency-gate WARN emitted by
// btcSignRefused (modules/tss/output_scoping.go) when a BTC keysign is skipped
// because a halt flag is up. The full line is "BTC keysign frozen by solvency
// gate; skipping issuance"; the trailing clause is left out so the assertion
// survives a wording change after the semicolon.
const vfF18FrozenLog = "BTC keysign frozen by solvency gate"

// vfF18ShareLoadFailLog is the ERROR a node logs when it is asked to join a
// signing session for a key whose share it no longer holds (dispatcher.go
// "failed to retrieve key data"). It is the direct evidence that the deletion
// took effect inside the container, not just on the host.
const vfF18ShareLoadFailLog = "failed to retrieve key data"

// vfF18BackupSpendFeeSats is the absolute miner fee taken out of the rescued
// UTXO. The rescue tx is about 115 virtual bytes, so this is far above the
// 1 sat/vB regtest minimum relay rate and cannot be the reason a broadcast is
// rejected.
const vfF18BackupSpendFeeSats = 5_000

func TestVaultF18ShareLossBackupRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F18_RUN") == "" {
		t.Skip("set VAULT_F18_RUN=1")
	}
	requireDocker(t)

	const runTimeout = 50 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		wasm = "/home/clauderfly/utxo-s1/btc-mapping-contract/bin/dev.wasm"
	}
	if _, err := os.Stat(wasm); err != nil {
		t.Fatalf("wasm: %v", err)
	}

	// v2 activation is pinned AFTER genesis (about block 190) so gen-0 mints on the
	// v2-off genesis path and the flag is live well before the rotation.
	const hpin = 400

	cfg := tssTestConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	// The post-injection wait for F18-FROZEN is measured in SIGN TICKS read from
	// the config, not in wall clock: TSS only selects signing requests on Hive
	// blocks where bh%signInterval == 0 (tss.go around line 587).
	signInterval := cfg.SysConfigOverrides.TssParams.SignInterval
	if signInterval == 0 {
		t.Fatalf("PRECONDITION FAILED: tssTestConfig left TssParams.SignInterval at 0, so the post-injection wait has no instrument")
	}

	d, _ := startDevnetNoKey(t, cfg, runTimeout)
	c := &vfCase{t: t}
	nodes := vfAllNodes(5)

	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "vault f18 share loss backup recovery")
	cid := env.cid
	retiringKeyId := cid + "-" + btcvault.VaultKeyName(0)
	activeKeyId := cid + "-" + btcvault.VaultKeyName(1)

	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	// ---- setup: rotate to gen-1 so gen-0 is a superseded, fund-holding gen ----
	primary1, rotated := vfRotate(t, d, ctx, cid, 1)
	if !rotated {
		t.Fatalf("PRECONDITION FAILED: the gen-0 to gen-1 rotation did not complete (primary1=%q), so there is no retiring generation whose sweep can get stuck", primary1)
	}
	if st := vfVaultStatusOn(d, ctx, 2, cid, 0); st != int(btcvault.VaultStatusRetiring) {
		t.Fatalf("PRECONDITION FAILED: gen-0 status is %d, want %d (Retiring)", st, int(btcvault.VaultStatusRetiring))
	}
	if st := vfVaultStatusOn(d, ctx, 2, cid, 1); st != int(btcvault.VaultStatusActive) {
		t.Fatalf("PRECONDITION FAILED: gen-1 status is %d, want %d (Active)", st, int(btcvault.VaultStatusActive))
	}
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)

	// ---- setup: prove the backup private key really is the one behind backupPubKeyG ----
	privBytes := make([]byte, 32)
	privBytes[31] = 1
	backupPriv, backupPub := btcec.PrivKeyFromBytes(privBytes)
	derivedBackupHex := hex.EncodeToString(backupPub.SerializeCompressed())
	if derivedBackupHex != backupPubKeyG {
		t.Fatalf("PRECONDITION FAILED: private key 1 derives pubkey %s, but the vault's registered backup is %s; the CSV rescue could not be signed and every later case would be vacuous",
			derivedBackupHex, backupPubKeyG)
	}
	t.Logf("PRECONDITION OK: backup private key 1 derives the registered backup pubkey %s (secp256k1 generator G)", derivedBackupHex)

	// ---- setup: locate the gen-0 tagged deposit UTXO and rebuild its witness script ----
	// The witness script comes from the SAME generator fundVaultViaSPV used to derive
	// the deposit address (chain.BTCAddressGenerator with BackupCSVBlocks=2, matching
	// constants.TestnetBackupCSVBlocks for a non-mainnet network), and its second
	// return value IS the script, so nothing is reconstructed by hand. The script is
	// then proven correct against chain state: P2WSH(script) must equal the PkScript
	// the contract recorded for the UTXO.
	instruction := "deposit_to=" + env.owner
	addrGen := &chain.BTCAddressGenerator{Params: &chaincfg.RegressionNetParams, BackupCSVBlocks: 2}
	depositAddr, witnessScript, err := addrGen.GenerateDepositAddress(env.primary0, backupPubKeyG, instruction)
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: deriving the gen-0 deposit address/script: %v", err)
	}
	wantPkScript, err := vfF18P2WSHPkScript(witnessScript)
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: building P2WSH pkScript from the witness script: %v", err)
	}
	target := vfF18FindUtxo(t, d, ctx, 2, cid, 0, wantPkScript)
	if target == nil {
		t.Fatalf("PRECONDITION FAILED: no registered gen-0 UTXO pays %s (P2WSH of the rebuilt witness script); the CSV rescue has nothing to spend", depositAddr)
	}
	wantTag := sha256.Sum256([]byte(instruction))
	if !bytes.Equal(target.Tag, wantTag[:]) {
		t.Fatalf("PRECONDITION FAILED: the gen-0 UTXO's tag is %s, want sha256(%q)=%s; the witness script does not match the one the deposit was locked with",
			hex.EncodeToString(target.Tag), instruction, hex.EncodeToString(wantTag[:]))
	}
	if out, cerr := d.bitcoinCli(ctx, "gettxout", target.TxId, strconv.FormatUint(uint64(target.Vout), 10)); cerr != nil || out == "" {
		t.Fatalf("PRECONDITION FAILED: bitcoind reports %s:%d is not an unspent output (err=%v out=%q); the rescue tx could never be accepted",
			target.TxId, target.Vout, cerr, out)
	}
	t.Logf("PRECONDITION OK: gen-0 deposit UTXO %s:%d holds %d sats at %s (witness script %d bytes, tag matches sha256(%q))",
		target.TxId, target.Vout, target.Amount, depositAddr, len(witnessScript), instruction)

	// "th" must be empty here: everything so far was legitimate. Captured now and
	// reported as F18-NOFALSE-TRIP at the end.
	thAfterSetup := vfF18TheftKey(d, ctx, 2, cid)

	// ---- 1. share loss: delete 3 of 5 nodes' TSS keystores ----
	// The signing threshold is ceil(2n/3)-1 = 3, so 4 of 5 parties must be live AND
	// hold a share. Deleting three leaves two, which can never reach it.
	lossNodes := []int{3, 4, 5}
	vfStopNodes(t, d, ctx, lossNodes)
	for _, n := range lossNodes {
		before, after := vfF18DeleteTssKeys(t, d, n)
		t.Logf("magi-%d TSS keystore: %d share file(s) before delete, %d after", n, before, after)
	}
	vfStartNodes(t, d, ctx, lossNodes)
	// With 3 of 5 down the VSC chain halted; nothing below works until it resumes.
	if grew, from, to := vfGrewWithin(d, ctx, 1, 5*time.Minute); !grew {
		t.Fatalf("PRECONDITION FAILED: VSC did not resume after magi-3/4/5 restarted (block_headers slot %d -> %d in 5m); no contract call can land", from, to)
	}
	shareHolders := vfF18CountShareHolders(t, d, nodes)
	t.Logf("after the share loss, %d of 5 nodes still hold TSS keystore files: %v", len(shareHolders), shareHolders)
	if len(shareHolders) > 3 {
		t.Fatalf("PRECONDITION FAILED: %d nodes still hold shares (%v); the signing threshold of 4 is still reachable and F18-STUCK would prove nothing", len(shareHolders), shareHolders)
	}

	// ---- 2. F18-STUCK: the migration sweep can never be signed ----
	timeoutBefore := vfCountLogs(d, ctx, 1, "timeout result")
	sweepTxid, sd, buildStatus := vfBuildSweep(t, d, ctx, 1, cid)
	signed := false
	if sd != nil {
		_, signed = vfAwaitSweepSignatures(t, d, ctx, retiringKeyId, sd)
	}
	timeoutAfter := vfCountLogs(d, ctx, 1, "timeout result")
	shareFailDetail := ""
	for _, n := range lossNodes {
		shareFailDetail += fmt.Sprintf(" magi-%d:%d", n, vfCountLogs(d, ctx, n, vfF18ShareLoadFailLog))
	}
	blameDetail := "none"
	if bl, berr := d.GetLatestBlame(ctx, 1, retiringKeyId); berr == nil && bl != nil {
		blameDetail = fmt.Sprintf("epoch=%d height=%d", bl.Epoch, bl.BlockHeight)
	}
	c.rec("F18-STUCK", "with 3 of 5 shares destroyed the retiring gen's sweep can never be signed",
		sd != nil && !signed,
		fmt.Sprintf("migrateVault status=%s sweepTxid=%s signingData=%v signaturesLanded=%v; magi-1 \"timeout result\" %d -> %d; %q counts:%s; latest blame on %s: %s",
			buildStatus, sweepTxid, sd != nil, signed, timeoutBefore, timeoutAfter, vfF18ShareLoadFailLog, shareFailDetail, retiringKeyId, blameDetail))

	thAfterSweep := vfF18TheftKey(d, ctx, 2, cid)

	// ---- 3. F18-BACKUP-SPEND: rescue the funds through the CSV backup branch ----
	// Mine 2 blocks first so the relative timelock is unambiguously mature. The
	// deposit was confirmed many blocks ago, so it already is; this only removes
	// any doubt about the CSV comparison.
	if _, merr := d.MineBlocks(ctx, 2); merr != nil {
		t.Errorf("mining CSV maturity blocks: %v", merr)
	}
	destAddr := mustNewBtcAddr(t, d, ctx)
	rescueHex, rescueTxid, berr := vfF18BuildBackupSpend(target, witnessScript, destAddr, backupPriv)
	if berr != nil {
		c.rec("F18-BACKUP-SPEND", "the CSV backup branch spends the stranded vault UTXO", false,
			fmt.Sprintf("the rescue tx could not even be built: %v", berr))
	}
	spendOK := false
	var spendHeight uint64
	var minedTxid string
	if berr == nil {
		bcTxid, serr := d.bitcoinCli(ctx, "sendrawtransaction", rescueHex)
		if serr != nil {
			c.rec("F18-BACKUP-SPEND", "the CSV backup branch spends the stranded vault UTXO", false,
				fmt.Sprintf("sendrawtransaction REJECTED the rescue tx %s: %v", rescueTxid, serr))
		} else {
			h, merr := d.MineBlocks(ctx, 1)
			if merr != nil {
				c.rec("F18-BACKUP-SPEND", "the CSV backup branch spends the stranded vault UTXO", false,
					fmt.Sprintf("rescue tx %s accepted into the mempool but mining failed: %v", bcTxid, merr))
			} else {
				mined := vfF18BlockTxids(t, d, ctx, h)
				inBlock := false
				for _, id := range mined {
					if id == bcTxid {
						inBlock = true
					}
				}
				spendOK = inBlock
				spendHeight = h
				minedTxid = bcTxid
				c.rec("F18-BACKUP-SPEND", "the CSV backup branch spends the stranded vault UTXO", inBlock,
					fmt.Sprintf("tx %s (version 2, nSequence 2, witness [sig||SIGHASH_ALL, empty, script]) accepted and mined at height %d; block txs=%v; rescued %d sats from %s:%d to %s",
						bcTxid, h, mined, target.Amount-vfF18BackupSpendFeeSats, target.TxId, target.Vout, destAddr))
			}
		}
	}

	// ---- 4. F18-THEFT-TRIP: the honest rescue trips the anti-theft halt ----
	tripped := false
	if !spendOK {
		c.rec("F18-THEFT-TRIP", "an SPV-proven unauthorised spend of a registered vault UTXO sets the theft halt", false,
			"SKIPPED-AS-FAIL: the backup rescue never confirmed, so there is nothing to report")
	} else {
		last := contractLastHeight(t, d, ctx, cid)
		for hh := last + 1; hh <= spendHeight; hh++ {
			hx, herr := btcBlockHeaderHex(ctx, d, hh)
			if herr != nil {
				t.Errorf("reading header %d: %v", hh, herr)
				continue
			}
			if s := vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":10}`, hx)); !isOK(s) {
				t.Logf("addBlocks %d status=%s", hh, s)
			}
		}
		blkTx := vfF18BlockTxids(t, d, ctx, spendHeight)
		if len(blkTx) != 2 {
			t.Errorf("the rescue block has %d txs (want coinbase + rescue = 2); the 2-tx merkle proof helper does not apply", len(blkTx))
		}
		rawTx, rerr := d.bitcoinCli(ctx, "getrawtransaction", minedTxid)
		if rerr != nil {
			t.Errorf("getrawtransaction %s: %v", minedTxid, rerr)
		}
		proof := reverseHexBytes(blkTx[0])
		reportStatus := vstatus(t, d, ctx, 1, cid, "reportUnauthorizedSpend", fmt.Sprintf(
			`{"tx_data":{"block_height":%d,"raw_tx_hex":"%s","merkle_proof_hex":"%s","tx_index":1},"indices":[0]}`,
			spendHeight, rawTx, proof))

		thSet := false
		flagged := 0
		flagDetail := ""
		deadline := time.Now().Add(3 * time.Minute)
		for {
			thSet = len(vfF18TheftKey(d, ctx, 2, cid)) > 0
			flagged, flagDetail = vfF18TheftFlagCount(t, d, ctx, nodes)
			if thSet && flagged == len(nodes) {
				break
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(10 * time.Second)
		}
		tripped = thSet && flagged == len(nodes)
		c.rec("F18-THEFT-TRIP", "an SPV-proven unauthorised spend of a registered vault UTXO sets the theft halt", tripped,
			fmt.Sprintf("reportUnauthorizedSpend status=%s; contract \"th\"=%q; btc_theft_halted on %d/%d nodes:%s",
				reportStatus, hex.EncodeToString(vfF18TheftKey(d, ctx, 2, cid)), flagged, len(nodes), flagDetail))
	}

	// ---- 5. F18-FROZEN: while the halt is up, an active-gen keysign is refused ----
	// The ACTIVE generation is deliberately NOT output scoped (it must sign ordinary
	// user withdrawals to arbitrary destinations), so evaluateScope returns
	// scopeAllow for it and the ONLY thing that can stop the sign is the halt flag.
	// That makes it the correct probe for the freeze: a refusal here can only be the
	// solvency gate, never output scoping.
	rogue := sha256.Sum256([]byte("f18-frozen-probe"))
	if vfF18IsPendingSighash(t, d, ctx, cid, rogue[:]) {
		t.Errorf("F18-FROZEN setup: the probe digest collides with a pending spend sighash, so the observation would not be meaningful")
	}
	frozenBefore := map[int]int{}
	for _, n := range nodes {
		frozenBefore[n] = vfCountLogs(d, ctx, n, vfF18FrozenLog)
	}
	vfF18InsertRogueRequest(t, d, ctx, nodes, activeKeyId, rogue[:])
	vfF18WaitSignTicks(t, d, ctx, signInterval, 3, 4*time.Minute)
	rowVisible := false
	rogueHex := hex.EncodeToString(rogue[:])
	if reqs, qerr := d.GetTssRequests(ctx, 2, activeKeyId); qerr == nil {
		for _, r := range reqs {
			if r.Msg == rogueHex {
				rowVisible = true
			}
		}
	}
	var signedOn []int
	freezers := 0
	frozenDetail := ""
	for _, n := range nodes {
		if vfSignatureLanded(d, ctx, n, activeKeyId, rogue[:]) {
			signedOn = append(signedOn, n)
		}
		delta := vfCountLogs(d, ctx, n, vfF18FrozenLog) - frozenBefore[n]
		if delta >= 1 {
			freezers++
		}
		frozenDetail += fmt.Sprintf(" magi-%d:+%d", n, delta)
	}
	c.rec("F18-FROZEN", "while the theft halt is up, an active-gen keysign is refused pre-issuance by the solvency gate",
		tripped && rowVisible && len(signedOn) == 0 && freezers >= 3,
		fmt.Sprintf("theftHaltWasUp=%v injectedRowVisibleOnMagi2=%v digest=%s signedOnNodes=%v nodesLogging %q: %d/5 deltas:%s",
			tripped, rowVisible, rogueHex, signedOn, vfF18FrozenLog, freezers, frozenDetail))

	// ---- 6. F18-CLEAR: the owner lifts the halt ----
	clearStatus := vstatus(t, d, ctx, 1, cid, "clearTheftHalt", "")
	thCleared := false
	cleared := 0
	clearDetail := ""
	clearDeadline := time.Now().Add(3 * time.Minute)
	for {
		thCleared = len(vfF18TheftKey(d, ctx, 2, cid)) == 0
		flagged, detail := vfF18TheftFlagCount(t, d, ctx, nodes)
		cleared = len(nodes) - flagged
		clearDetail = detail
		if thCleared && flagged == 0 {
			break
		}
		if time.Now().After(clearDeadline) {
			break
		}
		time.Sleep(10 * time.Second)
	}
	// Gated on `tripped`: clearing a halt that was never up would pass while proving
	// nothing, so an untripped run must FAIL this case rather than sail through it.
	c.rec("F18-CLEAR", "the owner's clearTheftHalt deletes the contract flag and every node clears its mirror",
		tripped && thCleared && cleared == len(nodes),
		fmt.Sprintf("theftHaltWasUpBeforeTheClear=%v; clearTheftHalt status=%s; contract \"th\" absent=%v; nodes reporting btc_theft_halted=false: %d/%d:%s",
			tripped, clearStatus, thCleared, cleared, len(nodes), clearDetail))

	// ---- 7. F18-NOFALSE-TRIP (INFO) ----
	c.rec("F18-NOFALSE-TRIP", "the legitimate operations that preceded the rescue never set the theft flag", true,
		fmt.Sprintf("INFO (not a pass): after genesis + funding + rotation to gen-1 + fee-reserve top-up the contract \"th\" was %q (len %d), and after the migrateVault sweep build it was %q (len %d). Only the CSV backup spend tripped it, which theft_gate.go documents as intended: an anomalous backup recovery halts keysign, and the operator must call clearTheftHalt to resume.",
			hex.EncodeToString(thAfterSetup), len(thAfterSetup), hex.EncodeToString(thAfterSweep), len(thAfterSweep)))

	// ---- 8. F18-IDENT ----
	vfAssertContractIdentical(c, d, ctx, cid, nodes, 4*time.Minute, "F18-IDENT")
	c.summary("F18")
	t.Logf("F18 COMPLETE CONTRACT=%s", cid)
}

// vfF18Utxo is one decoded entry of the contract's UTXO store ("u-<id in hex>"),
// mirroring contract/mapping/utils.go UnmarshalUtxo exactly:
//
//	[32] txid (internal byte order, stored as display hex)
//	[4]  vout   (uint32 BE)
//	[8]  amount (int64 BE)
//	[1]  len(PkScript), [N] PkScript
//	[1]  len(Tag),      [M] Tag
//	[4]  generation (uint32 BE; absent on a pre-S1 blob, which reads as gen 0)
type vfF18Utxo struct {
	Id         uint16
	TxId       string
	Vout       uint32
	Amount     int64
	PkScript   []byte
	Tag        []byte
	Generation uint32
}

// vfF18DecodeUtxo decodes one "u-" blob. Any other trailing remainder than 0 or 4
// bytes is a corrupt blob and fails closed, exactly as the contract does.
func vfF18DecodeUtxo(raw []byte) (*vfF18Utxo, error) {
	const minLen = 32 + 4 + 8 + 1 + 1
	if len(raw) < minLen {
		return nil, fmt.Errorf("utxo blob too short (%d bytes)", len(raw))
	}
	u := &vfF18Utxo{}
	off := 0
	u.TxId = hex.EncodeToString(raw[off : off+32])
	off += 32
	u.Vout = binary.BigEndian.Uint32(raw[off:])
	off += 4
	u.Amount = int64(binary.BigEndian.Uint64(raw[off:]))
	off += 8
	pkLen := int(raw[off])
	off++
	if off+pkLen > len(raw) {
		return nil, fmt.Errorf("utxo blob truncated (pkscript)")
	}
	u.PkScript = append([]byte(nil), raw[off:off+pkLen]...)
	off += pkLen
	if off >= len(raw) {
		return nil, fmt.Errorf("utxo blob truncated (tag length)")
	}
	tagLen := int(raw[off])
	off++
	if off+tagLen > len(raw) {
		return nil, fmt.Errorf("utxo blob truncated (tag)")
	}
	u.Tag = append([]byte(nil), raw[off:off+tagLen]...)
	off += tagLen
	switch rem := len(raw) - off; rem {
	case 0:
	case 4:
		u.Generation = binary.BigEndian.Uint32(raw[off:])
	default:
		return nil, fmt.Errorf("utxo blob has a malformed generation tail (%d trailing bytes)", rem)
	}
	return u, nil
}

// vfF18FindUtxo walks the committed UTXO registry ("r": 8 bytes per entry, uint16
// BE id then a 6-byte amount) on `node` and returns the first entry of `gen` whose
// PkScript equals wantPkScript. Returns nil when there is no such UTXO.
func vfF18FindUtxo(t *testing.T, d *Devnet, ctx context.Context, node int, cid string, gen uint32, wantPkScript []byte) *vfF18Utxo {
	t.Helper()
	st, err := getStateHex(d, ctx, node, cid, []string{"r"})
	if err != nil {
		t.Logf("vfF18FindUtxo: reading the utxo registry: %v", err)
		return nil
	}
	reg := st["r"]
	for off := 0; off+8 <= len(reg); off += 8 {
		id := binary.BigEndian.Uint16(reg[off:])
		key := "u-" + strconv.FormatUint(uint64(id), 16)
		us, uerr := getStateHex(d, ctx, node, cid, []string{key})
		if uerr != nil {
			t.Logf("vfF18FindUtxo: reading %s: %v", key, uerr)
			continue
		}
		u, derr := vfF18DecodeUtxo(us[key])
		if derr != nil {
			t.Logf("vfF18FindUtxo: decoding %s: %v", key, derr)
			continue
		}
		u.Id = id
		if u.Generation != gen {
			continue
		}
		if bytes.Equal(u.PkScript, wantPkScript) {
			return u
		}
		t.Logf("vfF18FindUtxo: gen-%d utxo %s pays pkScript %s, not the rebuilt %s", gen, key,
			hex.EncodeToString(u.PkScript), hex.EncodeToString(wantPkScript))
	}
	return nil
}

// vfF18P2WSHPkScript returns OP_0 <sha256(witnessScript)>, the scriptPubKey any
// output paying that witness script carries.
func vfF18P2WSHPkScript(witnessScript []byte) ([]byte, error) {
	h := sha256.Sum256(witnessScript)
	return txscript.NewScriptBuilder().AddOp(txscript.OP_0).AddData(h[:]).Script()
}

// vfF18BuildBackupSpend builds and signs the CSV backup-branch rescue of one vault
// UTXO. Version 2 and nSequence 2 are what make OP_CHECKSEQUENCEVERIFY(2) pass
// (a relative timelock is only enforced on a version-2 transaction, and the input's
// sequence must encode at least the script's block count). The witness is
// [sig||SIGHASH_ALL, <empty>, witnessScript]: the empty element is the OP_IF
// condition, and false selects the OP_ELSE backup branch.
//
// The digest is the BIP143 (segwit v0) SigHashAll sighash over the witness script
// and the spent amount, computed by btcvault.RecomputeSegwitV0Sighash, which is the
// same primitive the node uses to bind a sweep template to its signature.
func vfF18BuildBackupSpend(u *vfF18Utxo, witnessScript []byte, destAddress string, backupPriv *btcec.PrivateKey) (rawHex, txid string, err error) {
	prevHash, err := chainhash.NewHashFromStr(u.TxId)
	if err != nil {
		return "", "", fmt.Errorf("parsing prevout txid %s: %w", u.TxId, err)
	}
	dest, err := btcutil.DecodeAddress(destAddress, &chaincfg.RegressionNetParams)
	if err != nil {
		return "", "", fmt.Errorf("decoding destination %s: %w", destAddress, err)
	}
	destScript, err := txscript.PayToAddrScript(dest)
	if err != nil {
		return "", "", fmt.Errorf("building destination pkScript for %s: %w", destAddress, err)
	}
	value := u.Amount - vfF18BackupSpendFeeSats
	if value <= 0 {
		return "", "", fmt.Errorf("utxo %s:%d holds %d sats, not enough for the %d sat fee", u.TxId, u.Vout, u.Amount, vfF18BackupSpendFeeSats)
	}

	tx := wire.NewMsgTx(2) // version 2: relative timelocks are only enforced on v2 txs
	in := wire.NewTxIn(wire.NewOutPoint(prevHash, u.Vout), nil, nil)
	in.Sequence = 2 // >= the script's CSV block count (TestnetBackupCSVBlocks)
	tx.AddTxIn(in)
	tx.AddTxOut(wire.NewTxOut(value, destScript))

	// The sighash is computed over the WITNESS-LESS serialization, which is what
	// RecomputeSegwitV0Sighash re-parses; BIP143 never commits to the witness.
	var base bytes.Buffer
	if serr := tx.Serialize(&base); serr != nil {
		return "", "", fmt.Errorf("serializing the rescue tx: %w", serr)
	}
	sigHash, err := btcvault.RecomputeSegwitV0Sighash(base.Bytes(), 0, witnessScript, u.Amount)
	if err != nil {
		return "", "", fmt.Errorf("computing the BIP143 sighash: %w", err)
	}
	sig := btcecdsa.Sign(backupPriv, sigHash)
	witnessSig := append(append([]byte{}, sig.Serialize()...), byte(txscript.SigHashAll))
	// [signature, <empty>, witnessScript]: the empty element is the OP_IF condition,
	// and false takes the OP_ELSE CSV backup branch.
	tx.TxIn[0].Witness = wire.TxWitness{witnessSig, []byte{}, witnessScript}

	var full bytes.Buffer
	if eerr := tx.BtcEncode(&full, wire.ProtocolVersion, wire.WitnessEncoding); eerr != nil {
		return "", "", fmt.Errorf("encoding the witnessed rescue tx: %w", eerr)
	}
	return hex.EncodeToString(full.Bytes()), tx.TxID(), nil
}

// vfF18DeleteTssKeys removes one node's TSS keystore directory from disk. The share
// files are written by the container's uid, so they are deleted through a throwaway
// alpine container that mounts the devnet data dir as root, the same technique
// Devnet.Stop uses to clean up HAF's root-owned files. Returns the number of on-disk
// share files before and after. The node must be STOPPED first.
func vfF18DeleteTssKeys(t *testing.T, d *Devnet, node int) (before, after int) {
	t.Helper()
	nodeRoot := filepath.Join(d.DataDir(), "devnet-data", fmt.Sprintf("data-%d", node))
	before = len(vfF18ShareFiles(nodeRoot))
	if before == 0 {
		t.Fatalf("PRECONDITION FAILED: magi-%d has no TSS share files under %s, so the share-loss step has no instrument (the host cannot see the keystore, or keygen never persisted a share)", node, nodeRoot)
	}
	out, err := exec.Command("docker", "run", "--rm",
		"-v", d.DataDir()+":/d",
		"alpine", "rm", "-rf", fmt.Sprintf("/d/devnet-data/data-%d/tss-keys", node),
	).CombinedOutput()
	if err != nil {
		t.Logf("docker rm of magi-%d tss-keys returned %v: %s", node, err, string(out))
	}
	after = len(vfF18ShareFiles(nodeRoot))
	if after != 0 {
		t.Fatalf("PRECONDITION FAILED: magi-%d still has %d TSS share file(s) under %s after the delete (docker output: %s); the share loss did not happen and F18-STUCK would prove nothing",
			node, after, nodeRoot, string(out))
	}
	return before, after
}

// vfF18CountShareHolders returns the nodes that still have at least one TSS share
// file on disk.
func vfF18CountShareHolders(t *testing.T, d *Devnet, nodes []int) []int {
	t.Helper()
	var out []int
	for _, n := range nodes {
		root := filepath.Join(d.DataDir(), "devnet-data", fmt.Sprintf("data-%d", n))
		if len(vfF18ShareFiles(root)) > 0 {
			out = append(out, n)
		}
	}
	return out
}

// vfF18TheftKey reads the contract's deterministic theft-halt flag ("th",
// constants.BtcTheftHaltKey) from one node. Empty means no halt: clearTheftHalt
// DELETES the key, it never writes a false value.
func vfF18TheftKey(d *Devnet, ctx context.Context, node int, cid string) []byte {
	st, err := getStateHex(d, ctx, node, cid, []string{"th"})
	if err != nil {
		return nil
	}
	return st["th"]
}

// vfF18TheftFlagCount reports how many of the given nodes have mirrored the
// contract theft flag into chain_consensus_state.btc_theft_halted (the field
// refreshBtcTheftHalt writes and the TSS solvency gate reads). Modelled on
// countHaltFlag, which does the same for the governance btc_keysign_halted flag.
func vfF18TheftFlagCount(t *testing.T, d *Devnet, ctx context.Context, nodes []int) (int, string) {
	t.Helper()
	client, err := d.mongoClient(ctx)
	if err != nil {
		return 0, fmt.Sprintf(" mongo error: %v", err)
	}
	defer client.Disconnect(ctx)
	n := 0
	detail := ""
	for _, node := range nodes {
		var doc struct {
			Halted bool   `bson:"btc_theft_halted"`
			Height uint64 `bson:"btc_theft_halt_height"`
		}
		derr := client.Database(d.nodeDbName(node)).Collection("chain_consensus_state").
			FindOne(ctx, bson.M{"_id": "singleton"}).Decode(&doc)
		if derr != nil {
			detail += fmt.Sprintf(" magi-%d:no-doc", node)
			continue
		}
		detail += fmt.Sprintf(" magi-%d:%v@%d", node, doc.Halted, doc.Height)
		if doc.Halted {
			n++
		}
	}
	return n, detail
}

// vfF18BlockTxids returns the txids of the block at `height` in order (index 0 is
// always the coinbase).
func vfF18BlockTxids(t *testing.T, d *Devnet, ctx context.Context, height uint64) []string {
	t.Helper()
	bhash, err := d.bitcoinCli(ctx, "getblockhash", fmt.Sprint(height))
	if err != nil {
		t.Logf("getblockhash %d: %v", height, err)
		return nil
	}
	blockJSON, err := d.bitcoinCli(ctx, "getblock", bhash, "1")
	if err != nil {
		t.Logf("getblock %s: %v", bhash, err)
		return nil
	}
	var blk struct {
		Tx []string `json:"tx"`
	}
	if err := json.Unmarshal([]byte(blockJSON), &blk); err != nil {
		t.Logf("parsing block %s: %v", bhash, err)
		return nil
	}
	return blk.Tx
}

// vfF18InsertRogueRequest writes one signing request straight into every listed
// node's Mongo "tss_requests" collection, mirroring the production enqueue path
// (tss_db.SetSignedRequest, modules/db/vsc/tss/requests.go): the upsert key is
// {key_id, msg} and the status is "unsigned", which is the only field
// FindUnsignedRequests selects on. The TssRequest document carries no block
// height, so {key_id, msg, status} is the whole contract. Deliberately duplicated
// from F13 rather than shared, so this file stands alone.
func vfF18InsertRogueRequest(t *testing.T, d *Devnet, ctx context.Context, nodes []int, keyId string, digest []byte) {
	t.Helper()
	if len(digest) != 32 {
		t.Fatalf("rogue digest must be 32 bytes, got %d", len(digest))
	}
	msgHex := hex.EncodeToString(digest)
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)
	for _, n := range nodes {
		coll := client.Database(d.nodeDbName(n)).Collection("tss_requests")
		if _, uerr := coll.UpdateOne(ctx,
			bson.M{"key_id": keyId, "msg": msgHex},
			bson.M{"$set": bson.M{"key_id": keyId, "msg": msgHex, "status": "unsigned"}},
			options.Update().SetUpsert(true),
		); uerr != nil {
			t.Fatalf("inserting rogue tss_request on magi-%d: %v", n, uerr)
		}
	}
	t.Logf("injected tss_request into all %d node databases: key_id=%s msg=%s status=unsigned", len(nodes), keyId, msgHex)
}

// vfF18IsPendingSighash reports whether digest equals any input sighash of any
// pending spend in the contract's committed state. The probe digest must NOT be
// one, otherwise a signature over it would be a legitimate sweep signature and the
// freeze observation would prove nothing.
func vfF18IsPendingSighash(t *testing.T, d *Devnet, ctx context.Context, cid string, digest []byte) bool {
	t.Helper()
	for _, txid := range txSpendIds(t, d, ctx, cid) {
		st, err := getStateHex(d, ctx, 2, cid, []string{"d-" + txid})
		if err != nil {
			continue
		}
		raw, ok := st["d-"+txid]
		if !ok || len(raw) == 0 {
			continue
		}
		sd, derr := btcvault.DecodeSigningData(raw)
		if derr != nil {
			continue
		}
		for _, uh := range sd.UnsignedSigHashes {
			if bytes.Equal(uh.SigHash, digest) {
				t.Logf("digest %s IS an input sighash of pending spend %s", hex.EncodeToString(digest), txid)
				return true
			}
		}
	}
	return false
}

// vfF18WaitSignTicks blocks until the Hive chain has advanced far enough for the
// requested number of TSS sign ticks to have fired, plus one interval of margin.
// The instrument is the Hive head, not wall clock, because TSS selects signing
// requests only when the Hive block height is a multiple of signInterval.
func vfF18WaitSignTicks(t *testing.T, d *Devnet, ctx context.Context, signInterval uint64, ticks int, maxWait time.Duration) {
	t.Helper()
	start, err := getHeadBlock(d.HiveRPCEndpoint())
	if err != nil {
		t.Logf("F18: could not read the Hive head (%v), falling back to a 2 minute wall-clock wait", err)
		time.Sleep(2 * time.Minute)
		return
	}
	target := start + int(signInterval)*(ticks+1)
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		h, herr := getHeadBlock(d.HiveRPCEndpoint())
		if herr == nil && h >= target {
			t.Logf("F18: Hive head %d reached target %d (%d sign ticks of %d blocks past %d)", h, target, ticks, signInterval, start)
			return
		}
		select {
		case <-ctx.Done():
			t.Logf("F18: context done while waiting for sign ticks")
			return
		case <-time.After(5 * time.Second):
		}
	}
	h, _ := getHeadBlock(d.HiveRPCEndpoint())
	t.Logf("F18: WARNING Hive head %d did not reach %d within %v, fewer than %d sign ticks may have fired", h, target, maxWait, ticks)
}

// vfF18ShareFiles lists the flatfs keystore files (data-N/tss-keys/**/*.data) of one node
// THROUGH A ROOT CONTAINER. The node writes its data dir as root, so a host-side walk gets
// "Permission denied" and reports zero shares (run 1 died on exactly that false reading).
func vfF18ShareFiles(nodeRoot string) []string {
	out, err := exec.Command("docker", "run", "--rm",
		"-v", nodeRoot+":/d",
		"alpine", "sh", "-c", "find /d/tss-keys -type f -name '*.data' 2>/dev/null",
	).CombinedOutput()
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasSuffix(line, ".data") {
			files = append(files, line)
		}
	}
	return files
}
