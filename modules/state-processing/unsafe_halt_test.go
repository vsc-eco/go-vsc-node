package state_engine

import (
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

// SetUnsafeHalt + ConsumeUnsafeHalt are tiny enough that we exercise them
// without the testEnv harness. The interesting property is that under
// concurrent calls, exactly ONE first-cause survives — which the prior
// Load/Store implementation did not guarantee.

func TestUnsafeHalt_ConsumeReturnsNilWhenUnset(t *testing.T) {
	se := &StateEngine{}
	assert.Nil(t, se.ConsumeUnsafeHalt())
}

func TestUnsafeHalt_FirstCauseSurvivesSingleThreaded(t *testing.T) {
	se := &StateEngine{BlockHeight: 100}
	se.SetUnsafeHalt("first", errors.New("boom-1"))
	se.SetUnsafeHalt("second", errors.New("boom-2"))
	se.SetUnsafeHalt("third", errors.New("boom-3"))

	got := se.ConsumeUnsafeHalt()
	assert.NotNil(t, got)
	assert.Contains(t, got.Error(), "first")
	assert.Contains(t, got.Error(), "boom-1")
	assert.Contains(t, got.Error(), "100", "should record block height")
	assert.NotContains(t, got.Error(), "boom-2")
	assert.NotContains(t, got.Error(), "boom-3")

	// Second consume returns nil — Swap clears the pointer.
	assert.Nil(t, se.ConsumeUnsafeHalt())
}

func TestUnsafeHalt_RaceFreeFirstCause(t *testing.T) {
	// Hammer SetUnsafeHalt from N goroutines. If the implementation is a
	// Load-then-Store (TOCTOU race), two callers can both observe nil and
	// both Store, breaking "first cause wins." With CompareAndSwap, exactly
	// one goroutine's error survives.
	const N = 64

	se := &StateEngine{BlockHeight: 42}

	var wg sync.WaitGroup
	wg.Add(N)
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			se.SetUnsafeHalt("site", errors.New(string(rune('A'+i%26))))
		}()
	}
	close(start)
	wg.Wait()

	got := se.ConsumeUnsafeHalt()
	assert.NotNil(t, got)
	// Whichever goroutine won the CAS, the consume should be a single
	// error and the second consume should be nil.
	assert.Nil(t, se.ConsumeUnsafeHalt(), "Swap must atomically clear")
}

func TestUnsafeHalt_AllowSkipDoesNotHalt(t *testing.T) {
	// AllowUnsafeSkip is a global. Save and restore around the test so
	// nothing else in the package observes a flipped value.
	prev := AllowUnsafeSkip
	AllowUnsafeSkip = true
	defer func() { AllowUnsafeSkip = prev }()

	se := &StateEngine{}
	se.SetUnsafeHalt("site", errors.New("skipped"))
	assert.Nil(t, se.ConsumeUnsafeHalt(),
		"AllowUnsafeSkip must not record an error — operator opted into divergence")
}

func TestUnsafeHalt_BlockHeightCapturedFromEngine(t *testing.T) {
	// blockHeight in the error reflects se.BlockHeight at the time of
	// SetUnsafeHalt — not at consume time. This is what makes drift
	// between the engine's notion of the current block and the streamer's
	// visible.
	se := &StateEngine{BlockHeight: 12345}
	se.SetUnsafeHalt("site", errors.New("err"))

	se.BlockHeight = 99999 // simulate the engine having moved on
	got := se.ConsumeUnsafeHalt()
	assert.NotNil(t, got)
	assert.Contains(t, got.Error(), "12345", "block height frozen at SetUnsafeHalt time")
	assert.NotContains(t, got.Error(), "99999")
}
