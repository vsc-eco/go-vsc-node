package state_engine

import (
	"testing"

	"vsc-node/modules/common/params"
	"vsc-node/modules/db/vsc/consensus_state"
)

// N4: the tally is THORChain's solvency consensus by election weight: 2/3
// insolvent halts, 2/3 solvent lifts; one vote per member per checkpoint,
// counted only inside the checkpoint's interval.
func TestN4_TallyBtcSolvencyVote(t *testing.T) {
	every := params.BTC_SOLVENCY_CHECK_EVERY
	c := 10 * every
	members := []string{"a", "b", "c"}
	ins := btcSolvencyVote{Checkpoint: c, Insolvent: true}
	vote := func(st consensus_state.BtcSolvencyState, who string, v btcSolvencyVote, h uint64, w []uint64) (consensus_state.BtcSolvencyState, bool) {
		t.Helper()
		return tallyBtcSolvencyVote(st, who, v, h, members, w)
	}

	st, ok := vote(consensus_state.BtcSolvencyState{}, "a", ins, c+5, nil)
	if !ok || st.Halted {
		t.Fatalf("one of three: counted=%v halted=%v", ok, st.Halted)
	}
	if _, ok := vote(st, "a", ins, c+6, nil); ok {
		t.Fatal("a second vote by the same member counted")
	}
	if _, ok := vote(st, "mallory", ins, c+6, nil); ok {
		t.Fatal("a non-member's vote counted")
	}
	if _, ok := vote(st, "b", btcSolvencyVote{Checkpoint: c + 1, Insolvent: true}, c+6, nil); ok {
		t.Fatal("a vote for a height that is not a checkpoint counted")
	}
	if _, ok := vote(st, "b", ins, c+every, nil); ok {
		t.Fatal("a vote after its checkpoint's interval counted")
	}
	if _, ok := vote(st, "b", btcSolvencyVote{Checkpoint: c + every, Insolvent: true}, c+6, nil); ok {
		t.Fatal("a vote for a future checkpoint counted")
	}
	st, ok = vote(st, "b", ins, c+7, nil)
	if !ok || !st.Halted || st.HaltHeight != c+7 {
		t.Fatalf("two of three insolvent: %+v", st)
	}

	// Halted: insolvent votes keep it, 2/3 solvent at a later checkpoint lifts it.
	c2 := c + every
	st, _ = vote(st, "a", btcSolvencyVote{Checkpoint: c2}, c2+1, nil)
	if !st.Halted || len(st.Votes.Insolvent) != 0 || st.Votes.Checkpoint != c2 {
		t.Fatalf("a new checkpoint must start a fresh count and keep the halt: %+v", st)
	}
	if _, ok := vote(st, "c", ins, c2+2, nil); ok {
		t.Fatal("a vote for the earlier checkpoint counted after a later one began")
	}
	st, _ = vote(st, "c", btcSolvencyVote{Checkpoint: c2}, c2+2, nil)
	if st.Halted || st.HaltHeight != 0 {
		t.Fatalf("two of three solvent: %+v", st)
	}

	// Weight, not head count: a member with 5 of 7 weight decides alone.
	st, _ = vote(consensus_state.BtcSolvencyState{}, "a", ins, c+1, []uint64{5, 1, 1})
	if !st.Halted {
		t.Fatal("5/7 of the weight insolvent did not halt")
	}
	st, _ = vote(consensus_state.BtcSolvencyState{}, "b", ins, c+1, []uint64{5, 1, 1})
	st, _ = vote(st, "c", ins, c+1, []uint64{5, 1, 1})
	if st.Halted {
		t.Fatal("2/7 of the weight halted")
	}
	// No weight at all never halts; the governance switch ignores votes.
	if st, _ := vote(consensus_state.BtcSolvencyState{}, "a", ins, c+1, []uint64{0, 0, 0}); st.Halted {
		t.Fatal("zero total weight halted")
	}
	if _, ok := vote(consensus_state.BtcSolvencyState{WatchOff: true}, "a", ins, c+1, nil); ok {
		t.Fatal("a vote counted while the watch is off")
	}
}
