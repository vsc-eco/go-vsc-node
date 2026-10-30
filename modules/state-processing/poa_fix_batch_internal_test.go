package state_engine

import (
	"strings"
	"testing"

	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/poaseats"
	"vsc-node/modules/governance"
	ledgerSystem "vsc-node/modules/ledger-system"
)

// The 0.9.0 POA fix batch. Every rule here was proven wrong on the live testnet
// (POA-5, H-1, POA-7, POA-3 in the campaign report), and every test pairs the
// fixed behaviour at 0.9.0 with the original behaviour below it, because the
// testnet ran 0.7.0 with the original rules and must replay identically.

// recordingLedger stands in for the ledger session: only ConsensusUnstake is
// reachable in these paths, and reaching it means the unstake passed every hold.
type recordingLedger struct {
	ledgerSystem.LedgerSession
	unstakes int
}

func (r *recordingLedger) ConsensusUnstake(ledgerSystem.ConsensusParams) ledgerSystem.LedgerResult {
	r.unstakes++
	return ledgerSystem.LedgerResult{Ok: true, Msg: "ok"}
}

func delegatedUnstake(from, to string, height uint64) *TxConsensusUnstake {
	return &TxConsensusUnstake{
		Self:   TxSelf{BlockHeight: height, TxId: "tx-unstake", RequiredAuths: []string{from}},
		From:   from,
		To:     to,
		Amount: "1.000",
		Asset:  "hive",
		NetId:  systemconfig.MocknetConfig().NetId(),
	}
}

// ★ POA-5. A delegator (here an operator's own alt) unstaking from a SEATED node
// debits that node's bond. At 0.9.0 the node's exit-halt applies and the unstake
// is refused before the ledger is touched. Live testnet (0.7.0): e4ff7924 was
// accepted and later paid 50 HIVE out from under the seated node.
func TestPoa5_DelegatorUnstakeFromHaltedNodeIsRefusedAt090(t *testing.T) {
	se, seats, _ := poaEnv(t, 9)
	seats.seed("node", "ubo-n", 10, 100) // seated, no exit: halted
	led := &recordingLedger{}

	res := delegatedUnstake("hive:alt", "hive:node", 200).ExecuteTx(se, led, nil, nil, "")

	if res.Success || led.unstakes != 0 {
		t.Fatalf("delegator unstake from a halted seat went through (success=%v, ledger calls=%d): collateral backing a seat left while it holds keys", res.Success, led.unstakes)
	}
	if !strings.Contains(res.Ret, "node you delegated to") {
		t.Fatalf("refused, but not by the node's halt: %q", res.Ret)
	}
}

// Control: below 0.9.0 the original rule stands (only the signer is tested), so
// history replays byte for byte. This is the live POA-5 behaviour.
func TestPoa5_Below090TheOriginalSignerOnlyRuleStands(t *testing.T) {
	se, seats, _ := poaEnv(t, 7)
	seats.seed("node", "ubo-n", 10, 100)
	led := &recordingLedger{}

	res := delegatedUnstake("hive:alt", "hive:node", 200).ExecuteTx(se, led, nil, nil, "")

	if led.unstakes != 1 || !res.Success {
		t.Fatalf("below 0.9.0 the delegated unstake must still reach the ledger (replay-identical); success=%v calls=%d ret=%q", res.Success, led.unstakes, res.Ret)
	}
}

// Positive control: a delegator of a node WITHOUT a seat is not held at 0.9.0.
// The fix must not freeze ordinary delegations.
func TestPoa5_DelegatorOfUnseatedNodeStillUnstakesAt090(t *testing.T) {
	se, _, _ := poaEnv(t, 9)
	led := &recordingLedger{}

	res := delegatedUnstake("hive:alt", "hive:plainnode", 200).ExecuteTx(se, led, nil, nil, "")

	if led.unstakes != 1 || !res.Success {
		t.Fatalf("a delegator of an unseated node was held at 0.9.0: success=%v calls=%d ret=%q", res.Success, led.unstakes, res.Ret)
	}
}

// ★ POA-7. At 0.9.0 an expired admission re-opens as a fresh round on the next
// vote, and only votes cast in the new round count. Live testnet (0.7.0): the
// pair (magi.test9, ubo-poa-2026-mc) was barred forever after expiry.
func TestPoa7_ExpiredAdmissionReopensAsAFreshRoundAt090(t *testing.T) {
	se, seats, gov := admitEnv(t, 9, "alice", "bob", "carol", "dave")
	p := admitPayload(t, "newop", "ubo-new")
	id := governance.AdmitSeatProposalID("newop", "ubo-new")

	se.handleAdmitVote(p, "alice", "tx-a", 100)
	se.handleAdmitVote(p, "bob", "tx-b", 101)
	// mocknet window 120: carol's vote finds round 1 over and starts round 2 itself.
	se.handleAdmitVote(p, "carol", "tx-c", 220)
	if gov.proposals[id].Status != string(governance.StatusOpen) || gov.proposals[id].CreationBlock != 220 {
		t.Fatalf("the late vote did not start a new round at 220: status %q creation %d",
			gov.proposals[id].Status, gov.proposals[id].CreationBlock)
	}
	// dave joins round 2: carol + dave = 2 of 4, below the bar of 3. alice and bob
	// voted in round 1 only; if their votes carried over this would admit.
	se.handleAdmitVote(p, "dave", "tx-d", 300)
	if _, seated, _ := seats.GetSeat("newop"); seated {
		t.Fatal("2 round-2 votes + 2 round-1 votes admitted the operator: stale votes carried into the new round")
	}
	// alice re-votes in round 2: 3 of 4.
	se.handleAdmitVote(p, "alice", "tx-a2", 302)
	if _, seated, _ := seats.GetSeat("newop"); !seated {
		t.Fatal("3 of 4 seats in the new round did not admit: the pair is still unadmittable after expiry")
	}
}

// A proposal already marked expired (for example before 0.9.0 activated) also
// re-opens on the next vote.
func TestPoa7_AlreadyExpiredProposalReopensAt090(t *testing.T) {
	se, _, gov := admitEnv(t, 9, "alice", "bob", "carol", "dave")
	p := admitPayload(t, "newop", "ubo-new")
	id := governance.AdmitSeatProposalID("newop", "ubo-new")
	se.handleAdmitVote(p, "alice", "tx-a", 100)
	prop := gov.proposals[id]
	prop.Status = string(governance.StatusExpired)
	gov.proposals[id] = prop

	se.handleAdmitVote(p, "bob", "tx-b", 150)
	if gov.proposals[id].Status != string(governance.StatusOpen) || gov.proposals[id].CreationBlock != 150 {
		t.Fatalf("expired proposal did not re-open at 150: status %q creation %d",
			gov.proposals[id].Status, gov.proposals[id].CreationBlock)
	}
	if v := se.governanceVoterSetSinceOrBlock(id, 150); !v["bob"] || v["alice"] {
		t.Fatalf("round-2 voters = %v, want bob only", v)
	}
}

// Control: the existing TestAdmitVoteProposalExpires pins the original terminal
// behaviour at 0.7.0 (a later vote cannot resurrect it); it must keep passing.

// ★ POA-3. At 0.9.0 a seat out of the committee for a whole departure window
// no longer votes, so the live seats can reach 2/3 again after more than a
// third have left. Live testnet/devnet: an exited seat's vote crossed the bar,
// and 3 live of 5 could never admit (bar 4).
func TestPoa3_DepartedSeatsStopVotingAfterTheDepartureWindowAt090(t *testing.T) {
	se, seats, _ := admitEnv(t, 9, "a", "b", "c", "d", "e")
	for _, x := range []string{"c", "d", "e"} { // three seats left the set at 50
		if err := seats.SetExit(x, 50); err != nil {
			t.Fatal(err)
		}
	}
	dep := systemconfig.MocknetConfig().ConsensusParams().EffectivePoaVoteDeparture()
	if dep <= testHalt {
		t.Fatalf("departure window %d must be longer than the exit-halt %d", dep, testHalt)
	}
	// One exit-halt after leaving (bond withdrawable) a departed seat still votes:
	// an outage of that length must not lower the bar.
	if el, _ := se.poaSeatElectorate(50 + testHalt); len(el) != 5 {
		t.Fatalf("electorate one halt after the exit = %d, want all 5 (inside the departure window)", len(el))
	}
	after := 50 + dep // departure window run out
	el, ok := se.poaSeatElectorate(after)
	if !ok || len(el) != 2 {
		t.Fatalf("electorate after the halt = %d (ok=%v), want the 2 remaining seats", len(el), ok)
	}
	// Inside the window the departed seats still vote (temporary absence keeps its vote).
	el, _ = se.poaSeatElectorate(after - 1)
	if len(el) != 5 {
		t.Fatalf("electorate inside the halt window = %d, want all 5", len(el))
	}
	// And the two live seats can now admit (bar for 2 is 2).
	p := admitPayload(t, "newop", "ubo-new")
	se.handleAdmitVote(p, "a", "tx-a", after)
	se.handleAdmitVote(p, "b", "tx-b", after+1)
	if _, seated, _ := seats.GetSeat("newop"); !seated {
		t.Fatal("the remaining live seats still cannot admit after the departed seats' halt ran out: the POA-3 deadlock remains")
	}
	// A departed seat's vote is ignored.
	p2 := admitPayload(t, "other", "ubo-o")
	se.handleAdmitVote(p2, "c", "tx-c", after+2)
	if len(se.governanceVoterSetOrBlock(governance.AdmitSeatProposalID("other", "ubo-o"))) != 0 {
		t.Fatal("a departed seat (halt run out) still opened/voted on an admission")
	}
}

// Control: below 0.9.0 every seat ever admitted votes forever (the live POA-3).
func TestPoa3_Below090DepartedSeatsStillVote(t *testing.T) {
	se, seats, _ := admitEnv(t, 7, "a", "b", "c", "d", "e")
	for _, x := range []string{"c", "d", "e"} {
		if err := seats.SetExit(x, 50); err != nil {
			t.Fatal(err)
		}
	}
	dep := systemconfig.MocknetConfig().ConsensusParams().EffectivePoaVoteDeparture()
	el, ok := se.poaSeatElectorate(50 + dep + 1000)
	if !ok || len(el) != 5 {
		t.Fatalf("below 0.9.0 the electorate changed (%d, ok=%v): replay would diverge", len(el), ok)
	}
}

// POA-3 also covers a seat admitted but never elected: SetExit only fires for a
// seat that has been seated, so without its own clock such a seat voted forever.
func TestPoa3_SeatHoldsAdmissionVoteClocks(t *testing.T) {
	const halt = uint64(100)
	cases := []struct {
		name   string
		seat   poaseats.Seat
		height uint64
		want   bool
	}{
		{"seated votes long after admission", poaseats.Seat{AdmittedHeight: 10, LastSeatedHeight: 20}, 1_000_000, true},
		{"exited, inside the window", poaseats.Seat{AdmittedHeight: 10, LastSeatedHeight: 20, ExitHeight: 50}, 149, true},
		{"exited, window run out", poaseats.Seat{AdmittedHeight: 10, LastSeatedHeight: 20, ExitHeight: 50}, 150, false},
		{"never seated, inside the window from admission", poaseats.Seat{AdmittedHeight: 10}, 109, true},
		{"never seated, window run out", poaseats.Seat{AdmittedHeight: 10}, 110, false},
		{"exit clock wins over admission", poaseats.Seat{AdmittedHeight: 10, LastSeatedHeight: 20, ExitHeight: 500}, 200, true},
		{"overflowing halt keeps the vote", poaseats.Seat{AdmittedHeight: 10, LastSeatedHeight: 20, ExitHeight: ^uint64(0) - 5}, ^uint64(0), true},
	}
	for _, c := range cases {
		if got := seatHoldsAdmissionVote(c.seat, c.height, halt); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPoa3_NeverSeatedSeatStopsVotingOneWindowAfterAdmissionAt090(t *testing.T) {
	se, seats, _ := admitEnv(t, 9, "a", "b", "c")
	seats.seed("ghost", "ubo-ghost", 100, 0) // admitted at 100, never elected
	dep := systemconfig.MocknetConfig().ConsensusParams().EffectivePoaVoteDeparture()
	el, ok := se.poaSeatElectorate(100 + dep - 1)
	if !ok || len(el) != 4 {
		t.Fatalf("electorate inside the window = %d (ok=%v), want 4 (the new seat votes)", len(el), ok)
	}
	el, ok = se.poaSeatElectorate(100 + dep)
	if !ok || len(el) != 3 {
		t.Fatalf("electorate after the window = %d (ok=%v), want the 3 serving seats", len(el), ok)
	}
	// Seated later: the vote comes back.
	if err := seats.SetSeating("ghost", 100+dep+10); err != nil {
		t.Fatal(err)
	}
	if el, _ = se.poaSeatElectorate(100 + dep + 10); len(el) != 4 {
		t.Fatalf("electorate after the seat was elected = %d, want 4", len(el))
	}
}

// Control: below 0.9.0 a never-seated seat keeps its vote (replay).
func TestPoa3_Below090NeverSeatedSeatStillVotes(t *testing.T) {
	se, seats, _ := admitEnv(t, 7, "a", "b", "c")
	seats.seed("ghost", "ubo-ghost", 100, 0)
	dep := systemconfig.MocknetConfig().ConsensusParams().EffectivePoaVoteDeparture()
	if el, ok := se.poaSeatElectorate(100 + dep + 1000); !ok || len(el) != 4 {
		t.Fatalf("below 0.9.0 the electorate changed (%d, ok=%v): replay would diverge", len(el), ok)
	}
}

// The POA-5 check is a TxResult, so a read error must not decide it on one node
// alone: PoaExitHaltOrBlock retries until the read succeeds and returns the real
// verdict in either direction. IsPoaExitHalted keeps its fail-closed behaviour
// for its existing callers.
func TestPoa5_BondedNodeCheckRetriesReadsInsteadOfDecidingOnAnError(t *testing.T) {
	se, seats, _ := poaEnv(t, 9)
	seats.seed("node", "ubo-n", 10, 100) // seated and electable: held

	seats.failGetSeatFor = 2
	if halted, _, _ := se.PoaExitHaltOrBlock("node", 200); !halted {
		t.Fatal("seated, electable node reported free after transient read errors")
	}
	if seats.failGetSeatFor != 0 {
		t.Fatalf("did not retry through the read errors (%d left)", seats.failGetSeatFor)
	}

	seats.failGetSeatFor = 2
	if halted, _, _ := se.PoaExitHaltOrBlock("nobody", 200); halted {
		t.Fatal("a transient read error decided the verdict: an account with no seat came back held on this node only")
	}

	seats.failGetSeatFor = 1
	if !se.IsPoaExitHalted("nobody", 200) {
		t.Fatal("IsPoaExitHalted no longer holds on a read error: its existing callers changed behaviour")
	}
}

// POA-10: the self-unstake exit-halt check is a TxResult too. A transient read
// error must not refuse on one node an unstake that its peers accept; it now
// retries and returns the real verdict. Applies at every version (it changes
// only what happens when a read fails).
func TestPoa10_SelfUnstakeCheckRetriesReadsInsteadOfRefusing(t *testing.T) {
	for _, ver := range []uint64{7, 9} {
		se, seats, _ := poaEnv(t, ver)
		seats.failGetSeatFor = 2 // two transient failures, then the read works
		led := &recordingLedger{}

		res := delegatedUnstake("hive:plain", "hive:plain", 200).ExecuteTx(se, led, nil, nil, "")

		if !res.Success || led.unstakes != 1 {
			t.Fatalf("v0.%d.0: a self-unstake by an account with no seat was refused after transient read errors (success=%v calls=%d ret=%q): that node would diverge from its peers",
				ver, res.Success, led.unstakes, res.Ret)
		}
		if seats.failGetSeatFor != 0 {
			t.Fatalf("v0.%d.0: the check did not retry through the read errors (%d left)", ver, seats.failGetSeatFor)
		}
	}
}
