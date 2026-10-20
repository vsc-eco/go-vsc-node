package gateway

import (
	"testing"
	"time"
)

// A leader round must count from the moment it is launched until it returns:
// the devnet block pacemaker holds real cadence while InFlight is non-zero, and
// a round counted only once its goroutine is scheduled leaves a window in which
// the chain can run ahead of the signers' waitCheckBh window.
func TestGoLeadingCountsTheRoundUntilItReturns(t *testing.T) {
	ms := &MultiSig{}
	release := make(chan struct{})
	ms.goLeading(func() { <-release })
	ms.goLeading(func() { <-release })
	if got := ms.InFlight(); got != 2 {
		t.Fatalf("right after launch: InFlight = %d, want 2", got)
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for ms.InFlight() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("after the rounds returned: InFlight = %d, want 0", ms.InFlight())
		}
		time.Sleep(time.Millisecond)
	}
}
