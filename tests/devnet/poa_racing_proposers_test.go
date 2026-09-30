package devnet

import (
	"context"
	"fmt"
	"testing"
	"time"

	"vsc-node/modules/common/params"
)

// TestPoaRacingProposers is scenario D21: two committee members propose
// DIFFERENT consensus-version targets inside the same window.
//
// executeProposeConsensusVersion (state-processing/consensus_version.go) keeps
// ONE candidate slot per proposer, and a re-propose replaces only your own.
// There is deliberately no "must be strictly higher than the standing proposal"
// rule, precisely so a junk target cannot block a legitimate one.
//
// Two properties matter, and they are different in kind:
//
//  1. DETERMINISM. All 5 nodes must reach the identical outcome. Version
//     activation drives every consensus gate, so nodes disagreeing about which
//     proposal won is a fork, not a cosmetic difference. This is the assertion
//     that would catch a map-iteration or last-writer-wins bug in the candidate
//     slots.
//
//  2. NON-BLOCKING. A junk high target from one proposer must not prevent the
//     legitimate target from activating. If it could, any single committee
//     member could indefinitely veto version upgrades for the whole network by
//     spamming an unreachable version.
//
// ★ The floor is deliberately NOT pinned to 7 here. This test needs the chain to
// START below the POA line so that a version proposal has somewhere to go; if
// the floor already pinned 0.7.0 the proposals would be no-ops and both
// assertions would be vacuous. It pins the floor at consensus 5 (POA inert) and
// then races proposals at 7.
func TestPoaRacingProposers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	if cfg.SysConfigOverrides.ConsensusParams == nil {
		cfg.SysConfigOverrides.ConsensusParams = &params.ConsensusParams{}
	}
	// Start BELOW the POA line: consensus 5 is chain-active, 7 is the target the
	// proposals race toward.
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorMajor = 0
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = 5
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1

	d, ctx := startDevnetNoKey(t, cfg, 45*time.Minute)

	for n := 1; n <= cfg.Nodes; n++ {
		nctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		err := d.waitForElectionEpoch(nctx, n, 1, 8*time.Minute)
		cancel()
		if err != nil {
			t.Fatalf("magi-%d never ingested an election: %v", n, err)
		}
	}

	// ---- PRECONDITION: POA is INERT, so a rise to 7 is a real state change ----
	start, err := d.GetElectionGQL(ctx, 1, 1)
	if err != nil {
		t.Fatalf("reading election epoch 1: %v", err)
	}
	if ccAllFlat(start.Weights) {
		t.Fatalf("PRECONDITION FAILED: epoch 1 already carries flat weights %v, so POA is ALREADY "+
			"active and a proposal to 7 changes nothing. Both assertions below would be vacuous",
			start.Weights)
	}
	t.Logf("PRECONDITION OK: POA inert at epoch %d (stake weights %v); racing proposals to 0.7.0",
		start.Epoch, start.Weights)

	epoch, err := d.currentEpoch(ctx, 1)
	if err != nil {
		t.Fatalf("currentEpoch: %v", err)
	}

	// ---- THE RACE ----
	//
	// Witness 1 proposes the LEGITIMATE target (consensus 7). Witness 2 proposes
	// a JUNK target (consensus 99) that no binary can satisfy, in the same
	// window. Junk is sent SECOND so that a naive last-writer-wins implementation
	// would let it displace the legitimate proposal.
	legitEpoch := epoch + 2
	if _, err := d.proposeConsensusVersion(1, 0, 7, legitEpoch); err != nil {
		t.Fatalf("witness 1 proposing the legitimate 0.7.0 target: %v", err)
	}
	t.Logf("witness 1 proposed consensus 7 at activation_epoch %d", legitEpoch)

	if _, err := d.proposeConsensusVersion(2, 0, 99, legitEpoch); err != nil {
		t.Fatalf("witness 2 proposing the junk 0.99.0 target: %v", err)
	}
	t.Logf("witness 2 proposed junk consensus 99 at the same activation_epoch %d", legitEpoch)

	// ---- ASSERTION 2: the junk proposal must not block the legitimate one ----
	//
	// Flat weight appearing is the on-chain proof that 0.7.0 actually activated.
	activated := false
	var seenEpoch uint64
	var seenWeights []uint64
	deadline := time.Now().Add(18 * time.Minute)
	for time.Now().Before(deadline) {
		cur, err := d.currentEpoch(ctx, 1)
		if err == nil && cur > epoch {
			elec, err := d.GetElectionGQL(ctx, 1, cur)
			if err == nil {
				seenEpoch, seenWeights = elec.Epoch, elec.Weights
				if ccAllFlat(elec.Weights) {
					activated = true
					break
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context ended while waiting for activation: %v", ctx.Err())
		case <-time.After(20 * time.Second):
		}
	}

	if !activated {
		t.Errorf("0.7.0 did NOT activate within 18 minutes while a junk 0.99.0 proposal stood. "+
			"Last seen epoch %d weights=%v. A single committee member able to park an "+
			"unsatisfiable target would hold a unilateral veto over every future version upgrade",
			seenEpoch, seenWeights)
	} else {
		t.Logf("NOT BLOCKED: 0.7.0 activated at epoch %d (flat weights %v) despite the standing "+
			"junk 0.99.0 proposal", seenEpoch, seenWeights)
	}

	// ---- ASSERTION 1: every node reached the IDENTICAL outcome ----
	//
	// Read the same epoch on all 5 nodes and compare members, weights and total.
	// A divergence here is a fork.
	target := seenEpoch
	if target == 0 {
		target, _ = d.currentEpoch(ctx, 1)
	}
	var wantMembers, wantWeights string
	for n := 1; n <= cfg.Nodes; n++ {
		nctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
		err := d.waitForElectionEpoch(nctx, n, target, 6*time.Minute)
		cancel()
		if err != nil {
			t.Errorf("magi-%d never reached epoch %d: %v", n, target, err)
			continue
		}
		elec, err := d.GetElectionGQL(ctx, n, target)
		if err != nil {
			t.Errorf("magi-%d reading epoch %d: %v", n, target, err)
			continue
		}
		gotM := fmt.Sprint(ccBareAccounts(elec.Members))
		gotW := fmt.Sprint(elec.Weights)
		t.Logf("magi-%d epoch %d: members=%s weights=%s total=%d", n, target, gotM, gotW, elec.TotalWeight)
		if n == 1 {
			wantMembers, wantWeights = gotM, gotW
			continue
		}
		if gotM != wantMembers || gotW != wantWeights {
			t.Errorf("PROPOSAL RACE DIVERGENCE at epoch %d between magi-1 and magi-%d — this is a "+
				"fork, not a cosmetic difference\n  magi-1: members=%s weights=%s\n  magi-%d: members=%s weights=%s",
				target, n, wantMembers, wantWeights, n, gotM, gotW)
		}
	}
}
