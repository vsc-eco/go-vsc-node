package devnet

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/btcvault"

	"github.com/btcsuite/btcd/wire"
	"go.mongodb.org/mongo-driver/bson"
)

// vault_custody_devnet_test.go: the key-custody batch (consensus 0.11.0) on a
// real five-node devnet, against what it is meant to fix and where it could
// still break.
//
//	CUST-WAIT   funds do not move into a new vault key before it has been
//	            reshared once; they do right after, and a dry run agrees.
//	CUST-SAVE   a node that cannot save its share does not vouch for the keygen,
//	            the key is still made by the others, the node flags its missing
//	            share, and the vault still activates (or the stall is measured).
//	CUST-HOLD   holders of the active key that leave the election still sign it
//	            and hand it to the members who replaced them; once handed on,
//	            they can go offline and the key still signs.
//	CUST-SOLE   the same with the active generation alone, so no retiring
//	            committee keeps the departed holders in the parties.
//
//	VAULT_CUSTODY_RUN=1 BTC_MAPPING_WASM_PATH=... go test -v -run TestVaultCustody -timeout 150m ./tests/devnet/
//
// Run on develop with the old contract (A) the WAIT, SAVE and SOLE assertions
// are expected to fail; on the custody branch with the new contract (B) they
// are expected to pass. CUST-HOLD passes on develop too (see its test).

const custodyHpin uint64 = 400

func custodyDevnet(t *testing.T, budget time.Duration, desc string, fundSats int64, rotateInterval uint64) (*Devnet, context.Context, *vfEnv, *vfCase) {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_CUSTODY_RUN") == "" {
		t.Skip("set VAULT_CUSTODY_RUN=1")
	}
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), vfTestBudget(budget))
	t.Cleanup(cancel)
	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if _, err := os.Stat(wasm); wasm == "" || err != nil {
		t.Fatalf("BTC_MAPPING_WASM_PATH must point at the btc-mapping-contract regtest wasm: %q %v", wasm, err)
	}
	cfg := vfSlowReshareConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = custodyHpin
	cfg.SysConfigOverrides.TssParams.RotateInterval = rotateInterval
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, budget)
	env := vfSetup(t, d, ctx, wasm, custodyHpin, fundSats, desc)
	vfWaitV2On(t, d, ctx, custodyHpin)
	vfPreconditionV2Active(t, d, ctx, 2, env.cid, custodyHpin)
	return d, ctx, env, &vfCase{t: t}
}

// vfSimulate dry-runs one contract call (simulateContractCalls) and returns
// whether it would succeed and, if not, the contract's error message.
func vfSimulate(d *Devnet, ctx context.Context, node int, cid, caller, action, payload string) (bool, string) {
	var out struct {
		Sim []struct {
			Success bool    `json:"success"`
			Err     *string `json:"err"`
			ErrMsg  *string `json:"err_msg"`
		} `json:"simulateContractCalls"`
	}
	err := d.gqlQuery(ctx, node, `query($i: SimulateContractCallsInput!){ simulateContractCalls(input: $i){ success err err_msg } }`,
		map[string]any{"i": map[string]any{
			"tx_id":          fmt.Sprintf("devnet-dryrun-%d", time.Now().UnixNano()),
			"required_auths": []string{caller},
			"calls": []map[string]any{{
				"contract_id": cid, "action": action, "payload": payload, "rc_limit": 100000,
			}},
		}}, &out)
	if err != nil || len(out.Sim) != 1 {
		return false, fmt.Sprintf("simulate unavailable: %v", err)
	}
	msg := ""
	if out.Sim[0].ErrMsg != nil {
		msg = *out.Sim[0].ErrMsg
	} else if out.Sim[0].Err != nil {
		msg = *out.Sim[0].Err
	}
	return out.Sim[0].Success, msg
}

// custodyEpochs returns keyId's current epoch on node and the epoch of the
// keygen commitment that created it (the one carrying its public key).
func custodyEpochs(d *Devnet, ctx context.Context, node int, keyId string) (keyEpoch, keygenEpoch uint64, ok bool) {
	keys, err := d.GetTssKeys(ctx, node, bson.M{"id": keyId})
	if err != nil || len(keys) == 0 || keys[0].PublicKey == "" {
		return 0, 0, false
	}
	cs, err := d.GetCommitments(ctx, node, bson.M{"key_id": keyId, "type": "keygen"})
	if err != nil {
		return 0, 0, false
	}
	found := false
	for _, c := range cs {
		if c.PublicKey == nil || *c.PublicKey != keys[0].PublicKey {
			continue
		}
		if !found || c.Epoch < keygenEpoch {
			keygenEpoch, found = c.Epoch, true
		}
	}
	return keys[0].Epoch, keygenEpoch, found
}

// custodyCurrentEpoch reads the election epoch in force on node.
func custodyCurrentEpoch(d *Devnet, ctx context.Context, node int) uint64 {
	var out struct {
		ElectionByBlockHeight struct {
			Epoch uint64 `json:"epoch"`
		} `json:"electionByBlockHeight"`
	}
	if err := d.gqlQuery(ctx, node, `{ electionByBlockHeight { epoch } }`, nil, &out); err != nil {
		return 0
	}
	return out.ElectionByBlockHeight.Epoch
}

func custodyDockerRoot(ctx context.Context, d *Devnet, node int, script string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "exec", "-u", "root", d.containerName(node), "sh", "-c", script).CombinedOutput()
	return string(out), err
}

// custodyLogLineHas reports whether one log line of the node contains every sub.
// (vfLogsContainAny returns the matched substring, not the line.)
func custodyLogLineHas(d *Devnet, ctx context.Context, node int, subs ...string) bool {
	logs, err := d.Logs(ctx, fmt.Sprintf("magi-%d", node))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(logs, "\n") {
		all := true
		for _, s := range subs {
			if !strings.Contains(line, s) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// CUST-WAIT
// ---------------------------------------------------------------------------

func TestVaultCustodyMigrationWaitsForReshare(t *testing.T) {
	d, ctx, env, c := custodyDevnet(t, 110*time.Minute, "custody: migration waits for the first reshare", 50_000_000, 60)
	cid := env.cid
	caller := "hive:" + d.witnessAccount(1)

	// The fee reserve (the sweep's miner fee) is funded before the rotation, so
	// nothing but the reshare check can refuse the first sweep after activation.
	fundFeeReserve(t, d, ctx, cid, env.primary0, backupPubKeyG, 10_000_000)

	primary1, ok := vfRotate(t, d, ctx, cid, 1)
	if !ok {
		t.Fatalf("PRECONDITION FAILED: gen-1 never activated (primary1=%q)", primary1)
	}
	key1 := cid + "-" + btcvault.VaultKeyName(1)

	// 1a. Straight after activation, before anything else, a dry run.
	keyEpoch, keygenEpoch, found := custodyEpochs(d, ctx, 2, key1)
	if !found {
		t.Fatalf("PRECONDITION FAILED: no keygen commitment carrying gen-1's key")
	}
	firstOK, firstMsg := vfSimulate(d, ctx, 1, cid, caller, "migrateVault", "")
	t.Logf("right after activation: key epoch=%d keygen=%d; dry run ok=%v msg=%q", keyEpoch, keygenEpoch, firstOK, firstMsg)
	firstNotReshared := keyEpoch == keygenEpoch
	gateNamed := !firstOK && strings.Contains(firstMsg, "has not been reshared")

	// 1b. If the key still has not been reshared, a real call: nothing may move.
	g0Before := vfGenUtxoCountOn(d, ctx, 2, cid, 0)
	keyEpoch, keygenEpoch, _ = custodyEpochs(d, ctx, 2, key1)
	switch {
	case !firstNotReshared:
		c.rec("CUST-WAIT", "migrateVault refused before the new key's first reshare", false,
			fmt.Sprintf("INCONCLUSIVE: gen-1 was already reshared at activation (epoch %d > keygen %d)", keyEpoch, keygenEpoch))
	case keyEpoch > keygenEpoch:
		c.rec("CUST-WAIT", "migrateVault refused before the new key's first reshare (dry run right after activation)", gateNamed,
			fmt.Sprintf("dry run ok=%v msg=%q; the reshare landed before the real call", firstOK, firstMsg))
	default:
		simOK, simMsg := vfSimulate(d, ctx, 1, cid, caller, "migrateVault", "")
		before := txSpendIds(t, d, ctx, cid)
		status, _ := vstatusTx(t, d, ctx, 1, cid, "migrateVault", "")
		time.Sleep(15 * time.Second)
		after := txSpendIds(t, d, ctx, cid)
		g0 := vfGenUtxoCountOn(d, ctx, 2, cid, 0)
		refused := gateNamed && !simOK && strings.Contains(simMsg, "has not been reshared") && status != "CONFIRMED" && len(after) == len(before) && g0 == g0Before
		c.rec("CUST-WAIT", "migrateVault refused before the new key's first reshare; no sweep built, gen-0 keeps its UTXOs", refused,
			fmt.Sprintf("first dry run ok=%v msg=%q; funded dry run ok=%v msg=%q; call=%s; pending spends %d -> %d; gen-0 utxos %d -> %d",
				firstOK, firstMsg, simOK, simMsg, status, len(before), len(after), g0Before, g0))
	}

	// 2. The first reshare lands.
	reshared := false
	deadline := time.Now().Add(vfTestBudget(20 * time.Minute))
	for time.Now().Before(deadline) {
		if e, kg, ok := custodyEpochs(d, ctx, 2, key1); ok && e > kg {
			reshared = true
			t.Logf("gen-1 reshared: epoch %d > keygen %d", e, kg)
			break
		}
		time.Sleep(20 * time.Second)
	}
	c.rec("CUST-RESHARE", "the new key is reshared to the committee within the wait", reshared, "")
	if !reshared {
		c.summary("TestVaultCustodyMigrationWaitsForReshare")
		t.FailNow()
	}

	// 2b. A dry run sees the reshare too. The mapping bot dry-runs migrateVault
	// before sending it, so a preview stuck on "not reshared" stalls every rotation.
	simOK, simMsg := false, ""
	for end := time.Now().Add(2 * time.Minute); time.Now().Before(end); time.Sleep(10 * time.Second) {
		if simOK, simMsg = vfSimulate(d, ctx, 1, cid, caller, "migrateVault", ""); simOK {
			break
		}
	}
	c.rec("CUST-SIM", "a dry run of migrateVault passes once the key has been reshared", simOK,
		fmt.Sprintf("dry run ok=%v msg=%q", simOK, simMsg))

	// 3. Now the migration goes, and settles.
	txid, sd, buildStatus := vfBuildSweep(t, d, ctx, 1, cid)
	c.rec("CUST-GO", "migrateVault builds the sweep once the key has been reshared", sd != nil,
		fmt.Sprintf("txid=%s status=%s", txid, buildStatus))
	if sd != nil {
		raw, signed := vfAwaitSweepSignatures(t, d, ctx, cid+"-main", sd)
		settled := false
		if signed {
			if bcTxid, h, err := vfBroadcastAndMine(t, d, ctx, raw); err == nil {
				vfRelayAndConfirm(t, d, ctx, 1, cid, bcTxid, h)
				for end := time.Now().Add(6 * time.Minute); time.Now().Before(end) && !settled; time.Sleep(15 * time.Second) {
					settled = vfGenUtxoCountOn(d, ctx, 2, cid, 0) < g0Before && vfGenUtxoCountOn(d, ctx, 2, cid, 1) >= 1
				}
			} else {
				t.Logf("broadcast: %v", err)
			}
		}
		c.rec("CUST-SETTLE", "the sweep signs, confirms and settles into gen 1", settled,
			fmt.Sprintf("signed=%v gen-0 utxos %d -> %d, gen-1 utxos %d", signed, g0Before, vfGenUtxoCountOn(d, ctx, 2, cid, 0), vfGenUtxoCountOn(d, ctx, 2, cid, 1)))
	}
	vfAssertContractIdentical(c, d, ctx, cid, vfAllNodes(5), 4*time.Minute, "CUST-WAIT-IDENT")
	c.summary("TestVaultCustodyMigrationWaitsForReshare")
}

// ---------------------------------------------------------------------------
// CUST-SAVE
// ---------------------------------------------------------------------------

func TestVaultCustodyShareSaveFailure(t *testing.T) {
	d, ctx, env, c := custodyDevnet(t, 100*time.Minute, "custody: a node that cannot save its share", 0, 60)
	cid := env.cid
	const bad = 5
	keysDir := fmt.Sprintf("/data/devnet/data-%d/tss-keys", bad)
	key1 := cid + "-" + btcvault.VaultKeyName(1)

	// The disk refuses every new write under the key-share store.
	if out, err := custodyDockerRoot(ctx, d, bad, "chmod -R a-w "+keysDir+" && ls -ld "+keysDir); err != nil {
		t.Fatalf("PRECONDITION FAILED: making magi-%d's key store read-only: %v %s", bad, err, out)
	} else {
		t.Logf("magi-%d key store read-only: %s", bad, strings.TrimSpace(out))
	}
	t.Cleanup(func() { custodyDockerRoot(context.Background(), d, bad, "chmod -R u+w "+keysDir) })

	primary1, minted := vfMintNextGen(t, d, ctx, cid, 1, 2, 10*time.Minute)
	c.rec("CUST-SAVE-COMMIT", "the keygen commitment still lands with the four nodes that saved", minted,
		fmt.Sprintf("primary1=%s", primary1))
	if !minted {
		c.summary("TestVaultCustodyShareSaveFailure")
		t.FailNow()
	}

	_, notSaved := vfLogsContainAny(d, ctx, bad, "keygen key share NOT saved")
	_, notVouching := vfLogsContainAny(d, ctx, bad, "share not saved, not vouching")
	c.rec("CUST-SAVE-NOVOUCH", fmt.Sprintf("magi-%d reports its share was not saved and does not vouch for the keygen", bad), notSaved && notVouching,
		fmt.Sprintf("NOT-saved log=%v not-vouching log=%v", notSaved, notVouching))

	// The missing-share check runs every rotate interval (60 blocks here).
	alarm := false
	for end := time.Now().Add(vfTestBudget(10 * time.Minute)); time.Now().Before(end) && !alarm; time.Sleep(20 * time.Second) {
		alarm = custodyLogLineHas(d, ctx, bad, "cannot load its share", key1)
	}
	quiet := true
	for _, n := range []int{1, 2, 3, 4} {
		if _, ok := vfLogsContainAny(d, ctx, n, "cannot load its share"); ok {
			quiet = false
		}
	}
	c.rec("CUST-SAVE-ALARM", fmt.Sprintf("magi-%d flags the share it is listed for but cannot load; the other nodes stay quiet", bad), alarm && quiet,
		fmt.Sprintf("alarm=%v others quiet=%v", alarm, quiet))

	// Still broken? A listed holder without its share is in every signing party
	// until blame leaves it out. Measure whether the check-signature still lands.
	start := time.Now()
	vfWaitPreparams(t, d, ctx, time.Duration(float64(12*time.Minute)*vfTimeoutScale()))
	activated := vfRegisterAndActivate(t, d, ctx, cid, primary1, 60)
	c.rec("CUST-SAVE-ACTIVATE", "the vault still activates on the four saved shares", activated,
		fmt.Sprintf("after %s", time.Since(start).Round(time.Second)))
	c.summary("TestVaultCustodyShareSaveFailure")
}

// ---------------------------------------------------------------------------
// CUST-HOLD
// ---------------------------------------------------------------------------

// custodyWithdrawSigned requests a withdrawal from the active vault and reports
// whether the active key signed every input within `within`.
func custodyWithdrawSigned(t *testing.T, d *Devnet, ctx context.Context, cid, keyId string, sats int64, within time.Duration) (bool, string) {
	t.Helper()
	dest, err := d.bitcoinCli(ctx, "getnewaddress")
	if err != nil {
		return false, fmt.Sprintf("getnewaddress: %v", err)
	}
	before := txSpendIds(t, d, ctx, cid)
	if s, tx := vstatusTx(t, d, ctx, 1, cid, "unmap", fmt.Sprintf(`{"amount":"%d","to":"%s"}`, sats, dest)); !isOK(s) {
		return false, "unmap status " + s + " " + vfCallResult(d, ctx, 1, tx)
	}
	txid := ""
	for i := 0; i < 20 && txid == ""; i++ {
		time.Sleep(3 * time.Second)
		for _, id := range txSpendIds(t, d, ctx, cid) {
			if !contains(before, id) {
				txid = id
			}
		}
	}
	if txid == "" {
		return false, "no pending spend"
	}
	sd := waitSigningData(t, d, ctx, cid, txid)
	if sd == nil {
		return false, "no signing data for " + txid
	}
	var mtx wire.MsgTx
	_ = mtx.Deserialize(bytes.NewReader(sd.Tx))
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		all := true
		for _, uh := range sd.UnsignedSigHashes {
			if !vfSignatureLanded(d, ctx, 2, keyId, uh.SigHash) {
				all = false
			}
		}
		if all {
			return true, fmt.Sprintf("txid=%s inputs=%d signed", txid, len(sd.UnsignedSigHashes))
		}
		time.Sleep(15 * time.Second)
	}
	return false, fmt.Sprintf("txid=%s inputs=%d not signed within %s (first sighash %s)", txid, len(sd.UnsignedSigHashes), within,
		hex.EncodeToString(sd.UnsignedSigHashes[0].SigHash))
}

func TestVaultCustodyActiveHoldersOutsideElection(t *testing.T) {
	d, ctx, env, c := custodyDevnet(t, 130*time.Minute, "custody: active key holders outside the election", 0, 60)
	cid := env.cid
	key1 := cid + "-" + btcvault.VaultKeyName(1)

	primary1, ok := vfRotate(t, d, ctx, cid, 1)
	if !ok {
		t.Fatalf("PRECONDITION FAILED: gen-1 never activated (primary1=%q)", primary1)
	}
	// Money in the active vault to withdraw.
	fundVaultViaSPV(t, d, ctx, cid, primary1, backupPubKeyG, env.owner, 30_000_000, contractLastHeight(t, d, ctx, cid))
	for i := 0; i < 12 && !balanceCredited(t, d, ctx, cid, env.owner); i++ {
		time.Sleep(10 * time.Second)
	}
	if !balanceCredited(t, d, ctx, cid, env.owner) {
		t.Fatalf("PRECONDITION FAILED: the deposit into gen 1 was not credited")
	}
	// A second deposit, so the second withdrawal has a confirmed input of its own
	// (an unmap's change is not spendable until its spend confirms).
	fundVaultViaSPV(t, d, ctx, cid, primary1, backupPubKeyG, env.owner, 20_000_000, contractLastHeight(t, d, ctx, cid))
	for i := 0; i < 18 && vfGenUtxoCountOn(d, ctx, 2, cid, 1) < 2; i++ {
		time.Sleep(10 * time.Second)
	}
	if n := vfGenUtxoCountOn(d, ctx, 2, cid, 1); n < 2 {
		t.Fatalf("PRECONDITION FAILED: gen 1 holds %d utxo(s) after two deposits", n)
	}

	// Gen 0 is now retiring and its committee (all five) is readiness-eligible
	// through V-A, so this case passes before 0.11.0 too;
	// TestVaultCustodyHoldersOutsideElectionNoOlderCommittee is the one that
	// isolates the 0.11.0 widening.
	custodyHoldersLeave(t, d, ctx, c, cid, key1, 1, "CUST-HOLD")
	c.summary("TestVaultCustodyActiveHoldersOutsideElection")
}

// TestVaultCustodyHoldersOutsideElectionNoOlderCommittee: the active key is the
// only generation, so no retiring committee carries the departing holders and
// only the 0.11.0 active-holder widening can keep them in the signing and
// reshare parties.
func TestVaultCustodyHoldersOutsideElectionNoOlderCommittee(t *testing.T) {
	d, ctx, env, c := custodyDevnet(t, 130*time.Minute, "custody: active key holders outside the election, no older committee", 30_000_000, 60)
	cid := env.cid
	key0 := cid + "-" + btcvault.VaultKeyName(0)

	fundVaultViaSPV(t, d, ctx, cid, env.primary0, backupPubKeyG, env.owner, 20_000_000, contractLastHeight(t, d, ctx, cid))
	for i := 0; i < 18 && vfGenUtxoCountOn(d, ctx, 2, cid, 0) < 2; i++ {
		time.Sleep(10 * time.Second)
	}
	if n := vfGenUtxoCountOn(d, ctx, 2, cid, 0); n < 2 {
		t.Fatalf("PRECONDITION FAILED: gen 0 holds %d utxo(s) after two deposits", n)
	}
	if vs := vfVaultRegistryOn(d, ctx, 2, cid); len(vs) != 1 {
		t.Fatalf("PRECONDITION FAILED: the vault registry has %d generations; this case needs the active one alone", len(vs))
	}
	custodyHoldersLeave(t, d, ctx, c, cid, key0, 0, "CUST-SOLE")
	c.summary("TestVaultCustodyHoldersOutsideElectionNoOlderCommittee")
}

// custodyHoldersLeave takes magi-4 and magi-5 out of the election while they
// still hold shares of keyId (gen's active key, held by all five; 4 must sign),
// then checks that the key still signs, is handed to the 3 remaining members,
// and signs again with the departed holders offline.
func custodyHoldersLeave(t *testing.T, d *Devnet, ctx context.Context, c *vfCase, cid, keyId string, gen int, tag string) {
	t.Helper()
	departed := []int{4, 5}
	keyEpoch, keygenEpoch, _ := custodyEpochs(d, ctx, 2, keyId)
	e0 := custodyCurrentEpoch(d, ctx, 2)
	t.Logf("gen-%d key epoch=%d keygen=%d; election epoch=%d", gen, keyEpoch, keygenEpoch, e0)

	// Two holders stand down (witness disabled everywhere), nodes stay up.
	for _, n := range departed {
		disableWitnessEverywhere(t, d, ctx, d.witnessAccount(n), 5)
	}
	out := false
	var members []string
	for end := time.Now().Add(vfTestBudget(15 * time.Minute)); time.Now().Before(end) && !out; time.Sleep(20 * time.Second) {
		ep := custodyCurrentEpoch(d, ctx, 2)
		if ep <= e0 {
			continue
		}
		members, _ = d.GetElectionMembers(ctx, 2, ep)
		out = len(members) == 3
		for _, n := range departed {
			if contains(members, d.witnessAccount(n)) {
				out = false
			}
		}
	}
	if !out {
		t.Fatalf("PRECONDITION FAILED: the departing holders never left the election (members=%v)", members)
	}
	t.Logf("election now %v; magi-4 and magi-5 hold gen-%d shares from outside it", members, gen)

	// The active key must still sign a withdrawal: only 3 holders are in the
	// election and 4 are needed.
	signed, detail := custodyWithdrawSigned(t, d, ctx, cid, keyId, 5_000_000, vfTestBudget(20*time.Minute))
	c.rec(tag+"-SIGN", "the active key signs a withdrawal with only 3 of its 5 holders still in the election", signed, detail)

	// And it is handed to the members who replaced them.
	handed := false
	handDetail := ""
	for end := time.Now().Add(vfTestBudget(20 * time.Minute)); time.Now().Before(end) && !handed; time.Sleep(20 * time.Second) {
		cs, _ := d.GetCommitments(ctx, 2, bson.M{"key_id": keyId, "type": "reshare", "epoch": bson.M{"$gt": e0}})
		for _, cm := range cs {
			if names, err := d.GetElectionMembers(ctx, 2, cm.Epoch); err == nil && len(names) == 3 {
				handed = true
				handDetail = fmt.Sprintf("reshare commitment at epoch %d (committee %v)", cm.Epoch, names)
			}
		}
	}
	c.rec(tag+"-HANDOVER", "the key is reshared from the departed holders to the 3 remaining members", handed, handDetail)

	// Once handed on, the departed holders can go offline for good.
	if handed {
		vfStopNodes(t, d, ctx, departed)
		signed2, detail2 := custodyWithdrawSigned(t, d, ctx, cid, keyId, 3_000_000, vfTestBudget(15*time.Minute))
		c.rec(tag+"-AFTER", "after the handover the key signs with the departed holders offline", signed2, detail2)
		vfStartNodes(t, d, ctx, departed)
	}
}
