package rewards

import (
	"encoding/base64"
	"math/big"

	"vsc-node/lib/dids"
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

// AttestationQuorum carries the signature bar an L2 block leader actually waits
// for, expressed as a weight over the committee being scored, so attestation
// scoring can tell a block that CLOSED AT the bar from one broadcast below it.
// A nil quorum means "score every absence" — the pre-0.14.0 rule, kept so a
// replay of settled history re-derives byte-identically.
//
// REWARD-ATTEST-1 (live on mainnet until 0.14.0). The leader stops collecting
// the moment dids.LeaderSigsComplete holds — 90% of the folded committee weight
// — and broadcasts. On the live 16-seat committee (POA flat weight 1) that bar
// is 14, so two seats are missing from the Signers list of every healthy block,
// and WHICH two is decided by network latency rather than by anything a witness
// did or failed to do. Charging 25 bps for each such absence billed honest
// witnesses for signatures nobody waited for: in mainnet epoch 2221 all 16
// seats were docked, from 3.00% (mahdiyari.vsc) to 66.25% (atexoras.vsc), and
// no seat ever reached zero.
//
// So: a block at or above the bar says nothing about who was willing to sign
// it, and scores nothing. A block BELOW the bar was broadcast on the collection
// timeout — the leader waited the whole window and the signatures still did not
// arrive — so there the absences are real evidence and are scored as before.
//
// Residual, deliberately accepted: a committee degraded by no more than the bar
// already tolerates (2 seats of 16) escapes attestation scoring entirely. That
// is not a hole in liveness scoring. A witness that is genuinely down also
// misses its own slots, and ScoreBlockProduction charges BlockProductionMissBps
// for each — ~8750 bps over a mainnet epoch after forgiveness, far past
// anything attestation contributes — with the per-witness attribution that
// attestation, reading only a final signer set, cannot provide.
type AttestationQuorum struct {
	// WeightByAccount is each committee account's folded election weight. A
	// duplicate-key seat carries 0 and the first occurrence keeps the weight,
	// exactly as dids.FoldSignedWeight counts them, so seating one key twice
	// cannot make a block look closed that in fact was not.
	WeightByAccount map[string]uint64

	// FoldedTotal is the duplicate-collapsed committee weight and ElectionTotal
	// the election's raw TotalWeight; the leader's bar needs both terms.
	FoldedTotal   uint64
	ElectionTotal uint64
}

// closedAtBar reports whether `signers` alone already met the bar the leader
// waits for — i.e. whether the block would have been broadcast without waiting
// for anybody else. A nil quorum, or an election whose weight could not be
// folded, reports false: score every absence, as before the gate.
func (q *AttestationQuorum) closedAtBar(signers map[string]struct{}) bool {
	if q == nil || q.FoldedTotal == 0 {
		return false
	}
	signed := uint64(0)
	for s := range signers {
		// A name that is not a seat, or a duplicate seat, weighs 0.
		signed += q.WeightByAccount[s]
	}
	return dids.LeaderSigsComplete(signed, q.FoldedTotal, q.ElectionTotal)
}

// ScoreBlockAttestation returns per-witness Tier-2 (block attestation miss)
// bps: BlockAttestationMissBps × (number of VSC blocks in the tick window that
// were broadcast BELOW the leader's signature bar and that this committee
// member is absent from the BLS Signers list of).
//
// `quorum` nil restores the pre-0.14.0 strict rule (D10): every produced block
// expects every committee member's signature, regardless of whether the block
// closed without them. See AttestationQuorum for why that was wrong.
func ScoreBlockAttestation(blocks []vscBlocks.VscHeaderRecord, committee []string, quorum *AttestationQuorum) map[string]int {
	if len(blocks) == 0 {
		return nil
	}
	signerLists := make([][]string, 0, len(blocks))
	for _, b := range blocks {
		signerLists = append(signerLists, b.Signers)
	}
	return scoreAttestation(signerLists, committee, quorum)
}

// scoreAttestation is the one attestation scorer; both entry points project
// their block shape to signer lists and call it, so the quorum rule cannot be
// applied by one caller and skipped by the other.
func scoreAttestation(signerLists [][]string, committee []string, quorum *AttestationQuorum) map[string]int {
	if len(signerLists) == 0 {
		return nil
	}
	cm := committeeFromMembers(committee)
	if len(cm) == 0 {
		return nil
	}
	out := make(map[string]int)
	for _, list := range signerLists {
		signers := make(map[string]struct{}, len(list))
		for _, s := range list {
			signers[s] = struct{}{}
		}
		if quorum.closedAtBar(signers) {
			continue
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

// ScoreTssKeygenExclusion returns per-witness keygen-exclusion bps — the
// keygen-path analogue of ScoreTssReshareExclusion (G15). Each entry in
// `keygens` represents one canonical successful keygen commitment landing in
// the tick window. The committee for each keygen is the committee at that
// keygen's epoch (the committee elected for the epoch the keygen opened).
//
// For each keygen: any member of the new committee not represented in the
// commitment's bitset accumulates TssKeygenExclusionBps. Keygen carries the
// same participant bitset as reshare (KeyGenResult.Serialize emits Type
// "keygen" via setToCommitment — identical semantics), so this mirrors the
// reshare scorer 1:1.
//
// Strict per-key: if a witness is excluded from N keys' keygens in the tick,
// they accumulate N × TssKeygenExclusionBps before max-of and per-tick clamp.
func ScoreTssKeygenExclusion(keygens []KeygenWithCommittee) map[string]int {
	if len(keygens) == 0 {
		return nil
	}
	out := make(map[string]int)
	for _, k := range keygens {
		bits, ok := decodeBitset(k.Commitment.Commitment)
		if !ok {
			// Unparseable bitset = treat as everyone excluded would be too
			// punitive; treat as no info, no penalty. Logged at the wire
			// layer if it ever happens.
			continue
		}
		for idx, member := range k.NewCommittee {
			if member == "" {
				continue
			}
			if bits.Bit(idx) == 1 {
				continue
			}
			out[member] += TssKeygenExclusionBps
		}
	}
	return out
}

// KeygenWithCommittee pairs a keygen commitment with the new committee its
// bitset is encoded against. The aggregator must look up the election at the
// keygen's commitment.Epoch (NOT the current epoch — same requirement as
// ReshareWithCommittee).
type KeygenWithCommittee struct {
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
