package tss

import (
	"math/big"

	"vsc-node/modules/db/vsc/elections"
)

// buildSignParticipants returns the signing participant list for a key, plus
// the full pre-filter committee size (used for threshold math).
//
// The on-chain inputs (commitElection, commitmentBitset, blamed, banned) are
// strictly deterministic. The optional readyAccounts argument is gossip-derived
// liveness data — see Determinism note below.
//
// readyAccounts:
//   - nil  → no readiness filter (party list is purely on-chain).
//   - non-nil → only members with an entry in readyAccounts are kept. The
//     caller MUST take a snapshot of gossip state at a deterministic block
//     under gossipLock and pass it. Used to exclude offline participants
//     so btss CanProceed() does not block on missing messages (CLAUDE.md
//     Constraint 1).
//
// # Determinism (CLAUDE.md Constraint 2)
//
// readyAccounts is derived from pubsub gossip and is therefore NOT strictly
// deterministic across nodes — two nodes with brief network differences may
// have slightly different sets at the dispatch block. The long readiness
// window (DEFAULT_READINESS_OFFSET blocks of repeated bundle re-broadcast)
// drives convergence in practice but does not guarantee it. When two honest
// nodes build different lists the failure mode is silent: divergent
// SortedPartyIDs → SSID mismatch → session times out → blame commitment.
//
// This is an intentional trade-off accepted to make the offline-node case
// usable: without the readiness filter, every offline old member forces a full
// btss timeout (~1m for sign, 2m for reshare). The trade-off is that
// occasional gossip divergence will cause silent session failures that the
// blame-and-recover loop must handle.
func buildSignParticipants(
	commitElection *elections.ElectionResult,
	commitmentBitset *big.Int,
	blamed map[string]bool,
	banned map[string]bool,
	readyAccounts map[string]bool,
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
		if readyAccounts != nil && !readyAccounts[member.Account] {
			continue
		}
		participants = append(participants, Participant{Account: member.Account})
	}
	return participants, fullSize
}

// buildReshareParticipants returns the old and new committees for a reshare,
// plus the full pre-filter old committee size. Same on-chain determinism
// contract as buildSignParticipants for the on-chain inputs; the optional
// readyAccounts argument carries the same caveats as documented there.
//
// oldCommittee is derived from the prior commitment's bitset against the
// commitment's epoch election. newCommittee is derived from currentElection
// (the target epoch). Both apply the (optional) readiness filter symmetrically:
// reshare requires every listed party — old and new — to send messages, so an
// offline party in either committee blocks the round (CLAUDE.md Constraint 1).
func buildReshareParticipants(
	commitmentElection *elections.ElectionResult,
	commitmentBitset *big.Int,
	currentElection *elections.ElectionResult,
	blamed map[string]bool,
	banned map[string]bool,
	readyAccounts map[string]bool,
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
		if readyAccounts != nil && !readyAccounts[member.Account] {
			continue
		}
		old = append(old, Participant{Account: member.Account})
	}
	new_ = make([]Participant, 0, len(currentElection.Members))
	for _, member := range currentElection.Members {
		if blamed[member.Account] || banned[member.Account] {
			continue
		}
		if readyAccounts != nil && !readyAccounts[member.Account] {
			continue
		}
		new_ = append(new_, Participant{Account: member.Account})
	}
	return old, new_, oldFullSize
}
