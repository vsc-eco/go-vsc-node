package tss

import (
	"testing"
	"time"
)

func TestParseSessionBlockHeight(t *testing.T) {
	tests := []struct {
		sessionId string
		expected  uint64
	}{
		{"keygen-1000-0-keyAbc", 1000},
		{"sign-12345-1-keyXyz", 12345},
		{"reshare-99999-0-key123", 99999},
		{"sign-0-0-key", 0},
		{"", 0},
		{"garbage", 0},
		{"sign-notanumber-0-key", 0},
	}
	for _, tt := range tests {
		got := parseSessionBlockHeight(tt.sessionId)
		if got != tt.expected {
			t.Errorf("parseSessionBlockHeight(%q) = %d, want %d", tt.sessionId, got, tt.expected)
		}
	}
}

func msg(data string) bufferedMessage {
	return bufferedMessage{Data: []byte(data), Time: time.Now()}
}

func TestSessionBuffer_PushAndLen(t *testing.T) {
	sb := newSessionBuffer()
	if sb.Len() != 0 {
		t.Fatalf("expected empty buffer, got %d", sb.Len())
	}

	sb.Push("sign-100-0-k1", 100, msg("a"))
	sb.Push("sign-100-0-k1", 100, msg("b")) // same session, second message
	sb.Push("sign-200-0-k2", 200, msg("c"))

	if sb.Len() != 2 {
		t.Fatalf("expected 2 sessions, got %d", sb.Len())
	}
	if sb.MsgCount("sign-100-0-k1") != 2 {
		t.Fatalf("expected 2 messages for k1, got %d", sb.MsgCount("sign-100-0-k1"))
	}
	if sb.MsgCount("sign-200-0-k2") != 1 {
		t.Fatalf("expected 1 message for k2, got %d", sb.MsgCount("sign-200-0-k2"))
	}
}

func TestSessionBuffer_Has(t *testing.T) {
	sb := newSessionBuffer()
	if sb.Has("sign-100-0-k1") {
		t.Fatal("expected Has=false for empty buffer")
	}
	sb.Push("sign-100-0-k1", 100, msg("a"))
	if !sb.Has("sign-100-0-k1") {
		t.Fatal("expected Has=true after push")
	}
}

func TestSessionBuffer_PeekMinBlockHeight(t *testing.T) {
	sb := newSessionBuffer()
	id, bh := sb.PeekMinBlockHeight()
	if id != "" || bh != 0 {
		t.Fatalf("expected empty peek, got %q %d", id, bh)
	}

	sb.Push("sign-300-0-k3", 300, msg("x"))
	sb.Push("sign-100-0-k1", 100, msg("y"))
	sb.Push("sign-200-0-k2", 200, msg("z"))

	id, bh = sb.PeekMinBlockHeight()
	if id != "sign-100-0-k1" || bh != 100 {
		t.Fatalf("expected min (sign-100-0-k1, 100), got (%q, %d)", id, bh)
	}
}

func TestSessionBuffer_EvictMin(t *testing.T) {
	sb := newSessionBuffer()

	sb.Push("sign-300-0-k3", 300, msg("x"))
	sb.Push("sign-100-0-k1", 100, msg("y"))
	sb.Push("sign-200-0-k2", 200, msg("z"))

	evicted := sb.EvictMin()
	if evicted != "sign-100-0-k1" {
		t.Fatalf("expected to evict sign-100-0-k1, got %q", evicted)
	}
	if sb.Len() != 2 {
		t.Fatalf("expected 2 sessions after evict, got %d", sb.Len())
	}
	if sb.Has("sign-100-0-k1") {
		t.Fatal("evicted session still present")
	}

	// Next eviction should be block 200
	evicted = sb.EvictMin()
	if evicted != "sign-200-0-k2" {
		t.Fatalf("expected to evict sign-200-0-k2, got %q", evicted)
	}
}

func TestSessionBuffer_Drain(t *testing.T) {
	sb := newSessionBuffer()
	sb.Push("sign-100-0-k1", 100, msg("a"))
	sb.Push("sign-100-0-k1", 100, msg("b"))
	sb.Push("sign-200-0-k2", 200, msg("c"))

	msgs := sb.Drain("sign-100-0-k1")
	if len(msgs) != 2 {
		t.Fatalf("expected 2 drained messages, got %d", len(msgs))
	}
	if sb.Has("sign-100-0-k1") {
		t.Fatal("drained session still present")
	}
	if sb.Len() != 1 {
		t.Fatalf("expected 1 session after drain, got %d", sb.Len())
	}

	// Drain non-existent returns nil
	if sb.Drain("nonexistent") != nil {
		t.Fatal("expected nil for non-existent drain")
	}
}

func TestSessionBuffer_Delete(t *testing.T) {
	sb := newSessionBuffer()
	sb.Push("sign-100-0-k1", 100, msg("a"))
	sb.Push("sign-200-0-k2", 200, msg("b"))

	sb.Delete("sign-100-0-k1")
	if sb.Has("sign-100-0-k1") {
		t.Fatal("deleted session still present")
	}
	if sb.Len() != 1 {
		t.Fatalf("expected 1 session after delete, got %d", sb.Len())
	}

	// Delete non-existent is a no-op
	sb.Delete("nonexistent")
	if sb.Len() != 1 {
		t.Fatalf("expected 1 session after no-op delete, got %d", sb.Len())
	}
}

func TestSessionBuffer_EvictionOrder(t *testing.T) {
	sb := newSessionBuffer()
	// Insert in random order, verify eviction is always ascending by block height
	sb.Push("sign-500-0-k5", 500, msg("e"))
	sb.Push("sign-100-0-k1", 100, msg("a"))
	sb.Push("sign-300-0-k3", 300, msg("c"))
	sb.Push("sign-200-0-k2", 200, msg("b"))
	sb.Push("sign-400-0-k4", 400, msg("d"))

	expectedOrder := []uint64{100, 200, 300, 400, 500}
	for i, expected := range expectedOrder {
		_, bh := sb.PeekMinBlockHeight()
		if bh != expected {
			t.Fatalf("eviction %d: expected block height %d, got %d", i, expected, bh)
		}
		sb.EvictMin()
	}

	if sb.Len() != 0 {
		t.Fatalf("expected empty buffer after full eviction, got %d", sb.Len())
	}
}

func TestSessionBuffer_DeleteMiddlePreservesHeap(t *testing.T) {
	sb := newSessionBuffer()
	sb.Push("sign-100-0-k1", 100, msg("a"))
	sb.Push("sign-200-0-k2", 200, msg("b"))
	sb.Push("sign-300-0-k3", 300, msg("c"))
	sb.Push("sign-400-0-k4", 400, msg("d"))

	// Delete from the middle
	sb.Delete("sign-200-0-k2")

	// Min should still be 100
	_, bh := sb.PeekMinBlockHeight()
	if bh != 100 {
		t.Fatalf("expected min 100 after middle delete, got %d", bh)
	}

	// Delete the min
	sb.Delete("sign-100-0-k1")
	_, bh = sb.PeekMinBlockHeight()
	if bh != 300 {
		t.Fatalf("expected min 300 after deleting 100, got %d", bh)
	}
}

func TestSessionBuffer_MsgCountAbsent(t *testing.T) {
	sb := newSessionBuffer()
	if sb.MsgCount("nonexistent") != 0 {
		t.Fatal("expected 0 for absent session")
	}
}
