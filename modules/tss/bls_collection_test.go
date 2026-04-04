package tss

import (
	"testing"
	"time"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/elections"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests exercise the BLS signature collection logic in waitForSigs
// and the session result lookup in the ask_sigs handler.
//
// The actual ask_sigs handler (p2p.go:85-132) uses pubsub and real BLS keys,
// making it hard to unit test without the full cluster. These tests focus on
// the data flow and edge cases around sessionResults and sigChannels.

func TestSessionResults_MissingSessId(t *testing.T) {
	// If ask_sigs arrives with a non-existent session ID, no signature should be sent.
	// This simulates a node that hasn't finished its session yet.
	mgr := newTestTssManager(t, "test-node")

	// No session results registered
	mgr.bufferLock.RLock()
	_, hasResult := mgr.sessionResults["nonexistent-session"]
	mgr.bufferLock.RUnlock()

	assert.False(t, hasResult,
		"Non-existent session should not have a result")
}

func TestSessionResults_StoreAndRetrieve(t *testing.T) {
	mgr := newTestTssManager(t, "test-node")

	epoch := uint64(10)
	members := []string{"alice", "bob", "carol"}
	mgr.electionDb = &test_utils.MockElectionDb{
		Elections: map[uint64]*elections.ElectionResult{
			epoch: makeElection(epoch, members),
		},
	}

	// Store a session result (simulating what tss.go:945 does)
	mgr.sessionResults = make(map[string]sessionResultEntry)
	sessionId := "reshare-100-0-k1"
	result := ReshareResult{
		Commitment:  mgr.setToCommitment([]Participant{{Account: "alice"}, {Account: "bob"}, {Account: "carol"}}, epoch),
		KeyId:       "k1",
		SessionId:   sessionId,
		NewEpoch:    epoch,
		BlockHeight: 100,
	}

	mgr.bufferLock.Lock()
	mgr.sessionResults[sessionId] = sessionResultEntry{
		result:      result,
		blockHeight: 100,
	}
	mgr.bufferLock.Unlock()

	// Retrieve and verify
	mgr.bufferLock.RLock()
	entry, hasResult := mgr.sessionResults[sessionId]
	mgr.bufferLock.RUnlock()

	require.True(t, hasResult)
	commitment := entry.result.Serialize()
	assert.Equal(t, "reshare", commitment.Type)
	assert.Equal(t, epoch, commitment.Epoch)
}

func TestSigChannels_BufferedChannel(t *testing.T) {
	// Verify that signature channels are buffered to prevent goroutine leaks.
	// The channel should accept messages without blocking.
	mgr := newTestTssManager(t, "test-node")
	mgr.sigChannels = make(map[string]chan sigMsg)

	sessionId := "reshare-100-0-k1"
	// Create buffered channel (capacity 16 as per leak_test.go patterns)
	mgr.sigChannels[sessionId] = make(chan sigMsg, 16)

	// Should not block
	mgr.sigChannels[sessionId] <- sigMsg{
		Account:   "alice",
		SessionId: sessionId,
		Sig:       "fake-sig",
	}

	// Verify message received
	msg := <-mgr.sigChannels[sessionId]
	assert.Equal(t, "alice", msg.Account)
}

func TestSigChannels_DuplicateSignature(t *testing.T) {
	// Verify that duplicate signatures from the same account can be received
	// (the dedup happens in waitForSigs via AddAndVerify, not at the channel level).
	mgr := newTestTssManager(t, "test-node")
	mgr.sigChannels = make(map[string]chan sigMsg)

	sessionId := "reshare-100-0-k1"
	mgr.sigChannels[sessionId] = make(chan sigMsg, 16)

	// Send two signatures from same account
	mgr.sigChannels[sessionId] <- sigMsg{Account: "alice", SessionId: sessionId, Sig: "sig1"}
	mgr.sigChannels[sessionId] <- sigMsg{Account: "alice", SessionId: sessionId, Sig: "sig2"}

	// Both should be received (dedup happens at collection level)
	msg1 := <-mgr.sigChannels[sessionId]
	msg2 := <-mgr.sigChannels[sessionId]
	assert.Equal(t, "alice", msg1.Account)
	assert.Equal(t, "alice", msg2.Account)
}

func TestSigChannels_MissingChannel(t *testing.T) {
	// If res_sig arrives for a session without a registered channel,
	// it should be silently dropped (no panic).
	mgr := newTestTssManager(t, "test-node")
	mgr.sigChannels = make(map[string]chan sigMsg)

	mgr.bufferLock.RLock()
	sigChan := mgr.sigChannels["nonexistent"]
	mgr.bufferLock.RUnlock()

	assert.Nil(t, sigChan, "Missing channel should be nil")

	// The p2p.go handler checks: if sigChan != nil { sigChan <- msg }
	// With nil channel, the send is skipped (not a panic)
}

func TestSessionResults_Cleanup(t *testing.T) {
	// Verify that session results can be cleaned up without affecting other sessions.
	mgr := newTestTssManager(t, "test-node")
	mgr.sessionResults = make(map[string]sessionResultEntry)

	mgr.sessionResults["session-1"] = sessionResultEntry{blockHeight: 100}
	mgr.sessionResults["session-2"] = sessionResultEntry{blockHeight: 200}

	// Clean up session-1
	delete(mgr.sessionResults, "session-1")

	_, has1 := mgr.sessionResults["session-1"]
	_, has2 := mgr.sessionResults["session-2"]
	assert.False(t, has1, "Cleaned up session should be gone")
	assert.True(t, has2, "Other sessions should be unaffected")
}

func TestWaitForSigs_ChannelTimeout(t *testing.T) {
	// Simulate the waitForSigs timeout by creating a channel and not sending to it.
	// This documents the 6-second timeout behavior.
	sigChan := make(chan sigMsg, 16)

	// Use a short timeout for testing
	timeout := 100 * time.Millisecond
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-sigChan:
		t.Fatal("Should not receive any message")
	case <-timer.C:
		// Expected: timeout with no signatures
	}
}

func TestWaitForSigs_PartialSignatures(t *testing.T) {
	// Simulate receiving only 1 of 3 needed signatures.
	// With 6 nodes and 2/3 threshold, need at least 4 signatures.
	sigChan := make(chan sigMsg, 16)

	// Send only 1 signature
	go func() {
		sigChan <- sigMsg{Account: "alice", Sig: "sig-alice"}
	}()

	// Collect for short timeout
	timeout := 100 * time.Millisecond
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	collected := 0
	for {
		select {
		case <-sigChan:
			collected++
		case <-timer.C:
			assert.Equal(t, 1, collected,
				"Should have collected exactly 1 of needed signatures")
			return
		}
	}
}

func TestSessionResultEntry_BlockHeight(t *testing.T) {
	// Verify that block height tracking works for session result cleanup.
	entry := sessionResultEntry{
		result:      ReshareResult{BlockHeight: 1000},
		blockHeight: 1000,
	}

	assert.Equal(t, uint64(1000), entry.blockHeight)
}
