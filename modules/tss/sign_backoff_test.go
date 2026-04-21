package tss

import (
	"testing"

	tss_db "vsc-node/modules/db/vsc/tss"

	"github.com/stretchr/testify/assert"
)

// TestComputeNextAttemptHeight_ExponentialCurve pins down the backoff shape
// called out in the design: min(signInterval << attemptCount, SignBackoffMaxBlocks).
// With signInterval=50 and cap=1200 that yields:
//   attempt 1 → bh+100
//   attempt 2 → bh+200
//   attempt 3 → bh+400
//   attempt 4 → bh+800
//   attempt 5 → bh+1200 (cap hits)
//   attempt 6+ → bh+1200 (saturates)
func TestComputeNextAttemptHeight_ExponentialCurve(t *testing.T) {
	const signInterval uint64 = 50
	const bh uint64 = 10_000

	cases := []struct {
		attempt uint
		want    uint64
	}{
		{attempt: 1, want: bh + 100},
		{attempt: 2, want: bh + 200},
		{attempt: 3, want: bh + 400},
		{attempt: 4, want: bh + 800},
		{attempt: 5, want: bh + tss_db.SignBackoffMaxBlocks},
		{attempt: 6, want: bh + tss_db.SignBackoffMaxBlocks},
		{attempt: 20, want: bh + tss_db.SignBackoffMaxBlocks},
	}

	for _, c := range cases {
		got := computeNextAttemptHeight(bh, c.attempt, signInterval)
		assert.Equal(t, c.want, got, "attempt=%d", c.attempt)
	}
}

// TestComputeNextAttemptHeight_ZeroAttempt guards against the edge where a
// caller accidentally passes attemptCount=0 (pre-bump). The dispatch bump
// always passes count+1, so 0 is an error but we still handle it safely.
func TestComputeNextAttemptHeight_ZeroAttempt(t *testing.T) {
	const signInterval uint64 = 50
	// 50 << 0 = 50 — it shouldn't panic or overflow.
	got := computeNextAttemptHeight(1000, 0, signInterval)
	assert.Equal(t, uint64(1050), got)
}
