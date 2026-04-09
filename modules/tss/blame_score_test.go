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
	// When all nodes are blamed in every commit, those commits are systemic
	// (blamed count > threshold) and are filtered out. No nodes should be banned.
	accounts := []string{"alice", "bob", "carol", "dave"}
	current := makeElectionResult(10, accounts)

	prev := []elections.ElectionResult{
		makeElectionResult(7, accounts),
		makeElectionResult(4, accounts),
		makeElectionResult(1, accounts),
	}

	// All 4 nodes blamed in every epoch — systemic failure
	// threshold=2, maxBlamed = 4-(2+1) = 1, blaming 4 > 1 → filtered
	blames := map[uint64][]tss_db.TssCommitment{
		10: {{Type: "blame", Epoch: 10, Commitment: makeBlameBitset(0, 1, 2, 3), KeyId: "k1"}},
		7:  {{Type: "blame", Epoch: 7, Commitment: makeBlameBitset(0, 1, 2, 3), KeyId: "k1"}},
		4:  {{Type: "blame", Epoch: 4, Commitment: makeBlameBitset(0, 1, 2, 3), KeyId: "k1"}},
		1:  {{Type: "blame", Epoch: 1, Commitment: makeBlameBitset(0, 1, 2, 3), KeyId: "k1"}},
	}

	mgr := newBlameScoreMgr(current, prev, blames)
	result := mgr.BlameScore()

	// Systemic blames are skipped — no individual node should be banned
	for _, acct := range accounts {
		assert.False(t, result.BannedNodes[acct],
			"Node %s should NOT be banned — all blames are systemic (all nodes blamed)", acct)
	}
}

func TestBlameScore_BanCap(t *testing.T) {
	// Even with many individual blames, the ban cap prevents banning so many
	// nodes that the network becomes inoperable.
	// 6 members: threshold=3, maxBlamed = 6-4 = 2, maxBans = 6-4 = 2
	accounts := []string{"alice", "bob", "carol", "dave", "eve", "frank"}
	current := makeElectionResult(10, accounts)

	prev := []elections.ElectionResult{
		makeElectionResult(7, accounts),
		makeElectionResult(4, accounts),
		makeElectionResult(1, accounts),
	}

	// Each epoch has 1 blame commit naming 1 node.
	// 1 <= maxBlamed(2), so none are systemic. Each non-systemic commit
	// adds weight=1 to all members. alice: score=2 (epochs 10,1), weight=4.
	blames := map[uint64][]tss_db.TssCommitment{
		10: {{Type: "blame", Epoch: 10, Commitment: makeBlameBitset(0), KeyId: "k1"}},
		7:  {{Type: "blame", Epoch: 7, Commitment: makeBlameBitset(1), KeyId: "k1"}},
		4:  {{Type: "blame", Epoch: 4, Commitment: makeBlameBitset(2), KeyId: "k1"}},
		1:  {{Type: "blame", Epoch: 1, Commitment: makeBlameBitset(0), KeyId: "k1"}},
	}

	mgr := newBlameScoreMgr(current, prev, blames)
	result := mgr.BlameScore()

	// alice: score=2 (blamed in epoch 10 and 1), weight=4 → 50% → not banned
	assert.Empty(t, result.BannedNodes, "No node exceeds 60%% in this setup")

	// New test: alice and bob blamed in all commits → 100%, carol in none.
	blames2 := map[uint64][]tss_db.TssCommitment{
		10: {{Type: "blame", Epoch: 10, Commitment: makeBlameBitset(0, 1), KeyId: "k1"}},
		7:  {{Type: "blame", Epoch: 7, Commitment: makeBlameBitset(0, 1), KeyId: "k1"}},
		4:  {{Type: "blame", Epoch: 4, Commitment: makeBlameBitset(0, 1), KeyId: "k1"}},
		1:  {{Type: "blame", Epoch: 1, Commitment: makeBlameBitset(0, 1, 2), KeyId: "k1"}},
	}
	// 2 blamed per commit (except epoch 1 has 3). maxBlamed=2, so epoch 1 is
	// systemic (3 > 2) and gets filtered. Remaining 3 non-systemic commits:
	// alice: score=3, weight=3 → 100% → banned
	// bob: score=3, weight=3 → 100% → banned
	// carol: only in systemic commit → score=0 → not banned
	// maxBans=2, so both alice and bob can be banned.

	mgr2 := newBlameScoreMgr(current, prev, blames2)
	result2 := mgr2.BlameScore()

	assert.True(t, result2.BannedNodes["alice"], "Alice at 100%% should be banned")
	assert.True(t, result2.BannedNodes["bob"], "Bob at 100%% should be banned")
	assert.False(t, result2.BannedNodes["carol"], "Carol should not be banned (only in systemic commit)")

	// Now test the actual cap with a larger election.
	// 10 members: threshold=6, maxBlamed=3, maxBans=3.
	// Rotate blames across 4 nodes so each is blamed in 3 of 4 commits → 75%.
	accounts10 := []string{"alice", "bob", "carol", "dave", "eve", "frank", "grace", "heidi", "ivan", "judy"}
	current10 := makeElectionResult(10, accounts10)
	prev10 := []elections.ElectionResult{
		makeElectionResult(7, accounts10),
		makeElectionResult(4, accounts10),
		makeElectionResult(1, accounts10),
	}
	blames3 := map[uint64][]tss_db.TssCommitment{
		10: {{Type: "blame", Epoch: 10, Commitment: makeBlameBitset(0, 1, 2), KeyId: "k1"}}, // alice, bob, carol
		7:  {{Type: "blame", Epoch: 7, Commitment: makeBlameBitset(0, 1, 3), KeyId: "k1"}},  // alice, bob, dave
		4:  {{Type: "blame", Epoch: 4, Commitment: makeBlameBitset(0, 2, 3), KeyId: "k1"}},  // alice, carol, dave
		1:  {{Type: "blame", Epoch: 1, Commitment: makeBlameBitset(1, 2, 3), KeyId: "k1"}},  // bob, carol, dave
	}
	// 3 blamed per commit, maxBlamed=3, 3 <= 3 → NOT systemic
	// alice: score=3 (epochs 10,7,4), weight=4 → 75% → candidate for ban
	// bob: score=3 (epochs 10,7,1), weight=4 → 75% → candidate for ban
	// carol: score=3 (epochs 10,4,1), weight=4 → 75% → candidate for ban
	// dave: score=3 (epochs 7,4,1), weight=4 → 75% → candidate for ban
	// 4 candidates but maxBans=3 → only 3 can be banned

	mgr3 := newBlameScoreMgr(current10, prev10, blames3)
	result3 := mgr3.BlameScore()

	bannedCount := 0
	for _, acct := range []string{"alice", "bob", "carol", "dave"} {
		if result3.BannedNodes[acct] {
			bannedCount++
		}
	}
	assert.Equal(t, 3, bannedCount,
		"Ban cap should limit bans to 3 (election size 10, threshold 6)")

	for _, acct := range []string{"eve", "frank", "grace", "heidi", "ivan", "judy"} {
		assert.False(t, result3.BannedNodes[acct],
			"Node %s has 0%% failure and should not be banned", acct)
	}
}

func TestBlameScore_SystemicBlameFiltered(t *testing.T) {
	// Blame commits naming more nodes than maxBlamed are systemic and should
	// be excluded from scoring entirely.
	// 6 members: threshold=3, maxBlamed = 6-(3+1) = 2. Blaming 3 > 2 → systemic.
	accounts := []string{"alice", "bob", "carol", "dave", "eve", "frank"}
	current := makeElectionResult(10, accounts)

	prev := []elections.ElectionResult{
		makeElectionResult(7, accounts),
		makeElectionResult(4, accounts),
		makeElectionResult(1, accounts),
	}

	blames := map[uint64][]tss_db.TssCommitment{
		// Systemic blame (3 of 6 blamed, 3 > maxBlamed 2) — should be filtered
		10: {{Type: "blame", Epoch: 10, Commitment: makeBlameBitset(0, 1, 2), KeyId: "k1"}},
		// Individual blame (1 blamed, 1 <= maxBlamed 2) — should count
		7: {{Type: "blame", Epoch: 7, Commitment: makeBlameBitset(0), KeyId: "k1"}},
	}

	mgr := newBlameScoreMgr(current, prev, blames)
	result := mgr.BlameScore()

	// alice: score=1 (only the individual blame counts), weight=1
	// 1 > 1*60/100 = 1 > 0 → banned
	assert.True(t, result.BannedNodes["alice"],
		"Alice should be banned from individual blame (systemic blame filtered)")
	// bob, carol were only in the systemic blame — should not be banned
	assert.False(t, result.BannedNodes["bob"],
		"Bob should NOT be banned — only appeared in systemic blame")
	assert.False(t, result.BannedNodes["carol"],
		"Carol should NOT be banned — only appeared in systemic blame")
}

func TestBlameScore_ExactBanThreshold(t *testing.T) {
	// Node at exactly 60% failure rate should NOT be banned (> check, not >=).
	// TSS_BAN_THRESHOLD_PERCENT = 60, check is: Score > Weight * 60 / 100
	// Use 6 members so threshold=3, maxBlamed=2, and individual blames (1 node)
	// pass the systemic filter (1 <= 2).
	accounts := []string{"alice", "bob", "carol", "dave", "eve", "frank"}
	current := makeElectionResult(10, accounts)

	prev := []elections.ElectionResult{
		makeElectionResult(7, accounts),
		makeElectionResult(4, accounts),
		makeElectionResult(1, accounts),
	}

	// 5 blame commits total, alice blamed in 3 of them → 3/5 = 60%
	blames := map[uint64][]tss_db.TssCommitment{
		uint64(10): {
			{Type: "blame", Epoch: 10, Commitment: makeBlameBitset(0), KeyId: "k1"}, // alice
			{Type: "blame", Epoch: 10, Commitment: makeBlameBitset(1), KeyId: "k2"}, // bob only
		},
		uint64(7): {
			{Type: "blame", Epoch: 7, Commitment: makeBlameBitset(0), KeyId: "k1"}, // alice
		},
		uint64(4): {
			{Type: "blame", Epoch: 4, Commitment: makeBlameBitset(0), KeyId: "k1"}, // alice
			{Type: "blame", Epoch: 4, Commitment: makeBlameBitset(1), KeyId: "k2"}, // bob only
		},
	}

	mgr := newBlameScoreMgr(current, prev, blames)
	result := mgr.BlameScore()

	// alice: weight = 5 (5 non-systemic blame commits across epochs she's in), score = 3
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
	// Use 6 members so threshold=3, maxBans=2, and individual blames pass systemic filter.
	accounts := []string{"alice", "bob", "carol", "dave", "eve", "frank"}
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
	blames[uint64(30)] = []tss_db.TssCommitment{
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
	// Use 6 members so individual blames pass systemic filter and ban cap allows bans.
	accounts := []string{"alice", "bob", "carol", "dave", "eve", "frank"}
	current := makeElectionResult(10, accounts)

	prev := []elections.ElectionResult{
		makeElectionResult(7, accounts),
		makeElectionResult(4, accounts),
	}

	timeoutErr := "timeout"
	regularErr := "SSID mismatch"

	blames := map[uint64][]tss_db.TssCommitment{
		uint64(10): {
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
	// alice: 2 blames out of 2 opportunities = 100% → banned
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
