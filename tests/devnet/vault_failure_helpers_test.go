package devnet

// vault_failure_helpers_test.go — shared helpers for the BTC vault-rotation-v2
// FAILURE-STATE suite (vault_f*_test.go). Every helper here is prefixed vf so it can
// never collide with the July harness helpers (vault_stage*_test.go) or with the POA
// suite on fix/poa-hardening-c1-c8 when both land on develop.
//
// Design rules for the suite (see /mnt/o/MAGI-VAULT-V2-TESTNET-2026-09-05/01-STATUS-AND-TEST-PLAN.md):
//   1. Every test asserts its PRECONDITION (v2 really active, registry really populated)
//      so it cannot pass against an inert system.
//   2. The halt instrument is block_headers (a row exists only for a VSC block that
//      gathered BLS quorum). hive_blocks advances regardless of quorum and must not be
//      used to observe a halt.
//   3. Every test ends with vfAssertContractIdentical across ALL nodes, and the ones
//      that touch a stopped/replaying node also compare a node that was fully re-indexed.
//   4. During a quorum-loss halt a contract call's transaction status never reaches
//      CONFIRMED, so tests read contract STATE from a live node instead of trusting
//      vstatus' terminal status.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/btcvault"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// vfStateKeys is the set of contract state keys whose bytes define "the vault state"
// for cross-node comparison. r = UTXO registry, p = pending spends, s = supply,
// v/vn/va = vault registry + counters, msl = migration sweep index, mv = migrate
// version, th = theft halt, paused, vaultop = operator, h = last BTC height.
var vfStateKeys = []string{"v", "vn", "va", "r", "p", "s", "msl", "mv", "th", "paused", "vaultop", "h"}

// vfCase is the pass/fail recorder every failure test uses so a summary line can be
// grepped: "VF SUMMARY: <n> PASS <m> FAIL".
type vfCase struct {
	t    *testing.T
	pass int
	fail int
}

func (c *vfCase) rec(id, desc string, ok bool, detail string) {
	c.t.Helper()
	if ok {
		c.pass++
		c.t.Logf("CASE %s PASS — %s | %s", id, desc, detail)
	} else {
		c.fail++
		c.t.Errorf("CASE %s FAIL — %s | %s", id, desc, detail)
	}
}

func (c *vfCase) summary(name string) {
	c.t.Logf("VF SUMMARY %s: %d PASS %d FAIL", name, c.pass, c.fail)
}

// vfMaxSlotHeight returns the highest slot_height in a node's block_headers — the
// correct halt instrument (a row exists only for a VSC block that reached BLS quorum).
func vfMaxSlotHeight(d *Devnet, ctx context.Context, node int) (int, error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return 0, err
	}
	defer client.Disconnect(ctx)
	var doc struct {
		SlotHeight int `bson:"slot_height"`
	}
	opts := options.FindOne().SetSort(bson.D{{Key: "slot_height", Value: -1}})
	err = client.Database(d.nodeDbName(node)).Collection("block_headers").FindOne(ctx, bson.M{}, opts).Decode(&doc)
	if err != nil {
		return 0, fmt.Errorf("block_headers on magi-%d: %w", node, err)
	}
	return doc.SlotHeight, nil
}

// vfGrewWithin reports whether a node's VSC block height (block_headers) advanced
// during the window. Returns (grew, start, last).
func vfGrewWithin(d *Devnet, ctx context.Context, node int, window time.Duration) (bool, int, int) {
	start, err := vfMaxSlotHeight(d, ctx, node)
	if err != nil {
		start = 0
	}
	deadline := time.Now().Add(window)
	last := start
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Second)
		h, err := vfMaxSlotHeight(d, ctx, node)
		if err != nil {
			continue
		}
		last = h
		if h > start {
			return true, start, h
		}
	}
	return false, start, last
}

// vfStateOn reads the vault state keys from ONE node.
func vfStateOn(d *Devnet, ctx context.Context, node int, cid string) (map[string][]byte, error) {
	return getStateHex(d, ctx, node, cid, vfStateKeys)
}

// vfFingerprint hashes a state map deterministically (sorted keys, key|len|bytes).
func vfFingerprint(st map[string][]byte) string {
	keys := make([]string, 0, len(st))
	for k := range st {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s|%d|", k, len(st[k]))
		h.Write(st[k])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// vfDiffKeys lists the keys whose bytes differ between two state maps.
func vfDiffKeys(a, b map[string][]byte) []string {
	var out []string
	for _, k := range vfStateKeys {
		if !bytes.Equal(a[k], b[k]) {
			out = append(out, fmt.Sprintf("%s(%d vs %d bytes)", k, len(a[k]), len(b[k])))
		}
	}
	return out
}

// vfAssertContractIdentical waits up to `within` for every listed node to report the
// byte-identical vault state (same fingerprint) for the contract, then records the
// verdict on the recorder. It is the fork detector for contract state. Nodes that
// cannot be read at all (down) fail the case; call it only with live nodes.
func vfAssertContractIdentical(c *vfCase, d *Devnet, ctx context.Context, cid string, nodes []int, within time.Duration, caseId string) bool {
	c.t.Helper()
	deadline := time.Now().Add(within)
	var states map[int]map[string][]byte
	var fps map[int]string
	for {
		states = map[int]map[string][]byte{}
		fps = map[int]string{}
		allOK := true
		for _, n := range nodes {
			st, err := vfStateOn(d, ctx, n, cid)
			if err != nil {
				allOK = false
				c.t.Logf("  magi-%d state read failed: %v", n, err)
				break
			}
			states[n] = st
			fps[n] = vfFingerprint(st)
		}
		if allOK {
			same := true
			ref := fps[nodes[0]]
			for _, n := range nodes {
				if fps[n] != ref {
					same = false
				}
			}
			if same {
				c.rec(caseId, "vault contract state byte-identical across nodes", true,
					fmt.Sprintf("nodes=%v fp=%s", nodes, ref))
				return true
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Second)
	}
	detail := ""
	for _, n := range nodes {
		detail += fmt.Sprintf(" magi-%d=%s", n, fps[n])
		if n != nodes[0] && states[n] != nil && states[nodes[0]] != nil {
			detail += fmt.Sprintf(" diff=%v", vfDiffKeys(states[nodes[0]], states[n]))
		}
	}
	c.rec(caseId, "vault contract state byte-identical across nodes", false, "DIVERGED:"+detail)
	return false
}

// vfVaultStatusOn returns a generation's status as read from a specific node (-1 if
// unreadable or absent).
func vfVaultStatusOn(d *Devnet, ctx context.Context, node int, cid string, gen uint32) int {
	st, err := getStateHex(d, ctx, node, cid, []string{"v"})
	if err != nil {
		return -1
	}
	vs, err := btcvault.UnmarshalVaultRegistry(st["v"])
	if err != nil {
		return -1
	}
	for _, v := range vs {
		if v.Generation == gen {
			return int(v.Status)
		}
	}
	return -1
}

// vfVaultRegistryOn decodes the vault registry from one node (nil if absent).
func vfVaultRegistryOn(d *Devnet, ctx context.Context, node int, cid string) []btcvault.Vault {
	st, err := getStateHex(d, ctx, node, cid, []string{"v"})
	if err != nil || len(st["v"]) == 0 {
		return nil
	}
	vs, err := btcvault.UnmarshalVaultRegistry(st["v"])
	if err != nil {
		return nil
	}
	return vs
}

// vfSweepRecordOn reports whether the "ms-<txid>" migration record is live on a node.
func vfSweepRecordOn(d *Devnet, ctx context.Context, node int, cid, txid string) bool {
	st, err := getStateHex(d, ctx, node, cid, []string{"ms-" + txid})
	if err != nil {
		return false
	}
	return len(st["ms-"+txid]) > 0
}

// vfGenUtxoCountOn is genUtxoCount read from a specific node.
func vfGenUtxoCountOn(d *Devnet, ctx context.Context, node int, cid string, gen uint32) int {
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
		raw := us[key]
		if len(raw) < 4 {
			continue
		}
		g := uint32(raw[len(raw)-4])<<24 | uint32(raw[len(raw)-3])<<16 | uint32(raw[len(raw)-2])<<8 | uint32(raw[len(raw)-1])
		if g == gen {
			n++
		}
	}
	return n
}

// vfPreconditionV2Active asserts, from `node`, that the rotation flag is REALLY in
// force: the node has processed past hpin AND the vault registry is populated. A test
// that runs against an inert system (flag 0, registry absent) passes while proving
// nothing, so this is mandatory before any v2 assertion.
func vfPreconditionV2Active(t *testing.T, d *Devnet, ctx context.Context, node int, cid string, hpin uint64) {
	t.Helper()
	bh, err := d.getLastProcessedBlock(ctx, node)
	if err != nil || bh <= hpin {
		t.Fatalf("PRECONDITION FAILED: magi-%d processed=%d (err=%v) is not past hpin=%d — v2 is NOT active, the test would be vacuous", node, bh, err, hpin)
	}
	vs := vfVaultRegistryOn(d, ctx, node, cid)
	if len(vs) == 0 {
		t.Fatalf("PRECONDITION FAILED: vault registry 'v' is empty on magi-%d — the contract never folded/minted, v2 gates are inert", node)
	}
	t.Logf("PRECONDITION OK: magi-%d processed=%d > hpin=%d, vault registry has %d generation(s)", node, bh, hpin, len(vs))
}

// vfSetup deploys the v2 contract onto a fresh devnet, seeds BTC headers, wires the
// oracle, mints + registers gen-0 (v2 OFF at that point when hpin > genesis height,
// which avoids the genesis path) and funds gen-0 via a real SPV deposit.
// Returns contractId, seed height, gen-0 primary pubkey hex, and the owner account.
type vfEnv struct {
	cid      string
	seedH    uint64
	primary0 string
	owner    string
	hpin     uint64
}

func vfSetup(t *testing.T, d *Devnet, ctx context.Context, wasm string, hpin uint64, fundSats int64, desc string) *vfEnv {
	t.Helper()
	seedH, err := d.MineBlocks(ctx, 101)
	if err != nil {
		t.Fatalf("mine: %v", err)
	}
	hdr1, _ := btcBlockHeaderHex(ctx, d, seedH)
	cid, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm, Name: "btc-mapping-contract", Description: desc, DeployerNode: 1, GQLNode: 2,
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	t.Logf("CONTRACT=%s hpin=%d", cid, hpin)
	if s := vstatus(t, d, ctx, 1, cid, "seedBlocks", fmt.Sprintf(`{"block_header":"%s","block_height":%d}`, hdr1, seedH)); !isOK(s) {
		t.Fatalf("seedBlocks: %s", s)
	}
	d.WriteOracleConfigs(ctx)
	d.SetOracleContractIDs(map[string]string{"BTC": cid})
	d.RestartAllMagiNodes(ctx)
	time.Sleep(10 * time.Second)

	vstatus(t, d, ctx, 1, cid, "createKey", "")
	kd0, err := d.WaitForTssKey(ctx, 2, bson.M{"id": cid + "-main", "status": "active"}, 8*time.Minute)
	if err != nil {
		t.Fatalf("gen0 keygen: %v", err)
	}
	primary0 := kd0.PublicKey
	if s := vstatus(t, d, ctx, 1, cid, "registerPublicKey",
		fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, primary0, backupPubKeyG)); !isOK(s) {
		t.Fatalf("gen0 register: %s", s)
	}
	owner := "hive:" + fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, 1)
	if fundSats > 0 {
		fundVaultViaSPV(t, d, ctx, cid, primary0, backupPubKeyG, owner, fundSats, seedH)
		// The map is CONFIRMED on the calling node before magi-2 (the reading node) has
		// applied it; F13 run 1 died here on a stale read. Poll for up to 2 minutes.
		credited := balanceCredited(t, d, ctx, cid, owner)
		for i := 0; i < 12 && !credited; i++ {
			time.Sleep(10 * time.Second)
			credited = balanceCredited(t, d, ctx, cid, owner)
		}
		if !credited {
			t.Fatalf("gen-0 funding failed: %s balance still 0 on magi-2 two minutes after the CONFIRMED map", owner)
		}
		t.Logf("gen-0 funded: %s = %d sats", owner, balanceSats(t, d, ctx, cid, owner))
	}
	return &vfEnv{cid: cid, seedH: seedH, primary0: primary0, owner: owner, hpin: hpin}
}

// vfWaitV2On blocks until node 2 has processed past hpin+5 (v2 gates live).
func vfWaitV2On(t *testing.T, d *Devnet, ctx context.Context, hpin uint64) {
	t.Helper()
	// 18 minutes, not 8: under load (two devnet lanes plus compiles on one box) VSC
	// processing lags Hive by tens of blocks, and the July suite's 8-minute wait was
	// observed to expire at block 292 with hpin=400, after which its assertions ran
	// against an INERT system and produced a false FAIL (BondLock, 2026-09-05). This
	// helper never continues past a missed activation.
	if err := d.WaitForBlockProcessing(ctx, 2, hpin+5, 18*time.Minute); err != nil {
		t.Fatalf("PRECONDITION FAILED: v2 never switched on (magi-2 processed height never passed hpin=%d): %v", hpin, err)
	}
	// Every node that a later assertion may read must ALSO be past the pin, or a read
	// from a lagging node sees pre-v2 behaviour.
	for _, n := range vfAllNodes(d.cfg.Nodes) {
		if !vfWaitProcessed(t, d, ctx, n, hpin+1, 6*time.Minute) {
			bh, err := d.getLastProcessedBlock(ctx, n)
			t.Fatalf("PRECONDITION FAILED: magi-%d processed=%d (err=%v) has not passed hpin=%d", n, bh, err, hpin)
		}
	}
}

// vfMintNextGen calls createKey and waits for the generation's keygen to land as an
// ACTIVE tss_keys row on `node`. Returns the new primary pubkey hex.
func vfMintNextGen(t *testing.T, d *Devnet, ctx context.Context, cid string, gen int, node int, wait time.Duration) (string, bool) {
	t.Helper()
	s := vstatus(t, d, ctx, 1, cid, "createKey", "")
	if !isOK(s) {
		t.Logf("createKey status=%s", s)
		return "", false
	}
	kd, err := d.WaitForTssKey(ctx, node, bson.M{"id": cid + "-" + btcvault.VaultKeyName(uint32(gen)), "status": "active"}, wait)
	if err != nil {
		return "", false
	}
	return kd.PublicKey, true
}

// vfRegisterAndActivate registers the pending generation's keys and retries
// activateKey until the BRK-2 check-signature has been verified (or attempts run out).
func vfRegisterAndActivate(t *testing.T, d *Devnet, ctx context.Context, cid, primary string, attempts int) bool {
	t.Helper()
	if s := vstatus(t, d, ctx, 1, cid, "registerPublicKey",
		fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, primary, backupPubKeyG)); !isOK(s) {
		t.Logf("registerPublicKey status=%s", s)
		return false
	}
	for i := 0; i < attempts; i++ {
		if isOK(vstatus(t, d, ctx, 1, cid, "activateKey", "")) {
			return true
		}
		t.Logf("activateKey not yet (awaiting BRK-2 check-sig)... retry %d", i)
		time.Sleep(15 * time.Second)
	}
	vfDumpCheckSigDiagnostics(t, d, ctx, cid)
	return false
}

// vfDumpCheckSigDiagnostics explains a check-signature that never landed: which
// generation is Pending, how many sign requests its key has (and how many carry a
// signature) on every node, and the node-log counts that name the mechanism (the
// scoping gate issuing or refusing the sign, and TSS session timeouts). F19 run 1 failed
// here with no evidence; this makes the next failure self-explaining.
func vfDumpCheckSigDiagnostics(t *testing.T, d *Devnet, ctx context.Context, cid string) {
	t.Helper()
	keyId := ""
	for g := uint32(1); g <= 4 && keyId == ""; g++ {
		if vfVaultStatusOn(d, ctx, 2, cid, g) == 0 {
			keyId = cid + "-" + btcvault.VaultKeyName(g)
		}
	}
	t.Logf("CHECK-SIG DIAGNOSTICS: pending keyId=%q", keyId)
	for _, n := range vfAllNodes(d.cfg.Nodes) {
		reqs, sigs := 0, 0
		if keyId != "" {
			if rs, err := d.GetTssRequests(ctx, n, keyId); err == nil {
				reqs = len(rs)
				for _, r := range rs {
					if r.Sig != "" {
						sigs++
					}
				}
			}
		}
		keys, _ := d.GetTssKeys(ctx, n, bson.M{"id": keyId})
		keyStatus := "absent"
		if len(keys) > 0 {
			keyStatus = keys[0].Status
		}
		t.Logf("  magi-%d: tss_key=%s sign_requests=%d signed=%d | logs: issuing=%d scoping_refused=%d timeout_result=%d successor_refused=%d",
			n, keyStatus, reqs, sigs,
			vfCountLogs(d, ctx, n, "BRK-2 check-signature for a pending vault generation; issuing"),
			vfCountLogs(d, ctx, n, "BTC keysign refused by output scoping"),
			vfCountLogs(d, ctx, n, "timeout result"),
			vfCountLogs(d, ctx, n, "successor key not committed/active"))
	}
}

// vfRotate performs the full gen-N -> gen-N+1 rotation (mint, keygen, register,
// check-sig, activate). Returns the successor primary pubkey hex.
func vfRotate(t *testing.T, d *Devnet, ctx context.Context, cid string, nextGen int) (string, bool) {
	t.Helper()
	p, ok := vfMintNextGen(t, d, ctx, cid, nextGen, 2, 8*time.Minute)
	if !ok {
		return "", false
	}
	// 20 attempts (about 10 minutes with the call round-trips): F19 run 1 saw no
	// check-signature within 12 attempts (~6.5 min) under two-lane load.
	if !vfRegisterAndActivate(t, d, ctx, cid, p, 20) {
		return p, false
	}
	return p, true
}

// vfBuildSweep issues migrateVault from opNode and returns the new pending sweep txid
// plus its signing data (nil if none appeared).
func vfBuildSweep(t *testing.T, d *Devnet, ctx context.Context, opNode int, cid string) (string, *btcvault.SigningData, string) {
	t.Helper()
	before := txSpendIds(t, d, ctx, cid)
	s := vstatus(t, d, ctx, opNode, cid, "migrateVault", "")
	if !isOK(s) {
		return "", nil, s
	}
	var txid string
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
		return "", nil, s
	}
	return txid, waitSigningData(t, d, ctx, cid, txid), s
}

// vfAwaitSweepSignatures collects the retiring key's signatures for every input of
// the sweep and returns the fully witnessed raw tx hex. ok=false if any input never
// received a signature within the per-input wait (40 polls of 3s in waitSignature).
func vfAwaitSweepSignatures(t *testing.T, d *Devnet, ctx context.Context, retiringKeyId string, sd *btcvault.SigningData) (string, bool) {
	t.Helper()
	var mtx wire.MsgTx
	if err := mtx.Deserialize(bytes.NewReader(sd.Tx)); err != nil {
		t.Logf("deser sweep: %v", err)
		return "", false
	}
	for _, uh := range sd.UnsignedSigHashes {
		sig := waitSignature(t, d, ctx, retiringKeyId, uh.SigHash)
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

// vfSignatureLanded reports whether ANY signature exists on `node` for the given
// key + sighash (without waiting).
func vfSignatureLanded(d *Devnet, ctx context.Context, node int, keyId string, sighash []byte) bool {
	msgHex := hex.EncodeToString(sighash)
	reqs, err := d.GetTssRequests(ctx, node, keyId)
	if err != nil {
		return false
	}
	for _, r := range reqs {
		if r.Msg == msgHex && r.Sig != "" {
			return true
		}
	}
	return false
}

// vfBroadcastAndMine broadcasts a witnessed tx to regtest and mines one block.
// Returns the broadcast txid and the block height.
func vfBroadcastAndMine(t *testing.T, d *Devnet, ctx context.Context, rawHex string) (string, uint64, error) {
	t.Helper()
	bcTxid, err := d.bitcoinCli(ctx, "sendrawtransaction", rawHex)
	if err != nil {
		return "", 0, err
	}
	h, err := d.MineBlocks(ctx, 1)
	if err != nil {
		return bcTxid, 0, err
	}
	return bcTxid, h, nil
}

// vfRelayAndConfirm relays every header from the contract's last height +1 to h via
// addBlocks (from callNode) and then issues confirmSpend for txid mined at h with the
// 2-tx-block merkle proof. Returns the confirmSpend status string.
func vfRelayAndConfirm(t *testing.T, d *Devnet, ctx context.Context, callNode int, cid, bcTxid string, h uint64) string {
	t.Helper()
	return vfRelayAndConfirmIndex(t, d, ctx, callNode, cid, bcTxid, h, 0)
}

// vfChangeVout returns the index of the first output of txid whose address is NOT
// `dest` (the vault change of a withdrawal), or -1.
func vfChangeVout(d *Devnet, ctx context.Context, txid, dest string) int {
	raw, err := d.bitcoinCli(ctx, "getrawtransaction", txid, "1")
	if err != nil {
		return -1
	}
	var tx struct {
		Vout []struct {
			N            int `json:"n"`
			ScriptPubKey struct {
				Address string `json:"address"`
			} `json:"scriptPubKey"`
		} `json:"vout"`
	}
	if json.Unmarshal([]byte(raw), &tx) != nil {
		return -1
	}
	for _, o := range tx.Vout {
		if o.ScriptPubKey.Address != dest {
			return o.N
		}
	}
	return -1
}

// vfDumpRegistry logs every UTXO registry entry of the contract as read from node:
// id, pool (confirmed when id >= 1024), amount and generation (contract layout: a
// blob whose length modulo 4 is 0 is legacy gen 0, otherwise the trailing 4 bytes).
func vfDumpRegistry(t *testing.T, d *Devnet, ctx context.Context, node int, cid, label string) {
	t.Helper()
	st, err := getStateHex(d, ctx, node, cid, []string{"r"})
	if err != nil {
		t.Logf("registry dump %s: %v", label, err)
		return
	}
	reg := st["r"]
	line := fmt.Sprintf("registry %s (magi-%d): %d entries;", label, node, len(reg)/8)
	for off := 0; off+8 <= len(reg); off += 8 {
		id := uint16(reg[off])<<8 | uint16(reg[off+1])
		amt := uint64(0)
		for _, b := range reg[off+2 : off+8] {
			amt = amt<<8 | uint64(b)
		}
		gen := "?"
		if us, err := getStateHex(d, ctx, node, cid, []string{"u-" + fmt.Sprintf("%x", id)}); err == nil {
			gen = vfUtxoGenLabel(us["u-"+fmt.Sprintf("%x", id)])
		}
		pool := "unconfirmed"
		if id >= 1024 {
			pool = "confirmed"
		}
		line += fmt.Sprintf(" [id=%d %s %d sats gen=%s]", id, pool, amt, gen)
	}
	t.Logf("%s", line)
}

// vfRelayAndConfirmIndex is vfRelayAndConfirm with an explicit output index.
func vfRelayAndConfirmIndex(t *testing.T, d *Devnet, ctx context.Context, callNode int, cid, bcTxid string, h uint64, index int) string {
	t.Helper()
	last := contractLastHeight(t, d, ctx, cid)
	for hh := last + 1; hh <= h; hh++ {
		hx, _ := btcBlockHeaderHex(ctx, d, hh)
		if s := vstatus(t, d, ctx, callNode, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":10}`, hx)); !isOK(s) {
			t.Logf("addBlocks %d status=%s", hh, s)
		}
	}
	bhash, _ := d.bitcoinCli(ctx, "getblockhash", fmt.Sprint(h))
	blockJSON, _ := d.bitcoinCli(ctx, "getblock", bhash, "1")
	var blk struct {
		Tx []string `json:"tx"`
	}
	json.Unmarshal([]byte(blockJSON), &blk)
	if len(blk.Tx) != 2 {
		t.Logf("confirm block has %d txs (want 2) — merkle proof helper assumes coinbase+1", len(blk.Tx))
	}
	rawTx, _ := d.bitcoinCli(ctx, "getrawtransaction", bcTxid)
	proof := reverseHexBytes(blk.Tx[0])
	return vstatus(t, d, ctx, callNode, cid, "confirmSpend", fmt.Sprintf(
		`{"tx_data":{"block_height":%d,"raw_tx_hex":"%s","merkle_proof_hex":"%s","tx_index":1},"indices":[%d]}`, h, rawTx, proof, index))
}

// vfStopNodes / vfStartNodes stop or start a set of magi nodes, logging each.
func vfStopNodes(t *testing.T, d *Devnet, ctx context.Context, nodes []int) {
	t.Helper()
	for _, n := range nodes {
		if err := d.StopNode(ctx, n); err != nil {
			t.Fatalf("stopping magi-%d: %v", n, err)
		}
		t.Logf("stopped magi-%d", n)
	}
}

func vfStartNodes(t *testing.T, d *Devnet, ctx context.Context, nodes []int) {
	t.Helper()
	for _, n := range nodes {
		if err := d.StartNode(ctx, n); err != nil {
			t.Fatalf("starting magi-%d: %v", n, err)
		}
		t.Logf("started magi-%d", n)
	}
}

// vfDropNodeDb wipes a STOPPED node's entire Mongo database so that on the next start
// it re-indexes from the first Hive block. The node keeps its identity + tss-keys
// flatfs on disk (exactly a real operator restoring a node from scratch).
func vfDropNodeDb(t *testing.T, d *Devnet, ctx context.Context, node int) {
	t.Helper()
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)
	if err := client.Database(d.nodeDbName(node)).Drop(ctx); err != nil {
		t.Fatalf("dropping db of magi-%d: %v", node, err)
	}
	t.Logf("dropped Mongo database %s (magi-%d will re-index from genesis)", d.nodeDbName(node), node)
}

// vfWaitProcessed waits until `node` has processed at least minBlock.
func vfWaitProcessed(t *testing.T, d *Devnet, ctx context.Context, node int, minBlock uint64, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		bh, err := d.getLastProcessedBlock(ctx, node)
		if err == nil && bh >= minBlock {
			return true
		}
		time.Sleep(10 * time.Second)
	}
	return false
}

// vfLogsContainAny reports whether any of the substrings appears in the node's logs.
func vfLogsContainAny(d *Devnet, ctx context.Context, node int, subs ...string) (string, bool) {
	logs, err := d.Logs(ctx, fmt.Sprintf("magi-%d", node))
	if err != nil {
		return "", false
	}
	for _, s := range subs {
		if strings.Contains(logs, s) {
			return s, true
		}
	}
	return "", false
}

// vfCountLogs counts occurrences of a substring in a node's logs.
func vfCountLogs(d *Devnet, ctx context.Context, node int, sub string) int {
	logs, err := d.Logs(ctx, fmt.Sprintf("magi-%d", node))
	if err != nil {
		return 0
	}
	return strings.Count(logs, sub)
}

// vfAllNodes returns [1..n].
func vfAllNodes(n int) []int {
	out := make([]int, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, i)
	}
	return out
}

// vfExcept returns nodes minus the excluded ones.
func vfExcept(nodes []int, excl ...int) []int {
	out := make([]int, 0, len(nodes))
	for _, n := range nodes {
		skip := false
		for _, e := range excl {
			if n == e {
				skip = true
			}
		}
		if !skip {
			out = append(out, n)
		}
	}
	return out
}

// vfUtxoGenLabel decodes the generation of a MarshalUtxo blob (txid 32, vout 4, amount 8,
// pkScript len+bytes, tag len+bytes, then an OPTIONAL 4-byte generation). Pre-S1 blobs
// have no generation field and belong to gen-0 by definition (contract UnmarshalUtxo);
// reading their last four bytes as a generation printed garbage in earlier dumps.
func vfUtxoGenLabel(raw []byte) string {
	off := 32 + 4 + 8
	if len(raw) < off+1 {
		return "?"
	}
	off += 1 + int(raw[off])
	if len(raw) < off+1 {
		return "?"
	}
	off += 1 + int(raw[off])
	switch {
	case len(raw) == off:
		return "0(legacy)"
	case len(raw) == off+4:
		return fmt.Sprint(uint32(raw[off])<<24 | uint32(raw[off+1])<<16 | uint32(raw[off+2])<<8 | uint32(raw[off+3]))
	default:
		return fmt.Sprintf("?(len=%d)", len(raw))
	}
}
