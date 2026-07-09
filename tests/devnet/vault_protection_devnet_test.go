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

// Unhalt broadcasts a vsc.unhalt signed by a recovery-multisig roster account
// (empty haltTxId lifts all active halts).
func (d *Devnet) Unhalt(rosterWitness int, haltTxId string) (string, error) {
	acct := d.witnessAccount(rosterWitness)
	payload := map[string]interface{}{}
	if haltTxId != "" {
		payload["halt_tx_id"] = haltTxId
	}
	return d.BroadcastCustomJSON("vsc.unhalt", []string{acct}, payload, d.cfg.InitminerWIF)
}

// pendingActionAmounts returns the amounts of all still-pending gateway actions of
// a type on a node — used to observe the value-scaled outbound delay (a delayed
// withdrawal stays pending; a fast one settles out).
func (d *Devnet) pendingActionAmounts(ctx context.Context, node int, actionType string) ([]int64, error) {
	cli, err := d.mongoClient(ctx)
	if err != nil {
		return nil, err
	}
	cur, err := cli.Database(d.nodeDbName(node)).Collection("ledger_actions").
		Find(ctx, bson.M{"status": "pending", "type": actionType})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []int64
	for cur.Next(ctx) {
		var a struct {
			Amount int64 `bson:"amount"`
		}
		if cur.Decode(&a) == nil {
			out = append(out, a.Amount)
		}
	}
	return out, nil
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

// TestVaultProtectionSuiteDevnet extends devnet coverage to fix 4 (value-scaled
// delay), halt stacking, and the vsc.unhalt safety valve — all in one devnet run:
//   - Fix 4: a large withdrawal is HELD (its gateway action stays pending) while a
//     dust withdrawal settles out.
//   - Fix 3 stacking: two committee members' halts coexist in consensus_state.
//   - vsc.unhalt: the recovery multisig lifts BOTH halts EARLY, before their window
//     would auto-expire.
func TestVaultProtectionSuiteDevnet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	cfg.SkipFunding = false
	if cfg.SysConfigOverrides.ConsensusParams == nil {
		cfg.SysConfigOverrides.ConsensusParams = &params.ConsensusParams{}
	}
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorMajor = 0
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = 6
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1
	// Recovery roster for vsc.unhalt: threshold 1, one witness account — keeps
	// devnet signing single-sig; VerifyRecoveryMultisig's M-of-N path is identical.
	cfg.SysConfigOverrides.ConsensusParams.RecoveryMultisigAccounts = []string{cfg.WitnessPrefix + "2"}
	cfg.SysConfigOverrides.ConsensusParams.RecoveryMultisigThreshold = 1

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
	userA := d.witnessAccount(A)
	userAFull := "hive:" + userA

	// ===================== Fix 4: value-scaled outbound delay =====================
	// Use HIVE: devnet witnesses are funded with 10000 TESTS (vs only 100 TBD), and
	// the value-scaled delay is amount-based (asset-agnostic), so HIVE gives room
	// for a tier-1 (>=1000-coin) withdrawal.
	retryBroadcast(t, "deposit", func() (string, error) { return d.Deposit(ctx, A, "2500.000", "hive") })
	if !waitBalancePositive(t, d, ctx, 1, userAFull, "hive", 3*time.Minute) {
		t.Fatal("deposit never credited")
	}
	// large: 1500 coin -> tier-1 (~200-block delay); dust: 500 coin -> no delay.
	// Match by the tier-1 threshold (robust to any withdraw fee): a pending
	// withdraw action >= tier-1 is the "large" (held), one < tier-1 is the "dust".
	const tier1 = int64(1_000_000)
	hasLarge := func(amts []int64) bool {
		for _, a := range amts {
			if a >= tier1 {
				return true
			}
		}
		return false
	}
	hasDust := func(amts []int64) bool {
		for _, a := range amts {
			if a > 0 && a < tier1 {
				return true
			}
		}
		return false
	}
	retryBroadcast(t, "withdraw-large", func() (string, error) { return d.Withdraw(A, userA, "1500.000", "hive", "large-delayed") })
	retryBroadcast(t, "withdraw-dust", func() (string, error) { return d.Withdraw(A, userA, "500.000", "hive", "dust-fast") })

	// Phase 1: wait for the large withdrawal's action to be created and observed
	// pending (the ops take a few blocks to process — the previous run's assertion
	// misfired by checking before anything was pending). The large has a ~200-block
	// delay so it stays pending for minutes once created.
	largeSeen := false
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		amts, _ := d.pendingActionAmounts(ctx, 1, "withdraw")
		if hasLarge(amts) {
			largeSeen = true
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !largeSeen {
		t.Fatal("large withdrawal action never appeared pending (op not processed?)")
	}

	// Phase 2: the dust (0-delay) must settle OUT while the large (tier-1 delay)
	// stays pending — the value-scaling proof. If the large clears within this
	// window (well under its ~200-block delay), fix 4 is broken.
	delayProven := false
	deadline = time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		amts, _ := d.pendingActionAmounts(ctx, 1, "withdraw")
		if !hasLarge(amts) {
			t.Fatal("delay FAILED: the large withdrawal cleared pending — not held by its value-scaled delay")
		}
		if !hasDust(amts) {
			delayProven = true
			t.Log("value-scaled delay holds: dust settled while the large withdrawal is still pending")
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !delayProven {
		t.Fatal("delay inconclusive: dust never settled while large held (gateway slow?)")
	}

	// ================ Fix 3 stacking + vsc.unhalt (safety valve) ==================
	const window = uint64(600) // long enough it won't auto-expire during the test
	retryBroadcast(t, "halt-m1", func() (string, error) { return d.Halt(1, window, "m1") })
	retryBroadcast(t, "halt-m3", func() (string, error) { return d.Halt(3, window, "m3") })
	m1, m3 := d.witnessAccount(1), d.witnessAccount(3)

	var expiry1 uint64
	stacked := false
	deadline = time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		halts, _ := d.getOutboundHalts(ctx, 1)
		has1, has3 := false, false
		for _, h := range halts {
			if h.Account == m1 {
				has1, expiry1 = true, h.ExpiryHeight
			}
			if h.Account == m3 {
				has3 = true
			}
		}
		if has1 && has3 {
			stacked = true
			break
		}
		time.Sleep(3 * time.Second)
	}
	if !stacked {
		t.Fatal("two validators' halts never stacked in consensus_state")
	}
	t.Logf("stacking holds: both %s and %s halts active (m1 window expiry=%d)", m1, m3, expiry1)

	// Recovery multisig lifts BOTH halts EARLY (well before the window expiry).
	retryBroadcast(t, "unhalt", func() (string, error) { return d.Unhalt(2, "") })

	lifted := false
	deadline = time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		h, _ := d.getLastProcessedBlock(ctx, 1)
		halts, _ := d.getOutboundHalts(ctx, 1)
		if h > 0 && !haltActiveAt(halts, m1, h) && !haltActiveAt(halts, m3, h) {
			if h >= expiry1 {
				t.Fatalf("unhalt did not lift EARLY: height %d already past window expiry %d", h, expiry1)
			}
			lifted = true
			t.Logf("vsc.unhalt lifted both halts EARLY at height %d (window expiry was %d)", h, expiry1)
			break
		}
		time.Sleep(3 * time.Second)
	}
	if !lifted {
		t.Fatal("recovery-multisig vsc.unhalt never lifted the stacked halts")
	}
}
