package devnet

import (
	"context"
	"testing"
	"time"

	"vsc-node/modules/common/params"
)

// TestPoaLateJoinerDerivesSameRegistry is scenario D3.
//
// poa_seats is NOT part of any merklized state, and the reindex trigger keys off a
// hive_blocks marker, so the collection can be dropped or restored independently
// of chain history. bootstrapPoaSeats guards against that by refusing to seed
// anywhere except the activation transition — "the ratified election whose
// PREDECESSOR was still below the POA line" (poa_seats.go:122-128) — because a
// node that re-seeds from whatever committee it happens to see would silently
// disagree with every peer, with no checkpoint or repair path to catch it.
//
// This exercises that guard on the dangerous path: a node that was NOT running
// during the activation transition and only joins afterwards. It must reconstruct
// the identical registry by replaying history, and must NOT re-bootstrap from the
// current committee.
//
// ★★ WHAT STOPPING A NODE DOES AND DOES NOT DO — corrected after the first run.
// Stopping a node does NOT remove it from the committee and does NOT cost it a
// seat. Election generation reads
// GetWitnessesAtBlockHeight(blk, witnesses.EnabledOnly()) (election-proposer.go:372):
// it filters on the on-chain `enabled` flag, not on liveness, and banSystemEnabled
// is false. So the absent node is still ELECTED and still SEEDED into the founding
// registry while it is down. The first run of this test logged "activation happened
// without magi-5" and that phrasing was wrong — magi-5 was in the 5-seat registry
// the whole time.
//
// This test therefore proves REPLAY DETERMINISM, and must not be cited for the
// "excluded, then rejoins" case, which never occurs here. Exclusion requires
// announcing a BELOW-FLOOR VERSION — that is TestPoaLaggardExcludedFromFoundingCohort.
//
// The distinction is operationally load-bearing: an operator whose node is DOWN at
// the activation epoch keeps its founding seat, while an operator whose node is UP
// but NOT UPGRADED loses it permanently. Those look identical from the outside.
func TestPoaLateJoinerDerivesSameRegistry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	if cfg.SysConfigOverrides.ConsensusParams == nil {
		cfg.SysConfigOverrides.ConsensusParams = &params.ConsensusParams{}
	}
	// ★ FloorEpoch MUST be non-zero.
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorMajor = 0
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = 7
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1

	d, ctx := startDevnetNoKey(t, cfg, 45*time.Minute)

	late := cfg.Nodes // magi-5 will miss the activation

	// Take the late node down BEFORE activation so it is absent for the transition.
	if err := d.StopNode(ctx, late); err != nil {
		t.Fatalf("stopping magi-%d before activation: %v", late, err)
	}
	t.Logf("stopped magi-%d before the activation transition", late)

	// Let the remaining nodes cross the floor and reach flat weight (epoch 2).
	for n := 1; n < late; n++ {
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
	reference, err := d.poaSeats(ctx, 1)
	if err != nil || len(reference) == 0 {
		t.Fatalf("PRECONDITION FAILED: no reference registry on magi-1 (%v)", err)
	}
	refFp := seatFingerprint(reference)
	t.Logf("activation completed while magi-%d was DOWN. reference registry (%d seats): %s",
		late, len(reference), refFp)

	// Turn the first run's incidental observation into a CHECKED fact: an offline
	// node keeps its seat, because eligibility is the on-chain `enabled` flag and
	// not liveness. If this ever stops being true, the operational guidance above
	// (down != excluded) is wrong and must be revised.
	lateAcct := d.witnessAccount(late)
	offlineStillSeated := false
	for _, s := range reference {
		if s.Account == lateAcct {
			offlineStillSeated = true
			break
		}
	}
	if !offlineStillSeated {
		t.Errorf("magi-%d (%s) was NOT seeded while offline. That contradicts EnabledOnly()-based "+
			"eligibility and would mean being briefly DOWN at the activation epoch permanently costs "+
			"an operator its founding seat — a materially harsher operational rule than documented.",
			late, lateAcct)
	} else {
		t.Logf("CONFIRMED: %s kept its founding seat despite being offline for the whole transition "+
			"(eligibility is the on-chain enabled flag, not liveness)", lateAcct)
	}

	// ---- bring the late node back and let it catch up ----
	if err := d.StartNode(ctx, late); err != nil {
		t.Fatalf("restarting magi-%d: %v", late, err)
	}
	t.Logf("restarted magi-%d; it must REPLAY to the same registry, not re-seed", late)

	deadline := time.Now().Add(15 * time.Minute)
	var lateFp string
	var lateSeats []poaSeatDoc
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Second)
		s, err := d.poaSeats(ctx, late)
		if err != nil {
			continue
		}
		lateSeats, lateFp = s, seatFingerprint(s)
		if lateFp == refFp {
			break
		}
	}

	if lateFp == refFp {
		t.Logf("magi-%d rebuilt the IDENTICAL registry by replay (%d seats)", late, len(lateSeats))
		return
	}

	if len(lateSeats) == 0 {
		t.Errorf("magi-%d has an EMPTY registry after rejoining. It missed the activation transition "+
			"and correctly refused to re-seed, but it also never reconstructed the registry from "+
			"history — so its seat gate stays inert and it silently disagrees with its peers about "+
			"who may be elected.", late)
		return
	}
	t.Errorf("REGISTRY DIVERGENCE on the late joiner. magi-%d re-derived a DIFFERENT registry, which "+
		"is the silent fork this guard exists to prevent.\n  peers:      %s\n  magi-%d: %s",
		late, refFp, late, lateFp)
}
