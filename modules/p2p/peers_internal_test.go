package libp2p

import "testing"

// Peers is read by the devnet quiescence probe, which can arrive before Init has
// created the host; it must answer "no peers", not panic.
func TestPeersBeforeInitIsEmpty(t *testing.T) {
	if got := (&P2PServer{}).Peers(); len(got) != 0 {
		t.Fatalf("Peers() before Init = %v, want none", got)
	}
}
