package tss

import (
	"encoding/base64"
	"fmt"
	"math/big"
	"testing"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/elections"
	tss_db "vsc-node/modules/db/vsc/tss"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blamesToCommitments converts the test-friendly blamesByEpoch map into the
// format expected by test_utils.MockTssCommitmentsDb.
func blamesToCommitments(blamesByEpoch map[uint64][]tss_db.TssCommitment) map[string]tss_db.TssCommitment {
	result := make(map[string]tss_db.TssCommitment)
	for epoch, blames := range blamesByEpoch {
		for i, blame := range blames {
			key := fmt.Sprintf("%s:%d:%s:%d", blame.KeyId, epoch, blame.Type, i)
			result[key] = blame
		}
	}
	return result
}

// makeBlameBitset creates a base64-encoded bitset marking specific member indices.
func makeBlameBitset(indices ...int) string {
	bits := big.NewInt(0)
	for _, idx := range indices {
		bits.SetBit(bits, idx, 1)
	}
	return base64.RawURLEncoding.EncodeToString(bits.Bytes())
}

func newBlameScoreMgr(
	currentElection elections.ElectionResult,
	previousElections []elections.ElectionResult,
	blamesByEpoch map[uint64][]tss_db.TssCommitment,
) *TssManager {
	electionsByHeight := map[uint64]elections.ElectionResult{
		// BlameScore calls GetElectionByHeight(MaxInt64-1)
		0: currentElection,
	}

	electionsByEpoch := make(map[uint64]*elections.ElectionResult)
	ce := currentElection
	electionsByEpoch[currentElection.Epoch] = &ce
	for i := range previousElections {
		pe := previousElections[i]
		electionsByEpoch[pe.Epoch] = &pe
	}

	return &TssManager{
		electionDb: &test_utils.MockElectionDb{
			Elections:         electionsByEpoch,
			ElectionsByHeight: electionsByHeight,
			PreviousElections: previousElections,
		},
		tssCommitments: &test_utils.MockTssCommitmentsDb{
			Commitments: blamesToCommitments(blamesByEpoch),
		},
		metrics: &Metrics{BlameCount: make(map[string]int64)},
	}
}

// --- BlameScore edge case tests ---

func TestBlameScore_CorruptedBase64(t *testing.T) {
	// Corrupted blame commitment (invalid base64) should not panic or cause incorrect bans.
	current := makeElectionResult(10, []string{"alice", "bob", "carol"})
	epoch10 := uint64(10)

	mgr := newBlameScoreMgr(current, nil, map[uint64][]tss_db.TssCommitment{
		epoch10: {
			{Type: "blame", Epoch: 10, Commitment: "!!!INVALID_BASE64!!!", KeyId: "k1"},
		},
	})

	result := mgr.BlameScore()

	// Should not panic, and no node should be banned from corrupt data
	assert.Empty(t, result.BannedNodes,
		"Corrupted base64 blame should not cause bans")
}

func TestBlameScore_EmptyElectionInWindow(t *testing.T) {
	// Election with 0 members in blame window should not cause division by zero.
	current := makeElectionResult(10, []string{"alice", "bob"})
	emptyElection := elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 5},
		ElectionDataInfo:   elections.ElectionDataInfo{Members: nil},
	}

	mgr := newBlameScoreMgr(current, []elections.ElectionResult{emptyElection}, map[uint64][]tss_db.TssCommitment{})

	result := mgr.BlameScore()

	// Should not panic
	assert.NotNil(t, result.BannedNodes)
}

func TestBlameScore_AllNodesBanned(t *testing.T) {
	// If every node exceeds the blame threshold, all should be banned.
	// This tests what happens when the entire committee is unreliable.
	accounts := []string{"alice", "bob", "carol", "dave"}
	current := makeElectionResult(10, accounts)

	// Previous elections to establish history beyond grace period
	prev := []elections.ElectionResult{
		makeElectionResult(7, accounts),
		makeElectionResult(4, accounts),
		makeElectionResult(1, accounts),
	}

	// All 4 nodes blamed in every epoch (100% failure rate)
	blames := map[uint64][]tss_db.TssCommitment{
		10: {{Type: "blame", Epoch: 10, Commitment: makeBlameBitset(0, 1, 2, 3), KeyId: "k1"}},
		7:  {{Type: "blame", Epoch: 7, Commitment: makeBlameBitset(0, 1, 2, 3), KeyId: "k1"}},
		4:  {{Type: "blame", Epoch: 4, Commitment: makeBlameBitset(0, 1, 2, 3), KeyId: "k1"}},
		1:  {{Type: "blame", Epoch: 1, Commitment: makeBlameBitset(0, 1, 2, 3), KeyId: "k1"}},
	}

	mgr := newBlameScoreMgr(current, prev, blames)
	result := mgr.BlameScore()

	// All 4 should be banned (100% failure > 60% threshold)
	for _, acct := range accounts {
		assert.True(t, result.BannedNodes[acct],
			"Node %s should be banned at 100%% failure rate", acct)
	}
}

func TestBlameScore_ExactBanThreshold(t *testing.T) {
	// Node at exactly 60% failure rate should be banned (> check, not >=).
	// TSS_BAN_THRESHOLD_PERCENT = 60, check is: Score > Weight * 60 / 100
	accounts := []string{"alice", "bob", "carol"}
	current := makeElectionResult(10, accounts)

	prev := []elections.ElectionResult{
		makeElectionResult(7, accounts),
		makeElectionResult(4, accounts),
		makeElectionResult(1, accounts),
	}

	// Alice blamed in 3 out of 4 epochs = weight 4 (each epoch has 1 blame, 4 epochs total)
	// But we need: Score > Weight * 60/100
	// With 4 blames total (weight=4), and alice blamed in all 4: Score=4, Weight=4
	// 4 > 4*60/100 = 4 > 2.4 → banned
	// With alice blamed in 2 of 4: Score=2, Weight=4
	// 2 > 4*60/100 = 2 > 2.4 → NOT banned (just below)

	// First test: 3 of 5 blame opportunities → 60% exactly
	// Make 5 blames total, alice in 3 of them
	epoch10 := uint64(10)
	epoch7 := uint64(7)
	epoch4 := uint64(4)
	blames := map[uint64][]tss_db.TssCommitment{
		epoch10: {
			{Type: "blame", Epoch: 10, Commitment: makeBlameBitset(0), KeyId: "k1"},     // alice
			{Type: "blame", Epoch: 10, Commitment: makeBlameBitset(1), KeyId: "k1"},     // bob only
		},
		epoch7: {
			{Type: "blame", Epoch: 7, Commitment: makeBlameBitset(0), KeyId: "k1"},      // alice
		},
		epoch4: {
			{Type: "blame", Epoch: 4, Commitment: makeBlameBitset(0), KeyId: "k1"},      // alice
			{Type: "blame", Epoch: 4, Commitment: makeBlameBitset(1, 2), KeyId: "k1"},   // bob, carol
		},
	}

	mgr := newBlameScoreMgr(current, prev, blames)
	result := mgr.BlameScore()

	// alice: weight = 2+1+2 = 5 (blame count per epoch she's a member), score = 3
	// 3 > 5*60/100 = 3 > 3 → FALSE (not strictly greater)
	assert.False(t, result.BannedNodes["alice"],
		"Node at exactly 60%% failure rate (3/5) should NOT be banned (> not >=)")
}

func TestBlameScore_GracePeriodWithEpochGaps(t *testing.T) {
	// New node with non-contiguous epochs. Grace period should be based on
	// epochsSinceFirst = currentEpoch - firstEpoch, not number of elections.
	accounts := []string{"alice", "bob", "carol"}
	current := makeElectionResult(100, accounts)

	// carol only appeared at epoch 99 (gap from epoch 90)
	prev := []elections.ElectionResult{
		makeElectionResult(99, accounts),
		makeElectionResult(90, []string{"alice", "bob"}), // carol not present
	}

	// carol blamed at 100% (2/2 epochs she's in)
	epoch100 := uint64(100)
	epoch99 := uint64(99)
	blames := map[uint64][]tss_db.TssCommitment{
		epoch100: {{Type: "blame", Epoch: 100, Commitment: makeBlameBitset(2), KeyId: "k1"}},
		epoch99:  {{Type: "blame", Epoch: 99, Commitment: makeBlameBitset(2), KeyId: "k1"}},
	}

	mgr := newBlameScoreMgr(current, prev, blames)
	result := mgr.BlameScore()

	// carol's epochsSinceFirst = 100 - 99 = 1, which is < 3 grace period
	assert.False(t, result.BannedNodes["carol"],
		"Carol should be exempt from ban (epochsSinceFirst=1, grace period=3)")
}

func TestBlameScore_ManyEpochsAccumulation(t *testing.T) {
	// Verify blame accumulation across many epochs (close to TSS_BLAME_EPOCH_COUNT=27).
	accounts := []string{"alice", "bob"}
	current := makeElectionResult(30, accounts)

	prev := make([]elections.ElectionResult, 0)
	blames := make(map[uint64][]tss_db.TssCommitment)

	// Create 25 previous epochs with alice blamed in all of them
	for i := 29; i >= 5; i-- {
		e := makeElectionResult(uint64(i), accounts)
		prev = append(prev, e)
		epoch := uint64(i)
		blames[epoch] = []tss_db.TssCommitment{
			{Type: "blame", Epoch: epoch, Commitment: makeBlameBitset(0), KeyId: "k1"},
		}
	}
	// Also blame alice in current epoch
	epoch30 := uint64(30)
	blames[epoch30] = []tss_db.TssCommitment{
		{Type: "blame", Epoch: 30, Commitment: makeBlameBitset(0), KeyId: "k1"},
	}

	mgr := newBlameScoreMgr(current, prev, blames)
	result := mgr.BlameScore()

	// alice blamed in all epochs → 100% failure → banned
	assert.True(t, result.BannedNodes["alice"],
		"Alice with 100%% failure across 26 epochs should be banned")
	// bob never blamed → not banned
	assert.False(t, result.BannedNodes["bob"],
		"Bob with 0%% failure should not be banned")
}

func TestBlameScore_MultipleBlamesPerEpoch(t *testing.T) {
	// Multiple blame commitments in one epoch for different keys.
	accounts := []string{"alice", "bob", "carol"}
	current := makeElectionResult(10, accounts)

	prev := []elections.ElectionResult{
		makeElectionResult(7, accounts),
		makeElectionResult(4, accounts),
	}

	epoch10 := uint64(10)
	blames := map[uint64][]tss_db.TssCommitment{
		epoch10: {
			// Three different blame events in same epoch
			{Type: "blame", Epoch: 10, Commitment: makeBlameBitset(0), KeyId: "k1"},
			{Type: "blame", Epoch: 10, Commitment: makeBlameBitset(0), KeyId: "k2"},
			{Type: "blame", Epoch: 10, Commitment: makeBlameBitset(0), KeyId: "k3"},
		},
	}

	mgr := newBlameScoreMgr(current, prev, blames)
	result := mgr.BlameScore()

	// alice: weight from epoch 10 = 3 (3 blames, so 3 opportunities), score = 3
	// epoch 7 and 4 have 0 blames so weight doesn't increase from those
	// 3 > 3*60/100 = 3 > 1.8 → TRUE → banned
	assert.True(t, result.BannedNodes["alice"],
		"Alice with 100%% failure (3/3 blames in one epoch) should be banned")
}

func TestBlameScore_ElectionLookupFailure(t *testing.T) {
	// If GetElectionByHeight fails, BlameScore should return empty ban list.
	mgr := &TssManager{
		electionDb: &test_utils.MockElectionDb{
			ElectionsByHeight: map[uint64]elections.ElectionResult{}, // empty - will fail
		},
		tssCommitments: &test_utils.MockTssCommitmentsDb{
			Commitments: make(map[string]tss_db.TssCommitment),
		},
		metrics: &Metrics{BlameCount: make(map[string]int64)},
	}

	result := mgr.BlameScore()

	assert.NotNil(t, result.BannedNodes)
	assert.Empty(t, result.BannedNodes,
		"Election lookup failure should produce empty ban list, not panic")
}

func TestBlameScore_TimeoutVsErrorClassification(t *testing.T) {
	// Verify that timeout blames are correctly distinguished from error blames.
	accounts := []string{"alice", "bob"}
	current := makeElectionResult(10, accounts)

	prev := []elections.ElectionResult{
		makeElectionResult(7, accounts),
		makeElectionResult(4, accounts),
	}

	timeoutErr := "timeout"
	regularErr := "SSID mismatch"

	epoch10 := uint64(10)
	blames := map[uint64][]tss_db.TssCommitment{
		epoch10: {
			{
				Type: "blame", Epoch: 10, KeyId: "k1",
				Commitment: makeBlameBitset(0),
				Metadata:   &tss_db.CommitmentMetadata{Error: &timeoutErr},
			},
			{
				Type: "blame", Epoch: 10, KeyId: "k2",
				Commitment: makeBlameBitset(0),
				Metadata:   &tss_db.CommitmentMetadata{Error: &regularErr},
			},
		},
	}

	mgr := newBlameScoreMgr(current, prev, blames)
	result := mgr.BlameScore()

	require.NotNil(t, result)
	// Both timeout and error blames should contribute to the total score.
	// alice: 2 blames out of 2 opportunities in epoch 10 = 100% → banned
	assert.True(t, result.BannedNodes["alice"],
		"Both timeout and error blames should contribute to ban score")
}

func TestBlameScore_NoBlameMeansNoBan(t *testing.T) {
	// Sanity: nodes with zero blames should never be banned.
	accounts := []string{"alice", "bob", "carol"}
	current := makeElectionResult(10, accounts)

	prev := []elections.ElectionResult{
		makeElectionResult(7, accounts),
		makeElectionResult(4, accounts),
	}

	mgr := newBlameScoreMgr(current, prev, map[uint64][]tss_db.TssCommitment{})
	result := mgr.BlameScore()

	assert.Empty(t, result.BannedNodes, "No blames should mean no bans")
}

func TestBlameScore_NodeNotInCurrentElection(t *testing.T) {
	// A node blamed in a previous epoch but not in the current election
	// should not appear in the ban list (BlameScore only scores current members).
	current := makeElectionResult(10, []string{"alice", "bob"})
	prev := []elections.ElectionResult{
		makeElectionResult(7, []string{"alice", "bob", "carol"}),
		makeElectionResult(4, []string{"alice", "bob", "carol"}),
	}

	// carol blamed at 100% in epoch 7 but not in current election
	epoch7 := uint64(7)
	blames := map[uint64][]tss_db.TssCommitment{
		epoch7: {{Type: "blame", Epoch: 7, Commitment: makeBlameBitset(2), KeyId: "k1"}},
	}

	mgr := newBlameScoreMgr(current, prev, blames)
	result := mgr.BlameScore()

	assert.False(t, result.BannedNodes["carol"],
		"Carol is not in current election and should not be scored/banned")
}
