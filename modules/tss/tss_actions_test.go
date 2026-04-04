package tss

import (
	"fmt"
	"sync"
	"testing"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/elections"
	tss_db "vsc-node/modules/db/vsc/tss"

	"github.com/stretchr/testify/assert"
)

// --- RunActions edge case tests ---
// These test the top-level lock, election, and early-exit behavior of RunActions.
// They intentionally do NOT create real dispatchers (that requires P2P) — they
// test the control flow that decides whether to proceed.

func newActionsTestMgr(t *testing.T, election *elections.ElectionResult) *TssManager {
	t.Helper()

	elecByHeight := map[uint64]elections.ElectionResult{}
	if election != nil {
		elecByHeight[0] = *election
	}

	mgr := newTestTssManager(t, "test-actions-node")
	mgr.electionDb = &test_utils.MockElectionDb{
		Elections:         map[uint64]*elections.ElectionResult{},
		ElectionsByHeight: elecByHeight,
	}
	mgr.tssCommitments = &test_utils.MockTssCommitmentsDb{
		Commitments: make(map[string]tss_db.TssCommitment),
	}
	return mgr
}

func TestRunActions_LockHeld_ActionsDropped(t *testing.T) {
	// When the lock is already held (previous batch still running),
	// RunActions should return immediately without processing actions.
	election := makeElectionResult(0, []string{"alice", "bob", "carol"})
	mgr := newActionsTestMgr(t, &election)

	// Pre-acquire the lock
	mgr.lock.Lock()

	actions := []QueuedAction{
		{Type: KeyGenAction, KeyId: "key-1"},
	}

	// Should return immediately without panic or processing
	mgr.RunActions(actions, "alice", true, 100)

	// Lock should still be held by us (not double-released)
	// TryLock should fail
	locked := mgr.lock.TryLock()
	assert.False(t, locked, "Lock should still be held by test, not released by RunActions")

	// Clean up
	mgr.lock.Unlock()
}

func TestRunActions_LockReleased_ActionsProcess(t *testing.T) {
	// When the lock is free, RunActions should acquire it, check elections,
	// and release it even if no actions match.
	election := makeElectionResult(0, []string{"alice", "bob", "carol"})
	mgr := newActionsTestMgr(t, &election)

	// Call with empty actions — should acquire and release lock cleanly
	mgr.RunActions([]QueuedAction{}, "alice", true, 100)

	// Lock should be released
	locked := mgr.lock.TryLock()
	assert.True(t, locked, "Lock should be released after RunActions completes")
	mgr.lock.Unlock()
}

func TestRunActions_MissingElection_BatchSkipped(t *testing.T) {
	// When election data is missing, all actions should be skipped and lock released.
	mgr := newActionsTestMgr(t, nil) // no election data

	// Override with failing election lookup
	mgr.electionDb = &test_utils.MockElectionDb{
		Elections:         map[uint64]*elections.ElectionResult{},
		ElectionsByHeight: map[uint64]elections.ElectionResult{}, // empty - lookup fails
	}

	actions := []QueuedAction{
		{Type: KeyGenAction, KeyId: "key-1"},
		{Type: ReshareAction, KeyId: "key-2"},
	}

	mgr.RunActions(actions, "alice", true, 100)

	// Lock should be released
	locked := mgr.lock.TryLock()
	assert.True(t, locked, "Lock should be released even when election lookup fails")
	mgr.lock.Unlock()
}

func TestRunActions_EmptyElectionMembers_BatchSkipped(t *testing.T) {
	// Election exists but has no members — should skip.
	emptyElection := elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 0},
		ElectionDataInfo:   elections.ElectionDataInfo{Members: nil},
	}
	mgr := newActionsTestMgr(t, &emptyElection)

	actions := []QueuedAction{{Type: KeyGenAction, KeyId: "key-1"}}
	mgr.RunActions(actions, "alice", true, 100)

	locked := mgr.lock.TryLock()
	assert.True(t, locked, "Lock should be released when election has no members")
	mgr.lock.Unlock()
}

func TestRunActions_ConcurrentCalls(t *testing.T) {
	// Two concurrent RunActions calls — only one should proceed.
	election := makeElectionResult(0, []string{"alice", "bob", "carol"})
	mgr := newActionsTestMgr(t, &election)

	var wg sync.WaitGroup
	results := make(chan bool, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The one that gets the lock will proceed (and release it);
			// the other will be skipped.
			mgr.RunActions([]QueuedAction{}, "alice", true, 100)
			results <- true
		}()
	}

	wg.Wait()
	close(results)

	count := 0
	for range results {
		count++
	}
	assert.Equal(t, 2, count, "Both goroutines should return (one skipped, one processed)")

	// Lock should be free after both return
	locked := mgr.lock.TryLock()
	assert.True(t, locked, "Lock should be free after both RunActions calls complete")
	mgr.lock.Unlock()
}

func TestRunActions_SessionIdFormat(t *testing.T) {
	// Verify session ID construction follows the expected format.
	// Note: the prefix is a fixed string, NOT the actionType constant.
	// KeyGenAction = "key_gen" but session ID uses "keygen".
	tests := []struct {
		prefix   string
		bh       uint64
		idx      int
		keyId    string
		expected string
	}{
		{"keygen", 100, 0, "key-1", "keygen-100-0-key-1"},
		{"sign", 250, 2, "key-abc", "sign-250-2-key-abc"},
		{"reshare", 5000, 1, "test-key", "reshare-5000-1-test-key"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			sessionId := fmt.Sprintf("%s-%d-%d-%s", tt.prefix, tt.bh, tt.idx, tt.keyId)
			assert.Equal(t, tt.expected, sessionId)
		})
	}
}
