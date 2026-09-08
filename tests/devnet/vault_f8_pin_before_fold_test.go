package devnet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestVaultF8PinBeforeFold answers ONE question: does pinning
// VaultRotationV2ActivationHeight BEFORE the contract has a vault registry (the
// "pin before fold" ordering) FREEZE the vault?
//
// The July doctrine said YES. With the node's v2 gates live while the contract's
// "v" registry is still absent, output scoping was believed to refuse every BTC
// vault keysign, so a vault pinned in that order could never be born, never
// activate and never pay anyone.
//
// The current L9-1 code says NO. modules/tss/output_scoping.go tri-states the
// registry read instead of collapsing it: vaultAbsent (exactly this test's
// starting state) allows the legacy gen-0 "main" key and nothing else, and
// vaultGenesisPending admits the single Pending generation's BRK-2 check
// signature so genesis can still activate. If that is right, a vault pinned
// before the fold still mints gen-0, still activates, still TSS-signs a user
// withdrawal, and still rotates gen-0 to gen-1 and drains.
//
// So this test pins hpin=250 (early), deploys and seeds the contract, wires the
// oracle, and then waits until node 2 has processed past hpin+5 WITHOUT ever
// calling createKey. That is the exact pin-before-fold state: v2 gates ON, "v"
// absent, "pubkey" absent, only "mv" written by the fresh deploy. Only then does
// it run the whole legacy lifecycle and record what happens at each step.
//
// A run whose cases all PASS REFUTES the July doctrine on this tree. A run with a
// FAIL confirms it and names the exact step that froze.
//
//	VAULT_F8_RUN=1 DEVNET_KEEP=1 go test -v -run TestVaultF8PinBeforeFold -timeout 95m ./tests/devnet/
func TestVaultF8PinBeforeFold(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F8_RUN") == "" {
		t.Skip("set VAULT_F8_RUN=1")
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

	// hpin=250 is EARLY on purpose: the fleet crosses it while the contract still
	// has no vault registry at all. Every other test in this suite pins at 400,
	// AFTER a v2-off genesis, which is the ordering this one deliberately inverts.
	const hpin = 250
	cfg := vfSlowReshareConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 85*time.Minute)

	c := &vfCase{t: t}

	// ---- setup: vfSetup's deploy/seed/oracle steps, inlined WITHOUT its createKey.
	// vfSetup mints gen-0 immediately, which would fold the registry before the pin
	// takes effect and destroy the very state this test exists to observe. ----
	seedH, err := d.MineBlocks(ctx, 101)
	if err != nil {
		t.Fatalf("mine: %v", err)
	}
	hdr1, err := btcBlockHeaderHex(ctx, d, seedH)
	if err != nil {
		t.Fatalf("hdr: %v", err)
	}
	cid, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm, Name: "btc-mapping-contract", Description: "f8 pin before fold", DeployerNode: 1, GQLNode: 2,
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	t.Logf("CONTRACT=%s hpin=%d (activation pinned BEFORE any vault exists)", cid, hpin)
	if s := vstatus(t, d, ctx, 1, cid, "seedBlocks", fmt.Sprintf(`{"block_header":"%s","block_height":%d}`, hdr1, seedH)); !isOK(s) {
		t.Fatalf("seedBlocks: %s", s)
	}
	d.WriteOracleConfigs(ctx)
	d.SetOracleContractIDs(map[string]string{"BTC": cid})
	d.RestartAllMagiNodes(ctx)
	time.Sleep(10 * time.Second)

	finish := func() {
		vfAssertContractIdentical(c, d, ctx, cid, vfAllNodes(5), 4*time.Minute, "F8-IDENT")
		if c.fail == 0 {
			t.Logf("F8 VERDICT: pinning the activation height BEFORE the fold does NOT freeze the vault on this tree. " +
				"The July doctrine is REFUTED: with 'v' absent the node allowed the legacy gen-0 key, vaultGenesisPending " +
				"admitted the check-signature, and the vault minted, activated, signed a user withdrawal and rotated.")
		} else {
			t.Logf("F8 VERDICT: the pre-fold pin BLOCKED at least one step. Every FAIL case above other than F8-IDENT is a " +
				"live instance of the July doctrine on this tree; the first one names the step that froze.")
		}
		c.summary("F8")
		t.Logf("F8 COMPLETE CONTRACT=%s hpin=%d", cid, hpin)
	}

	// ---- step 1: v2 gates go live while "v" is still absent ----
	vfWaitV2On(t, d, ctx, hpin)

	processed, err := d.getLastProcessedBlock(ctx, 2)
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: cannot read magi-2 processed height: %v", err)
	}
	if processed <= hpin {
		t.Fatalf("PRECONDITION FAILED: magi-2 processed=%d is not past hpin=%d, the v2 gates are NOT on and the test would be vacuous", processed, hpin)
	}
	pre, err := getStateHex(d, ctx, 2, cid, []string{"v", "mv", "pubkey"})
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: cannot read contract state from magi-2: %v", err)
	}
	if vs := vfVaultRegistryOn(d, ctx, 2, cid); len(vs) != 0 {
		t.Fatalf("PRECONDITION FAILED: vault registry 'v' already holds %d generation(s) at processed=%d, so the contract folded BEFORE the pin took effect and this is NOT the pin-before-fold state", len(vs), processed)
	}
	c.rec("F8-PRE", "v2 gates live while the vault registry is still absent (the pin-before-fold state)", true,
		fmt.Sprintf("magi-2 processed=%d > hpin=%d, v=%d bytes, mv=%q, pubkey=%d bytes",
			processed, hpin, len(pre["v"]), string(pre["mv"]), len(pre["pubkey"])))

	// ---- step 2: the LEGACY genesis path, run with v2 ALREADY ON.
	// The registry is empty, so createKey takes the genesis branch and mints gen-0
	// as a self-predecessor Pending vault. Under v2 the check-signature is required,
	// and only output_scoping's vaultGenesisPending branch can admit it. ----
	vstatus(t, d, ctx, 1, cid, "createKey", "")
	kd0, err := d.WaitForTssKey(ctx, 2, bson.M{"id": cid + "-main", "status": "active"}, 10*time.Minute)
	if err != nil {
		all, _ := d.GetTssKeys(ctx, 2, bson.M{})
		for _, k := range all {
			t.Logf("  tss_key id=%s status=%s", k.Id, k.Status)
		}
		c.rec("F8-KEY", "gen-0 keygen completes with v2 on and the registry absent", false,
			fmt.Sprintf("WaitForTssKey(%s-main active): %v", cid, err))
		finish()
		return
	}
	primary0 := kd0.PublicKey
	c.rec("F8-KEY", "gen-0 keygen completes with v2 on and the registry absent", true,
		fmt.Sprintf("primary0=%s epoch=%d", primary0, kd0.Epoch))

	// registerPublicKey, retried exactly as vault_genesisfix_test.go does, but with
	// the spec's stronger break condition: chain state, not tx status. The genesis
	// check-signature has to land before the contract will flip gen-0 to Active.
	reg := fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, primary0, backupPubKeyG)
	activated := false
	lastStatus := ""
	for i := 0; i < 12; i++ {
		lastStatus = vstatus(t, d, ctx, 1, cid, "registerPublicKey", reg)
		if vfVaultStatusOn(d, ctx, 2, cid, 0) == 1 {
			activated = true
			break
		}
		t.Logf("gen-0 not Active yet (awaiting the genesis check-sig under v2), registerPublicKey status=%s, retry %d", lastStatus, i)
		time.Sleep(15 * time.Second)
	}
	if !activated {
		// genesisfix's fallback: drive the explicit activateKey path too, so a FAIL
		// here means the check-signature really never landed, not that we only ever
		// poked one entry point.
		for i := 0; i < 6; i++ {
			lastStatus = vstatus(t, d, ctx, 1, cid, "activateKey", "")
			if vfVaultStatusOn(d, ctx, 2, cid, 0) == 1 {
				activated = true
				break
			}
			t.Logf("activateKey fallback, gen-0 still not Active, status=%s, retry %d", lastStatus, i)
			time.Sleep(15 * time.Second)
		}
	}
	c.rec("F8-GENESIS", "gen-0 reaches Active under v2 ON with a pre-fold (absent) registry", activated,
		fmt.Sprintf("vfVaultStatusOn(magi-2, gen 0)=%d, last call status=%s", vfVaultStatusOn(d, ctx, 2, cid, 0), lastStatus))
	if !activated {
		// The vault never came alive, so every later step would be vacuous. Stop here
		// with the evidence, which is itself the July-doctrine verdict.
		finish()
		return
	}

	// From here the registry IS populated, so the standard v2 precondition applies.
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	// ---- step 3: fund gen-0 and prove a user withdrawal actually gets TSS-signed ----
	owner := "hive:" + fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, 1)
	fundVaultViaSPV(t, d, ctx, cid, primary0, backupPubKeyG, owner, 50_000_000, seedH)
	if !balanceCredited(t, d, ctx, cid, owner) {
		t.Fatalf("PRECONDITION FAILED: gen-0 funding did not credit %s, the withdrawal path cannot be tested", owner)
	}
	bal0 := balanceSats(t, d, ctx, cid, owner)
	sigs0 := f8SignedRequests(d, ctx, 2, cid+"-main")
	t.Logf("gen-0 funded: %s = %d sats, %d already-signed requests on %s-main (the genesis check-sig is one of them)", owner, bal0, sigs0, cid)

	unmapAndSettle(t, d, ctx, cid, owner, 1_000_000)

	bal1 := balanceSats(t, d, ctx, cid, owner)
	sigs1 := f8SignedRequests(d, ctx, 2, cid+"-main")
	c.rec("F8-SIGN", "a user withdrawal from the pre-fold-pinned vault is TSS-signed and debits the owner",
		sigs0 >= 0 && sigs1 > sigs0 && bal1 < bal0,
		fmt.Sprintf("signed requests on %s-main %d to %d, owner balance %d to %d sats", cid, sigs0, sigs1, bal0, bal1))

	// ---- step 4: a full rotation on top of the pre-fold pin ----
	vfDumpRegistry(t, d, ctx, 2, cid, "after the pre-rotation withdrawal settled, before rotation")
	primary1, rotated := vfRotate(t, d, ctx, cid, 1)
	c.rec("F8-ROTATE", "gen-0 to gen-1 rotation activates after a pre-fold pin", rotated,
		fmt.Sprintf("gen-1 primary=%s, gen-0 status=%d, gen-1 status=%d",
			primary1, vfVaultStatusOn(d, ctx, 2, cid, 0), vfVaultStatusOn(d, ctx, 2, cid, 1)))
	if rotated {
		fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)
		// Run 1 (2026-09-05) left gen-0 with THREE registry entries after one tranche
		// (gen-1 had two) with no registry dump to explain them; the first tranche of a
		// Retiring gen is also capped at MigrationCanaryValue (1,000,000 sats), so a
		// single tranche is not a drain by design. Dump the registry at every step and
		// sweep up to four tranches, stopping when a tranche makes no progress.
		vfDumpRegistry(t, d, ctx, 2, cid, "after rotation + fee top-up, before tranche 1")
		left := genUtxoCount(t, d, ctx, cid, 0)
		tranches := 0
		for i := 0; i < 4 && left > 0; i++ {
			migrateAndSettle(t, d, ctx, cid, cid+"-main", primary1, backupPubKeyG)
			tranches++
			next := f8WaitGenDrained(t, d, ctx, cid, 0, 3*time.Minute)
			vfDumpRegistry(t, d, ctx, 2, cid, fmt.Sprintf("after tranche %d", tranches))
			if next >= left {
				t.Logf("tranche %d did not reduce the gen-0 UTXO count (still %d), stopping the sweep loop", tranches, next)
				left = next
				break
			}
			left = next
		}
		c.rec("F8-DRAIN", "gen-0 drains into gen-1 (the retiring key still signs its successor sweeps)", left == 0,
			fmt.Sprintf("%d tranche(s), gen-0 utxo count=%d, gen-1 utxo count=%d", tranches, left, genUtxoCount(t, d, ctx, cid, 1)))
	} else {
		t.Logf("skipping the fee reserve and the migration sweep: gen-1 never activated, so there is no successor to sweep to")
	}

	// ---- step 5: no node may have taken a different view of any of that ----
	finish()
}

// f8SignedRequests counts the TSS signing requests for keyId on `node` that already
// carry a signature. The DELTA across an operation is the honest instrument for "did
// the vault key actually sign", because the genesis check-signature is already
// present before any withdrawal runs. Returns -1 if the node cannot be read.
func f8SignedRequests(d *Devnet, ctx context.Context, node int, keyId string) int {
	reqs, err := d.GetTssRequests(ctx, node, keyId)
	if err != nil {
		return -1
	}
	n := 0
	for _, r := range reqs {
		if r.Sig != "" {
			n++
		}
	}
	return n
}

// f8WaitGenDrained polls the committed UTXO registry ("r") until a generation holds
// no UTXOs, and returns the final count. Chain truth, not a tx status.
func f8WaitGenDrained(t *testing.T, d *Devnet, ctx context.Context, cid string, gen uint32, within time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(within)
	n := genUtxoCount(t, d, ctx, cid, gen)
	for n != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Second)
		n = genUtxoCount(t, d, ctx, cid, gen)
	}
	return n
}
