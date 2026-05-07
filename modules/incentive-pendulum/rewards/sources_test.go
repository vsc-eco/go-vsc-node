package rewards

import (
	"encoding/base64"
	"math/big"
	"testing"

	tss_db "vsc-node/modules/db/vsc/tss"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
)

// encodeBitset is a test helper that mirrors the production encoding
// (setToCommitment in tss.go and SerializeBlsCircuit in lib/dids/bls.go):
// big.Int.Bytes() base64-RawURL.
func encodeBitset(setIndices ...int) string {
	bs := new(big.Int)
	for _, i := range setIndices {
		bs.SetBit(bs, i, 1)
	}
	return base64.RawURLEncoding.EncodeToString(bs.Bytes())
}

func TestScoreBlockProduction_OnlyMissedSlotsCount(t *testing.T) {
	committee := []string{"alice", "bob"}
	slots := []SlotProposer{
		{SlotHeight: 100, Account: "alice"},
		{SlotHeight: 110, Account: "bob"},
		{SlotHeight: 120, Account: "alice"},
	}
	produced := map[uint64]struct{}{
		100: {}, // alice produced
		110: {}, // bob produced
		// 120: alice missed
	}
	got := ScoreBlockProduction(slots, produced, committee)
	if got["alice"] != BlockProductionMissBps {
		t.Errorf("alice: got %d want %d", got["alice"], BlockProductionMissBps)
	}
	if _, ok := got["bob"]; ok {
		t.Errorf("bob should have no penalty: %d", got["bob"])
	}
}

func TestScoreBlockProduction_NonCommitteeProposerIgnored(t *testing.T) {
	committee := []string{"alice"}
	slots := []SlotProposer{
		{SlotHeight: 100, Account: "stranger"}, // not in committee
	}
	got := ScoreBlockProduction(slots, nil, committee)
	if len(got) != 0 {
		t.Fatalf("expected non-committee proposer to be skipped, got %v", got)
	}
}

func TestScoreBlockAttestation_StrictlyEveryBlockCounts(t *testing.T) {
	committee := []string{"alice", "bob"}
	blocks := []vscBlocks.VscHeaderRecord{
		{Signers: []string{"alice"}}, // bob missed
		{Signers: []string{"alice"}}, // bob missed
		{Signers: []string{"alice", "bob"}},
	}
	got := ScoreBlockAttestation(blocks, committee)
	if got["alice"] != 0 {
		t.Errorf("alice should have 0 misses, got %d", got["alice"])
	}
	if got["bob"] != 2*BlockAttestationMissBps {
		t.Errorf("bob: got %d want %d (2 misses × %d)", got["bob"], 2*BlockAttestationMissBps, BlockAttestationMissBps)
	}
}

func TestScoreTssReshareExclusion_StrictPerKey(t *testing.T) {
	// Two keys' reshares; alice excluded from key A only, bob from both.
	in := []ReshareWithCommittee{
		{
			Commitment:   tss_db.TssCommitment{Type: "reshare", Commitment: encodeBitset(2)}, // only carol set
			NewCommittee: []string{"alice", "bob", "carol"},
		},
		{
			Commitment:   tss_db.TssCommitment{Type: "reshare", Commitment: encodeBitset(0, 2)}, // alice + carol
			NewCommittee: []string{"alice", "bob", "carol"},
		},
	}
	got := ScoreTssReshareExclusion(in)
	if got["alice"] != TssReshareExclusionBps {
		t.Errorf("alice: got %d want %d (excluded from one key)", got["alice"], TssReshareExclusionBps)
	}
	if got["bob"] != 2*TssReshareExclusionBps {
		t.Errorf("bob: got %d want %d (excluded from both keys)", got["bob"], 2*TssReshareExclusionBps)
	}
	if _, ok := got["carol"]; ok {
		t.Errorf("carol should have no penalty (in both): %d", got["carol"])
	}
}

func TestScoreTssReshareExclusion_BadBitsetSkipped(t *testing.T) {
	in := []ReshareWithCommittee{
		{
			Commitment:   tss_db.TssCommitment{Type: "reshare", Commitment: "not-base64!"},
			NewCommittee: []string{"alice"},
		},
	}
	got := ScoreTssReshareExclusion(in)
	if len(got) != 0 {
		t.Fatalf("bad bitset should be skipped, got %v", got)
	}
}

func TestScoreTssBlame_BlamedWitnessesPenalized(t *testing.T) {
	in := []BlameWithCommittee{
		{
			Commitment:     tss_db.TssCommitment{Type: "blame", Commitment: encodeBitset(0, 2)}, // alice + carol blamed
			BlameCommittee: []string{"alice", "bob", "carol"},
		},
	}
	got := ScoreTssBlame(in)
	if got["alice"] != TssBlameBps {
		t.Errorf("alice: got %d want %d", got["alice"], TssBlameBps)
	}
	if _, ok := got["bob"]; ok {
		t.Errorf("bob should not be blamed: %d", got["bob"])
	}
	if got["carol"] != TssBlameBps {
		t.Errorf("carol: got %d want %d", got["carol"], TssBlameBps)
	}
}

func TestScoreTssSignNonParticipation_LegacyEmptyBitsetSkipped(t *testing.T) {
	in := []SignResultWithCommittee{
		{
			// Pre-BitSet records have empty BitSet — must contribute zero.
			Commitment:    tss_db.TssCommitment{Type: "sign_result", BitSet: ""},
			SignCommittee: []string{"alice", "bob"},
		},
	}
	got := ScoreTssSignNonParticipation(in)
	if len(got) != 0 {
		t.Fatalf("legacy empty BitSet should contribute zero, got %v", got)
	}
}

func TestScoreTssSignNonParticipation_AbsentMembersPenalized(t *testing.T) {
	in := []SignResultWithCommittee{
		{
			// Only alice (idx 0) signed; bob (idx 1) and carol (idx 2) didn't.
			Commitment:    tss_db.TssCommitment{Type: "sign_result", BitSet: encodeBitset(0)},
			SignCommittee: []string{"alice", "bob", "carol"},
		},
	}
	got := ScoreTssSignNonParticipation(in)
	if _, ok := got["alice"]; ok {
		t.Errorf("alice signed; should have no penalty: %d", got["alice"])
	}
	if got["bob"] != TssSignNonParticipationBps {
		t.Errorf("bob: got %d want %d", got["bob"], TssSignNonParticipationBps)
	}
	if got["carol"] != TssSignNonParticipationBps {
		t.Errorf("carol: got %d want %d", got["carol"], TssSignNonParticipationBps)
	}
}

func TestScoreOracleQuoteDivergence_ChargesEachDivergentCommitteeMember(t *testing.T) {
	committee := []string{"alice", "bob", "carol"}
	got := ScoreOracleQuoteDivergence([]string{"bob"}, committee)
	if got["bob"] != OracleQuoteDivergenceBps {
		t.Errorf("bob: got %d want %d", got["bob"], OracleQuoteDivergenceBps)
	}
	if _, ok := got["alice"]; ok {
		t.Errorf("alice should not be penalized when not divergent")
	}
}

func TestScoreOracleQuoteDivergence_DropsNonCommittee(t *testing.T) {
	committee := []string{"alice", "bob"}
	// "carol" is divergent but not on the rewardable committee — drop her.
	got := ScoreOracleQuoteDivergence([]string{"carol", "alice"}, committee)
	if got["alice"] != OracleQuoteDivergenceBps {
		t.Errorf("alice: got %d want %d", got["alice"], OracleQuoteDivergenceBps)
	}
	if _, ok := got["carol"]; ok {
		t.Errorf("carol is non-committee; should not appear: %d", got["carol"])
	}
}

func TestScoreOracleQuoteDivergence_DedupesRepeatedAccounts(t *testing.T) {
	committee := []string{"alice"}
	// Same account listed twice in one tick must not double-charge.
	got := ScoreOracleQuoteDivergence([]string{"alice", "alice"}, committee)
	if got["alice"] != OracleQuoteDivergenceBps {
		t.Errorf("alice: got %d want %d (must be deduped)", got["alice"], OracleQuoteDivergenceBps)
	}
}

func TestScoreOracleQuoteDivergence_EmptyInputs(t *testing.T) {
	if got := ScoreOracleQuoteDivergence(nil, []string{"alice"}); len(got) != 0 {
		t.Fatalf("empty divergent list should yield empty map, got %v", got)
	}
	if got := ScoreOracleQuoteDivergence([]string{"alice"}, nil); len(got) != 0 {
		t.Fatalf("empty committee should yield empty map, got %v", got)
	}
	if got := ScoreOracleQuoteDivergence([]string{""}, []string{"alice"}); len(got) != 0 {
		t.Fatalf("blank witness must be dropped, got %v", got)
	}
}
