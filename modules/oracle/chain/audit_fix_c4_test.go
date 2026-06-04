package chain

import (
	"math"
	"testing"

	systemconfig "vsc-node/modules/common/system-config"
)

// TestFixGVL8_OracleHeightUnderflow — regression for GV-L8. Before the fix,
// getLatestValidBlockHeight computed uint64(blockCount) - validityThreshold
// unconditionally, so when blockCount < validityThreshold (a fresh/syncing
// bitcoind) the result wrapped to a value near MaxUint64. The fix delegates to
// validBlockHeight, which guards the subtraction and returns an error instead.
//
// This test drives the SAME pure helper the production path now calls, against
// the REAL mainnet validityThreshold set by Init(MainnetConfig()) — so it
// exercises the fixed code, not a re-implementation of it. Without the guard,
// the blockCount=0/1 cases would return ~MaxUint64 with a nil error and these
// assertions would fail.
func TestFixGVL8_OracleHeightUnderflow(t *testing.T) {
	// Establish the real mainnet threshold via the deployed Init path.
	b := &bitcoinRelayer{}
	if err := b.Init(systemconfig.MainnetConfig()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if b.validityThreshold != 2 {
		t.Fatalf("GV-L8: expected mainnet validityThreshold=2, got %d", b.validityThreshold)
	}
	thr := b.validityThreshold

	// Underflow cases: blockCount below the threshold MUST error, never wrap.
	for _, blockCount := range []int64{0, 1} {
		h, err := validBlockHeight(blockCount, thr)
		if err == nil {
			t.Fatalf("GV-L8 regressed: blockCount=%d threshold=%d returned no error (got height=%d, wrap=%v)",
				blockCount, thr, h, h > math.MaxInt64)
		}
		if h != 0 {
			t.Fatalf("GV-L8: on underflow guard the returned height must be 0, got %d", h)
		}
	}

	// Boundary: exactly at the threshold must succeed and yield 0.
	if h, err := validBlockHeight(int64(thr), thr); err != nil || h != 0 {
		t.Fatalf("GV-L8: blockCount==threshold must yield height=0,nil; got height=%d err=%v", h, err)
	}

	// Happy path: a realistic mainnet height subtracts cleanly.
	if h, err := validBlockHeight(900000, thr); err != nil || h != 900000-thr {
		t.Fatalf("GV-L8: happy path broke; got height=%d err=%v (want %d)", h, err, 900000-thr)
	}

	// Negative block count (defensive) must also error rather than wrap.
	if _, err := validBlockHeight(-1, thr); err == nil {
		t.Fatal("GV-L8: negative blockCount must return an error")
	}
}

// TestFixGVL8_TestnetThresholdZero — on testnet/devnet validityThreshold is 0,
// so any non-negative blockCount passes the guard and subtraction is a no-op.
// Confirms the fix respects the per-network threshold (constructor trace) and
// does not regress the threshold==0 path.
func TestFixGVL8_TestnetThresholdZero(t *testing.T) {
	b := &bitcoinRelayer{}
	if err := b.Init(systemconfig.TestnetConfig()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if b.validityThreshold != 0 {
		t.Fatalf("GV-L8: expected testnet validityThreshold=0, got %d", b.validityThreshold)
	}
	for _, blockCount := range []int64{0, 1, 7191} {
		h, err := validBlockHeight(blockCount, b.validityThreshold)
		if err != nil || h != uint64(blockCount) {
			t.Fatalf("GV-L8: threshold=0 must pass through blockCount; got height=%d err=%v for blockCount=%d",
				h, err, blockCount)
		}
	}
}
