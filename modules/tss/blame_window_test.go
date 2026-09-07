package tss

import "testing"

// M-4: the blame window must actually be a WINDOW.
//
// Blame statistics decide which accounts are excluded from a key ceremony. The
// query that gathers them sorts by height DESCENDING and applies a row limit, so
// the limit does not sample the window — it TRUNCATES it to the newest rows and
// silently discards the rest from both the numerator and the denominator.
//
// At 100 rows that was reachable in ordinary operation and trivially attackable.
func TestM4_BlameWindowBoundExceedsAnyHonestWindow(t *testing.T) {
	// Hive produces a block roughly every 3 seconds, so the 24-hour blame window
	// spans BLAME_EXPIRE blocks. Ceremonies run on roughly a two-minute cadence,
	// which is one every 40 blocks.
	const blocksPerCeremony = 40
	ceremoniesPerWindow := int(BLAME_EXPIRE) / blocksPerCeremony

	// Even if EVERY ceremony in the window produced a blame from every member of a
	// generously large committee, the bound must not be reached.
	const generousCommittee = 30
	worstHonestCase := ceremoniesPerWindow * generousCommittee

	if BLAME_WINDOW_MAX_ROWS <= worstHonestCase {
		t.Errorf("BLAME_WINDOW_MAX_ROWS = %d is reachable by honest operation "+
			"(%d ceremonies x %d members = %d): the window would truncate, dropping "+
			"older blames from both the count and the denominator",
			BLAME_WINDOW_MAX_ROWS, ceremoniesPerWindow, generousCommittee, worstHonestCase)
	}
}

// The old bound was reachable by a single member on purpose, which is what made
// it an escape hatch rather than a mere approximation: generating enough fresh
// blames naming OTHERS evicts every older blame naming YOURSELF, dropping your own
// count to zero while the denominator stays full.
//
// This pins the property that makes the new bound safe — that a member cannot
// flush the window within one ceremony cadence — so a future reduction of the
// bound has to confront it.
func TestM4_BoundCannotBeFlushedByOneCeremonysWorthOfBlames(t *testing.T) {
	const generousCommittee = 30
	if BLAME_WINDOW_MAX_ROWS <= generousCommittee {
		t.Fatalf("a single ceremony's blames (%d) could evict the entire window at a "+
			"bound of %d", generousCommittee, BLAME_WINDOW_MAX_ROWS)
	}

	// And the eviction cost scales: flushing the window takes at least this many
	// blame commitments, every one of which must be signed by a committee member
	// and land on chain.
	minCommitmentsToFlush := BLAME_WINDOW_MAX_ROWS
	if minCommitmentsToFlush < 1000 {
		t.Errorf("flushing the blame window costs only %d on-chain commitments; "+
			"that is too cheap to deter a member escaping the ban threshold",
			minCommitmentsToFlush)
	}
}
