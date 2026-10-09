package db

import "testing"

// TestVersionLagNeedsReindex pins the consensus-version-lag full-reindex decision:
// reindex iff the previous run's binary could NOT handle the chain-active version at
// the head but THIS binary can (the laggard-upgraded case).
func TestVersionLagNeedsReindex(t *testing.T) {
	cases := []struct {
		name                                              string
		runMaj, runCons, chMaj, chCons, procMaj, procCons uint64
		want                                              bool
	}{
		// Node has always been current with the chain → no reindex.
		{"current", 0, 3, 0, 3, 0, 3, false},
		// Laggard still on the OLD binary (below chain): can't fix under it yet → no reindex.
		{"laggard not yet upgraded", 0, 2, 0, 3, 0, 2, false},
		// Laggard upgraded: prev run was 0.2 < chain 0.3, this binary is 0.3 → reindex.
		{"laggard upgraded", 0, 3, 0, 3, 0, 2, true},
		// Big lag, upgraded all the way: prev 0.2, chain 0.4, now 0.4 → reindex.
		{"laggard upgraded over two versions", 0, 4, 0, 4, 0, 2, true},
		// Node running AHEAD of the chain the whole time (e.g. 0.4 binary, chain 0.3) →
		// it applied 0.3 rules via gates, never diverged → no reindex.
		{"ahead of chain", 0, 4, 0, 3, 0, 4, false},
		// Upgraded but the chain never moved past what the prev run already handled → no reindex.
		{"upgraded but no missed adoption", 0, 4, 0, 3, 0, 3, false},
		// Prev run could handle chain exactly (== ), this binary higher → no reindex.
		{"prev exactly met chain", 0, 4, 0, 3, 0, 3, false},
	}
	for _, c := range cases {
		got := versionLagNeedsReindex(c.runMaj, c.runCons, c.chMaj, c.chCons, c.procMaj, c.procCons)
		if got != c.want {
			t.Errorf("%s: versionLagNeedsReindex(run=%d.%d chain=%d.%d proc=%d.%d) = %v, want %v",
				c.name, c.runMaj, c.runCons, c.chMaj, c.chCons, c.procMaj, c.procCons, got, c.want)
		}
	}
}

// A DB written by a binary that predates the processed_under metadata is judged
// as processed under 0.3 (the last such release): a node that upgrades before a
// version rise keeps its DB, one that ran the old binary past the rise replays.
func TestProcessedVersionWithoutMetadata(t *testing.T) {
	maj, cons := processedVersion(SearchResult{})
	if maj != 0 || cons != preMetadataConsensus {
		t.Fatalf("no metadata: got %d.%d, want 0.%d", maj, cons, preMetadataConsensus)
	}
	// Upgraded from the old binary before the rise: chain still 0.3 -> keep.
	if versionLagNeedsReindex(0, 9, 0, 3, maj, cons) {
		t.Fatal("old binary, chain still at 0.3: must not reindex")
	}
	// Kept running the old binary past the rise to 0.9 -> replay.
	if !versionLagNeedsReindex(0, 9, 0, 9, maj, cons) {
		t.Fatal("old binary ran past the 0.9 rise: must reindex")
	}

	nine, zero := uint64(9), uint64(0)
	maj, cons = processedVersion(SearchResult{ProcessedUnderMajor: &zero, ProcessedUnderConsensus: &nine})
	if maj != 0 || cons != 9 {
		t.Fatalf("recorded 0.9: got %d.%d", maj, cons)
	}
	maj, cons = processedVersion(SearchResult{ProcessedUnderConsensus: &nine})
	if maj != 0 || cons != 9 {
		t.Fatalf("recorded consensus only: got %d.%d", maj, cons)
	}
}
