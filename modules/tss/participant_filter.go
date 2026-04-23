package tss

import (
	"math/big"

	"vsc-node/modules/db/vsc/elections"
)

// buildSignParticipants returns the deterministic signing participant list
// for a key, plus the full pre-filter committee size (used for threshold
// math). The output is a pure function of on-chain inputs — no gossip,
// no RPC, no witness DB state. Two nodes with identical arguments produce
// byte-identical output.
//
// See CLAUDE.md Constraint 2: "Party list must be identical across all
// nodes." Any non-deterministic filter here causes SortedPartyIDs to differ
// across nodes, which causes BuildLocalSaveDataSubset to remap Paillier
// keys to wrong positions, which causes btss signing round 2 to fail with
// "failed to calculate Bob_mid or Bob_mid_wc".
func buildSignParticipants(
	commitElection *elections.ElectionResult,
	commitmentBitset *big.Int,
	blamed map[string]bool,
	banned map[string]bool,
) (participants []Participant, fullSize int) {
	participants = make([]Participant, 0, len(commitElection.Members))
	for idx, member := range commitElection.Members {
		if commitmentBitset.Bit(idx) != 1 {
			continue
		}
		fullSize++
		if blamed[member.Account] {
			continue
		}
		if banned[member.Account] {
			continue
		}
		participants = append(participants, Participant{Account: member.Account})
	}
	return participants, fullSize
}

// buildReshareParticipants returns the deterministic old and new committees
// for a reshare, plus the full pre-filter old committee size. Same
// determinism contract as buildSignParticipants.
//
// oldCommittee is derived from the prior commitment's bitset against the
// commitment's epoch election. newCommittee is derived from currentElection
// (the target epoch). Both exclude blamed + banned. Readiness/liveness
// signals are explicitly NOT parameters — using them here would violate
// Constraint 2.
func buildReshareParticipants(
	commitmentElection *elections.ElectionResult,
	commitmentBitset *big.Int,
	currentElection *elections.ElectionResult,
	blamed map[string]bool,
	banned map[string]bool,
) (old []Participant, new_ []Participant, oldFullSize int) {
	old = make([]Participant, 0, len(commitmentElection.Members))
	for idx, member := range commitmentElection.Members {
		if idx >= commitmentBitset.BitLen() || commitmentBitset.Bit(idx) != 1 {
			continue
		}
		oldFullSize++
		if blamed[member.Account] || banned[member.Account] {
			continue
		}
		old = append(old, Participant{Account: member.Account})
	}
	new_ = make([]Participant, 0, len(currentElection.Members))
	for _, member := range currentElection.Members {
		if blamed[member.Account] || banned[member.Account] {
			continue
		}
		new_ = append(new_, Participant{Account: member.Account})
	}
	return old, new_, oldFullSize
}
