package tss

import "container/heap"

// sessionBuffer is a min-heap priority queue of buffered sessions keyed by block height.
// It provides O(1) eviction of the oldest session, O(log n) insertion (typically O(1)
// amortized when inserting at the highest block height), and O(1) lookup by session ID.
//
// All methods assume external synchronization (the caller holds bufferLock).
type sessionBuffer struct {
	h sessionHeap
}

func newSessionBuffer() *sessionBuffer {
	return &sessionBuffer{
		h: sessionHeap{
			index: make(map[string]int),
		},
	}
}

// Len returns the number of distinct sessions in the buffer.
func (sb *sessionBuffer) Len() int {
	return len(sb.h.entries)
}

// Has returns whether a session is present in the buffer.
func (sb *sessionBuffer) Has(sessionId string) bool {
	_, ok := sb.h.index[sessionId]
	return ok
}

// MsgCount returns the number of buffered messages for a session, or 0 if absent.
func (sb *sessionBuffer) MsgCount(sessionId string) int {
	i, ok := sb.h.index[sessionId]
	if !ok {
		return 0
	}
	return len(sb.h.entries[i].messages)
}

// Push adds a message to the given session, creating the session entry if needed.
// blockHeight is only used when creating a new entry.
func (sb *sessionBuffer) Push(sessionId string, blockHeight uint64, msg bufferedMessage) {
	if i, ok := sb.h.index[sessionId]; ok {
		sb.h.entries[i].messages = append(sb.h.entries[i].messages, msg)
		return
	}
	heap.Push(&sb.h, &sessionEntry{
		sessionId:   sessionId,
		blockHeight: blockHeight,
		messages:    []bufferedMessage{msg},
	})
}

// PeekMinBlockHeight returns the lowest block height and session ID in the buffer.
// Returns ("", 0) if the buffer is empty.
func (sb *sessionBuffer) PeekMinBlockHeight() (string, uint64) {
	if len(sb.h.entries) == 0 {
		return "", 0
	}
	e := sb.h.entries[0]
	return e.sessionId, e.blockHeight
}

// EvictMin removes the session with the lowest block height.
// Returns the evicted session ID, or "" if the buffer is empty.
func (sb *sessionBuffer) EvictMin() string {
	if len(sb.h.entries) == 0 {
		return ""
	}
	e := heap.Pop(&sb.h).(*sessionEntry)
	return e.sessionId
}

// Drain removes a session by ID and returns its messages. Returns nil if absent.
func (sb *sessionBuffer) Drain(sessionId string) []bufferedMessage {
	i, ok := sb.h.index[sessionId]
	if !ok {
		return nil
	}
	e := heap.Remove(&sb.h, i).(*sessionEntry)
	return e.messages
}

// Delete removes a session by ID, discarding its messages.
func (sb *sessionBuffer) Delete(sessionId string) {
	i, ok := sb.h.index[sessionId]
	if !ok {
		return
	}
	heap.Remove(&sb.h, i)
}

// --- heap implementation ---

type sessionEntry struct {
	sessionId   string
	blockHeight uint64
	messages    []bufferedMessage
}

// sessionHeap implements heap.Interface as a min-heap on blockHeight.
// It maintains an index map from sessionId to slice position so that
// Swap keeps the index in sync for O(1) lookups.
type sessionHeap struct {
	entries []*sessionEntry
	index   map[string]int // sessionId -> position in entries
}

func (h *sessionHeap) Len() int           { return len(h.entries) }
func (h *sessionHeap) Less(i, j int) bool { return h.entries[i].blockHeight < h.entries[j].blockHeight }

func (h *sessionHeap) Swap(i, j int) {
	h.entries[i], h.entries[j] = h.entries[j], h.entries[i]
	h.index[h.entries[i].sessionId] = i
	h.index[h.entries[j].sessionId] = j
}

func (h *sessionHeap) Push(x any) {
	e := x.(*sessionEntry)
	h.index[e.sessionId] = len(h.entries)
	h.entries = append(h.entries, e)
}

func (h *sessionHeap) Pop() any {
	old := h.entries
	n := len(old)
	e := old[n-1]
	old[n-1] = nil // avoid memory leak
	h.entries = old[:n-1]
	delete(h.index, e.sessionId)
	return e
}
