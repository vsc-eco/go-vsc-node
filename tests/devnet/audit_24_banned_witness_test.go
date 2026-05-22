package devnet

import (
	"testing"
)

// TestAuditFix_24_BannedWitnessExcludedFromNextElection is the devnet
// shape for audit #24. Pre-fix (*electionProposer).scoreMap() ran the
// under-75% computation but no call site read BannedNodes; post-fix
// GenerateFullElection consults it and removes banned accounts.
//
// SKIPPED. A direct devnet driver — stop magi-3, let four ElectionInterval
// cycles pass with scoreMapMinSamples lowered via env var, restart, and
// inspect the next election's Members list — was written and exercised
// (see git history for f8dff4f7…audit_24_banned_witness_test.go), but the
// 5-node BLS aggregate cannot finalize an election while one member is
// down even at ConsensusParams.MinMembers=3. The election proposer keeps
// trying but no new election lands in the elections collection until the
// downed node returns, by which point the scoreMap window already includes
// the catchup blocks where magi-3 signs again — its score climbs back above
// 75% and it is not banned. Verified end-to-end: containers boot, magi-3
// stops, block production continues on the other 4 nodes, election epoch
// stays at the last pre-stop value (epoch 3 in the observed run) and the
// Members list remains unchanged.
//
// What unblocks a real devnet test:
//   1. Increase devnet committee size to >= 7 so 2/3 BLS weight is
//      achievable with 1 node down (MinMembers=3 isn't the binding
//      constraint — the BLS aggregate weight floor is).
//   2. Or stop magi-3 AFTER establishing 25+ healthy elections of history
//      (so scoreMap denominator is large enough for the down-window to
//      drop magi-3's score below 75% even after catchup signings) — needs
//      VSC_ELECTION_SCOREMAP_MIN_SAMPLES set, plus several minutes of
//      runtime per election.
//   3. Or stop magi-3 in a way that doesn't break BLS finalization —
//      e.g., temporary network partition that drops only proposal
//      messages, not the BLS sigs.
//
// The unit-test path (modules/election-proposer/audit_unfixed_test.go
// TestAuditFix_24_ScoreMapBannedNodesHasLiveCaller) asserts the wiring
// at the source level (>0 callers of scoreMap). The fix is wired in;
// what's skipped is purely the runtime-ban-fires-during-election devnet
// observation.
func TestAuditFix_24_BannedWitnessExcludedFromNextElection(t *testing.T) {
	t.Skip("requires either a 7+ node committee or 25+ baseline elections; " +
		"the 5-node default can't finalize elections with one member down. " +
		"Covered at unit level by modules/election-proposer/audit_unfixed_test.go.")
}
