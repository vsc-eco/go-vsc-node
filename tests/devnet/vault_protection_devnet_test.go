package devnet

import (
	"context"
	"testing"
	"time"

	"vsc-node/modules/common/params"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// outboundHaltDoc mirrors the consensus_state.OutboundHalt fields we assert on.
type outboundHaltDoc struct {
	Account      string `bson:"account"`
	SetHeight    uint64 `bson:"set_height"`
	ExpiryHeight uint64 `bson:"expiry_height"`
}

// getOutboundHalts reads the chain_consensus_state singleton's outbound_halts on a
// node — the deterministic on-chain effect of vsc.halt/vsc.unhalt.
func (d *Devnet) getOutboundHalts(ctx context.Context, node int) ([]outboundHaltDoc, error) {
	cli, err := d.mongoClient(ctx)
	if err != nil {
		return nil, err
	}
	var doc struct {
		OutboundHalts []outboundHaltDoc `bson:"outbound_halts"`
	}
	err = cli.Database(d.nodeDbName(node)).Collection("chain_consensus_state").
		FindOne(ctx, bson.M{"_id": "singleton"}).Decode(&doc)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return doc.OutboundHalts, nil
}

// Halt broadcasts a vsc.halt signed by a committee witness account (bare name in
// required_auths; the Self field is envelope-only, never in the body).
func (d *Devnet) Halt(witness int, durationBlocks uint64, reason string) (string, error) {
	acct := d.witnessAccount(witness)
	payload := map[string]interface{}{"duration": durationBlocks, "reason": reason}
	return d.BroadcastCustomJSON("vsc.halt", []string{acct}, payload, d.cfg.InitminerWIF)
}

// TestVaultProtectionOutboundHaltDevnet exercises brief fixes 2+3 on a real
// multi-node devnet with consensus 0.6.0 pinned active:
//   - a single committee member's vsc.halt is applied DETERMINISTICALLY on every
//     node (the halt entry appears in each node's consensus_state);
//   - while the halt is active the exit-freeze REJECTS a withdrawal (the L2
//     balance is untouched — the op never debits);
//   - the bounded window AUTO-EXPIRES by height, after which the same withdrawal
//     settles.
func TestVaultProtectionOutboundHaltDevnet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	cfg.SkipFunding = false // need L2 HBD to withdraw
	if cfg.SysConfigOverrides.ConsensusParams == nil {
		cfg.SysConfigOverrides.ConsensusParams = &params.ConsensusParams{}
	}
	// Pin consensus 0.6.0 active from epoch 1 so the vault protections are live.
	// (Requires the running binary at consensus 6 — version.go currentConsensus.)
	// NOTE: FloorEpoch must be NON-ZERO — PinnedVersionFloor treats 0 as "no floor".
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorMajor = 0
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = 6
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1

	d, ctx := startDevnetNoKey(t, cfg, 25*time.Minute)

	for n := 1; n <= cfg.Nodes; n++ {
		nctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		err := d.waitForElectionEpoch(nctx, n, 1, 8*time.Minute)
		cancel()
		if err != nil {
			t.Fatalf("magi-%d never ingested epoch >= 1: %v", n, err)
		}
	}

	const A = 1
	userA := d.witnessAccount(A) // bare "magi.test1" — committee member + required_auths
	userAFull := "hive:" + userA

	// Fund userA with HBD and wait for the deposit to credit the VSC ledger.
	retryBroadcast(t, "deposit", func() (string, error) { return d.Deposit(ctx, A, "100.000", "hbd") })
	if !waitBalancePositive(t, d, ctx, 1, userAFull, "hbd", 3*time.Minute) {
		t.Fatal("deposit never credited userA's VSC hbd balance")
	}
	b0, err := d.GetAccountBalance(ctx, 1, userAFull)
	if err != nil {
		t.Fatalf("read userA balance: %v", err)
	}
	t.Logf("userA hbd before halt = %d", b0.Hbd)

	// --- Fix 3: one committee member freezes outbounds. 60 blocks (~3min) window. ---
	retryBroadcast(t, "vsc.halt", func() (string, error) { return d.Halt(A, 60, "devnet-test") })

	// Deterministic across the whole network: the halt entry appears on EVERY node.
	var setHeight, expiryHeight uint64
	for n := 1; n <= cfg.Nodes; n++ {
		found := false
		deadline := time.Now().Add(2 * time.Minute)
		for time.Now().Before(deadline) {
			halts, _ := d.getOutboundHalts(ctx, n)
			for _, h := range halts {
				if h.Account == userA {
					found, setHeight, expiryHeight = true, h.SetHeight, h.ExpiryHeight
				}
			}
			if found {
				break
			}
			time.Sleep(3 * time.Second)
		}
		if !found {
			t.Fatalf("magi-%d: vsc.halt entry never appeared in consensus_state (0.6.0 inactive, or op rejected)", n)
		}
	}
	t.Logf("halt active on all %d nodes: set=%d expiry=%d (window=%d blocks)",
		cfg.Nodes, setHeight, expiryHeight, expiryHeight-setHeight)

	// --- Fix 2: exit-freeze — a withdrawal submitted while halted is rejected, so
	// the L2 balance is untouched (the withdraw op never debits). ---
	retryBroadcast(t, "withdraw(during-halt)", func() (string, error) { return d.Withdraw(A, userA, "10.000", "hbd", "during-halt") })
	time.Sleep(30 * time.Second) // several blocks; a live withdraw would have debited by now
	bHalted, err := d.GetAccountBalance(ctx, 1, userAFull)
	if err != nil {
		t.Fatalf("read balance during halt: %v", err)
	}
	if bHalted.Hbd != b0.Hbd {
		t.Fatalf("exit-freeze FAILED: hbd changed during halt (%d -> %d) — withdrawal was not rejected",
			b0.Hbd, bHalted.Hbd)
	}
	t.Logf("exit-freeze holds: hbd still %d during halt", bHalted.Hbd)

	// --- Auto-expiry: wait past ExpiryHeight, then the same withdrawal settles. ---
	if err := d.WaitForBlockProcessing(ctx, 1, expiryHeight+2, 4*time.Minute); err != nil {
		t.Fatalf("chain never passed halt expiry height %d: %v", expiryHeight, err)
	}
	// The halt must no longer be active on any node.
	for n := 1; n <= cfg.Nodes; n++ {
		if halts, _ := d.getOutboundHalts(ctx, n); haltActiveAt(halts, userA, expiryHeight) {
			t.Fatalf("magi-%d: halt still active at/after expiry height %d", n, expiryHeight)
		}
	}

	retryBroadcast(t, "withdraw(post-expiry)", func() (string, error) { return d.Withdraw(A, userA, "10.000", "hbd", "post-expiry") })
	settled := false
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		b, _ := d.GetAccountBalance(ctx, 1, userAFull)
		if b != nil && b.Hbd < b0.Hbd {
			settled = true
			t.Logf("post-expiry withdraw settled: hbd %d -> %d", b0.Hbd, b.Hbd)
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !settled {
		t.Fatal("withdrawal never settled after the halt auto-expired")
	}
}

// retryBroadcast runs a broadcast that may hit a transient Hive-RPC error (e.g.
// "server closed connection before returning the first response byte" from the
// drone/hived proxy) and retries it; only a persistent failure fails the test.
func retryBroadcast(t *testing.T, label string, fn func() (string, error)) {
	t.Helper()
	var err error
	for i := 0; i < 6; i++ {
		if _, err = fn(); err == nil {
			return
		}
		t.Logf("%s broadcast attempt %d failed: %v — retrying", label, i+1, err)
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("%s failed after retries: %v", label, err)
}

// haltActiveAt reports whether account has an outbound halt active at height.
func haltActiveAt(halts []outboundHaltDoc, account string, height uint64) bool {
	for _, h := range halts {
		if h.Account == account && h.SetHeight <= height && height < h.ExpiryHeight {
			return true
		}
	}
	return false
}
