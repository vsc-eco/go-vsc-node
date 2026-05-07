package rewards

import (
	"encoding/base64"
	"math/big"

	tss_db "vsc-node/modules/db/vsc/tss"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
)

// SlotProposer pairs a VSC slot's start height with the elected proposer
// for that slot. The aggregator builds this list from CalculateSlotInfo +
// GetSchedule for each slot in the tick window.
type SlotProposer struct {
	SlotHeight uint64
	Account    string
}

// committeeFromMembers returns a set of committee accounts for fast lookup.
// Accounts are passed in directly (not Member structs) to keep this package
// free of an elections import — the caller projects ElectionMember.Account
// before calling.
func committeeFromMembers(committee []string) map[string]struct{} {
	out := make(map[string]struct{}, len(committee))
	for _, m := range committee {
		if m != "" {
			out[m] = struct{}{}
		}
	}
	return out
}

// ScoreBlockProduction returns per-witness Tier-1 (block production miss)
// bps for the tick: BlockProductionMissBps × (number of slots where the
// scheduled proposer is in committee but did not produce a block).
//
// `slots` lists the expected proposers; `producedSlotHeights` is the set of
// slot heights that actually have a block in vsc_blocks for the tick window.
// Each scoring is independent of which specific block produced them.
func ScoreBlockProduction(slots []SlotProposer, producedSlotHeights map[uint64]struct{}, committee []string) map[string]int {
	if len(slots) == 0 {
		return nil
	}
	cm := committeeFromMembers(committee)
	if len(cm) == 0 {
		return nil
	}
	out := make(map[string]int)
	for _, s := range slots {
		if _, in := cm[s.Account]; !in {
			continue
		}
		if _, produced := producedSlotHeights[s.SlotHeight]; produced {
			continue
		}
		out[s.Account] += BlockProductionMissBps
	}
	return out
}

// ScoreBlockAttestation returns per-witness Tier-2 (block attestation miss)
// bps: BlockAttestationMissBps × (number of VSC blocks in the tick window
// where this committee member is absent from the BLS Signers list).
//
// Strict eligibility (D10): every produced block expects every committee
// member's signature, regardless of whether the block reached its 2/3
// threshold without them.
func ScoreBlockAttestation(blocks []vscBlocks.VscHeaderRecord, committee []string) map[string]int {
	if len(blocks) == 0 {
		return nil
	}
	cm := committeeFromMembers(committee)
	if len(cm) == 0 {
		return nil
	}
	out := make(map[string]int)
	for _, b := range blocks {
		signers := make(map[string]struct{}, len(b.Signers))
		for _, s := range b.Signers {
			signers[s] = struct{}{}
		}
		for member := range cm {
			if _, signed := signers[member]; signed {
				continue
			}
			out[member] += BlockAttestationMissBps
		}
	}
	return out
}

// ScoreTssReshareExclusion returns per-witness Tier-A (reshare exclusion)
// bps. Each entry in `reshares` represents one canonical successful reshare
// commitment landing in the tick window. The committee for each reshare is
// the committee at that reshare's epoch (the new committee elected for the
// epoch the reshare opened).
//
// For each reshare: any member of the new committee not represented in the
// commitment's bitset accumulates TssReshareExclusionBps.
//
// Strict per-key (D8): if a witness is excluded from N keys' reshares in the
// tick, they accumulate N × TssReshareExclusionBps before max-of and
// per-tick clamp.
func ScoreTssReshareExclusion(reshares []ReshareWithCommittee) map[string]int {
	if len(reshares) == 0 {
		return nil
	}
	out := make(map[string]int)
	for _, r := range reshares {
		bits, ok := decodeBitset(r.Commitment.Commitment)
		if !ok {
			// Unparseable bitset = treat as everyone excluded would be too
			// punitive; treat as no info, no penalty. Logged at the wire
			// layer if it ever happens.
			continue
		}
		for idx, member := range r.NewCommittee {
			if member == "" {
				continue
			}
			if bits.Bit(idx) == 1 {
				continue
			}
			out[member] += TssReshareExclusionBps
		}
	}
	return out
}

// ReshareWithCommittee pairs a reshare commitment with the new committee its
// bitset is encoded against. The aggregator must look up the election at the
// reshare's commitment.Epoch (NOT the current epoch — see Tier-B's same
// requirement).
type ReshareWithCommittee struct {
	Commitment   tss_db.TssCommitment
	NewCommittee []string // members in elected order so bit indexes match
}

// ScoreTssBlame returns per-witness Tier-B bps. For each blame commitment
// in the tick window, decode its bitset against the blame's own epoch
// election (avoiding the main-branch decoding bug at state_engine.go:718)
// and credit each named witness with TssBlameBps.
func ScoreTssBlame(blames []BlameWithCommittee) map[string]int {
	if len(blames) == 0 {
		return nil
	}
	out := make(map[string]int)
	for _, b := range blames {
		bits, ok := decodeBitset(b.Commitment.Commitment)
		if !ok {
			continue
		}
		for idx, member := range b.BlameCommittee {
			if member == "" {
				continue
			}
			if bits.Bit(idx) == 1 {
				out[member] += TssBlameBps
			}
		}
	}
	return out
}

// BlameWithCommittee pairs a blame commitment with the committee its bitset
// is encoded against (the commitment's own epoch's election members).
type BlameWithCommittee struct {
	Commitment     tss_db.TssCommitment
	BlameCommittee []string
}

// ScoreTssSignNonParticipation returns per-witness Tier-C bps. For each
// sign_result commitment in the tick window, parse the BLS BitSet (the "bv"
// field) against the committee active at the commitment's block height; any
// committee member with bit==0 accumulates TssSignNonParticipationBps.
//
// Legacy commitments persisted before TssCommitment.BitSet existed have
// BitSet=="" and are skipped — they contribute zero Tier-C bps.
func ScoreTssSignNonParticipation(signs []SignResultWithCommittee) map[string]int {
	if len(signs) == 0 {
		return nil
	}
	out := make(map[string]int)
	for _, s := range signs {
		if s.Commitment.BitSet == "" {
			continue
		}
		bits, ok := decodeBitset(s.Commitment.BitSet)
		if !ok {
			continue
		}
		for idx, member := range s.SignCommittee {
			if member == "" {
				continue
			}
			if bits.Bit(idx) == 1 {
				continue
			}
			out[member] += TssSignNonParticipationBps
		}
	}
	return out
}

// SignResultWithCommittee pairs a sign_result commitment with the committee
// active at its BlockHeight. The bit indexes in the BLS BitSet correspond
// to that committee's members in elected order.
type SignResultWithCommittee struct {
	Commitment    tss_db.TssCommitment
	SignCommittee []string
}

// ScoreOracleQuoteDivergence returns per-witness Tier-D bps. Each entry in
// `divergingWitnesses` is a trusted-group member whose latest HBD/HIVE quote
// drifted from the trusted-group mean by at least
// OracleQuoteDivergenceThresholdBps at this tick. Each member receives a
// single OracleQuoteDivergenceBps charge per tick — the divergence signal is
// observed once at tick close, not per swap.
//
// Witnesses outside `committee` are dropped (a witness can be trusted-group
// without being on the rewardable committee for the tick — only committee
// members earn pendulum rewards).
func ScoreOracleQuoteDivergence(divergingWitnesses []string, committee []string) map[string]int {
	if len(divergingWitnesses) == 0 {
		return nil
	}
	cm := committeeFromMembers(committee)
	if len(cm) == 0 {
		return nil
	}
	out := make(map[string]int)
	seen := make(map[string]struct{})
	for _, w := range divergingWitnesses {
		if w == "" {
			continue
		}
		if _, ok := cm[w]; !ok {
			continue
		}
		if _, dup := seen[w]; dup {
			continue
		}
		seen[w] = struct{}{}
		out[w] += OracleQuoteDivergenceBps
	}
	return out
}

// decodeBitset parses a base64-RawURL-encoded big.Int bitset (the format
// used by setToCommitment in tss.go and by SerializeBlsCircuit in
// lib/dids/bls.go). Returns (nil, false) on any parse error so callers can
// drop the entry without erroring the whole tick.
func decodeBitset(s string) (*big.Int, bool) {
	if s == "" {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	bits := new(big.Int)
	bits.SetBytes(raw)
	return bits, true
}
