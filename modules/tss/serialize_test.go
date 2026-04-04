package tss

import (
	"sort"
	"testing"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	tss_helpers "vsc-node/modules/tss/helpers"

	"github.com/multiformats/go-multicodec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serializeToCID is a helper that reproduces the exact serialization path
// used in BLS signature collection: Serialize() -> EncodeDagCbor -> HashBytes -> CID.
func serializeToCID(t *testing.T, result DispatcherResult) string {
	t.Helper()
	commitment := result.Serialize()
	cborBytes, err := common.EncodeDagCbor(commitment)
	require.NoError(t, err, "EncodeDagCbor should not fail")
	cidVal, err := common.HashBytes(cborBytes, multicodec.DagCbor)
	require.NoError(t, err, "HashBytes should not fail")
	return cidVal.String()
}

// --- Consensus boundary tests ---

func TestSerialize_ReshareResult_IdenticalOnTwoNodes(t *testing.T) {
	// Two independent TssManagers with identical election data must produce
	// identical CIDs for the same ReshareResult inputs.
	epoch := uint64(100)
	members := []string{"alice", "bob", "carol", "dave", "eve"}

	mgr1 := &TssManager{electionDb: &test_utils.MockElectionDb{
		Elections: map[uint64]*elections.ElectionResult{epoch: makeElection(epoch, members)},
	}}
	mgr2 := &TssManager{electionDb: &test_utils.MockElectionDb{
		Elections: map[uint64]*elections.ElectionResult{epoch: makeElection(epoch, members)},
	}}

	participants := []Participant{
		{Account: "alice"}, {Account: "bob"}, {Account: "carol"},
		{Account: "dave"}, {Account: "eve"},
	}

	result1 := ReshareResult{
		Commitment:  mgr1.setToCommitment(participants, epoch),
		KeyId:       "key-1",
		SessionId:   "reshare-1000-0-key-1",
		NewEpoch:    epoch,
		BlockHeight: 1000,
	}
	result2 := ReshareResult{
		Commitment:  mgr2.setToCommitment(participants, epoch),
		KeyId:       "key-1",
		SessionId:   "reshare-1000-0-key-1",
		NewEpoch:    epoch,
		BlockHeight: 1000,
	}

	cid1 := serializeToCID(t, result1)
	cid2 := serializeToCID(t, result2)
	assert.Equal(t, cid1, cid2, "Two nodes with identical inputs must produce identical CIDs")
}

func TestSerialize_TimeoutResult_IdenticalWithSameCulprits(t *testing.T) {
	epoch := uint64(50)
	members := []string{"alice", "bob", "carol", "dave", "eve"}

	mgr1 := &TssManager{electionDb: &test_utils.MockElectionDb{
		Elections: map[uint64]*elections.ElectionResult{epoch: makeElection(epoch, members)},
	}}
	mgr2 := &TssManager{electionDb: &test_utils.MockElectionDb{
		Elections: map[uint64]*elections.ElectionResult{epoch: makeElection(epoch, members)},
	}}

	culprits := []string{"bob", "dave"}
	sort.Strings(culprits)

	tr1 := TimeoutResult{
		tssMgr: mgr1, Culprits: culprits,
		SessionId: "reshare-500-0-key-1", KeyId: "key-1",
		BlockHeight: 500, Epoch: epoch,
	}
	tr2 := TimeoutResult{
		tssMgr: mgr2, Culprits: culprits,
		SessionId: "reshare-500-0-key-1", KeyId: "key-1",
		BlockHeight: 500, Epoch: epoch,
	}

	cid1 := serializeToCID(t, tr1)
	cid2 := serializeToCID(t, tr2)
	assert.Equal(t, cid1, cid2, "TimeoutResults with same culprits must produce identical CIDs")
}

func TestSerialize_TimeoutResult_DivergesWithDifferentCulprits(t *testing.T) {
	// Documents the consensus failure mode: if two nodes compute different
	// culprit sets, their CIDs will differ and BLS collection will fail.
	epoch := uint64(50)
	members := []string{"alice", "bob", "carol", "dave", "eve"}

	mgr := &TssManager{electionDb: &test_utils.MockElectionDb{
		Elections: map[uint64]*elections.ElectionResult{epoch: makeElection(epoch, members)},
	}}

	trA := TimeoutResult{
		tssMgr: mgr, Culprits: []string{"bob"},
		SessionId: "reshare-500-0-key-1", KeyId: "key-1",
		BlockHeight: 500, Epoch: epoch,
	}
	trB := TimeoutResult{
		tssMgr: mgr, Culprits: []string{"bob", "dave"},
		SessionId: "reshare-500-0-key-1", KeyId: "key-1",
		BlockHeight: 500, Epoch: epoch,
	}

	cidA := serializeToCID(t, trA)
	cidB := serializeToCID(t, trB)
	assert.NotEqual(t, cidA, cidB, "Different culprit sets must produce different CIDs (consensus failure mode)")
}

func TestSerialize_ErrorVsTimeout_Diverges(t *testing.T) {
	// If one node hits ErrorResult and another hits TimeoutResult for the same
	// culprit, their CIDs will differ because the Type metadata differs
	// (even though Metadata is excluded from CBOR via json:"-", the commitment
	// bitsets can differ if culprit lists differ due to different code paths).
	epoch := uint64(50)
	members := []string{"alice", "bob", "carol", "dave", "eve"}

	mgr := &TssManager{electionDb: &test_utils.MockElectionDb{
		Elections: map[uint64]*elections.ElectionResult{epoch: makeElection(epoch, members)},
	}}

	// Timeout: node A sees bob and dave as culprits
	trA := TimeoutResult{
		tssMgr: mgr, Culprits: []string{"bob", "dave"},
		SessionId: "reshare-500-0-key-1", KeyId: "key-1",
		BlockHeight: 500, Epoch: epoch,
	}

	// Error: node B only catches bob via tssErr.Culprits()
	// We can't easily construct a btss.Error, so we test with a regular error
	// which produces an empty commitment — guaranteed to diverge.
	erB := ErrorResult{
		tssMgr: mgr, err: assert.AnError,
		SessionId: "reshare-500-0-key-1", KeyId: "key-1",
		BlockHeight: 500, Epoch: epoch,
	}

	cidA := serializeToCID(t, trA)
	cidB := serializeToCID(t, erB)
	assert.NotEqual(t, cidA, cidB,
		"ErrorResult (regular error, empty commitment) vs TimeoutResult must produce different CIDs")
}

func TestSerialize_SetToCommitment_OrderIndependent(t *testing.T) {
	// setToCommitment uses bitset positions, so participant order shouldn't matter.
	epoch := uint64(10)
	members := []string{"alice", "bob", "carol", "dave", "eve"}

	mgr := &TssManager{electionDb: &test_utils.MockElectionDb{
		Elections: map[uint64]*elections.ElectionResult{epoch: makeElection(epoch, members)},
	}}

	order1 := []Participant{{Account: "alice"}, {Account: "carol"}, {Account: "eve"}}
	order2 := []Participant{{Account: "eve"}, {Account: "alice"}, {Account: "carol"}}
	order3 := []Participant{{Account: "carol"}, {Account: "eve"}, {Account: "alice"}}

	c1 := mgr.setToCommitment(order1, epoch)
	c2 := mgr.setToCommitment(order2, epoch)
	c3 := mgr.setToCommitment(order3, epoch)

	assert.Equal(t, c1, c2, "Participant order must not affect commitment")
	assert.Equal(t, c2, c3, "Participant order must not affect commitment")
}

func TestSerialize_SetToCommitment_MissingParticipant(t *testing.T) {
	// A participant not in the election should not set any bit and should not panic.
	epoch := uint64(10)
	members := []string{"alice", "bob", "carol"}

	mgr := &TssManager{electionDb: &test_utils.MockElectionDb{
		Elections: map[uint64]*elections.ElectionResult{epoch: makeElection(epoch, members)},
	}}

	participants := []Participant{
		{Account: "alice"},
		{Account: "unknown_node"}, // not in election
		{Account: "carol"},
	}

	commitment := mgr.setToCommitment(participants, epoch)

	// Decode and verify only alice and carol are set
	decoded := decodeBlameBitset(commitment, members2electionMembers(members))
	sort.Strings(decoded)
	assert.Equal(t, []string{"alice", "carol"}, decoded,
		"Missing participant should be silently skipped")
}

func TestSerialize_CBOR_Determinism(t *testing.T) {
	// Same BaseCommitment serialized twice must produce byte-identical CBOR.
	commitment := tss_helpers.BaseCommitment{
		Type:        "reshare",
		SessionId:   "reshare-1000-0-key-1",
		KeyId:       "key-1",
		Commitment:  "AQIDBA",
		PublicKey:   nil,
		Metadata:    nil,
		BlockHeight: 1000,
		Epoch:       50,
	}

	bytes1, err1 := common.EncodeDagCbor(commitment)
	require.NoError(t, err1)
	bytes2, err2 := common.EncodeDagCbor(commitment)
	require.NoError(t, err2)

	assert.Equal(t, bytes1, bytes2, "CBOR encoding must be deterministic")
}

func TestSerialize_CBOR_MetadataExcluded(t *testing.T) {
	// Metadata has json:"-", so different metadata values must produce identical CBOR.
	err1 := "timeout"
	err2 := "different error message"
	reason := "some reason"

	c1 := tss_helpers.BaseCommitment{
		Type: "blame", SessionId: "s1", KeyId: "k1",
		Commitment: "AA", BlockHeight: 100, Epoch: 10,
		Metadata: &tss_helpers.CommitmentMetadata{Error: &err1},
	}
	c2 := tss_helpers.BaseCommitment{
		Type: "blame", SessionId: "s1", KeyId: "k1",
		Commitment: "AA", BlockHeight: 100, Epoch: 10,
		Metadata: &tss_helpers.CommitmentMetadata{Error: &err2, Reason: &reason},
	}
	c3 := tss_helpers.BaseCommitment{
		Type: "blame", SessionId: "s1", KeyId: "k1",
		Commitment: "AA", BlockHeight: 100, Epoch: 10,
		Metadata: nil,
	}

	bytes1, _ := common.EncodeDagCbor(c1)
	bytes2, _ := common.EncodeDagCbor(c2)
	bytes3, _ := common.EncodeDagCbor(c3)

	assert.Equal(t, bytes1, bytes2, "Different Metadata values must produce identical CBOR")
	assert.Equal(t, bytes2, bytes3, "Metadata nil vs populated must produce identical CBOR")
}

func TestSerialize_BlameEpochMismatch_CIDDiverges(t *testing.T) {
	// Extension of blame_epoch_test.go: prove that encoding blame with wrong epoch
	// produces a different CID, which means BLS collection will fail.
	keygenEpoch := uint64(1)
	currentEpoch := uint64(2)

	// Different member lists between epochs (member added)
	keygenMembers := []string{"alice", "bob", "carol"}
	currentMembers := []string{"alice", "bob", "carol", "dave"}

	mgr := &TssManager{electionDb: &test_utils.MockElectionDb{
		Elections: map[uint64]*elections.ElectionResult{
			keygenEpoch:  makeElection(keygenEpoch, keygenMembers),
			currentEpoch: makeElection(currentEpoch, currentMembers),
		},
	}}

	culprits := []string{"carol"}
	participants := []Participant{{Account: "carol"}}

	// Node A encodes with wrong epoch (bug path)
	commitmentWrong := mgr.setToCommitment(participants, keygenEpoch)
	resultWrong := TimeoutResult{
		tssMgr: mgr, Culprits: culprits,
		SessionId: "reshare-100-0-k1", KeyId: "k1",
		BlockHeight: 100, Epoch: keygenEpoch,
	}

	// Node B encodes with correct epoch (fix path)
	commitmentCorrect := mgr.setToCommitment(participants, currentEpoch)
	resultCorrect := TimeoutResult{
		tssMgr: mgr, Culprits: culprits,
		SessionId: "reshare-100-0-k1", KeyId: "k1",
		BlockHeight: 100, Epoch: currentEpoch,
	}

	_ = commitmentWrong
	_ = commitmentCorrect

	cidWrong := serializeToCID(t, resultWrong)
	cidCorrect := serializeToCID(t, resultCorrect)

	assert.NotEqual(t, cidWrong, cidCorrect,
		"Encoding blame with wrong epoch must produce different CID (consensus failure)")
}

func TestSerialize_ReshareResult_DifferentEpoch_DifferentCID(t *testing.T) {
	// If two nodes use different epochs for the same participants,
	// the commitment bitset may differ (different election member lists)
	// or at minimum the Epoch field differs.
	epoch1 := uint64(10)
	epoch2 := uint64(20)
	members := []string{"alice", "bob", "carol"}

	mgr := &TssManager{electionDb: &test_utils.MockElectionDb{
		Elections: map[uint64]*elections.ElectionResult{
			epoch1: makeElection(epoch1, members),
			epoch2: makeElection(epoch2, members),
		},
	}}

	participants := []Participant{{Account: "alice"}, {Account: "bob"}, {Account: "carol"}}

	r1 := ReshareResult{
		Commitment: mgr.setToCommitment(participants, epoch1),
		KeyId: "k1", SessionId: "reshare-100-0-k1",
		NewEpoch: epoch1, BlockHeight: 100,
	}
	r2 := ReshareResult{
		Commitment: mgr.setToCommitment(participants, epoch2),
		KeyId: "k1", SessionId: "reshare-100-0-k1",
		NewEpoch: epoch2, BlockHeight: 100,
	}

	cid1 := serializeToCID(t, r1)
	cid2 := serializeToCID(t, r2)
	assert.NotEqual(t, cid1, cid2,
		"Different epochs must produce different CIDs even with same member names")
}

// --- Helpers ---

func members2electionMembers(accounts []string) []elections.ElectionMember {
	members := make([]elections.ElectionMember, len(accounts))
	for i, a := range accounts {
		members[i] = elections.ElectionMember{Account: a}
	}
	return members
}
