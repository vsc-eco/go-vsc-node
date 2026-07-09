package devnet

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	libhive "vsc-node/lib/hive"
	"vsc-node/modules/common/params"

	"github.com/vsc-eco/hivego"
)

// drainGateway moves HBD/HIVE OUT of the gateway account via a gateway-multisig
// transfer (4-of-6 gateway keys satisfy the 2/3 threshold) — simulating an
// out-of-band drain that reduces the gateway's observed on-chain balance without
// touching the L2 liability, exactly the condition the solvency monitor guards.
func drainGateway(t *testing.T, d *Devnet, to, amount string, signWith []*hivego.KeyPair) (string, error) {
	t.Helper()
	client := hivego.NewHiveRpc([]string{d.DroneEndpoint()})
	client.ChainID = devnetChainID
	creator := libhive.LiveTransactionCreator{
		TransactionBroadcaster: libhive.TransactionBroadcaster{Client: client},
		TransactionCrafter:     libhive.TransactionCrafter{},
	}
	op := hivego.TransferOperation{From: "vsc.gateway", To: to, Amount: amount, Memo: "devnet solvency drain"}
	tx := creator.MakeTransaction([]hivego.HiveOperation{op})
	if err := creator.PopulateSigningProps(&tx, nil); err != nil {
		return "", fmt.Errorf("populate signing props: %w", err)
	}
	for i, kp := range signWith {
		sig, err := tx.Sign(*kp, devnetChainID)
		if err != nil {
			return "", fmt.Errorf("sign with key %d: %w", i, err)
		}
		tx.AddSig(sig)
	}
	return client.BroadcastRaw(tx)
}

// TestVaultProtectionSolvencyAutoHaltDevnet proves fix 1 end-to-end: with the
// opt-in solvency monitor enabled (halt mode), a real drain of the gateway's
// on-chain HBD below the L2 liability is DETECTED by the per-node monitor, which
// broadcasts a node-signed vsc.halt — freezing outbounds with no vote. It also
// checks the monitor does NOT false-halt a solvent gateway first.
func TestVaultProtectionSolvencyAutoHaltDevnet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	cfg.Nodes = 6
	cfg.SkipFunding = false
	// Enable + speed up the opt-in solvency monitor, and use deterministic gateway
	// keys so the test can sign the drain as the gateway multisig. Merge into the
	// existing MagiEnv rather than replacing it.
	if cfg.MagiEnv == nil {
		cfg.MagiEnv = map[string]string{}
	}
	cfg.MagiEnv["VSC_SOLVENCY_MONITOR"] = "halt"
	cfg.MagiEnv["VSC_SOLVENCY_INTERVAL"] = "20" // sample every ~20 blocks (~1 min)
	cfg.MagiEnv["VSC_SOLVENCY_CONFIRMATIONS"] = "2"
	cfg.MagiEnv["VSC_SOLVENCY_GAP_BPS"] = "100"
	cfg.MagiEnv["DEVNET_DETERMINISTIC_BLS"] = "1"
	if cfg.SysConfigOverrides.ConsensusParams == nil {
		cfg.SysConfigOverrides.ConsensusParams = &params.ConsensusParams{}
	}
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorMajor = 0
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = 6
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1

	d, ctx := startDevnetNoKey(t, cfg, 35*time.Minute)
	for n := 1; n <= cfg.Nodes; n++ {
		nctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		err := d.waitForElectionEpoch(nctx, n, 1, 8*time.Minute)
		cancel()
		if err != nil {
			t.Fatalf("magi-%d never ingested epoch >= 1: %v", n, err)
		}
	}

	const A = 1
	userAFull := "hive:" + d.witnessAccount(A)

	// Establish an HBD liability with a matching gateway balance (solvent): deposit
	// 100 HBD -> gateway holds 100 HBD on-chain, L2 HBD liability == 100.
	retryBroadcast(t, "deposit", func() (string, error) { return d.Deposit(ctx, A, "100.000", "hbd") })
	if !waitBalancePositive(t, d, ctx, 1, userAFull, "hbd", 3*time.Minute) {
		t.Fatal("deposit never credited")
	}

	// The monitor must NOT halt a SOLVENT gateway. Give it several sample intervals.
	time.Sleep(2 * time.Minute)
	if halts, _ := d.getOutboundHalts(ctx, 1); len(halts) > 0 {
		t.Fatalf("solvency monitor FALSE-HALTED a solvent gateway: %+v", halts)
	}
	t.Log("baseline: monitor did not halt a solvent gateway")

	// Drain 60 HBD out of the gateway via the gateway multisig -> observed (~40) <
	// expected (100): a ~60% HBD shortfall, far past the 1% tolerance.
	gwKeys := make([]*hivego.KeyPair, 0, cfg.Nodes)
	for n := 1; n <= cfg.Nodes; n++ {
		gwKeys = append(gwKeys, devnetGatewayKeypair(t, fmt.Sprintf("%s%d", cfg.WitnessPrefix, n)))
	}
	drainTx, err := drainGateway(t, d, "initminer", "60.000 TBD", gwKeys[:4])
	if err != nil {
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 40)
		t.Fatalf("drain gateway: %v", err)
	}
	t.Logf("drained 60 HBD from the gateway tx=%s (observed HBD now ~40 vs expected 100)", drainTx)

	// The per-node monitor must detect the shortfall and broadcast a node-signed
	// vsc.halt. Poll consensus_state for a solvency-reason halt on every node.
	halted := false
	deadline := time.Now().Add(7 * time.Minute)
	for time.Now().Before(deadline) {
		halts, _ := d.getOutboundHalts(ctx, 1)
		for _, h := range halts {
			r := strings.ToLower(h.Reason)
			if strings.Contains(r, "solvency") || strings.Contains(r, "hbd") {
				halted = true
				t.Logf("solvency AUTO-HALT fired: account=%s reason=%q set=%d expiry=%d", h.Account, h.Reason, h.SetHeight, h.ExpiryHeight)
			}
		}
		if halted {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !halted {
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 60)
		t.Fatal("solvency monitor never auto-halted after the gateway was drained below its L2 liability")
	}

	// Deterministic effect: the halt is visible on every node's consensus_state.
	// Poll each node to convergence — a node may process the halt op a slot or two
	// after node 1 (it is an on-chain op every node applies, just not simultaneously).
	for n := 1; n <= cfg.Nodes; n++ {
		seen := false
		nodeDeadline := time.Now().Add(2 * time.Minute)
		for time.Now().Before(nodeDeadline) {
			if halts, _ := d.getOutboundHalts(ctx, n); len(halts) > 0 {
				seen = true
				break
			}
			time.Sleep(3 * time.Second)
		}
		if !seen {
			t.Errorf("magi-%d: solvency halt never appeared in consensus_state (fork?)", n)
		}
	}
	t.Log("PASS: solvency monitor auto-halted the drained gateway, deterministically on all nodes")
}
