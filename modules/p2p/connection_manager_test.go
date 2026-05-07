package libp2p

import (
	"testing"
	"time"
)

// Pentest findings N-M3 / N-M4 — pin the cap values so a future
// edit doesn't accidentally regress to "no manager" or
// "unmetered".

func TestNM4_ConnectionManagerEnforcesCaps(t *testing.T) {
	mgr := p2pConnectionManager()
	if mgr == nil {
		t.Fatal("p2pConnectionManager returned nil — host would have no caps")
	}

	if connMgrLowWater <= 0 || connMgrHighWater <= connMgrLowWater {
		t.Errorf("invariant: high water %d must exceed low water %d (both > 0)",
			connMgrHighWater, connMgrLowWater)
	}
	if connMgrGrace <= 0 {
		t.Errorf("invariant: grace period must be positive, got %v", connMgrGrace)
	}
	// Sanity: the values aren't insane (don't accept millions of
	// peers, don't have negative grace).
	if connMgrHighWater > 10_000 {
		t.Errorf("high water cap too permissive: %d", connMgrHighWater)
	}
	if connMgrGrace > 5*time.Minute {
		t.Errorf("grace period unexpectedly long: %v", connMgrGrace)
	}
}
