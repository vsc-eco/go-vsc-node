package devnet

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestPoaOfflineSeatsStayElectedAndStopBlocks (items 3 and 5, flat weight): with 6
// seats at weight 1, L2 blocks need signed weight > floor(2W/3) = 5 of 6, while an
// election needs (2W+2)/3 = 4 of 6. Two stopped seats therefore halt L2 block
// production while elections keep ratifying, and nothing removes the stopped seats:
// their witness records stay valid, so every new election re-elects them. A third
// stopped seat stops elections too (with no recovery path on chain).
//
// Expected RED while the gap exists: with 2 of 6 stopped, the L2 slot height stops
// moving, new epochs still ratify, and the stopped seats are members of them.
func TestPoaOfflineSeatsStayElectedAndStopBlocks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	cfg := tssTestConfig()
	cfg.Nodes = 6
	cfg.GenesisNode = 1
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ConsensusVersionFloorMajor = 0
	cp.ConsensusVersionFloorConsensus = poaDevnetFloor()
	cp.ConsensusVersionFloorEpoch = 1
	d, ctx := startDevnetNoKey(t, cfg, 70*time.Minute)

	acct := func(n int) string { return fmt.Sprintf("%s%d", cfg.WitnessPrefix, n) }
	bare := func(ms []string) []string {
		out := make([]string, len(ms))
		for i, m := range ms {
			out[i] = strings.TrimPrefix(m, "hive:")
		}
		return out
	}
	slot := func() int {
		s, err := d.pfMaxSlotHeight(ctx, 1)
		if err != nil {
			t.Logf("slot height: %v", err)
		}
		return s
	}
	epoch := func() uint64 { e, _ := latestElection(t, d, ctx, 1); return e }
	// observe waits about three election intervals and reports slot and epoch movement.
	observe := func(label string) (slotDelta int, e0, e1 uint64) {
		s0, e0 := slot(), epoch()
		h, _ := getHeadBlock(d.HiveRPCEndpoint())
		waitForBlock(t, d.HiveRPCEndpoint(), h+3*20+10, 8*time.Minute)
		s1, e1 := slot(), epoch()
		t.Logf("%s: L2 slot %d -> %d, epoch %d -> %d", label, s0, s1, e0, e1)
		return s1 - s0, e0, e1
	}

	for n := 1; n <= cfg.Nodes; n++ {
		if err := d.waitForElectionEpoch(ctx, n, 3, 10*time.Minute); err != nil {
			t.Fatalf("magi-%d never reached epoch 3: %v", n, err)
		}
	}
	if dSlot, _, _ := observe("0. all 6 up"); dSlot <= 0 {
		t.Fatalf("PRECONDITION FAILED: L2 blocks not advancing with all 6 up")
	}

	// A. Two seats gone.
	for _, n := range []int{5, 6} {
		if err := d.StopNode(ctx, n); err != nil {
			t.Fatalf("stop magi-%d: %v", n, err)
		}
	}
	t.Logf("A. stopped magi-5 and magi-6")
	// One interval for in-flight work to settle, then measure.
	h, _ := getHeadBlock(d.HiveRPCEndpoint())
	waitForBlock(t, d.HiveRPCEndpoint(), h+20, 3*time.Minute)
	dSlotA, eA0, eA1 := observe("A. 2 of 6 stopped")
	memA, _ := d.GetElectionMembers(ctx, 1, eA1)
	t.Logf("A. committee at epoch %d: %v", eA1, bare(memA))
	stillElected := contains(bare(memA), acct(5)) && contains(bare(memA), acct(6))

	// B. A third seat gone.
	if err := d.StopNode(ctx, 4); err != nil {
		t.Fatalf("stop magi-4: %v", err)
	}
	t.Logf("B. stopped magi-4 too (3 of 6)")
	h, _ = getHeadBlock(d.HiveRPCEndpoint())
	waitForBlock(t, d.HiveRPCEndpoint(), h+20, 3*time.Minute)
	dSlotB, eB0, eB1 := observe("B. 3 of 6 stopped")

	// Control: bring them back.
	for _, n := range []int{4, 5, 6} {
		if err := d.StartNode(ctx, n); err != nil {
			t.Logf("start magi-%d: %v", n, err)
		}
	}
	h, _ = getHeadBlock(d.HiveRPCEndpoint())
	waitForBlock(t, d.HiveRPCEndpoint(), h+40, 5*time.Minute)
	dSlotC, _, eC1 := observe("C. control, all 6 back")

	if dSlotA <= 0 && eA1 > eA0 && stillElected {
		t.Errorf("OFFLINE-SEATS: with 2 of 6 seats stopped, L2 blocks halted (slot +%d) while elections kept ratifying (epoch %d -> %d), and the stopped seats were re-elected into epoch %d", dSlotA, eA0, eA1, eA1)
	}
	if eB1 <= eB0 {
		t.Errorf("ELECTION-HALT: with 3 of 6 seats stopped no election ratified (epoch stuck at %d, slot +%d)", eB0, dSlotB)
	}
	t.Logf("summary: A slot+%d epochs %d->%d stillElected=%v | B slot+%d epochs %d->%d | C slot+%d epoch %d", dSlotA, eA0, eA1, stillElected, dSlotB, eB0, eB1, dSlotC, eC1)
}
