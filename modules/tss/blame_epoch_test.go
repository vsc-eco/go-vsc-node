package tss

import (
	"encoding/base64"
	"math/big"
	"sort"
	"testing"
	"vsc-node/modules/db/vsc/elections"

	"github.com/chebyrash/promise"
)

// mockElectionDb implements elections.Elections with an in-memory map.
// Only GetElection is needed for setToCommitment; the rest are stubs.
type mockElectionDb struct {
	data map[uint64]*elections.ElectionResult
}

func (m *mockElectionDb) Init() error                          { return nil }
func (m *mockElectionDb) Start() *promise.Promise[any]         { return nil }
func (m *mockElectionDb) Stop() error                          { return nil }
func (m *mockElectionDb) StoreElection(elections.ElectionResult) error { return nil }
func (m *mockElectionDb) GetPreviousElections(uint64, int) []elections.ElectionResult { return nil }
func (m *mockElectionDb) GetElectionByHeight(uint64) (elections.ElectionResult, error) {
	return elections.ElectionResult{}, nil
}
func (m *mockElectionDb) GetElection(epoch uint64) *elections.ElectionResult {
	if r, ok := m.data[epoch]; ok {
		return r
	}
	return nil
}

func makeElection(epoch uint64, accounts []string) *elections.ElectionResult {
	members := make([]elections.ElectionMember, len(accounts))
	for i, a := range accounts {
		members[i] = elections.ElectionMember{Account: a}
	}
	return &elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: epoch},
		ElectionDataInfo:   elections.ElectionDataInfo{Members: members},
	}
}

// decodeBlameBitset decodes a base64 blame bitset against an election member list,
// exactly as RunActions does at tss.go:707-712.
func decodeBlameBitset(encoded string, members []elections.ElectionMember) []string {
	blameBytes, _ := base64.RawURLEncoding.DecodeString(encoded)
	bits := new(big.Int).SetBytes(blameBytes)
	var names []string
	for idx, m := range members {
		if bits.Bit(idx) == 1 {
			names = append(names, m.Account)
		}
	}
	sort.Strings(names)
	return names
}

// TestBlameEpochMismatchProvesBug proves that encoding blame against the keygen
// epoch and decoding against the current epoch produces WRONG blame targets.
// This is the actual bug: dispatcher.epoch (keygen=1379, 18 members) was used
// to encode, but RunActions decoded against currentElection (epoch=1384, 19 members).
//
// The test calls the real setToCommitment through a TssManager with a mock electionDb,
// so it exercises the actual serialization code path.
func TestBlameEpochMismatchProvesBug(t *testing.T) {
	keygenEpoch := uint64(1379)
	currentEpoch := uint64(1384)

	// 18 members at keygen
	keygenMembers := []string{
		"arcange", "atexoras", "bala", "botlord", "bradleyarrow",
		"comptroller", "delta-p", "emrebeyler", "herman", "louis",
		"mahdiyari", "mengao", "milo", "prime", "sagarkothari",
		"techcoderx", "tibfox", "v4vapp",
	}
	// 19 members now — actifit added at position 0, shifts everything
	currentMembers := []string{
		"actifit", "arcange", "atexoras", "bala", "botlord", "bradleyarrow",
		"comptroller", "delta-p", "emrebeyler", "herman", "louis",
		"mahdiyari", "mengao", "milo", "prime", "sagarkothari",
		"techcoderx", "tibfox", "v4vapp",
	}

	mockDb := &mockElectionDb{
		data: map[uint64]*elections.ElectionResult{
			keygenEpoch:  makeElection(keygenEpoch, keygenMembers),
			currentEpoch: makeElection(currentEpoch, currentMembers),
		},
	}

	tssMgr := &TssManager{electionDb: mockDb}

	culprits := []string{"prime", "emrebeyler", "comptroller"}
	participants := make([]Participant, len(culprits))
	for i, c := range culprits {
		participants[i] = Participant{Account: c}
	}
	sort.Strings(culprits)

	// ---- BUG: encode with keygen epoch (what dispatcher.epoch was) ----
	bugEncoded := tssMgr.setToCommitment(participants, keygenEpoch)

	// Decode against current election (what RunActions does)
	bugDecoded := decodeBlameBitset(bugEncoded, mockDb.data[currentEpoch].Members)

	if equal(culprits, bugDecoded) {
		t.Fatal("BUG PATH: blame roundtrip should produce WRONG targets when epochs differ, but got correct targets — test is broken")
	}
	t.Logf("BUG PATH confirmed: encoded with keygen epoch %d, decoded with current epoch %d", keygenEpoch, currentEpoch)
	t.Logf("  Intended culprits: %v", culprits)
	t.Logf("  Decoded (WRONG):   %v", bugDecoded)

	// Verify NONE of the correct culprits appear (the shift is total)
	culpritSet := make(map[string]bool)
	for _, c := range culprits {
		culpritSet[c] = true
	}
	for _, d := range bugDecoded {
		if culpritSet[d] {
			t.Errorf("BUG PATH: %q appeared in decoded targets — shift should move all indices", d)
		}
	}

	// ---- FIX: encode with current epoch (what dispatcher.newEpoch gives) ----
	fixEncoded := tssMgr.setToCommitment(participants, currentEpoch)

	// Decode against current election (same as RunActions)
	fixDecoded := decodeBlameBitset(fixEncoded, mockDb.data[currentEpoch].Members)

	if !equal(culprits, fixDecoded) {
		t.Fatalf("FIX PATH: blame roundtrip failed\n  Intended: %v\n  Decoded:  %v", culprits, fixDecoded)
	}
	t.Logf("FIX PATH confirmed: encoded with current epoch %d, decoded with current epoch %d", currentEpoch, currentEpoch)
	t.Logf("  Intended culprits: %v", culprits)
	t.Logf("  Decoded (CORRECT): %v", fixDecoded)

	// ---- Verify the bitsets are actually different ----
	if bugEncoded == fixEncoded {
		t.Fatal("Bug and fix bitsets are identical — epoch difference had no effect, test is meaningless")
	}
	t.Logf("Bitsets differ: bug=%q fix=%q", bugEncoded, fixEncoded)
}

// TestBlameEpochDispatcherField verifies that the TimeoutResult struct
// carries the epoch that setToCommitment will use, and that Serialize()
// produces a commitment decodable against the same epoch's election.
func TestBlameEpochDispatcherField(t *testing.T) {
	currentEpoch := uint64(1384)
	currentMembers := []string{
		"actifit", "arcange", "atexoras", "bala", "botlord", "bradleyarrow",
		"comptroller", "delta-p", "emrebeyler", "herman", "louis",
		"mahdiyari", "mengao", "milo", "prime", "sagarkothari",
		"techcoderx", "tibfox", "v4vapp",
	}

	mockDb := &mockElectionDb{
		data: map[uint64]*elections.ElectionResult{
			currentEpoch: makeElection(currentEpoch, currentMembers),
		},
	}

	tssMgr := &TssManager{electionDb: mockDb}

	culprits := []string{"prime", "emrebeyler", "comptroller"}
	sort.Strings(culprits)

	// This is what the dispatcher creates after the fix (line 584: Epoch: dispatcher.newEpoch)
	result := TimeoutResult{
		tssMgr:      tssMgr,
		Culprits:    culprits,
		SessionId:   "reshare-105171200-0-test",
		KeyId:       "test-key",
		BlockHeight: 105171200,
		Epoch:       currentEpoch, // dispatcher.newEpoch
	}

	serialized := result.Serialize()

	// Verify the commitment epoch matches
	if serialized.Epoch != currentEpoch {
		t.Fatalf("Serialized epoch %d != expected %d", serialized.Epoch, currentEpoch)
	}

	// Decode the commitment against the same election (what RunActions does)
	decoded := decodeBlameBitset(serialized.Commitment, mockDb.data[currentEpoch].Members)

	if !equal(culprits, decoded) {
		t.Fatalf("Serialize() roundtrip failed\n  Culprits: %v\n  Decoded:  %v\n  Commitment: %s", culprits, decoded, serialized.Commitment)
	}
	t.Logf("Serialize() roundtrip OK: culprits=%v commitment=%s epoch=%d", culprits, serialized.Commitment, serialized.Epoch)
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
