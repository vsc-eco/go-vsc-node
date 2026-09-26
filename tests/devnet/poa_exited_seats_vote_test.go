package devnet

import (
	"context"
	"os"
	"testing"
	"time"

	"vsc-node/modules/common/params"
)

// TestPoaExitedSeatsStillDecideAdmission is scenario POA-3.
//
// The admission electorate is every seat ever admitted: GetSeatsAtHeight filters on
// admitted_height only (poaseats.go), and there is no removal op (poa_admission.go:41).
// So a seat that has LEFT the committee (exit_height set) keeps both its weight in
// the ceil(2/3) threshold and its right to vote.
//
// Setup: 5 bootstrap seats, then 2 of them stand down (witness disabled, which is
// what leaving actually is) and the next election records their exit. 3 live seats
// remain, which is still MinMembers on devnet, so the seat gate keeps applying.
//
// This test asserts the SAFE behaviour and therefore FAILS on a build that has POA-3:
//
//	A. the 3 live seats, voting unanimously, can admit a candidate
//	   (today: the bar stays ceil(2/3 x 5) = 4, so the live set is deadlocked);
//	B. a vote from an EXITED seat must not count
//	   (today: it is accepted, and it is the vote that crosses the bar).
func TestPoaExitedSeatsStillDecideAdmission(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	if lv := os.Getenv("POA_DEVNET_LOGLEVEL"); lv != "" {
		cfg.LogLevel = lv // e.g. "error,tss=warn,bp=trace,se=debug" to see block production
	}
	if cfg.SysConfigOverrides.ConsensusParams == nil {
		cfg.SysConfigOverrides.ConsensusParams = &params.ConsensusParams{}
	}
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorMajor = 0
	floor := poaDevnetFloor()
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = floor
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1
	// Short exit-halt so the 0.9.0 departure window (10 exit-halts,
	// params.PoaVoteDepartureHalts) fits in the run: 10 x 15 = 150 blocks.
	cfg.SysConfigOverrides.ConsensusParams.PoaExitHaltBlocks = 15

	d, ctx := startDevnetNoKey(t, cfg, 75*time.Minute)

	for n := 1; n <= cfg.Nodes; n++ {
		nctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		err := d.waitForElectionEpoch(nctx, n, 2, 10*time.Minute)
		cancel()
		if err != nil {
			t.Fatalf("magi-%d never reached epoch 2: %v", n, err)
		}
	}

	// ---- PRECONDITION: POA genuinely active with 5 seats ----
	base, err := d.GetElectionGQL(ctx, 1, 2)
	if err != nil {
		t.Fatalf("reading election epoch 2: %v", err)
	}
	for i, w := range base.Weights {
		if w != params.PoaSeatWeight {
			t.Fatalf("PRECONDITION FAILED: weight[%d]=%d want flat %d, POA inert", i, w, params.PoaSeatWeight)
		}
	}
	seats0 := pfMustSeats(t, d, ctx, 1)
	if len(seats0) != cfg.Nodes {
		t.Fatalf("PRECONDITION FAILED: want %d bootstrap seats, registry has %d: %s", cfg.Nodes, len(seats0), pfSeatFingerprint(seats0))
	}

	// ---- two seats stand down ----
	leavers := []int{4, 5}
	for _, w := range leavers {
		if n := poaDisableWitnessAllNodes(t, d, ctx, d.witnessAccount(w), cfg.Nodes); n == 0 {
			t.Fatalf("disabling %s matched no witness rows", d.witnessAccount(w))
		}
	}
	// the NEXT election excludes them and records exit_height; allow one more for propagation
	elec := waitElections(t, d, ctx, base.Epoch, 2, 15*time.Minute)
	if len(elec.Members) != cfg.Nodes-len(leavers) {
		t.Fatalf("PRECONDITION FAILED: committee at epoch %d has %d members, want %d (leavers still elected?)",
			elec.Epoch, len(elec.Members), cfg.Nodes-len(leavers))
	}
	seats1 := pfMustSeats(t, d, ctx, 1)
	exited := map[string]bool{}
	for _, s := range seats1 {
		if s.ExitHeight > 0 {
			exited[s.Account] = true
		}
	}
	for _, w := range leavers {
		if !exited[d.witnessAccount(w)] {
			t.Fatalf("PRECONDITION FAILED: %s left the committee but has no exit_height: %s",
				d.witnessAccount(w), pfSeatFingerprint(seats1))
		}
	}
	total := uint64(len(seats1))
	required := total - total/3
	live := cfg.Nodes - len(leavers)
	t.Logf("registry %d seats (%d exited), live seats %d, admission threshold computed over the registry: %d",
		total, len(exited), live, required)

	// At 0.9.0 a departed seat keeps its vote for the departure window (ten
	// exit-halts; it may be back), so the live set is judged only after the
	// window has run out. Waiting on the old rules too keeps the scenario
	// identical: there the exited seats vote forever, so the wait changes nothing.
	var maxExit uint64
	for _, s := range seats1 {
		if s.ExitHeight > maxExit {
			maxExit = s.ExitHeight
		}
	}
	halt := uint64(15)  // PoaExitHaltBlocks override above
	window := 10 * halt // departure window: params.PoaVoteDepartureHalts exit-halts
	target := maxExit + window + 5
	for {
		h, err := d.getLastProcessedBlock(ctx, 1)
		if err == nil && h >= target {
			t.Logf("departure window over: head %d >= exit %d + window %d", h, maxExit, window)
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for head %d (exit %d + window %d): %v", target, maxExit, window, ctx.Err())
		case <-time.After(10 * time.Second):
		}
	}

	candidate := "magi.poa3cand"
	ubo := "ubo-poa3-001"

	// ---- A: the live set votes unanimously ----
	for v := 1; v <= live; v++ {
		if _, err := d.pfAdmitVote(v, candidate, ubo); err != nil {
			t.Fatalf("admit_vote from magi-%d: %v", v, err)
		}
	}
	time.Sleep(60 * time.Second)
	seatedAfterLive := false
	for _, s := range pfMustSeats(t, d, ctx, 1) {
		if s.Account == candidate {
			seatedAfterLive = true
		}
	}
	if !seatedAfterLive {
		t.Errorf("POA-3 DEADLOCK REPRODUCED: all %d live seats voted for %s and it was NOT admitted; "+
			"the bar is still %d of %d because the %d exited seats count forever", live, candidate, required, total, len(exited))
	} else {
		t.Logf("live seats admitted %s on %d votes", candidate, live)
		if floor < 9 {
			return // B below needs an unseated candidate
		}
		// 0.9.0: an exited seat past its departure window must not count. One
		// live vote plus one exited vote is 2 = the live bar (3 live seats), so
		// the candidate is admitted only if the exited vote was counted.
		cand2, ubo2 := "magi.poa3cand2", "ubo-poa3-002"
		if _, err := d.pfAdmitVote(leavers[0], cand2, ubo2); err != nil {
			t.Fatalf("admit_vote from exited magi-%d: %v", leavers[0], err)
		}
		time.Sleep(30 * time.Second)
		if _, err := d.pfAdmitVote(1, cand2, ubo2); err != nil {
			t.Fatalf("admit_vote from magi-1: %v", err)
		}
		time.Sleep(60 * time.Second)
		for _, s := range pfMustSeats(t, d, ctx, 1) {
			if s.Account == cand2 {
				t.Errorf("the vote of EXITED seat %s (halt run out) was counted: %s admitted on 1 live + 1 exited vote",
					d.witnessAccount(leavers[0]), cand2)
				return
			}
		}
		t.Logf("exited seat's vote was not counted: %s not admitted on 1 live + 1 exited vote", cand2)
		return
	}

	// ---- B: one EXITED seat votes ----
	if _, err := d.pfAdmitVote(leavers[0], candidate, ubo); err != nil {
		t.Fatalf("admit_vote from exited magi-%d: %v", leavers[0], err)
	}
	time.Sleep(60 * time.Second)
	for _, s := range pfMustSeats(t, d, ctx, 1) {
		if s.Account == candidate {
			t.Errorf("POA-3 REPRODUCED: the vote of EXITED seat %s (exit recorded) was counted and crossed the bar; "+
				"%s admitted on %d live + 1 exited votes", d.witnessAccount(leavers[0]), candidate, live)
			return
		}
	}
	t.Logf("exited seat's vote did not admit %s", candidate)
}
