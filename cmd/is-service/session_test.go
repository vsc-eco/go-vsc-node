package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateSid_LengthAndUniqueness(t *testing.T) {
	a, err := GenerateSid()
	require.NoError(t, err)
	b, err := GenerateSid()
	require.NoError(t, err)
	assert.Len(t, a, 32, "sid must be 32 hex chars (128 bits)")
	assert.NotEqual(t, a, b, "two GenerateSid calls must produce different ids")
}

func TestBuildAuthInstruction(t *testing.T) {
	got := BuildAuthInstruction("abc123")
	assert.Equal(t, "op=auth;sid=abc123", got)
}

func TestBuildCallInstruction_NoAmount(t *testing.T) {
	got := BuildCallInstruction("vsc1Forwarder", "swap", "eyJhYg==", "sid1", 0)
	assert.Equal(t, "op=call;contract=vsc1Forwarder;method=swap;args=eyJhYg==;sid=sid1", got)
}

func TestBuildCallInstruction_WithAmount(t *testing.T) {
	got := BuildCallInstruction("vsc1Forwarder", "swap", "eyJhYg==", "sid1", 100_000_000)
	assert.Equal(t, "op=call;contract=vsc1Forwarder;method=swap;args=eyJhYg==;sid=sid1;amount=100000000", got)
}

func TestSessionState_IsTerminal(t *testing.T) {
	for _, c := range []struct {
		state    SessionState
		terminal bool
	}{
		{StateWaitingForIS, false},
		{StateISObserved, false},
		{StateAttesting, false},
		{StateOnChain, true},
		{StateAttestationTimeout, false},
		{StateSlowPathPending, false},
		{StateForwardFailed, true},
		{StateExpired, true},
	} {
		t.Run(string(c.state), func(t *testing.T) {
			assert.Equal(t, c.terminal, c.state.IsTerminal())
		})
	}
}

func TestSessionStore_PutGet(t *testing.T) {
	now := time.Unix(1000, 0)
	store := NewSessionStore(60 * time.Second).WithNowFunc(func() time.Time { return now })
	sess := &Session{
		Sid:            "sid1",
		Op:             OpAuth,
		DepositAddress: "tdash1q...",
		State:          StateWaitingForIS,
		CreatedAt:      now,
		ExpiresAt:      now.Add(30 * time.Minute),
	}
	store.Put(sess)
	got, ok := store.Get("sid1")
	require.True(t, ok)
	assert.Equal(t, sess, got)
}

func TestSessionStore_GetMiss(t *testing.T) {
	store := NewSessionStore(time.Hour)
	_, ok := store.Get("nonexistent")
	assert.False(t, ok)
}

func TestSessionStore_MutateState(t *testing.T) {
	now := time.Unix(1000, 0)
	store := NewSessionStore(time.Hour).WithNowFunc(func() time.Time { return now })
	sess := &Session{
		Sid:       "sid1",
		Op:        OpAuth,
		State:     StateWaitingForIS,
		CreatedAt: now,
		ExpiresAt: now.Add(30 * time.Minute),
	}
	store.Put(sess)

	ok := store.MutateState("sid1", func(s *Session) {
		s.State = StateISObserved
		s.DashTxId = "abc..."
	})
	assert.True(t, ok)

	got, _ := store.Get("sid1")
	assert.Equal(t, StateISObserved, got.State)
	assert.Equal(t, "abc...", got.DashTxId)
}

func TestSessionStore_MutateState_Miss(t *testing.T) {
	store := NewSessionStore(time.Hour)
	ok := store.MutateState("nonexistent", func(s *Session) { s.State = StateOnChain })
	assert.False(t, ok)
}

func TestSessionStore_ExpiryOnGet(t *testing.T) {
	now := time.Unix(1000, 0)
	store := NewSessionStore(time.Hour).WithNowFunc(func() time.Time { return now })
	sess := &Session{
		Sid:       "sid1",
		Op:        OpAuth,
		State:     StateWaitingForIS,
		CreatedAt: now,
		ExpiresAt: now.Add(60 * time.Second),
	}
	store.Put(sess)

	// Advance past expiry.
	now = time.Unix(2000, 0)
	got, ok := store.Get("sid1")
	require.True(t, ok, "Get returns the session even when expired (with EXPIRED state)")
	assert.Equal(t, StateExpired, got.State, "Get sets state to EXPIRED for expired sessions")
}

func TestSessionStore_Prune(t *testing.T) {
	now := time.Unix(1000, 0)
	store := NewSessionStore(time.Hour).WithNowFunc(func() time.Time { return now })

	for _, sid := range []string{"a", "b", "c"} {
		store.Put(&Session{
			Sid:       sid,
			State:     StateWaitingForIS,
			CreatedAt: now,
			ExpiresAt: now.Add(60 * time.Second),
		})
	}
	assert.Equal(t, 3, store.Len())

	// Advance past all expiries.
	now = time.Unix(2000, 0)
	removed := store.Prune()
	assert.Equal(t, 3, removed)
	assert.Equal(t, 0, store.Len())
}

func TestSessionStore_PruneOnlyExpired(t *testing.T) {
	now := time.Unix(1000, 0)
	store := NewSessionStore(time.Hour).WithNowFunc(func() time.Time { return now })

	store.Put(&Session{
		Sid:       "expired",
		State:     StateWaitingForIS,
		CreatedAt: now,
		ExpiresAt: now.Add(30 * time.Second),
	})
	store.Put(&Session{
		Sid:       "fresh",
		State:     StateWaitingForIS,
		CreatedAt: now,
		ExpiresAt: now.Add(30 * time.Minute),
	})

	now = time.Unix(1060, 0) // "expired" is past, "fresh" still in window
	removed := store.Prune()
	assert.Equal(t, 1, removed)
	assert.Equal(t, 1, store.Len())

	_, ok := store.Get("fresh")
	assert.True(t, ok)
}

func TestFormatInt(t *testing.T) {
	cases := []struct {
		n   int64
		out string
	}{
		{0, "0"},
		{1, "1"},
		{10, "10"},
		{100_000_000, "100000000"},
		{-42, "-42"},
		{MinCallFundingDuffs, "1000000"},
	}
	for _, c := range cases {
		t.Run(c.out, func(t *testing.T) {
			assert.Equal(t, c.out, formatInt(c.n))
		})
	}
}
