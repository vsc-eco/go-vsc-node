package devnet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"

	"github.com/vsc-eco/hivego"
)

// TestVaultTssHalt proves the emergency BTC-keysign halt op end-to-end: the ungated,
// live-on-merge governance flag (vsc.tss_halt). The flag->freeze logic is unit-tested
// (TestBtcKeysignFrozen_FlagAndScope); the one integration piece not covered is the OP
// PLUMBING — does a gateway-multisig vsc.tss_halt actually reach
// consensus_state.btc_keysign_halted on EVERY node, and clear again. This closes the gap
// the mixed-version test could not (it broadcast once, before vsc.gateway's authority was
// populated, so VSC silently dropped the op).
//
// It uses the known-working gateway-multisig setup (DefaultConfig + deterministic BLS
// keys) and RETRIES the op until the flag actually propagates — which also confirms VSC
// accepted it (a Hive-level broadcast success is not acceptance). No BTC keygen/vault is
// needed: the op handler only checks RequiredAuths[0] == GatewayWallet().
//
//	VAULT_TSSHALT_RUN=1 go test -v -run TestVaultTssHalt -timeout 40m ./tests/devnet/
func TestVaultTssHalt(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_TSSHALT_RUN") == "" {
		t.Skip("set VAULT_TSSHALT_RUN=1")
	}
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 38*time.Minute)
	defer cancel()

	cfg := DefaultConfig()
	cfg.Nodes = 6
	cfg.SkipFunding = true
	cfg.LogLevel = "info"
	// Deterministic gateway BLS keys so the test can re-derive them for the multisig —
	// the missing ingredient that made the mixed-version halt case unrunnable.
	cfg.MagiEnv = map[string]string{"DEVNET_DETERMINISTIC_BLS": "1"}
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{ElectionInterval: 20},
	}
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, ctx := startDevnetNoKey(t, cfg, 36*time.Minute)

	allNodes := []int{1, 2, 3, 4, 5, 6}

	pass, fail := 0, 0
	rec := func(id, desc string, ok bool, detail string) {
		if ok {
			pass++
			t.Logf("CASE %s PASS — %s | %s", id, desc, detail)
		} else {
			fail++
			t.Errorf("CASE %s FAIL — %s | %s", id, desc, detail)
		}
	}

	// gateway multisig signers (any 4 of 6 satisfy the 2/3 threshold)
	var signWith []*hivego.KeyPair
	for n := 1; n <= cfg.Nodes; n++ {
		signWith = append(signWith, devnetGatewayKeypair(t, fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, n)))
	}
	signWith = signWith[:4]

	// haltUntil re-broadcasts the gateway tss_halt op until the flag reaches wantCount
	// across all nodes (or times out). Retrying handles the window before vsc.gateway's
	// authority is populated, and the flag actually changing is what CONFIRMS VSC accepted
	// the op — a Hive-broadcast success alone does not.
	haltUntil := func(active bool, wantCount int) int {
		payload, _ := json.Marshal(map[string]any{"active": active, "keyId": "btc"})
		deadline := time.Now().Add(15 * time.Minute)
		got := -1
		for time.Now().Before(deadline) {
			if _, err := broadcastGatewayMultisig(t, d, "vsc.tss_halt", []string{"vsc.gateway"}, string(payload), signWith); err != nil {
				t.Logf("  tss_halt(active=%v) broadcast err (%v) — retrying", active, firstLine(err.Error()))
			}
			// give the op time to be included + processed on every node
			for i := 0; i < 6; i++ {
				time.Sleep(10 * time.Second)
				if got = countHaltFlag(t, ctx, d, allNodes); got == wantCount {
					return got
				}
			}
		}
		return got
	}

	// ── HALT-01: gateway tss_halt(true) sets btc_keysign_halted on EVERY node. ──
	haltedCount := haltUntil(true, len(allNodes))
	rec("HALT-01", "gateway vsc.tss_halt(true) set btc_keysign_halted on ALL nodes", haltedCount == len(allNodes),
		fmt.Sprintf("%d/%d nodes halted", haltedCount, len(allNodes)))

	if haltedCount != len(allNodes) {
		// Without a real halt there is nothing to clear; report and stop.
		t.Logf("TSSHALT SUMMARY: %d PASS %d FAIL", pass, fail)
		return
	}

	// ── HALT-02: gateway tss_halt(false) clears it on EVERY node. ──
	clearedCount := haltUntil(false, 0)
	rec("HALT-02", "gateway vsc.tss_halt(false) cleared btc_keysign_halted on ALL nodes", clearedCount == 0,
		fmt.Sprintf("%d/%d nodes still halted (want 0)", clearedCount, len(allNodes)))

	t.Logf("TSSHALT SUMMARY: %d PASS %d FAIL", pass, fail)
}
