package devnet

import (
	"context"
	"testing"
	"time"

	"vsc-node/modules/common/params"
)

// TestPoaAdmissionRoundTrip is scenario D6, and it answers a question that must
// be answered before POA is switched on anywhere: CAN A NEW SEAT EVER BE ADMITTED?
//
// D19 proved a duplicate admission is refused. D20 proved a below-quorum
// admission is refused. Both are NEGATIVE results, and a system that refuses
// EVERYTHING passes both of them. If admission never actually works, POA is a
// one-way door: the founding cohort is frozen forever, nobody can ever join, and
// the only remedy is a hard fork. That is a strictly worse outcome than not
// activating POA at all, and nothing so far distinguishes the two cases.
//
// This is therefore the POSITIVE control for the entire admission mechanism.
func TestPoaAdmissionRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	if cfg.SysConfigOverrides.ConsensusParams == nil {
		cfg.SysConfigOverrides.ConsensusParams = &params.ConsensusParams{}
	}
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorMajor = 0
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = 7
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1

	d, ctx := startDevnetNoKey(t, cfg, 45*time.Minute)

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
	before, err := d.poaSeats(ctx, 1)
	if err != nil || len(before) == 0 {
		t.Fatalf("PRECONDITION FAILED: no registry to admit into (%v)", err)
	}
	total := uint64(len(before))
	required := total - total/3 // ceil(2/3), beneficiary excluded
	t.Logf("registry has %d seats; admission threshold is %d votes", len(before), required)

	// A candidate that is NOT already a seat and NOT one of the witness accounts.
	const candidate = "magi.joiner"
	const uboId = "ubo-joiner-0001"
	for _, s := range before {
		if s.Account == candidate {
			t.Fatalf("PRECONDITION FAILED: %s already holds a seat", candidate)
		}
	}

	// Vote from ENOUGH seats to cross the threshold. The candidate is not a seat,
	// so the electorate is the full registry.
	voters := int(required)
	t.Logf("casting %d admit_vote(s) for %s (ubo=%s)", voters, candidate, uboId)
	for v := 1; v <= voters; v++ {
		if _, err := d.admitVote(v, candidate, uboId); err != nil {
			t.Fatalf("admit_vote from magi-%d: %v", v, err)
		}
		time.Sleep(2 * time.Second)
	}

	// ---- the seat must actually appear, on EVERY node ----
	var seated bool
	deadline := time.Now().Add(6 * time.Minute)
	for time.Now().Before(deadline) && !seated {
		time.Sleep(15 * time.Second)
		after, err := d.poaSeats(ctx, 1)
		if err != nil {
			continue
		}
		for _, s := range after {
			if s.Account == candidate {
				seated = true
				t.Logf("SEAT ADMITTED: %s at height %d (bootstrap=%v, ubo=%q)",
					s.Account, s.AdmittedHeight, s.Bootstrap, s.UboId)
				break
			}
		}
	}
	if !seated {
		final, _ := d.poaSeats(ctx, 1)
		t.Fatalf("ADMISSION NEVER SUCCEEDED. %d votes cast against a threshold of %d, yet %s is not "+
			"in the registry: %s\n\nThis is the one-way-door failure: if no new seat can EVER be "+
			"admitted, the founding cohort is permanent and POA cannot be operated. D19/D20 both pass "+
			"in this state because they only assert that admission is REFUSED.",
			voters, required, candidate, seatFingerprint(final))
	}

	// ---- and it must be identical on every node (it is consensus state) ----
	want := ""
	for n := 1; n <= cfg.Nodes; n++ {
		s, err := d.poaSeats(ctx, n)
		if err != nil {
			t.Errorf("magi-%d poa_seats: %v", n, err)
			continue
		}
		fp := seatFingerprint(s)
		found := false
		for _, x := range s {
			if x.Account == candidate {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("magi-%d does NOT have the admitted seat %s — the registry has DIVERGED: %s",
				n, candidate, fp)
		}
		if n == 1 {
			want = fp
		} else if fp != want {
			t.Errorf("REGISTRY DIVERGENCE after admission, magi-1 vs magi-%d\n  %s\n  %s", n, want, fp)
		}
	}
	if len(before)+1 != len(mustSeats(t, d, ctx, 1)) {
		t.Errorf("registry grew by more than the single admitted seat")
	}
	t.Logf("CONFIRMED: admission works end to end and is identical across all %d nodes. POA is NOT a "+
		"one-way door.", cfg.Nodes)
}

func mustSeats(t *testing.T, d *Devnet, ctx context.Context, node int) []poaSeatDoc {
	t.Helper()
	s, err := d.poaSeats(ctx, node)
	if err != nil {
		t.Fatalf("poa_seats on magi-%d: %v", node, err)
	}
	return s
}
