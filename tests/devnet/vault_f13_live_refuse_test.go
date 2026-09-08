package devnet

// vault_f13_live_refuse_test.go, F13: the LIVE anti-theft REFUSE case (P15).
//
// This is the live proof of NN#1 output scoping (modules/tss/output_scoping.go):
// a RETIRING generation's key is asked to sign a digest that is NOT a
// successor-paying sweep, and every node must refuse BEFORE it contributes a
// signature share. The retiring generation is the one the rotation exists to move
// away from, precisely because its key may be reconstructable, so letting it sign
// anything other than an evacuation to the committed successor P2WSH turns it into
// a theft oracle.
//
// Injection. Without a malicious contract there is no in-band way to ask the
// retiring key for an arbitrary digest, so the request is written straight into
// every node's Mongo "tss_requests" collection, exactly the way the production
// enqueue path writes it (tss_db.SetSignedRequest, modules/db/vsc/tss/requests.go):
// the upsert filter is {key_id, msg} and the only field it sets is
// status:"unsigned". FindUnsignedRequests(blockHeight) then selects purely on
// status:"unsigned"; it takes a block height for the "failed" revive pass but never
// filters on one, and the TssRequest document (modules/db/vsc/tss/interface.go)
// carries no height field at all, so {key_id, msg, status} is the whole contract.
// From that row tss.go (around line 590) builds a SignAction with Args = the hex
// decoded msg, and btcSignRefused must skip issuance before any session, party list
// or commitment work, logging "BTC keysign refused by output scoping (S3 NN#1)".
//
// Cases:
//
//	F13-REFUSED  the rogue digest for the RETIRING key is never signed on any node,
//	             and the output-scoping refusal is logged fleet wide.
//	F13-CONTROL  MANDATORY positive control: a LEGITIMATE successor sweep for the
//	             SAME retiring key still signs and settles. Without it, "refused"
//	             could simply mean "signing is broken".
//	F13-ACTIVE   INFO: the same injection against the ACTIVE generation. The active
//	             gen signs ordinary user withdrawals to arbitrary destinations and is
//	             DELIBERATELY not output scoped, so it is expected to sign. Recorded
//	             either way; the case passes when the observation itself is valid.
//	F13-IDENT    contract state byte identical across all 5 nodes at the end.
//
// Ordering note: the rogue row is deliberately left in place for the rest of the
// run, so every later sign tick re-refuses it. Mongo returns the unsigned requests
// in natural (insertion) order and the rogue row is inserted first on every node,
// so the per-block action index that feeds the TSS sessionId stays identical fleet
// wide and the later legitimate sweep is unaffected.
//
//	VAULT_F13_RUN=1 go test -v -run TestVaultF13LiveRefuse -timeout 95m ./tests/devnet/

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"vsc-node/lib/btcvault"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// vfF13RefuseLog is the substring of the output-scoping refusal WARN emitted by
// btcSignRefused (modules/tss/output_scoping.go). The full line is
// "BTC keysign refused by output scoping (S3 NN#1)"; this prefix is matched so the
// assertion survives a change to the parenthetical tag.
const vfF13RefuseLog = "BTC keysign refused by output scoping"

func TestVaultF13LiveRefuse(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F13_RUN") == "" {
		t.Skip("set VAULT_F13_RUN=1")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), vfTestBudget(85*time.Minute))
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		wasm = "/home/clauderfly/utxo-s1/btc-mapping-contract/bin/dev.wasm"
	}
	if _, err := os.Stat(wasm); err != nil {
		t.Fatalf("wasm: %v", err)
	}

	// v2 activation is pinned AFTER genesis (~block 190) so gen-0 mints on the
	// v2-off genesis path and the flag is live well before the rotation.
	const hpin = 400

	cfg := vfSlowReshareConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	// The wait between injection and observation is measured in SIGN TICKS, read
	// from the config rather than hardcoded: TSS ticks on Hive blocks and only
	// selects signing requests when bh%signInterval == 0 (tss.go around line 588).
	signInterval := cfg.SysConfigOverrides.TssParams.SignInterval
	if signInterval == 0 {
		t.Fatalf("PRECONDITION FAILED: tssTestConfig left TssParams.SignInterval at 0, so the post-injection wait has no instrument")
	}

	d, _ := startDevnetNoKey(t, cfg, 85*time.Minute)
	c := &vfCase{t: t}
	nodes := vfAllNodes(5)

	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "vault f13 live refuse")
	cid := env.cid
	retiringKeyId := cid + "-" + btcvault.VaultKeyName(0)
	activeKeyId := cid + "-" + btcvault.VaultKeyName(1)

	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	// Rotate gen-0 to gen-1 so a RETIRING generation actually exists. Everything
	// below is vacuous without it, so this is a hard precondition.
	primary1, rotated := vfRotate(t, d, ctx, cid, 1)
	if !rotated {
		t.Fatalf("PRECONDITION FAILED: the gen-0 to gen-1 rotation did not complete (primary1=%q), so there is no retiring generation to attack", primary1)
	}
	if st := vfVaultStatusOn(d, ctx, 2, cid, 0); st != int(btcvault.VaultStatusRetiring) {
		t.Fatalf("PRECONDITION FAILED: gen-0 status is %d, want %d (Retiring); the attack target does not exist", st, int(btcvault.VaultStatusRetiring))
	}
	if st := vfVaultStatusOn(d, ctx, 2, cid, 1); st != int(btcvault.VaultStatusActive) {
		t.Fatalf("PRECONDITION FAILED: gen-1 status is %d, want %d (Active); without a single Active successor the registry resolves UNRESOLVABLE and every keysign is refused for the wrong reason", st, int(btcvault.VaultStatusActive))
	}
	gen0Before := genUtxoCount(t, d, ctx, cid, 0)
	if gen0Before <= 0 {
		t.Fatalf("PRECONDITION FAILED: gen-0 holds %d UTXOs, so the mandatory positive control would have nothing to sweep", gen0Before)
	}
	// The sign selector marks a request FAILED for a non-active key BEFORE
	// btcSignRefused ever runs (tss.go around line 592). If the retiring key were
	// not active, the digest would go unsigned for a reason that has nothing to do
	// with output scoping and the whole test would be vacuous.
	for _, n := range nodes {
		ks, err := d.GetTssKeys(ctx, n, bson.M{"id": retiringKeyId})
		if err != nil || len(ks) == 0 {
			t.Fatalf("PRECONDITION FAILED: magi-%d has no tss_keys row for %s (err=%v)", n, retiringKeyId, err)
		}
		if ks[0].Status != "active" {
			t.Fatalf("PRECONDITION FAILED: magi-%d reports %s status=%q, not \"active\"; the sign selector would fail the rogue request before the output-scoping gate, making the refusal vacuous", n, retiringKeyId, ks[0].Status)
		}
	}
	t.Logf("PRECONDITION OK: gen-0 Retiring with %d UTXOs, gen-1 Active, retiring key %s active on all 5 nodes", gen0Before, retiringKeyId)

	// ---- 1. inject the rogue request against the RETIRING key ----
	// A 32 byte digest (sha256.Sum256 is [32]byte by construction) that is not any
	// pending sweep's sighash, checked against contract state below.
	rogue := sha256.Sum256([]byte("attacker"))
	if vfF13IsPendingSighash(t, d, ctx, cid, rogue[:]) {
		t.Fatalf("PRECONDITION FAILED: the rogue digest %s collides with a pending sweep sighash, so a signature would prove nothing", hex.EncodeToString(rogue[:]))
	}
	refuseBefore := map[int]int{}
	for _, n := range nodes {
		refuseBefore[n] = vfCountLogs(d, ctx, n, vfF13RefuseLog)
	}
	vfF13InsertRogueRequest(t, d, ctx, nodes, retiringKeyId, rogue[:])
	vfF13WaitSignTicks(t, d, ctx, signInterval, 3, 4*time.Minute)

	// ---- 2. F13-REFUSED ----
	var signedOn []int
	for _, n := range nodes {
		if vfSignatureLanded(d, ctx, n, retiringKeyId, rogue[:]) {
			signedOn = append(signedOn, n)
		}
	}
	refusers := 0
	logDetail := ""
	for _, n := range nodes {
		after := vfCountLogs(d, ctx, n, vfF13RefuseLog)
		delta := after - refuseBefore[n]
		if delta >= 1 {
			refusers++
		}
		logDetail += fmt.Sprintf(" magi-%d:+%d(total %d)", n, delta, after)
	}
	c.rec("F13-REFUSED", "retiring gen key refuses a digest that is not a successor-paying sweep",
		len(signedOn) == 0 && refusers >= 4,
		fmt.Sprintf("digest=%s signedOnNodes=%v refusingNodes=%d/5 refusalLogDelta:%s",
			hex.EncodeToString(rogue[:]), signedOn, refusers, logDetail))

	// ---- 3. F13-CONTROL (mandatory positive control) ----
	// The SAME retiring key must still sign a LEGITIMATE successor sweep. Without
	// this, F13-REFUSED is indistinguishable from "TSS signing is broken".
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
	migrateAndSettle(t, d, ctx, cid, retiringKeyId, primary1, backupPubKeyG)
	gen0After := gen0Before
	for i := 0; i < 12; i++ {
		gen0After = genUtxoCount(t, d, ctx, cid, 0)
		if gen0After == 0 {
			break
		}
		time.Sleep(15 * time.Second)
	}
	c.rec("F13-CONTROL", "positive control: a legitimate successor sweep for the SAME retiring key signs and settles",
		gen0After == 0,
		fmt.Sprintf("gen-0 UTXO count %d -> %d (0 means the retiring key signed and the sweep settled)", gen0Before, gen0After))

	stillUnsigned := true
	for _, n := range nodes {
		if vfSignatureLanded(d, ctx, n, retiringKeyId, rogue[:]) {
			stillUnsigned = false
		}
	}
	t.Logf("F13: after the legitimate sweep completed, the rogue retiring-gen digest is still unsigned on every node: %v", stillUnsigned)

	// ---- 4. F13-ACTIVE (INFO) ----
	// The ACTIVE generation signs ordinary user withdrawals whose destinations are
	// arbitrary, so evaluateScope returns scopeAllow for it by design. This records
	// what actually happens rather than asserting a verdict.
	rogueActive := sha256.Sum256([]byte("attacker-active-gen"))
	if vfF13IsPendingSighash(t, d, ctx, cid, rogueActive[:]) {
		t.Errorf("F13-ACTIVE setup: the rogue active-gen digest collides with a pending sweep sighash, so the observation is not meaningful")
	}
	vfF13InsertRogueRequest(t, d, ctx, nodes, activeKeyId, rogueActive[:])
	vfF13WaitSignTicks(t, d, ctx, signInterval, 3, 4*time.Minute)
	var activeSignedOn []int
	for _, n := range nodes {
		if vfSignatureLanded(d, ctx, n, activeKeyId, rogueActive[:]) {
			activeSignedOn = append(activeSignedOn, n)
		}
	}
	// The observation is only valid if the node can actually see the injected row;
	// otherwise "not signed" would just mean "never enqueued".
	rowVisible := false
	activeMsgHex := hex.EncodeToString(rogueActive[:])
	if reqs, err := d.GetTssRequests(ctx, 2, activeKeyId); err == nil {
		for _, r := range reqs {
			if r.Msg == activeMsgHex {
				rowVisible = true
			}
		}
	}
	c.rec("F13-ACTIVE", "INFO: active gen is deliberately NOT output scoped, outcome recorded either way",
		rowVisible,
		fmt.Sprintf("injected row visible on magi-2=%v, digest=%s signedOnNodes=%v (design expectation: signed, because the active gen must sign arbitrary user withdrawal destinations)",
			rowVisible, activeMsgHex, activeSignedOn))

	// ---- 5. F13-IDENT ----
	vfAssertContractIdentical(c, d, ctx, cid, nodes, 4*time.Minute, "F13-IDENT")
	c.summary("F13")
	t.Logf("F13 COMPLETE CONTRACT=%s", cid)
}

// vfF13InsertRogueRequest writes one signing request straight into every listed
// node's Mongo "tss_requests" collection, mirroring the production enqueue path
// (tss_db.SetSignedRequest): the upsert key is {key_id, msg} and the status is
// "unsigned", which is the ONLY thing FindUnsignedRequests filters on. No block
// height is written because the TssRequest document has no height field and the
// selection query never references one.
func vfF13InsertRogueRequest(t *testing.T, d *Devnet, ctx context.Context, nodes []int, keyId string, digest []byte) {
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
		if _, err := coll.UpdateOne(ctx,
			bson.M{"key_id": keyId, "msg": msgHex},
			bson.M{"$set": bson.M{"key_id": keyId, "msg": msgHex, "status": "unsigned"}},
			options.Update().SetUpsert(true),
		); err != nil {
			t.Fatalf("inserting rogue tss_request on magi-%d: %v", n, err)
		}
	}
	t.Logf("injected rogue tss_request into all %d node databases: key_id=%s msg=%s status=unsigned", len(nodes), keyId, msgHex)
}

// vfF13IsPendingSighash reports whether digest equals any input sighash of any
// pending spend in the contract's committed state. The rogue digest must NOT be
// one, otherwise a signature would be a legitimate sweep signature and the refuse
// case would prove nothing.
func vfF13IsPendingSighash(t *testing.T, d *Devnet, ctx context.Context, cid string, digest []byte) bool {
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
		sd, err := btcvault.DecodeSigningData(raw)
		if err != nil {
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

// vfF13WaitSignTicks blocks until the Hive chain has advanced far enough for the
// requested number of TSS sign ticks to have fired, plus one interval of margin.
// The instrument is the Hive head, not wall clock, because TSS selects signing
// requests only when the Hive block height is a multiple of signInterval.
func vfF13WaitSignTicks(t *testing.T, d *Devnet, ctx context.Context, signInterval uint64, ticks int, maxWait time.Duration) {
	t.Helper()
	start, err := getHeadBlock(d.HiveRPCEndpoint())
	if err != nil {
		t.Logf("F13: could not read the Hive head (%v), falling back to a 2 minute wall-clock wait", err)
		time.Sleep(2 * time.Minute)
		return
	}
	target := start + int(signInterval)*(ticks+1)
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		h, herr := getHeadBlock(d.HiveRPCEndpoint())
		if herr == nil && h >= target {
			t.Logf("F13: Hive head %d reached target %d (%d sign ticks of %d blocks past %d)", h, target, ticks, signInterval, start)
			return
		}
		select {
		case <-ctx.Done():
			t.Logf("F13: context done while waiting for sign ticks")
			return
		case <-time.After(5 * time.Second):
		}
	}
	h, _ := getHeadBlock(d.HiveRPCEndpoint())
	t.Logf("F13: WARNING Hive head %d did not reach %d within %v, fewer than %d sign ticks may have fired", h, target, maxWait, ticks)
}
