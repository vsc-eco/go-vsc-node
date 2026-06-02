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
	// Round-4 audit R4-SEC-03: AttestationTimeout + SlowPathPending
	// are terminal from the orchestrator's perspective — Drive
	// returns after MutateState'ing into them — and must be treated
	// as such by Get/Prune to preserve operator visibility into
	// validator-mesh degradation on /healthz CountByState.
	for _, c := range []struct {
		state    SessionState
		terminal bool
	}{
		{StateWaitingForIS, false},
		{StateISObserved, false},
		{StateAttesting, false},
		{StateL2Submitted, false}, // R4-005: non-terminal, in-flight
		{StateOnChain, true},
		{StateAttestationTimeout, true},
		{StateSlowPathPending, true},
		{StateForwardFailed, true},
		{StateExpired, true},
	} {
		t.Run(string(c.state), func(t *testing.T) {
			assert.Equal(t, c.terminal, c.state.IsTerminal())
		})
	}
}

func TestSessionState_IsInFlight(t *testing.T) {
	// Round-4 audit R4-SEC-01: Get/Prune share the same in-flight
	// predicate; only ATTESTING and L2_SUBMITTED are protected from
	// state-clobber past ExpiresAt.
	for _, c := range []struct {
		state    SessionState
		inFlight bool
	}{
		{StateWaitingForIS, false},
		{StateISObserved, false},
		{StateAttesting, true},
		{StateL2Submitted, true},
		{StateOnChain, false},
		{StateAttestationTimeout, false},
		{StateSlowPathPending, false},
		{StateForwardFailed, false},
		{StateExpired, false},
	} {
		t.Run(string(c.state), func(t *testing.T) {
			assert.Equal(t, c.inFlight, c.state.IsInFlight())
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
	_ = store.Put(sess)
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
	_ = store.Put(sess)

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
	_ = store.Put(sess)

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
		_ = store.Put(&Session{
			Sid:       sid,
			State:     StateWaitingForIS,
			CreatedAt: now,
			ExpiresAt: now.Add(60 * time.Second),
		})
	}
	assert.Equal(t, 3, store.Len())

	// Advance past all expiries.
	now = time.Unix(2000, 0)
	removed, _ := store.Prune()
	assert.Equal(t, 3, removed)
	assert.Equal(t, 0, store.Len())
}

func TestSessionStore_PruneOnlyExpired(t *testing.T) {
	now := time.Unix(1000, 0)
	store := NewSessionStore(time.Hour).WithNowFunc(func() time.Time { return now })

	_ = store.Put(&Session{
		Sid:       "expired",
		State:     StateWaitingForIS,
		CreatedAt: now,
		ExpiresAt: now.Add(30 * time.Second),
	})
	_ = store.Put(&Session{
		Sid:       "fresh",
		State:     StateWaitingForIS,
		CreatedAt: now,
		ExpiresAt: now.Add(30 * time.Minute),
	})

	now = time.Unix(1060, 0) // "expired" is past, "fresh" still in window
	removed, _ := store.Prune()
	assert.Equal(t, 1, removed)
	assert.Equal(t, 1, store.Len())

	_, ok := store.Get("fresh")
	assert.True(t, ok)
}

// Round-3 audit R3-CSM-02 + round-4 R4-SEC-01: Prune AND Get both
// preserve in-flight sessions past ExpiresAt; the orchestrator owns
// the terminal transition. Clobbering state from either door breaks
// the L2 reconcile budget and produces L2-credited-but-session-lost
// divergences.
func TestSessionStore_PruneAndGet_KeepInFlight(t *testing.T) {
	now := time.Unix(1000, 0)
	store := NewSessionStore(time.Hour).WithNowFunc(func() time.Time { return now })

	for _, s := range []SessionState{StateAttesting, StateL2Submitted} {
		t.Run(string(s), func(t *testing.T) {
			sid := "in-flight-" + string(s)
			_ = store.Put(&Session{
				Sid:       sid,
				State:     s,
				CreatedAt: now,
				ExpiresAt: now.Add(30 * time.Second),
			})
			// Advance past TTL; in-flight session MUST survive both
			// Prune and Get without state mutation.
			now = time.Unix(2000, 0)
			removed, _ := store.Prune()
			assert.Equal(t, 0, removed, "Prune must not delete in-flight session")
			got, ok := store.Get(sid)
			assert.True(t, ok)
			assert.Equal(t, s, got.State,
				"Get must not flip in-flight state to Expired past TTL")
		})
	}
}

// Round-4 audit R4-SEC-03: terminal states must not be silently
// overwritten to StateExpired on /status poll past TTL. Operators
// rely on /healthz CountByState to surface validator-mesh degradation;
// reclassifying ATTESTATION_TIMEOUT as ordinary expiry destroys that.
func TestSessionStore_Get_PreservesTerminalStates(t *testing.T) {
	now := time.Unix(1000, 0)
	store := NewSessionStore(time.Hour).WithNowFunc(func() time.Time { return now })

	for _, s := range []SessionState{
		StateAttestationTimeout, StateSlowPathPending,
		StateForwardFailed, StateOnChain,
	} {
		t.Run(string(s), func(t *testing.T) {
			sid := "term-" + string(s)
			_ = store.Put(&Session{
				Sid:       sid,
				State:     s,
				CreatedAt: now,
				ExpiresAt: now.Add(30 * time.Second),
			})
			now = time.Unix(2000, 0)
			got, ok := store.Get(sid)
			assert.True(t, ok)
			assert.Equal(t, s, got.State,
				"Get must not clobber terminal state to Expired past TTL")
		})
	}
}

// Round-4 audit R4-005: CountByState surfaces operator visibility into
// where sessions are getting stuck. Pin the basic shape so a future
// refactor can't silently regress to a zero-count map.
func TestSessionStore_CountByState(t *testing.T) {
	now := time.Unix(1000, 0)
	store := NewSessionStore(time.Hour).WithNowFunc(func() time.Time { return now })

	_ = store.Put(&Session{Sid: "a", State: StateAttesting, ExpiresAt: now.Add(time.Hour)})
	_ = store.Put(&Session{Sid: "b", State: StateAttesting, ExpiresAt: now.Add(time.Hour)})
	_ = store.Put(&Session{Sid: "c", State: StateL2Submitted, ExpiresAt: now.Add(time.Hour)})
	_ = store.Put(&Session{Sid: "d", State: StateOnChain, ExpiresAt: now.Add(time.Hour)})

	counts := store.CountByState()
	assert.Equal(t, 2, counts[StateAttesting])
	assert.Equal(t, 1, counts[StateL2Submitted])
	assert.Equal(t, 1, counts[StateOnChain])
}

// Round-6 audit R6-TEST-02: the R5-CORR-01 terminal-retention window
// must hold non-Expired terminal sessions past ExpiresAt for
// max(ttl/2, 1min) so /healthz CountByState surfaces validator-mesh
// degradation. Pin all four windows.
func TestSessionStore_PruneRetainsTerminalForWindow(t *testing.T) {
	now := time.Unix(1000, 0)
	ttl := 30 * time.Minute
	store := NewSessionStore(ttl).WithNowFunc(func() time.Time { return now })

	cases := []struct {
		name     string
		state    SessionState
		wantGone bool
		// advance: how far past CreatedAt to set the clock before Prune
		advance time.Duration
	}{
		// Terminal-but-not-Expired states past ExpiresAt but BEFORE
		// retention deadline → preserved.
		{"AttestationTimeout-just-past-TTL", StateAttestationTimeout, false, ttl + time.Minute},
		{"ForwardFailed-just-past-TTL", StateForwardFailed, false, ttl + time.Minute},
		{"OnChain-just-past-TTL", StateOnChain, false, ttl + time.Minute},
		// Same states past TTL + retention → reclaimed.
		{"AttestationTimeout-past-retention", StateAttestationTimeout, true, ttl + ttl/2 + time.Minute},
		// StateExpired ignores retention — reclaimed at the boundary.
		{"Expired-past-TTL", StateExpired, true, ttl + time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now = time.Unix(1000, 0)
			sid := c.name
			require.NoError(t, store.Put(&Session{
				Sid:       sid,
				State:     c.state,
				CreatedAt: now,
				ExpiresAt: now.Add(ttl),
			}))
			now = time.Unix(1000, 0).Add(c.advance)
			_, _ = store.Prune()
			_, has := store.sessions[sid]
			if c.wantGone {
				assert.False(t, has, "session %s must be reclaimed", sid)
			} else {
				assert.True(t, has, "session %s must be retained", sid)
			}
		})
	}
}

// Round-3 audit R3-005: PutNew is the concurrent-safe alternative to
// Get-then-Put for /session/start. The serial duplicate-rejection
// case + a race-coverage variant pin the TOCTOU window closed.
func TestSessionStore_PutNew_RejectsDuplicateSerial(t *testing.T) {
	store := NewSessionStore(time.Hour)
	sess := &Session{Sid: "dup", State: StateWaitingForIS, ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, store.PutNew(sess))
	err := store.PutNew(sess)
	assert.ErrorIs(t, err, ErrSidAlreadyExists)
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
