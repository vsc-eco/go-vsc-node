package devnet

import (
	"context"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common/params"
)

// TestPoaExitHaltReleases is scenario D13-RELEASE, the other one-way-door risk.
//
// D13a proved the exit-halt HOLDS a seated operator's consensus bond (the unstake
// transaction is processed and FAILS). Nothing proved the hold ever LIFTS. A hold
// that never lifts is not a deterrent, it is permanent confiscation: every seated
// operator's collateral would be locked forever with no code path to recover it.
// That is strictly worse than having no exit-halt at all, and D13a passes in that
// state because it only asserts the refusal.
//
// The mechanism has two shapes (transactions.go:887-899):
//
//	(a) still an ELECTABLE witness -> no fixed release; the clock has not started.
//	    The operator must stop being electable first.
//	(b) winding down -> PoaExitHaltReleaseHeight returns a concrete height.
//
// So the test must first make the operator non-electable (disable its witness,
// which is what standing down actually means), then wait out PoaExitHaltBlocks
// (120 on devnet, ~6 min) and require the unstake to SUCCEED.
func TestPoaExitHaltReleases(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	cfg.SkipFunding = false
	if cfg.SysConfigOverrides.ConsensusParams == nil {
		cfg.SysConfigOverrides.ConsensusParams = &params.ConsensusParams{}
	}
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorMajor = 0
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = 7
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1

	d, ctx := startDevnetNoKey(t, cfg, 55*time.Minute)

	for n := 1; n <= cfg.Nodes; n++ {
		nctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
		err := d.waitForElectionEpoch(nctx, n, 2, 12*time.Minute)
		cancel()
		if err != nil {
			t.Fatalf("magi-%d never reached epoch 2: %v", n, err)
		}
	}
	elec, err := d.GetElectionGQL(ctx, 1, 2)
	if err != nil {
		t.Fatalf("reading election epoch 2: %v", err)
	}
	for i, w := range elec.Weights {
		if w != params.PoaSeatWeight {
			t.Fatalf("PRECONDITION FAILED: weight[%d]=%d want flat %d — POA INERT", i, w, params.PoaSeatWeight)
		}
	}

	// Use the LAST node so disabling it cannot drop the committee below MinMembers.
	leaver := cfg.Nodes
	acct := d.witnessAccount(leaver)
	full := "hive:" + acct

	bal, err := d.GetAccountBalance(ctx, 1, full)
	if err != nil || bal == nil || bal.HiveConsensus <= 0 {
		t.Fatalf("PRECONDITION FAILED: %s has no readable consensus stake (%v) — it is a seated "+
			"committee member and must have one", full, err)
	}
	staked := bal.HiveConsensus
	t.Logf("%s hive_consensus = %d", full, staked)

	// ---- 1. while still electable, the unstake must be REFUSED ----
	tx1, err := d.ConsensusUnstake(leaver, "1.000")
	if err != nil {
		t.Fatalf("broadcasting first consensus_unstake: %v", err)
	}
	time.Sleep(60 * time.Second)
	st1, _ := d.FindTransactionStatus(ctx, 1, tx1)
	t.Logf("unstake while still seated+electable: tx=%s status=%s", tx1, st1)
	if !strings.EqualFold(st1, "FAILED") {
		t.Errorf("expected the first unstake to be REFUSED while the operator is still electable, "+
			"got status=%q. If it succeeded, the exit-halt is not holding at all.", st1)
	}

	// ---- 2. stand down: stop being electable, which starts the clock ----
	n := disableWitnessEverywhere(t, d, ctx, acct, cfg.Nodes)
	if n == 0 {
		t.Fatalf("no witness rows for %s to disable", acct)
	}
	t.Logf("stood %s down (disabled %d witness rows) — the exit-halt clock should now start", acct, n)

	// PoaExitHaltBlocks is 120 on devnet (~6 min at 3s). Give it generous margin,
	// and poll so a successful release is detected as soon as it happens.
	window := 14 * time.Minute
	t.Logf("waiting up to %v for the exit-halt to lift (PoaExitHaltBlocks=120 blocks ~ 6 min)", window)

	var released bool
	var lastStatus string
	deadline := time.Now().Add(window)
	attempt := 0
	for time.Now().Before(deadline) && !released {
		time.Sleep(90 * time.Second)
		attempt++
		txN, err := d.ConsensusUnstake(leaver, "1.000")
		if err != nil {
			t.Logf("attempt %d: broadcast error %v", attempt, err)
			continue
		}
		time.Sleep(45 * time.Second)
		st, _ := d.FindTransactionStatus(ctx, 1, txN)
		lastStatus = st
		t.Logf("attempt %d: unstake tx=%s status=%s", attempt, txN, st)
		if strings.EqualFold(st, "CONFIRMED") || strings.EqualFold(st, "PROCESSED") || strings.EqualFold(st, "INCLUDED") {
			released = true
		}
	}

	if !released {
		t.Errorf("EXIT-HALT NEVER RELEASED after %v of standing down (last status=%q). A hold that "+
			"never lifts is permanent confiscation of the operator's collateral, not a deterrent — "+
			"and there is no other code path that returns it. D13a passes in this state because it "+
			"only asserts the REFUSAL, so this is the failure D13a cannot see.",
			window, lastStatus)
		return
	}

	t.Logf("CONFIRMED: the exit-halt LIFTED after the operator stood down — the unstake was accepted "+
		"(status=%s). The bond is recoverable, so the halt is a timed deterrent rather than a "+
		"permanent lock.", lastStatus)
}
