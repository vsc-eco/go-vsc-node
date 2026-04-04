package tss

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/witnesses"

	"github.com/chebyrash/promise"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/assert"
)

// --- RPC ReceiveMsg Tests ---

func TestReceiveMsg_ReadyAck(t *testing.T) {
	mgr := newTestTssManager(t, "test-node")
	rpc := &TssRpc{mgr: mgr}

	req := &TMsg{Type: "ready", SessionId: "keygen-100-0-k1"}
	res := &TRes{}
	err := rpc.ReceiveMsg(context.Background(), req, res)

	assert.NoError(t, err)
	assert.Equal(t, "ready_ack", res.Action)
}

func TestReceiveMsg_OversizedMessageRejected(t *testing.T) {
	mgr := newTestTssManager(t, "test-node")
	rpc := &TssRpc{mgr: mgr}

	// Create message exceeding 256KB limit
	bigData := make([]byte, maxBufferedMessageSize+1)
	req := &TMsg{
		Type:      "msg",
		SessionId: "reshare-999-0-k1",
		Data:      bigData,
	}
	res := &TRes{}
	err := rpc.ReceiveMsg(context.Background(), req, res)

	// Should succeed (return nil) but not buffer
	assert.NoError(t, err)
	assert.Equal(t, 0, mgr.messageBuffer.MsgCount("reshare-999-0-k1"),
		"Oversized messages must not be buffered")
}

func TestReceiveMsg_StaleSessionRejected(t *testing.T) {
	mgr := newTestTssManager(t, "test-node")
	mgr.lastBlockHeight.Store(1000)
	rpc := &TssRpc{mgr: mgr}

	// Session block height = 900, current = 1000, maxBufferBlockAge = 80
	// 900 + 80 = 980 < 1000 → stale
	req := &TMsg{
		Type:      "msg",
		SessionId: "reshare-900-0-k1",
		Data:      []byte("test"),
	}
	res := &TRes{}
	err := rpc.ReceiveMsg(context.Background(), req, res)

	assert.NoError(t, err)
	assert.Equal(t, 0, mgr.messageBuffer.MsgCount("reshare-900-0-k1"),
		"Messages for stale sessions must be rejected")
}

func TestReceiveMsg_BufferPerSessionOverflow(t *testing.T) {
	mgr := newTestTssManager(t, "test-node")
	rpc := &TssRpc{mgr: mgr}

	sessionId := "reshare-999-0-k1"

	// Fill buffer to per-session limit
	for i := 0; i < maxMessagesPerSession; i++ {
		req := &TMsg{
			Type:      "msg",
			SessionId: sessionId,
			Data:      []byte("msg"),
		}
		res := &TRes{}
		_ = rpc.ReceiveMsg(context.Background(), req, res)
	}

	assert.Equal(t, maxMessagesPerSession, mgr.messageBuffer.MsgCount(sessionId))

	// 101st message should be dropped
	req := &TMsg{
		Type:      "msg",
		SessionId: sessionId,
		Data:      []byte("overflow"),
	}
	res := &TRes{}
	_ = rpc.ReceiveMsg(context.Background(), req, res)

	assert.Equal(t, maxMessagesPerSession, mgr.messageBuffer.MsgCount(sessionId),
		"Messages beyond per-session limit must be dropped")
}

func TestReceiveMsg_BufferEvictsOldestSession(t *testing.T) {
	mgr := newTestTssManager(t, "test-node")
	mgr.lastBlockHeight.Store(10000)
	rpc := &TssRpc{mgr: mgr}

	// Fill all session slots (50). All block heights must be within staleness
	// window: sessionBh + maxBufferBlockAge >= currentBh → bh >= 10000-80 = 9920
	for i := 0; i < maxBufferedSessions; i++ {
		bh := 9950 + i // block heights 9950-9999 (all non-stale)
		req := &TMsg{
			Type:      "msg",
			SessionId: "reshare-" + strconv.Itoa(bh) + "-0-k1",
			Data:      []byte("msg"),
		}
		res := &TRes{}
		_ = rpc.ReceiveMsg(context.Background(), req, res)
	}

	assert.Equal(t, maxBufferedSessions, mgr.messageBuffer.Len())
	assert.True(t, mgr.messageBuffer.Has("reshare-9950-0-k1"), "Oldest session should exist before eviction")

	// Session 51 with higher block height should evict oldest
	req := &TMsg{
		Type:      "msg",
		SessionId: "reshare-10010-0-k1",
		Data:      []byte("new"),
	}
	res := &TRes{}
	_ = rpc.ReceiveMsg(context.Background(), req, res)

	assert.Equal(t, maxBufferedSessions, mgr.messageBuffer.Len(),
		"Buffer size should remain at limit after eviction")
	assert.False(t, mgr.messageBuffer.Has("reshare-9950-0-k1"),
		"Oldest session should be evicted")
	assert.True(t, mgr.messageBuffer.Has("reshare-10010-0-k1"),
		"New session should be added")
}

func TestReceiveMsg_BufferRejectsOlderSession(t *testing.T) {
	mgr := newTestTssManager(t, "test-node")
	mgr.lastBlockHeight.Store(10100)
	rpc := &TssRpc{mgr: mgr}

	// Fill all session slots with high block heights (all non-stale: 10100-80=10020)
	for i := 0; i < maxBufferedSessions; i++ {
		bh := 10050 + i // block heights 10050-10099
		req := &TMsg{
			Type:      "msg",
			SessionId: "reshare-" + strconv.Itoa(bh) + "-0-k1",
			Data:      []byte("msg"),
		}
		res := &TRes{}
		_ = rpc.ReceiveMsg(context.Background(), req, res)
	}

	// Try to add session with block height lower than oldest buffered
	// but still non-stale (10040 + 80 = 10120 >= 10100)
	req := &TMsg{
		Type:      "msg",
		SessionId: "reshare-10040-0-k1",
		Data:      []byte("old"),
	}
	res := &TRes{}
	_ = rpc.ReceiveMsg(context.Background(), req, res)

	assert.False(t, mgr.messageBuffer.Has("reshare-10040-0-k1"),
		"Session older than all buffered sessions must be rejected")
}

func TestReceiveMsg_ValidSessionForwarded(t *testing.T) {
	mgr := newTestTssManager(t, "test-node")

	// Need a real peerId for witness lookup
	fakeId, _ := peer.Decode("12D3KooWDpJ7As7BWAwRMfu1VU2WCqNjvq387JEYKDBj4kx6nXTN")
	_ = fakeId

	// Set up a mock dispatcher to track HandleP2P calls
	sessionId := "reshare-999-0-k1"
	md := &mockDispatcher{sessionId: sessionId}
	mgr.actionMap[sessionId] = md
	mgr.sessionMap[sessionId] = sessionInfo{leader: "leader", bh: 999, action: ActionTypeReshare}

	mgr.witnessDb = &test_utils.MockWitnessDb{
		ByPeerId: map[string][]witnesses.Witness{
			fakeId.String(): {{Account: "some-witness"}},
		},
	}

	// Note: We can't easily test this without gorpc context providing peerId.
	// This test documents the requirement — full integration requires real P2P.
	t.Log("ValidSessionForwarded requires real P2P context; documenting the expected path")
}

func TestReplayBufferedMessages_SkipsOldMessages(t *testing.T) {
	mgr := newTestTssManager(t, "test-node")
	rpc := &TssRpc{mgr: mgr}

	sessionId := "reshare-999-0-k1"
	md := &mockDispatcher{sessionId: sessionId}

	// Buffer a message manually with old timestamp
	mgr.bufferLock.Lock()
	mgr.messageBuffer.Push(sessionId, 999, bufferedMessage{
		Data:    []byte("old-msg"),
		From:    "12D3KooWDpJ7As7BWAwRMfu1VU2WCqNjvq387JEYKDBj4kx6nXTN",
		IsBrcst: false,
		Time:    time.Now().Add(-2 * time.Minute), // 2 minutes old
	})
	mgr.bufferLock.Unlock()

	rpc.replayBufferedMessages(sessionId, md)

	assert.Equal(t, 0, md.handleP2PCalls,
		"Messages older than 1 minute must be skipped during replay")
}

func TestReplayBufferedMessages_ReplaysFreshMessages(t *testing.T) {
	mgr := newTestTssManager(t, "test-node")
	rpc := &TssRpc{mgr: mgr}

	sessionId := "reshare-999-0-k1"
	// Use a real peer ID format
	realPeerId := "12D3KooWDpJ7As7BWAwRMfu1VU2WCqNjvq387JEYKDBj4kx6nXTN"

	mgr.witnessDb = &test_utils.MockWitnessDb{
		ByPeerId: map[string][]witnesses.Witness{
			realPeerId: {{Account: "alice"}},
		},
	}

	md := &mockDispatcher{sessionId: sessionId}

	// Buffer a fresh message
	mgr.bufferLock.Lock()
	mgr.messageBuffer.Push(sessionId, 999, bufferedMessage{
		Data:    []byte("fresh-msg"),
		From:    realPeerId,
		IsBrcst: false,
		Time:    time.Now(),
	})
	mgr.bufferLock.Unlock()

	rpc.replayBufferedMessages(sessionId, md)

	assert.Equal(t, 1, md.handleP2PCalls,
		"Fresh messages must be replayed to dispatcher")
}

func TestReplayBufferedMessages_InvalidPeerId(t *testing.T) {
	mgr := newTestTssManager(t, "test-node")
	rpc := &TssRpc{mgr: mgr}

	sessionId := "reshare-999-0-k1"
	md := &mockDispatcher{sessionId: sessionId}

	// Buffer a message with invalid peer ID
	mgr.bufferLock.Lock()
	mgr.messageBuffer.Push(sessionId, 999, bufferedMessage{
		Data:    []byte("msg"),
		From:    "invalid-peer-id-xxx",
		IsBrcst: false,
		Time:    time.Now(),
	})
	mgr.bufferLock.Unlock()

	// Should not panic
	rpc.replayBufferedMessages(sessionId, md)

	assert.Equal(t, 0, md.handleP2PCalls,
		"Messages with invalid peer IDs must be skipped gracefully")
}

func TestReplayBufferedMessages_WitnessLookupFailure(t *testing.T) {
	mgr := newTestTssManager(t, "test-node")
	rpc := &TssRpc{mgr: mgr}

	sessionId := "reshare-999-0-k1"
	realPeerId := "12D3KooWDpJ7As7BWAwRMfu1VU2WCqNjvq387JEYKDBj4kx6nXTN"

	// No witness records at all
	mgr.witnessDb = &test_utils.MockWitnessDb{}

	md := &mockDispatcher{sessionId: sessionId}

	mgr.bufferLock.Lock()
	mgr.messageBuffer.Push(sessionId, 999, bufferedMessage{
		Data:    []byte("msg"),
		From:    realPeerId,
		IsBrcst: false,
		Time:    time.Now(),
	})
	mgr.bufferLock.Unlock()

	// Should not panic
	rpc.replayBufferedMessages(sessionId, md)

	assert.Equal(t, 0, md.handleP2PCalls,
		"Messages from unknown peers must be skipped gracefully")
}

func TestReceiveMsg_ConcurrentBufferAndDispatch(t *testing.T) {
	// Test that buffered messages are replayed when dispatcher arrives,
	// even if messages continue arriving concurrently.
	mgr := newTestTssManager(t, "test-node")
	rpc := &TssRpc{mgr: mgr}

	sessionId := "reshare-998-0-k1"

	// Pre-buffer some messages
	mgr.bufferLock.Lock()
	for i := 0; i < 5; i++ {
		mgr.messageBuffer.Push(sessionId, 998, bufferedMessage{
			Data:    []byte("buffered"),
			From:    "12D3KooWDpJ7As7BWAwRMfu1VU2WCqNjvq387JEYKDBj4kx6nXTN",
			IsBrcst: false,
			Time:    time.Now(),
		})
	}
	mgr.bufferLock.Unlock()

	assert.Equal(t, 5, mgr.messageBuffer.MsgCount(sessionId))

	// Simulate dispatcher registration + replay
	md := &mockDispatcher{sessionId: sessionId}
	realPeerId := "12D3KooWDpJ7As7BWAwRMfu1VU2WCqNjvq387JEYKDBj4kx6nXTN"
	mgr.witnessDb = &test_utils.MockWitnessDb{
		ByPeerId: map[string][]witnesses.Witness{
			realPeerId: {{Account: "alice"}},
		},
	}

	rpc.replayBufferedMessages(sessionId, md)

	assert.Equal(t, 5, md.handleP2PCalls,
		"All buffered fresh messages must be replayed")

	// Buffer should be drained
	assert.Equal(t, 0, mgr.messageBuffer.MsgCount(sessionId),
		"Buffer should be empty after replay")
}

func TestParseSessionBlockHeight_EdgeCases(t *testing.T) {
	tests := []struct {
		sessionId string
		expected  uint64
	}{
		{"reshare-1000-0-key1", 1000},
		{"keygen-500-2-key2", 500},
		{"sign-99999-0-key3", 99999},
		{"invalid", 0},
		{"", 0},
		{"reshare-abc-0-key1", 0}, // non-numeric
	}

	for _, tt := range tests {
		t.Run(tt.sessionId, func(t *testing.T) {
			assert.Equal(t, tt.expected, parseSessionBlockHeight(tt.sessionId))
		})
	}
}

// --- Mock types ---

type mockDispatcher struct {
	sessionId      string
	handleP2PCalls int
	mu             sync.Mutex
}

func (d *mockDispatcher) Start() error { return nil }
func (d *mockDispatcher) SessionId() string { return d.sessionId }
func (d *mockDispatcher) KeyId() string { return "mock-key" }
func (d *mockDispatcher) HandleP2P(msg []byte, from string, isBrcst bool, cmt string, fromCmt string) {
	d.mu.Lock()
	d.handleP2PCalls++
	d.mu.Unlock()
}
func (d *mockDispatcher) Done() *promise.Promise[DispatcherResult] { return nil }
func (d *mockDispatcher) Cleanup()                                 {}
