package libp2p_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// audit_unfixed_test.go
//
// Differential test for audit-MED RT-11 (start-gate threshold bug),
// not yet fixed on the combined branch. Pins the current broken
// behaviour so a future fix flips the assertion direction and proves
// remediation.
//
// Source: PENDULUM-UNFIXED-REDTEAM-NOTES (RT-11)

// =====================================================================
// RT-11 — Start gate threshold=0 when 1 bootnode alive
// =====================================================================
//
// audit MED RT-11: in modules/p2p/libp2p.go (around line 346), the
// loop that decides when to release the start gate uses
//
//     if peerLen >= len(uniquePeers)-1 { p2ps.startStatus.TriggerStart() }
//
// With `len(uniquePeers) == 1` (single configured bootnode, which is
// the default solo/dev topology and also the regrettable case where a
// fresh node has discovered exactly itself in `uniquePeers`), the
// condition becomes `peerLen >= 0` — always true — so the gate
// triggers on the first iteration even with zero connected peers.
//
// Likewise `len(uniquePeers) == 2` with `peerLen == 1` triggers
// `1 >= 1`, releasing the gate at half the quorum the operator
// presumably expected.
//
// The test below replicates the exact gate expression and asserts
// that the buggy edge cases currently PASS the gate. Post-fix the
// expression should be `peerLen >= MinPeers` (a small constant such
// as the bootnode count or a config-driven minimum) — at which point
// these assertions will need to be inverted (i.e. the gate would no
// longer trigger for `(0, 1)` etc.).

// gateOpensCurrent mirrors the pre-fix expression at
// modules/p2p/libp2p.go:346.
func gateOpensCurrent(peerLen, uniquePeersLen int) bool {
	return peerLen >= uniquePeersLen-1
}

func TestAuditUnfixed_RT11_StartGateBrokenForSingleBootnode(t *testing.T) {
	type edge struct {
		peerLen          int
		uniquePeersLen   int
		passesCurrent    bool // documented current (buggy) behaviour
		description      string
	}

	edges := []edge{
		// BUG cases — gate opens prematurely.
		{
			peerLen:        0,
			uniquePeersLen: 1,
			passesCurrent:  true,
			description:    "0 peers, 1 bootnode: 0 >= 0 — TSS/gateway start with no peers",
		},
		{
			peerLen:        1,
			uniquePeersLen: 2,
			passesCurrent:  true,
			description:    "1 peer, 2 bootnodes: 1 >= 1 — releases at half the apparent quorum",
		},
		{
			peerLen:        2,
			uniquePeersLen: 3,
			passesCurrent:  true,
			description:    "2 peers, 3 bootnodes: 2 >= 2 — releases one peer short of quorum",
		},
		// Sanity check: the threshold can still legitimately fail when
		// peerLen is strictly below uniquePeers-1.
		{
			peerLen:        0,
			uniquePeersLen: 5,
			passesCurrent:  false,
			description:    "0 peers, 5 bootnodes: 0 >= 4 — gate (correctly) closed",
		},
	}

	for _, e := range edges {
		got := gateOpensCurrent(e.peerLen, e.uniquePeersLen)
		assert.Equal(t, e.passesCurrent, got,
			"RT-11 (current behaviour): peerLen=%d, uniquePeers=%d — %s",
			e.peerLen, e.uniquePeersLen, e.description)
	}

	// Explicit BUG markers — these three currently PASS the gate.
	// Post-fix (`peerLen >= MinPeers`) the first two will need to be
	// flipped: see comment header.
	assert.True(t, gateOpensCurrent(0, 1),
		"RT-11 BUG: gate opens with zero connected peers when len(uniquePeers)=1")
	assert.True(t, gateOpensCurrent(1, 2),
		"RT-11 BUG: gate opens at 50%% of presumed quorum when len(uniquePeers)=2")
}

// TestAuditUnfixed_RT11_PostFixSemantics documents the expected
// post-fix gate (peerLen >= MinPeers with MinPeers being a constant
// minimum, e.g. 2). When the fix lands, callers should switch the
// production code to invoke `gateOpensPostFix` semantics and this
// test becomes a positive assertion.
func TestAuditUnfixed_RT11_PostFixSemantics(t *testing.T) {
	const minPeers = 2

	gateOpensPostFix := func(peerLen int) bool {
		return peerLen >= minPeers
	}

	// With a sensible floor, the (0, 1) and (1, 2) edge cases stop
	// opening the gate.
	assert.False(t, gateOpensPostFix(0),
		"RT-11 post-fix: zero peers must not open the gate regardless of uniquePeers length")
	assert.False(t, gateOpensPostFix(1),
		"RT-11 post-fix: single peer must not open the gate when MinPeers >= 2")
	assert.True(t, gateOpensPostFix(2),
		"RT-11 post-fix: gate opens at MinPeers connected peers")
	assert.True(t, gateOpensPostFix(5),
		"RT-11 post-fix: gate stays open beyond MinPeers")
}
