package state_engine

// review8 GV-H9 — fr_sync accepts negative amounts and turns them into a
// phantom credit to the fractional-reserve virtual account.
//
// The handler computed `amt = -frSync.UnstakedAmount` whenever stake_amt <= 0,
// so a gateway-signed (or replayed/malformed) fr_sync of {stake_amt:0,
// unstake_amt:-1000} wrote a +1000 hbd_savings record to system FR with no L1
// backing — the unary minus flipped a negative magnitude into a positive one.
// The fix rejects any fr_sync whose amounts are negative.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReview8_GVH9_FrSyncRejectsNegativeAmounts(t *testing.T) {
	cases := []struct {
		name     string
		staked   int64
		unstaked int64
		wantAmt  int64
		wantOk   bool
	}{
		{name: "stake credits reserve", staked: 5000, unstaked: 0, wantAmt: 5000, wantOk: true},
		{name: "unstake debits reserve", staked: 0, unstaked: 1000, wantAmt: -1000, wantOk: true},
		{name: "no-op both zero", staked: 0, unstaked: 0, wantAmt: 0, wantOk: true},
		// The exploit: a negative unstake_amt must NOT become a positive credit.
		{name: "negative unstake rejected (phantom credit)", staked: 0, unstaked: -1000, wantAmt: 0, wantOk: false},
		{name: "negative stake rejected", staked: -5000, unstaked: 0, wantAmt: 0, wantOk: false},
		{name: "both negative rejected", staked: -1, unstaked: -1, wantAmt: 0, wantOk: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			amt, ok := frSyncLedgerAmount(tc.staked, tc.unstaked)
			require.Equal(t, tc.wantOk, ok, "ok mismatch")
			if ok {
				require.Equal(t, tc.wantAmt, amt, "amount mismatch")
			}
		})
	}
}
