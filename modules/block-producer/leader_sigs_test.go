package blockproducer

import "testing"

// L2-STALL-1: wherever the leader stops collecting, the block must pass the
// strict 2/3 check that follows (signed > total*2/3). The old stop rule
// (signed >= total*9/10) stopped at 2 of 3, which that check rejects, so a
// 3-member committee never produced a block.
func TestLeaderStopsOnlyWhenTheBlockCanPass(t *testing.T) {
	for total := uint64(1); total <= 12; total++ {
		stop := uint64(0)
		for !leaderSigsComplete(stop, total, total) && stop <= total {
			stop++
		}
		if stop > total {
			t.Fatalf("total %d: the leader never stops even with every member signed", total)
		}
		if !(stop > total*2/3) {
			t.Fatalf("total %d: the leader stops at %d, which fails the block's 2/3 check (needs > %d)", total, stop, total*2/3)
		}
	}
}
