package state_engine

import (
	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc/elections"
)

// BlsQuorumMet reports whether the set of BLS-included signer DIDs carries at
// least 2/3 of the total election weight.
//
// review2 CRITICAL #6: the state-engine previously accepted any
// cryptographically-valid aggregate signature without checking how much weight
// actually signed, so a sub-quorum commitment (e.g. 3 of 6 equal members, below
// the 2/3 threshold of 4 — observed for epochs 444-486) was accepted and could
// activate a TSS key. This predicate mirrors exactly the rule the leader enforces
// while collecting signatures in tss.go waitForSigs, with weights taken from the
// on-chain election so every node agrees.
//
// B13: the weight fold itself now lives in lib/dids so that every consensus site
// asking "did enough weight sign?" gives the same answer. This one gates TSS
// keygen and reshare commitments — i.e. the activation of a BTC custody key — and
// its previous inline fold was exploitable in its own right: it built
// weightByDID with a plain map assignment, so a duplicate key was LAST-WRITE-WINS
// while both seats still counted toward the total. Members arrive account-sorted,
// so an attacker who copied an honest witness's key into an account sorting after
// theirs CHOSE which weight the honest signature would be credited. The comment
// claimed "duplicates counted once"; the code substituted the attacker's weight.
//
// dedupActive gates the corrected fold on the chain-active consensus version.
// This is an accept/reject gate on historical commitments, so flipping it ungated
// would change past verdicts during a reindex. Below the line the original fold
// runs byte-identically.
func BlsQuorumMet(included []dids.BlsDID, members []elections.ElectionMember, weights []uint64, dedupActive bool) bool {
	if len(members) == 0 || len(members) != len(weights) {
		return false
	}

	if dedupActive {
		memberKeys := make([]string, len(members))
		for i, m := range members {
			memberKeys[i] = m.Key
		}
		return dids.QuorumMet(memberKeys, weights, included)
	}

	return legacyBlsQuorumMet(included, members, weights)
}

// legacyBlsQuorumMet is the pre-B13 fold, retained verbatim so that replaying
// history below the version line reproduces the original verdicts exactly. Its
// duplicate handling is the defect described above; do not call it from new code.
func legacyBlsQuorumMet(included []dids.BlsDID, members []elections.ElectionMember, weights []uint64) bool {
	weightByDID := make(map[dids.BlsDID]uint64, len(members))
	var weightTotal uint64
	for i, m := range members {
		weightByDID[dids.BlsDID(m.Key)] = weights[i]
		weightTotal += weights[i]
	}
	if weightTotal == 0 {
		return false
	}

	var signedWeight uint64
	counted := make(map[dids.BlsDID]bool, len(included))
	for _, d := range included {
		if counted[d] {
			continue
		}
		counted[d] = true
		signedWeight += weightByDID[d] // unknown DID → 0
	}

	// GV-L9: overflow-safe 2/3 quorum. The naive `signedWeight*3 >= weightTotal*2`
	// wraps mod 2^64 once weightTotal > MaxUint64/2, so a zero-weight commitment
	// could falsely satisfy quorum. ceil(2N/3) == N - floor(N/3) cannot overflow.
	quorumThreshold := weightTotal - weightTotal/3
	return signedWeight >= quorumThreshold
}
