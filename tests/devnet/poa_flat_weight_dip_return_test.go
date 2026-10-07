package devnet

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestPoaFlatWeightDipThenReturn (item 14) is TestBondGateActive_DipThenReturn at
// the POA floor. Under stake weight a returning established member re-enters at
// the next election (the min(current, last-ratified) exemption). Under flat weight
// last-ratified weight is 1, so the exemption is below MinStake and the member must
// re-serve the inclusion window (86,400 blocks, 3 days, on mainnet).
//
// Expected RED while the gap exists: "RETURNING ESTABLISHED MEMBER NEVER
// RE-ENTERED" within the polling budget, although it re-staked the full amount.
func TestPoaFlatWeightDipThenReturn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet bond-gate dip-return test in short mode")
	}
	requireDocker(t)

	cfg := regressionConfig()
	if pj := os.Getenv("DEVNET_PROJECT"); pj != "" {
		cfg.ProjectName = pj // so the runner can tear it down by project
	}
	cp := cfg.SysConfigOverrides.ConsensusParams
	// FAST config (feedback_devnet_fast_config_intervals): ~60s/epoch instead of
	// regressionConfig's ~5min/epoch, so the consensus-unstaking maturation
	// (~4 epochs) + the re-stake + the return election all fit one run. The
	// first (slow) run proved the DROP but ran out of wall-clock before the
	// return epoch.
	cp.ElectionInterval = 20
	cp.BondInclusionActivationHeight = 40 // epoch-2 boundary, after epoch-1 committee forms
	// W must exceed the restake→re-entry gap so matured stake CANNOT explain the
	// re-entry (only the min(current,last-ratified) exemption can). 200 blocks ≈
	// 10 epochs ≫ the unstaking-delay + restake + 1-election gap.
	cp.BondInclusionWindowBlocks = 200
	cp.BondInclusionSampleCount = 8
	cp.MaxNewMembersPerElection = 1
	// Grace must comfortably cover the whole dip. Lookback = 4000/20+16 = 216
	// elections, well under the 2048 cap.
	cp.BondInclusionEstablishedGraceBlocks = 4000
	// Item 14: the same scenario under POA (flat weight 1 per seat). The
	// established-member exemption and the floor guard take min(current, last
	// ratified weight) = min(stake, 1) < MinStake, so a returning seat is expected
	// to re-serve the whole inclusion window instead of re-entering at once.
	cp.ConsensusVersionFloorMajor = 0
	cp.ConsensusVersionFloorConsensus = poaDevnetFloor()
	cp.ConsensusVersionFloorEpoch = 1

	ctx, cancel := context.WithTimeout(context.Background(), vfTestBudget(60*time.Minute))
	t.Cleanup(cancel)

	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() { d.Stop() })

	t.Log("starting 7-node devnet, bond gate ACTIVE at 150, W=800, cap=1, grace=1600...")
	if err := d.Start(ctx); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("starting devnet: %v", err)
	}
	if err := d.WaitForBlockProcessing(ctx, 1, 30, 6*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("network never reached block 30: %v", err)
	}

	// Epoch 1: genesis committee (gate inactive below activation height).
	if err := d.waitForElectionEpoch(ctx, 1, 1, 10*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("first election never ratified: %v", err)
	}
	members1, _, err := d.electionMembersWithHeight(ctx, 1, 1)
	if err != nil {
		t.Fatalf("get epoch-1 members: %v", err)
	}
	t.Logf("epoch 1 committee: %d members %v", len(members1), members1)
	if len(members1) < 4 {
		t.Fatalf("epoch-1 committee unexpectedly small: %d (devnet bootstrap problem, not the gate)", len(members1))
	}

	// The dipper: the last witness. stays = everyone else (must never flicker).
	dipper := d.witnessAccount(d.cfg.Nodes)
	stays := make([]string, 0, len(members1)-1)
	foundDipper := false
	for _, m := range members1 {
		if m == dipper {
			foundDipper = true
			continue
		}
		stays = append(stays, m)
	}
	if !foundDipper {
		t.Fatalf("dipper %s not in the epoch-1 committee %v — cannot run the dip scenario", dipper, members1)
	}

	// Epoch 2: the first gated election (grandfather path — already proven by
	// TestBondGateActive_NoResetOfEstablishedCommittee; sanity-assert here).
	if err := d.waitForElectionEpoch(ctx, 1, 2, 12*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("second (gated) election never ratified: %v", err)
	}
	members2, _, err := d.electionMembersWithHeight(ctx, 1, 2)
	if err != nil {
		t.Fatalf("get epoch-2 members: %v", err)
	}
	assertSubset(t, members1, members2, "epoch-2 (gate activation) dropped an established member")

	// ── DIP: full consensus unstake ────────────────────────────────────────
	bhUnstake, epUnstake, _ := d.LocalNodeInfo(ctx, 1)
	txU, err := d.ConsensusUnstake(d.cfg.Nodes, "2000.000")
	if err != nil {
		t.Fatalf("consensus_unstake broadcast: %v", err)
	}
	t.Logf("DIP: %s fully unstaked (tx %s) at ~block %d (epoch %d)", dipper, txU, bhUnstake, epUnstake)

	// From 0.7 the collateral exit-halt holds an electable seat's bond
	// (poa_seats.go poaExitHalt), so this unstake is REFUSED and a seated member
	// cannot dip by unstaking at all. The dip that remains is a slash; a slashed
	// seat that tops up re-serves the inclusion window before it returns
	// (TestPoaSlashedSeatReturnsAfterTopUp, RED at 0.9 by design: item 14,
	// accepted). At such a floor the scenario below never starts: assert the
	// refusal and stop. (It used to wait 24 elections for an exit the protocol
	// forbids, and fail.)
	if vfActiveConsensus(d, ctx) >= 7 {
		assertTxRefused(t, d, ctx, 2, txU)
		t.Logf("exit-halt in force: the seated dipper's full unstake was refused, as required")
		return
	}

	// Poll ratified elections until the dipper is gone (their stake is 0 — a
	// legitimate exit, NOT a reset). The floor guard must NOT resurrect them.
	epochOut, err := d.pollUntilMembership(ctx, t, stays, dipper, false, epUnstake+1, 24, 12*time.Minute)
	if err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("dipper never left the committee after full unstake: %v", err)
	}
	outMembers, _, _ := d.electionMembersWithHeight(ctx, 1, epochOut)
	t.Logf("DIP CONFIRMED: epoch %d committee without %s: %d members %v", epochOut, dipper, len(outMembers), outMembers)

	// ── RETURN: FRESH capital (new L1 deposit), within grace ───────────────
	if _, err := d.Deposit(ctx, d.cfg.Nodes, "2000.000", "hive"); err != nil {
		t.Fatalf("fresh deposit: %v", err)
	}
	// Wait for the deposit to credit the dipper's VSC hive balance.
	depositOk := false
	for end := time.Now().Add(5 * time.Minute); time.Now().Before(end); {
		bal, bErr := d.GetAccountBalance(ctx, 1, "hive:"+dipper)
		if bErr == nil && bal.Hive >= 2_000_000 {
			depositOk = true
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("ctx done waiting for deposit: %v", ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
	if !depositOk {
		t.Fatalf("fresh 2000.000 hive deposit never credited %s", dipper)
	}
	bhRestake, epRestake, _ := d.LocalNodeInfo(ctx, 1)
	txS, err := d.ConsensusStake(d.cfg.Nodes, d.cfg.Nodes, "2000.000")
	if err != nil {
		t.Fatalf("consensus_stake broadcast: %v", err)
	}
	t.Logf("RETURN: %s re-staked 2000.000 FRESH hive (tx %s) at ~block %d (epoch %d)", dipper, txS, bhRestake, epRestake)

	// Poll until the dipper is BACK. With cap=1 and the established exemption,
	// re-entry must be immediate at the first election that sees the stake.
	epochBack, err := d.pollUntilMembership(ctx, t, stays, dipper, true, epRestake+1, 10, 12*time.Minute)
	if err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("RETURNING ESTABLISHED MEMBER NEVER RE-ENTERED (reset!): %v", err)
	}
	backMembers, backBh, err := d.electionMembersWithHeight(ctx, 1, epochBack)
	if err != nil {
		t.Fatalf("get re-entry election: %v", err)
	}
	assertSubset(t, members1, backMembers, "re-entry election lost an original member")
	t.Logf("RETURN CONFIRMED: epoch %d (block %d) committee: %d members %v", epochBack, backBh, len(backMembers), backMembers)

	// Attribution: if the re-entry election is within W−W/samples blocks of the
	// re-stake, the min-over-window matured stake was still 0 (the window holds
	// pre-restake zero samples) — so ONLY the grandfather/established exemption
	// floors can have re-admitted them. Outside that bound the result still
	// proves dip-then-return no-reset, but cannot discriminate the path.
	w := uint64(200)
	discriminant := w - w/8 // 175
	if backBh > bhRestake && backBh-bhRestake < discriminant {
		t.Logf("PROVEN on devnet: dip-then-return re-entry at +%d blocks after re-stake (< %d) — matured stake was still 0; the min(current,last-ratified) exemption re-admitted %s with FRESH capital, no re-wait, churn-cap-exempt; all %d continuously-staked members held their seats throughout",
			backBh-bhRestake, discriminant, dipper, len(stays))
	} else {
		t.Logf("dip-then-return re-entry CONFIRMED (no reset), but at +%d blocks after re-stake the window attribution is inconclusive (>= %d)",
			backBh-bhRestake, discriminant)
	}
}
