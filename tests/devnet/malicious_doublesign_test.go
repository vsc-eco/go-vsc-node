package devnet

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
)

// gqlConsensus reads an account's HIVE_CONSENSUS bond (base units) from a
// node's GQL endpoint. -1 on error, 0 if no balance record.
func gqlConsensus(t *testing.T, gqlURL, account string) int64 {
	t.Helper()
	q := `{ getAccountBalance(account: "` + account + `") { hive_consensus } }`
	reqBody, _ := json.Marshal(map[string]string{"query": q})
	resp, err := http.Post(gqlURL, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Data struct {
			GetAccountBalance *struct {
				HiveConsensus int64 `json:"hive_consensus"`
			} `json:"getAccountBalance"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return -1
	}
	if out.Data.GetAccountBalance == nil {
		return 0
	}
	return out.Data.GetAccountBalance.HiveConsensus
}

// TestMaliciousDoubleSign runs 6 nodes on origin/main with SafetySlashEnabled=true.
// node-3 (magi.test3) is forced, whenever it is the elected producer, to broadcast
// a SECOND competing vsc.produce_block for the same slot height (different block
// ref) — classic equivocation. The on-chain double-sign detector
// (safetyslash.EvidenceVSCDoubleBlockSign) should fire on every node and slash
// 10% of magi.test3's HIVE_CONSENSUS bond. We measure node-3's bond vs an honest
// node's, on all 6 nodes, and grep logs for the detection/slash.
func TestMaliciousDoubleSign(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := DefaultConfig()
	cfg.Nodes = 6
	cfg.SkipFunding = true // witnesses are still consensus-staked by devnet-setup
	cfg.LogLevel = "info"
	cfg.MagiEnv = map[string]string{"VSC_DOUBLE_SIGN_ACCOUNT": "magi.test3"}
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ElectionInterval: 20,
		},
	}

	d, ctx := startDevnetNoKey(t, cfg, 45*time.Minute)

	const (
		attacker = "magi.test3"
		honest   = "magi.test1"
	)

	head, _ := getHeadBlock(d.HiveRPCEndpoint())
	base3 := gqlConsensus(t, d.GQLEndpoint(1), "hive:"+attacker)
	baseH := gqlConsensus(t, d.GQLEndpoint(1), "hive:"+honest)
	t.Logf("[ds] head=%d baseline hive_consensus: %s=%d %s(honest)=%d", head, attacker, base3, honest, baseH)

	// Let node-3 take several producer slots (and double-sign each).
	t.Logf("[ds] observing ~80 blocks while node-3 double-signs every slot it produces")
	waitForBlock(t, d.HiveRPCEndpoint(), head+80, 12*time.Minute)

	// Per-node view of the bonds — node-3 should be slashed ~10% vs honest.
	t.Logf("[ds] === per-node hive_consensus (node-3 = malicious double-signer) ===")
	slashed := false
	for n := 1; n <= cfg.Nodes; n++ {
		ep := d.GQLEndpoint(n)
		a3 := gqlConsensus(t, ep, "hive:"+attacker)
		aH := gqlConsensus(t, ep, "hive:"+honest)
		pct := 0.0
		if aH > 0 && a3 >= 0 {
			pct = float64(aH-a3) / float64(aH) * 100
		}
		t.Logf("[ds] node-%d view: %s=%d  %s(honest)=%d  (node-3 down %.1f%% vs honest)", n, attacker, a3, honest, aH, pct)
		if aH > 0 && a3 >= 0 && a3 < aH {
			slashed = true
		}
	}
	if slashed {
		t.Logf("[ds] RESULT: node-3's HIVE_CONSENSUS bond is BELOW the honest node's — double-sign slash fired.")
	} else {
		t.Logf("[ds] RESULT: node-3's bond NOT below honest — slash did not fire (inspect logs: did node-3 produce? did the competing block land? is SafetySlashEnabled true?).")
	}

	for _, kw := range []string{
		"MALICIOUS DOUBLE-SIGN",                             // node-3 broadcast the competing block
		"principal slash applied for double block proposal", // detector slashed
		"double block",
		"safety slashing disabled", // would mean the gate is still false
	} {
		if node, ok := anyNodeLogsContain(d, ctx, cfg.Nodes, kw); ok {
			t.Logf("[ds] log %q seen (e.g. magi-%d)", kw, node)
		} else {
			t.Logf("[ds] log %q NOT seen", kw)
		}
	}
}
