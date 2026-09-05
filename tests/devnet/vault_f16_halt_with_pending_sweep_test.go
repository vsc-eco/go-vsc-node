package devnet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"vsc-node/lib/btcvault"

	"github.com/vsc-eco/hivego"
)

// TestVaultF16HaltWithPendingSweep proves the V-8 evacuation exemption LIVE, with
// vault-rotation-v2 ON, against a real 6 node devnet: a governance BTC keysign halt
// (vsc.tss_halt, the ungated emergency flag) freezes ORDINARY user withdrawals from
// the ACTIVE vault generation, yet the successor paying MIGRATION SWEEP from the
// RETIRING generation still signs, and the freeze is reversible (clearing the flag
// lets the very same withdrawal sign).
//
// Why that combination matters: the halt exists to contain a suspected theft, and
// the correct response to a suspected theft is to rotate the vault and evacuate the
// funds to the fresh generation. If the halt also froze the evacuation sweep, the
// containment action would lock the funds inside the compromised generation, which
// is why btcSignGateDecision (modules/tss/output_scoping.go) issues a
// scopeSuccessorSweep verdict even while halted, and skips everything else with the
// log line "BTC keysign frozen by solvency gate; skipping issuance".
//
// The op plumbing recipe is copied from vault_tsshalt_test.go: 6 nodes so the
// vsc.gateway active authority threshold is 6*2/3 = 4, one gateway keypair per
// witness re-derived by devnetGatewayKeypair, and a retry loop that re-broadcasts
// until countHaltFlag reports the flag on every node (a Hive level broadcast
// success is NOT proof that VSC accepted the op).
//
// CONFIG NOTE. The spec says "DefaultConfig style keys". Reading the code, the
// actual ingredient vault_tsshalt_test.go needed is NOT DefaultConfig: it is
// MagiEnv["DEVNET_DETERMINISTIC_BLS"]="1", which makes devnet-setup derive each
// witness BLS seed as sha256("devnet-bls-"+witness) so devnetGatewayKeypair can
// re-derive the matching gateway multisig keys (cmd/devnet-setup/main.go:137,
// tests/devnet/devnet.go:193, safety_slash_reserve_reverse_test.go:191). Nothing in
// the halt path depends on any other DefaultConfig field. This test therefore uses
// tssTestConfig() (fast keygen and sign intervals, which the vault rotation needs)
// plus that one env flag. vault_mixed_version_test.go broadcast the same op on
// tssTestConfig WITHOUT the flag, which is exactly why its halt case could not
// propagate.
//
// Instrument honesty: the sweep is built BEFORE the halt is broadcast (spec
// ordering), and the sign interval is 10 blocks, so the sweep inputs can already be
// signed by the time the flag reaches all 6 nodes. The test counts how many sweep
// inputs were signed at the moment the halt landed and stamps that on the
// F16-SWEEP-SIGNS detail, so a vacuous pass is visible rather than silent.
//
//	VAULT_F16_RUN=1 go test -v -run TestVaultF16HaltWithPendingSweep -timeout 50m ./tests/devnet/
func TestVaultF16HaltWithPendingSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_F16_RUN") == "" {
		t.Skip("set VAULT_F16_RUN=1")
	}
	requireDocker(t)

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		wasm = "/home/clauderfly/utxo-s1/btc-mapping-contract/bin/dev.wasm"
	}
	if _, err := os.Stat(wasm); err != nil {
		t.Fatalf("wasm: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	const hpin = 400
	const f16Nodes = 6

	cfg := tssTestConfig()
	cfg.Nodes = f16Nodes
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	// See the CONFIG NOTE above: this flag, not DefaultConfig, is what makes the
	// gateway multisig re-derivable, so the vsc.tss_halt op is actually accepted.
	cfg.MagiEnv = map[string]string{"DEVNET_DETERMINISTIC_BLS": "1"}
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 45*time.Minute)

	allNodes := vfAllNodes(f16Nodes)
	c := &vfCase{t: t}

	// ---------------------------------------------------------------------
	// Setup: gen-0 genesis + funding pre hpin, v2 on, rotate to gen-1, fee reserve.
	// ---------------------------------------------------------------------
	env := vfSetup(t, d, ctx, wasm, hpin, 50_000_000, "F16 halt with pending sweep")
	cid := env.cid
	vfWaitV2On(t, d, ctx, hpin)
	vfPreconditionV2Active(t, d, ctx, 2, cid, hpin)

	primary1, rotated := vfRotate(t, d, ctx, cid, 1)
	if !rotated {
		t.Fatalf("PRECONDITION FAILED: gen-0 to gen-1 rotation did not complete (primary1=%q). Without a Retiring gen-0 and an Active gen-1 there is no successor sweep and no active generation withdrawal, so the V-8 exemption cannot be observed", primary1)
	}
	t.Logf("rotation done: gen-1 active primary=%s, gen-0 status=%d, gen-0 UTXOs on magi-1 = %d",
		primary1, vfVaultStatusOn(d, ctx, 2, cid, 0), vfGenUtxoCountOn(d, ctx, 1, cid, 0))
	fundFeeReserve(t, d, ctx, cid, primary1, backupPubKeyG, 10_000_000)

	// ---------------------------------------------------------------------
	// Local helpers. All prefixed f16 so sibling failure-state files cannot collide.
	// ---------------------------------------------------------------------

	// f16SignWith: one gateway keypair per witness, then the first 4 (the 6-node
	// vsc.gateway weight threshold is 6*2/3 = 4). Exact recipe from vault_tsshalt.
	var f16AllKeys []*hivego.KeyPair
	for n := 1; n <= cfg.Nodes; n++ {
		f16AllKeys = append(f16AllKeys, devnetGatewayKeypair(t, fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, n)))
	}
	f16SignWith := f16AllKeys[:4]

	// f16HaltUntil re-broadcasts the gateway vsc.tss_halt op until btc_keysign_halted
	// reaches wantCount across all nodes. Retrying covers the window before the
	// gateway active authority is populated, and the flag actually changing is the
	// only proof VSC accepted the op.
	f16HaltUntil := func(active bool, wantCount int, budget time.Duration) int {
		payload, _ := json.Marshal(map[string]any{"active": active, "keyId": cid + "-main"})
		deadline := time.Now().Add(budget)
		got := -1
		for time.Now().Before(deadline) {
			if _, err := broadcastGatewayMultisig(t, d, "vsc.tss_halt", []string{"vsc.gateway"}, string(payload), f16SignWith); err != nil {
				t.Logf("  tss_halt(active=%v) broadcast err (%v), retrying", active, firstLine(err.Error()))
			}
			for i := 0; i < 6; i++ {
				time.Sleep(10 * time.Second)
				if got = countHaltFlag(t, ctx, d, allNodes); got == wantCount {
					return got
				}
			}
		}
		return got
	}

	// f16SignedInputs counts how many of a pending spend's inputs already carry a
	// landed signature for keyId on magi-1.
	f16SignedInputs := func(keyId string, sd *btcvault.SigningData) int {
		n := 0
		for _, uh := range sd.UnsignedSigHashes {
			if vfSignatureLanded(d, ctx, 1, keyId, uh.SigHash) {
				n++
			}
		}
		return n
	}

	// f16IssueUnmap issues an ordinary user withdrawal from the ACTIVE generation and
	// captures the new pending spend plus its signing data. It deliberately stops
	// there: unlike unmapAndSettle (vault_stage3_test.go) it never waits for a
	// signature, because whether the signature lands IS the measurement.
	f16IssueUnmap := func(sats int64) (string, *btcvault.SigningData, string) {
		dest, err := d.bitcoinCli(ctx, "getnewaddress")
		if err != nil {
			t.Logf("getnewaddress: %v", err)
			return "", nil, "GETADDR_ERR"
		}
		before := txSpendIds(t, d, ctx, cid)
		s := vstatus(t, d, ctx, 1, cid, "unmap", fmt.Sprintf(`{"amount":"%d","to":"%s"}`, sats, dest))
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
		t.Logf("unmap pending spend txid=%s dest=%s", txid, dest)
		return txid, waitSigningData(t, d, ctx, cid, txid), s
	}

	// ---------------------------------------------------------------------
	// 1. Build the migration sweep, THEN halt BTC keysign fleet wide.
	// ---------------------------------------------------------------------
	sweepTxid, sd, migStatus := vfBuildSweep(t, d, ctx, 1, cid)
	if sd == nil {
		t.Fatalf("PRECONDITION FAILED: migrateVault produced no pending migration sweep (status=%s txid=%q). Without a pending successor sweep the V-8 exemption has nothing to exempt and every case below would be vacuous", migStatus, sweepTxid)
	}
	t.Logf("pending migration sweep txid=%s with %d input(s), migrateVault status=%s",
		sweepTxid, len(sd.UnsignedSigHashes), migStatus)

	haltedCount := f16HaltUntil(true, len(allNodes), 8*time.Minute)
	// How much of the sweep was ALREADY signed when the halt landed: the vacuity
	// measure for F16-SWEEP-SIGNS.
	preSigned := f16SignedInputs(cid+"-main", sd)
	if haltedCount != len(allNodes) {
		t.Fatalf("PRECONDITION FAILED: vsc.tss_halt(true) reached only %d/%d nodes. Nothing below measures a freeze if the fleet is not halted", haltedCount, len(allNodes))
	}
	t.Logf("BTC keysign halted on %d/%d nodes; %d of %d sweep inputs were already signed before the halt landed",
		haltedCount, len(allNodes), preSigned, len(sd.UnsignedSigHashes))

	// ---------------------------------------------------------------------
	// 2. F16-SWEEP-SIGNS: the successor paying sweep signs anyway (V-8).
	// ---------------------------------------------------------------------
	raw, sweepOK := vfAwaitSweepSignatures(t, d, ctx, cid+"-main", sd)
	vacuity := "V-8 EXERCISED: sweep inputs were still unsigned when the halt reached all nodes"
	switch {
	case preSigned == len(sd.UnsignedSigHashes):
		vacuity = "VACUOUS: every sweep input was ALREADY signed before the halt reached all nodes, so this case did NOT exercise V-8"
	case preSigned > 0:
		vacuity = fmt.Sprintf("PARTIAL: %d of %d inputs were already signed pre halt", preSigned, len(sd.UnsignedSigHashes))
	}
	c.rec("F16-SWEEP-SIGNS", "successor paying migration sweep still signs while BTC keysign is halted (V-8 exemption)",
		sweepOK, fmt.Sprintf("txid=%s inputs=%d halted=%d/%d | %s",
			sweepTxid, len(sd.UnsignedSigHashes), haltedCount, len(allNodes), vacuity))

	if sweepOK {
		bcTxid, bh, err := vfBroadcastAndMine(t, d, ctx, raw)
		if err != nil {
			t.Errorf("halted sweep was signed but regtest rejected the broadcast: %v", err)
		} else {
			cs := vfRelayAndConfirm(t, d, ctx, 1, cid, bcTxid, bh)
			t.Logf("sweep %s mined at BTC height %d, confirmSpend status=%s, gen-0 UTXO count on magi-1 now %d",
				bcTxid, bh, cs, vfGenUtxoCountOn(d, ctx, 1, cid, 0))
		}
	}

	// ---------------------------------------------------------------------
	// 3. F16-WITHDRAW-FROZEN: an ordinary withdrawal from the ACTIVE gen must NOT
	//    sign while halted, and the node must say so in its logs.
	// ---------------------------------------------------------------------
	const unmapSats = 1_000_000
	t.Logf("owner %s mapped balance before the withdrawal: %d sats", env.owner, balanceSats(t, d, ctx, cid, env.owner))
	frozenBase := vfCountLogs(d, ctx, 1, "frozen by solvency gate")
	unmapTxid, usd, unmapStatus := f16IssueUnmap(unmapSats)
	if usd == nil {
		c.rec("F16-WITHDRAW-FROZEN", "ordinary withdrawal from the ACTIVE generation does not sign while halted", false,
			fmt.Sprintf("unmap produced no pending spend (status=%s txid=%q), so the freeze could not be observed", unmapStatus, unmapTxid))
	} else {
		frozenDeadline := time.Now().Add(2 * time.Minute)
		signedWhileHalted := 0
		for time.Now().Before(frozenDeadline) {
			time.Sleep(10 * time.Second)
			if signedWhileHalted = f16SignedInputs(cid+"-mainv1", usd); signedWhileHalted > 0 {
				break // a leak: stop early and report it
			}
		}
		frozenNow := vfCountLogs(d, ctx, 1, "frozen by solvency gate")
		stillHalted := countHaltFlag(t, ctx, d, allNodes)
		c.rec("F16-WITHDRAW-FROZEN", "ordinary withdrawal from the ACTIVE generation does not sign while halted",
			signedWhileHalted == 0 && frozenNow >= 1,
			fmt.Sprintf("txid=%s inputs=%d signed=%d (want 0) | magi-1 \"frozen by solvency gate\" lines %d -> %d (want >=1) | halted=%d/%d",
				unmapTxid, len(usd.UnsignedSigHashes), signedWhileHalted, frozenBase, frozenNow, stillHalted, len(allNodes)))
	}

	// ---------------------------------------------------------------------
	// 4. F16-UNFROZEN: clear the halt, the SAME withdrawal signs.
	// ---------------------------------------------------------------------
	clearedCount := f16HaltUntil(false, 0, 8*time.Minute)
	if clearedCount != 0 {
		t.Errorf("vsc.tss_halt(false) did not clear everywhere: %d/%d nodes still report btc_keysign_halted", clearedCount, len(allNodes))
	}
	if usd != nil {
		unfrozenDeadline := time.Now().Add(3 * time.Minute)
		signedAfterClear := 0
		for {
			signedAfterClear = f16SignedInputs(cid+"-mainv1", usd)
			if signedAfterClear == len(usd.UnsignedSigHashes) || time.Now().After(unfrozenDeadline) {
				break
			}
			time.Sleep(10 * time.Second)
		}
		if signedAfterClear > 0 && signedAfterClear < len(usd.UnsignedSigHashes) {
			t.Logf("note: only %d of %d withdrawal inputs signed within 3 minutes of the clear (the freeze is lifted, the remainder is a sign interval timing tail)",
				signedAfterClear, len(usd.UnsignedSigHashes))
		}
		c.rec("F16-UNFROZEN", "the SAME withdrawal signs once the halt is cleared (the freeze is reversible, not a brick)",
			signedAfterClear > 0,
			fmt.Sprintf("txid=%s inputs=%d signed=%d | halted=%d/%d after clear",
				unmapTxid, len(usd.UnsignedSigHashes), signedAfterClear, clearedCount, len(allNodes)))
	} else {
		c.rec("F16-UNFROZEN", "the SAME withdrawal signs once the halt is cleared (the freeze is reversible, not a brick)",
			false, "no withdrawal pending spend was ever created, so there is nothing to unfreeze")
	}

	// ---------------------------------------------------------------------
	// 5. F16-IDENT: every node agrees byte for byte on the vault contract state.
	// ---------------------------------------------------------------------
	vfAssertContractIdentical(c, d, ctx, cid, allNodes, 4*time.Minute, "F16-IDENT")
	c.summary("F16")
}
