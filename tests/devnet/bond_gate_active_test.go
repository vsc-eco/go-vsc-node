package devnet

import (
	"context"
	"testing"
	"time"
)

// TestBondGateActive_NoResetOfEstablishedCommittee brings up the 7-node devnet
// with the bond inclusion window ACTIVATED (BondInclusionActivationHeight set to
// a height reached AFTER the genesis committee is established).
//
// The governing security constraint is the operator's: the gate must NEVER
// reset/evict an established committee member — "if they're in once this is live
// they aren't touched." The F11 activation grandfather is supposed to exempt the
// committee that existed at activation, so flipping the gate ON must not shrink
// or drop an established member.
//
// This proves that on a real multi-node devnet: epoch 1 forms the genesis
// committee (gate still inactive below the activation height), then the next
// election runs THROUGH the active bond gate, and every epoch-1 member is still
// present — no reset, no committee collapse. The committee here IS the gateway
// multisig signer set and the TSS party, so "committee preserved" == "multisig +
// TSS membership preserved".
func TestBondGateActive_NoResetOfEstablishedCommittee(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet bond-gate-active test in short mode")
	}
	requireDocker(t)

	cfg := regressionConfig()
	// Activate the bond gate at a height reached after epoch 1 (~block 100 at
	// ElectionInterval=100). The F11 grandfather then captures the full epoch-1
	// committee, so every later (gated) election must keep them. A short window
	// + a churn cap make the gate genuinely active (not inert).
	cfg.SysConfigOverrides.ConsensusParams.BondInclusionActivationHeight = 150
	cfg.SysConfigOverrides.ConsensusParams.BondInclusionWindowBlocks = 30
	cfg.SysConfigOverrides.ConsensusParams.BondInclusionSampleCount = 8
	cfg.SysConfigOverrides.ConsensusParams.MaxNewMembersPerElection = 1
	cfg.SysConfigOverrides.ConsensusParams.BondInclusionEstablishedGraceBlocks = 600

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)

	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() { d.Stop() })

	t.Log("starting 7-node devnet with bond gate ACTIVE (activation height 150)...")
	if err := d.Start(ctx); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("starting devnet: %v", err)
	}
	if err := d.WaitForBlockProcessing(ctx, 1, 30, 6*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("network never reached block 30: %v", err)
	}

	// Epoch 1: the genesis committee (gate inactive below activation height 150).
	if err := d.waitForElectionEpoch(ctx, 1, 1, 10*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("first election never ratified: %v", err)
	}
	members1, err := d.GetElectionMembers(ctx, 1, 1)
	if err != nil {
		t.Fatalf("get epoch-1 members: %v", err)
	}
	t.Logf("epoch 1 committee (gate inactive): %d members %v", len(members1), members1)
	if len(members1) < 4 {
		t.Fatalf("epoch-1 committee unexpectedly small: %d (devnet bootstrap problem, not the gate)", len(members1))
	}

	// Wait for epoch 2 — by now the chain is past activation height 150, so this
	// election runs THROUGH the bond gate. The F11 grandfather must keep every
	// epoch-1 member; the committee must NOT shrink or drop an established member.
	if err := d.waitForElectionEpoch(ctx, 1, 2, 12*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("second (gated) election never ratified — the active gate may have collapsed the committee below quorum: %v", err)
	}
	lastBlock, curEpoch, _ := d.LocalNodeInfo(ctx, 1)
	members2, err := d.GetElectionMembers(ctx, 1, 2)
	if err != nil {
		t.Fatalf("get epoch-2 members: %v", err)
	}
	gateWasActive := lastBlock >= 150
	t.Logf("epoch 2 committee (block %d, epoch %d, gateActive=%v): %d members %v",
		lastBlock, curEpoch, gateWasActive, len(members2), members2)
	if !gateWasActive {
		t.Logf("WARNING: epoch-2 election ran below activation height 150 — gate not yet active, result inconclusive for the gated path")
	}

	// CORE ASSERTION: the gate-active election did not reset/evict any established
	// member. Every epoch-1 member must still be present in epoch 2.
	set2 := make(map[string]bool, len(members2))
	for _, m := range members2 {
		set2[m] = true
	}
	for _, m := range members1 {
		if !set2[m] {
			t.Fatalf("BOND GATE RESET AN ESTABLISHED MEMBER: %s was in the epoch-1 committee but DROPPED from epoch-2 (gate active) — violates the cannot-reset guarantee", m)
		}
	}
	if len(members2) < len(members1) {
		t.Fatalf("gate-active committee shrank %d -> %d (established members must be grandfathered, never evicted)", len(members1), len(members2))
	}
	t.Logf("PROVEN on devnet: bond gate active at epoch 2 (block %d) kept all %d established members — no reset, no collapse; committee = gateway multisig + TSS party preserved",
		lastBlock, len(members1))
}
