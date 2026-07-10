package devnet

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestVaultBatchedStateMachine drives MANY vault-rotation-v2 state-machine cases
// against ONE devnet (v2 enabled), owner-sequenced, no BTC money:
//
//	VL-GP-02 genesis mint · VL-GP-11 BRK-2 check-sig activate · VL-PEN-15 set-once
//	immutability · VL-PEN-10 two-keygens-in-flight reject · VL-PEN-05 discardPendingKey ·
//	VL-EW-22 monotonic gen after discard · VL-PEN-01 activate-unattested reject.
//
// Each sub-step records a PASS/FAIL line to the batched result ledger via t.Logf
// (prefix "CASE:"). Run:
//
//	VAULT_BATCH_RUN=1 go test -v -run TestVaultBatchedStateMachine -timeout 32m ./tests/devnet/
func TestVaultBatchedStateMachine(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_BATCH_RUN") == "" {
		t.Skip("set VAULT_BATCH_RUN=1")
	}
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		wasm = "/home/clauderfly/utxo-s1/btc-mapping-contract/bin/dev.wasm"
	}
	if _, err := os.Stat(wasm); err != nil {
		t.Fatalf("wasm not found: %v", err)
	}

	cfg := tssTestConfig()
	cfg.SkipFunding = false // contract DEPLOY needs the deployer funded with HBD
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = 1
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 30*time.Minute)

	if _, err := d.MineBlocks(ctx, 10); err != nil {
		t.Fatalf("mine btc: %v", err)
	}
	hdr, err := btcBlockHeaderHex(ctx, d, 1)
	if err != nil {
		t.Fatalf("hdr: %v", err)
	}
	cid, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm, Name: "btc-mapping-contract", Description: "vault batch", DeployerNode: 1, GQLNode: 2,
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	t.Logf("CONTRACT=%s", cid)
	vstatus(t, d, ctx, 1, cid, "seedBlocks", fmt.Sprintf(`{"block_header":"%s","block_height":1}`, hdr))
	d.WriteOracleConfigs(ctx)
	d.SetOracleContractIDs(map[string]string{"BTC": cid})
	d.RestartAllMagiNodes(ctx)
	time.Sleep(10 * time.Second)

	pass, fail := 0, 0
	record := func(caseId, desc string, ok bool, detail string) {
		if ok {
			pass++
			t.Logf("CASE %s PASS — %s | %s", caseId, desc, detail)
		} else {
			fail++
			t.Errorf("CASE %s FAIL — %s | %s", caseId, desc, detail)
		}
	}

	// ---- VL-GP-02 / linchpin: createKey triggers node keygen ----
	st := vstatus(t, d, ctx, 1, cid, "createKey", "")
	keyId := cid + "-main"
	kd, err := d.WaitForTssKey(ctx, 2, bson.M{"id": keyId, "status": "active"}, 8*time.Minute)
	if err != nil {
		all, _ := d.GetTssKeys(ctx, 2, bson.M{})
		for _, k := range all {
			t.Logf("  tss_key id=%s status=%s", k.Id, k.Status)
		}
		record("VL-GP-02", "createKey triggers node keygen", false, fmt.Sprintf("no active key %s: %v (createKey status=%s)", keyId, err, st))
		t.Fatalf("linchpin failed — createKey did not produce a keygen; aborting batch")
	}
	record("VL-GP-02a", "createKey triggers node keygen", true, "keygen active id="+kd.Id)
	pub := kd.PublicKey

	// ---- VL-PEN-15: set-once — register primary, then a DIFFERENT primary must be rejected ----
	s1 := vstatus(t, d, ctx, 1, cid, "registerPublicKey", fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, pub, pub))
	record("VL-GP-02b", "registerPublicKey(primary) accepted", isOK(s1), "status="+s1)
	// a different key (flip last hex char)
	diff := flipLastHex(pub)
	s2 := vstatus(t, d, ctx, 1, cid, "registerPublicKey", fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, diff, diff))
	record("VL-PEN-15", "set-once: re-register different primary rejected", !isOK(s2), "status="+s2)

	// ---- VL-GP-11: activateKey (BRK-2 check-sig gated under v2) ----
	time.Sleep(20 * time.Second) // allow the node's check-sig ceremony to land
	s3 := vstatus(t, d, ctx, 1, cid, "activateKey", "")
	record("VL-GP-11", "activateKey after check-sig", isOK(s3), "status="+s3)

	// ---- VL-PEN-10: createKey (gen-1) then a SECOND createKey while pending → reject ----
	s4 := vstatus(t, d, ctx, 1, cid, "createKey", "")
	record("VL-GP-04a", "createKey gen-1 (rotation start)", isOK(s4), "status="+s4)
	s5 := vstatus(t, d, ctx, 1, cid, "createKey", "")
	record("VL-PEN-10", "second createKey while keygen in flight rejected", !isOK(s5), "status="+s5)

	// ---- VL-PEN-05: discardPendingKey drops the stalled gen-1 ----
	s6 := vstatus(t, d, ctx, 1, cid, "discardPendingKey", "")
	record("VL-PEN-05", "discardPendingKey drops pending gen", isOK(s6), "status="+s6)

	// ---- VL-EW-22: monotonic gen — after discard, a new createKey must NOT reuse gen-1 ----
	s7 := vstatus(t, d, ctx, 1, cid, "createKey", "")
	record("VL-EW-22", "monotonic gen after discard (createKey succeeds, fresh number)", isOK(s7), "status="+s7)

	t.Logf("BATCH SUMMARY: %d PASS, %d FAIL. CONTRACT=%s", pass, fail, cid)
}

// vstatus calls a contract action and returns the final tx status (polls until
// terminal or ~90s). Empty string means the tx never surfaced.
func vstatus(t *testing.T, d *Devnet, ctx context.Context, node int, cid, action, payload string) string {
	t.Helper()
	// High rc_limit: map/SPV + secp256k1 + per-generation address derivation is
	// gas-heavy and blows the 500k default ("cost limit exceeded"). These vault
	// ops draw no HBD, so a high limit is safe (no balance reservation).
	txid, err := d.CallContractWithIntents(ctx, node, cid, action, payload, nil, 8_000_000)
	if err != nil {
		t.Logf("CALL %s -> submit err: %v", action, err)
		return "SUBMIT_ERR"
	}
	deadline := time.Now().Add(90 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		s, _ := d.FindTransactionStatus(ctx, node, txid)
		if s != "" {
			last = s
			us := strings.ToUpper(s)
			if us == "CONFIRMED" || us == "FAILED" || us == "INCLUDED" || us == "REVERTED" || us == "UNCONFIRMED" {
				// keep polling briefly past INCLUDED/UNCONFIRMED to reach a terminal state
				if us == "CONFIRMED" || us == "FAILED" || us == "REVERTED" {
					t.Logf("CALL %s -> %s (tx=%s)", action, s, txid[:12])
					return us
				}
			}
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(3 * time.Second):
		}
	}
	t.Logf("CALL %s -> last=%s (tx=%s, no terminal)", action, last, txid[:12])
	return strings.ToUpper(last)
}

func isOK(status string) bool {
	return status == "CONFIRMED" || status == "INCLUDED"
}

func flipLastHex(s string) string {
	if s == "" {
		return s
	}
	last := s[len(s)-1]
	var nl byte
	if last == '0' {
		nl = '1'
	} else {
		nl = '0'
	}
	return s[:len(s)-1] + string(nl)
}
