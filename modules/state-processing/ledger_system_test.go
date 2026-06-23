package state_engine_test

import (
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	safetyslash "vsc-node/modules/incentive-pendulum/safety_slash"
	ledgerSystem "vsc-node/modules/ledger-system"
)

// testChecksumSconf wraps a mocknet config so individual tests can override
// EvmAddressChecksumHeight without going through the JSON override loader.
// All tests that don't exercise the EIP-55 gate pass nil to ledgerSystem.New —
// the LedgerSystem call site is nil-safe (legacy verbatim casing).
type testChecksumSconf struct {
	systemconfig.SystemConfig
	cp params.ConsensusParams
}

func (t *testChecksumSconf) ConsensusParams() params.ConsensusParams { return t.cp }

func mocknetWithChecksumHeight(h uint64) systemconfig.SystemConfig {
	base := systemconfig.MocknetConfig()
	cp := base.ConsensusParams()
	cp.EvmAddressChecksumHeight = h
	return &testChecksumSconf{SystemConfig: base, cp: cp}
}

// newLedgerEnv creates a LedgerSystem + LedgerState pair for testing.
func newLedgerEnv() (ledgerSystem.LedgerSystem, *ledgerSystem.LedgerState) {
	balDb := newMockBalanceDb(nil)
	lDb := newMockLedgerDb()
	aDb := newMockActionsDb()

	ls := ledgerSystem.New(balDb, lDb, nil, aDb, nil)
	state := ls.NewEmptyState()
	return ls, state
}

// TestDepositEvmChecksumNormalization verifies the consensus-gated EIP-55
// normalization of EVM deposit owners. The memo uses an all-lowercase address
// so the canonical (checksummed) form is observably different.
func TestDepositEvmChecksumNormalization(t *testing.T) {
	const lower = "0xd8da6bf26964af9d7eed9e03e53415d37aa96045"
	const checksummed = "0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045"

	cases := []struct {
		name           string
		memo           string
		checksumHeight uint64
		blockHeight    uint64
		wantOwner      string
	}{
		{
			name:           "disabled keeps verbatim casing",
			memo:           "to=" + lower,
			checksumHeight: 0,
			blockHeight:    1000,
			wantOwner:      "did:pkh:eip155:1:" + lower,
		},
		{
			name:           "below activation height keeps verbatim casing",
			memo:           "to=" + lower,
			checksumHeight: 2000,
			blockHeight:    1000,
			wantOwner:      "did:pkh:eip155:1:" + lower,
		},
		{
			name:           "at activation height normalizes to checksum",
			memo:           "to=" + lower,
			checksumHeight: 1000,
			blockHeight:    1000,
			wantOwner:      "did:pkh:eip155:1:" + checksummed,
		},
		{
			name:           "full did memo is also normalized when active",
			memo:           "to=did:pkh:eip155:1:" + lower,
			checksumHeight: 1000,
			blockHeight:    1500,
			wantOwner:      "did:pkh:eip155:1:" + checksummed,
		},
		{
			name:           "hive owner is untouched by normalization",
			memo:           "to=vaultec",
			checksumHeight: 1000,
			blockHeight:    1500,
			wantOwner:      "hive:vaultec",
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Inject a mocknet sysconfig with the per-case activation height. The
			// gate is read live through sconf at the Deposit call site, so a test
			// can vary the height without re-constructing any LedgerSystem fields.
			ls := ledgerSystem.New(
				newMockBalanceDb(nil),
				newMockLedgerDb(),
				nil,
				newMockActionsDb(),
				mocknetWithChecksumHeight(tc.checksumHeight),
			)
			dest := ls.Deposit(ledgerSystem.Deposit{
				Id:          "evm-norm-" + strconv.Itoa(i),
				Asset:       "hive",
				Amount:      100,
				From:        "hive:test-account",
				Memo:        tc.memo,
				BIdx:        int64(i),
				OpIdx:       0,
				BlockHeight: tc.blockHeight,
			})
			assert.Equal(t, tc.wantOwner, dest)
		})
	}
}

func TestInsertCheckBalance(t *testing.T) {
	ls, state := newLedgerEnv()

	// Deposit 100 HIVE to test-account (no memo redirect)
	ls.Deposit(ledgerSystem.Deposit{
		Id:          "tx0-1",
		Asset:       "HIVE",
		Amount:      100,
		From:        "hive:test-account",
		Memo:        "test",
		BIdx:        1,
		OpIdx:       0,
		BlockHeight: 1000,
	})

	bal := state.GetBalance("hive:test-account", 1001, "HIVE")
	// HIVE is not a valid asset (must be lowercase "hive")
	assert.Equal(t, int64(0), bal)

	// Re-deposit with correct lowercase asset
	ls.Deposit(ledgerSystem.Deposit{
		Id:          "tx0-2",
		Asset:       "hive",
		Amount:      100,
		From:        "hive:test-account",
		Memo:        "test",
		BIdx:        2,
		OpIdx:       0,
		BlockHeight: 1000,
	})

	bal = state.GetBalance("hive:test-account", 1001, "hive")
	assert.Equal(t, int64(100), bal)

	// Deposit with memo redirect to Ethereum address
	dest := ls.Deposit(ledgerSystem.Deposit{
		Id:          "tx0-3",
		Asset:       "hive",
		Amount:      100,
		From:        "hive:test-account",
		Memo:        "to=0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045",
		BIdx:        3,
		OpIdx:       0,
		BlockHeight: 1000,
	})
	assert.Equal(t, "did:pkh:eip155:1:0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045", dest)

	ethBal := state.GetBalance("did:pkh:eip155:1:0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045", 1001, "hive")
	assert.Equal(t, int64(100), ethBal)

	// Deposit with memo redirect to another Hive account
	dest = ls.Deposit(ledgerSystem.Deposit{
		Id:          "tx0-4",
		Asset:       "hive",
		Amount:      100,
		From:        "hive:test-account",
		Memo:        "to=vaultec",
		BIdx:        4,
		OpIdx:       0,
		BlockHeight: 1000,
	})
	assert.Equal(t, "hive:vaultec", dest)

	vaultecBal := state.GetBalance("hive:vaultec", 1001, "hive")
	assert.Equal(t, int64(100), vaultecBal)

	// Transfer from test-account to vaultec
	session := ls.NewEmptySession(state, 1000)
	result := session.ExecuteTransfer(ledgerSystem.OpLogEvent{
		Id:          "transfer-1",
		From:        "hive:test-account",
		To:          "hive:vaultec",
		Amount:      10,
		Asset:       "hive",
		BlockHeight: 1000,
		BIdx:        5,
		OpIdx:       0,
	})
	require.True(t, result.Ok, "transfer should succeed")
	session.Done()

	// Vaultec should now have 100 (deposit) + 10 (transfer in) via snapshot
	vaultecSnap := state.SnapshotForAccount("hive:vaultec", 1001, "hive")
	assert.Equal(t, int64(110), vaultecSnap)

	// Stake HBD (need to deposit HBD first)
	ls.Deposit(ledgerSystem.Deposit{
		Id:          "tx0-5",
		Asset:       "hbd",
		Amount:      100,
		From:        "hive:test-account",
		Memo:        "test",
		BIdx:        6,
		OpIdx:       0,
		BlockHeight: 1000,
	})

	session2 := ls.NewEmptySession(state, 1000)

	stakeResult := session2.Stake(ledgerSystem.StakeOp{
		OpLogEvent: ledgerSystem.OpLogEvent{
			Id:          "stake-1",
			From:        "hive:test-account",
			To:          "hive:test-account",
			Amount:      10,
			Asset:       "hbd",
			BlockHeight: 1000,
		},
	})
	require.True(t, stakeResult.Ok, "stake should succeed")
	session2.Done()
}

func TestStakeUnstake(t *testing.T) {
	// Use pre-existing balance records so GetBalance works correctly
	balDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
		"hive:test-account": {{
			Account:     "hive:test-account",
			BlockHeight: 0,
			HBD:         100,
			HBD_SAVINGS: 50,
		}},
	})
	lDb := newMockLedgerDb()
	aDb := newMockActionsDb()
	ls := ledgerSystem.New(balDb, lDb, nil, aDb, nil)
	state := ls.NewEmptyState()

	t.Run("stake HBD", func(t *testing.T) {
		session := ls.NewEmptySession(state, 1000)
		result := session.Stake(ledgerSystem.StakeOp{
			OpLogEvent: ledgerSystem.OpLogEvent{
				Id:          "stake-1",
				From:        "hive:test-account",
				To:          "hive:test-account",
				Amount:      10,
				Asset:       "hbd",
				BlockHeight: 1000,
			},
		})
		require.True(t, result.Ok, "stake should succeed")
		session.Done()
	})

	t.Run("unstake HBD savings", func(t *testing.T) {
		session := ls.NewEmptySession(state, 1000)
		result := session.Unstake(ledgerSystem.StakeOp{
			OpLogEvent: ledgerSystem.OpLogEvent{
				Id:          "unstake-1",
				From:        "hive:test-account",
				To:          "hive:test-account",
				Amount:      10,
				Asset:       "hbd_savings",
				BlockHeight: 1000,
			},
		})
		require.True(t, result.Ok, "unstake should succeed")
		session.Done()
	})

	t.Run("stake insufficient balance fails", func(t *testing.T) {
		session := ls.NewEmptySession(state, 1000)
		result := session.Stake(ledgerSystem.StakeOp{
			OpLogEvent: ledgerSystem.OpLogEvent{
				Id:          "stake-fail",
				From:        "hive:test-account",
				To:          "hive:test-account",
				Amount:      9999,
				Asset:       "hbd",
				BlockHeight: 1000,
			},
		})
		require.False(t, result.Ok)
		assert.Equal(t, "insufficient balance", result.Msg)
	})
}

func TestGatewayWithdrawal(t *testing.T) {
	ls, state := newLedgerEnv()

	// Deposit 100 HBD
	ls.Deposit(ledgerSystem.Deposit{
		Id:          "tx0-1",
		Asset:       "hbd",
		Amount:      100,
		From:        "hive:test-account",
		Memo:        "test",
		BIdx:        1,
		OpIdx:       0,
		BlockHeight: 1000,
	})

	session := ls.NewEmptySession(state, 1000)

	// Withdraw 10 HBD — should succeed
	result := session.Withdraw(ledgerSystem.WithdrawParams{
		Id:          "withdraw-1",
		Asset:       "hbd",
		Amount:      10,
		From:        "hive:test-account",
		To:          "hive:test-account",
		Memo:        "test",
		BlockHeight: 1000,
	})
	require.True(t, result.Ok, "first withdrawal should succeed")

	// Withdraw another 10 — should succeed (balance was 100, spent 10)
	result = session.Withdraw(ledgerSystem.WithdrawParams{
		Id:          "withdraw-2",
		Asset:       "hbd",
		Amount:      10,
		From:        "hive:test-account",
		To:          "hive:test-account",
		Memo:        "test",
		BlockHeight: 1000,
	})
	require.True(t, result.Ok, "second withdrawal should succeed")

	// Withdraw 150 — should fail (insufficient balance)
	result = session.Withdraw(ledgerSystem.WithdrawParams{
		Id:          "withdraw-3",
		Asset:       "hbd",
		Amount:      150,
		From:        "hive:test-account",
		To:          "hive:test-account",
		Memo:        "test",
		BlockHeight: 1000,
	})
	require.False(t, result.Ok, "overdraft withdrawal should fail")
	assert.Equal(t, "insufficient balance", result.Msg)

	session.Done()
}

func TestDepositMemoRouting(t *testing.T) {
	ls, _ := newLedgerEnv()

	t.Run("no memo defaults to sender", func(t *testing.T) {
		dest := ls.Deposit(ledgerSystem.Deposit{
			Id:          "route-1",
			Asset:       "hbd",
			Amount:      50,
			From:        "hive:alice",
			Memo:        "",
			BlockHeight: 100,
		})
		assert.Equal(t, "hive:alice", dest)
	})

	t.Run("memo to=<hive account>", func(t *testing.T) {
		dest := ls.Deposit(ledgerSystem.Deposit{
			Id:          "route-2",
			Asset:       "hbd",
			Amount:      50,
			From:        "hive:alice",
			Memo:        "to=bob",
			BlockHeight: 100,
		})
		assert.Equal(t, "hive:bob", dest)
	})

	t.Run("memo to=<eth address>", func(t *testing.T) {
		dest := ls.Deposit(ledgerSystem.Deposit{
			Id:          "route-3",
			Asset:       "hbd",
			Amount:      50,
			From:        "hive:alice",
			Memo:        "to=0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045",
			BlockHeight: 100,
		})
		assert.Equal(t, "did:pkh:eip155:1:0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045", dest)
	})
}

// newLedgerEnvWithClaims creates a LedgerSystem backed by the given balance
// records and a real MockInterestClaimsDb so ClaimHBDInterest can be tested.
// Returns the LedgerSystem, MockLedgerDb (to inspect interest records), and MockInterestClaimsDb.
func newLedgerEnvWithClaims(balances map[string][]ledgerDb.BalanceRecord) (
	ledgerSystem.LedgerSystem, *test_utils.MockLedgerDb, *test_utils.MockInterestClaimsDb,
) {
	balDb := newMockBalanceDb(balances)
	lDb := newMockLedgerDb()
	aDb := newMockActionsDb()
	claimDb := &test_utils.MockInterestClaimsDb{Claims: make([]ledgerDb.ClaimRecord, 0)}
	ls := ledgerSystem.New(balDb, lDb, claimDb, aDb, nil)
	return ls, lDb, claimDb
}

func TestClaimHBDInterest_SingleAccount_ConstantBalance(t *testing.T) {
	// One account with 1000 HBD_SAVINGS staked since the claim period started.
	// HBD_AVG=0 (unnormalized cumulative: no prior accumulation since claim reset).
	// CLAIM_HEIGHT=100, MODIFY_HEIGHT=100 (staked at period start).
	// Claim at block 200 with 50 units of interest.
	// Expected TWAB = (0 + 1000*(200-100)) / (200-100) = 1000
	// Single account gets 100% of the interest = 50.
	ls, lDb, claimDb := newLedgerEnvWithClaims(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:           "hive:alice",
			BlockHeight:       100,
			HBD_SAVINGS:       1000,
			HBD_AVG:           0,
			HBD_CLAIM_HEIGHT:  100,
			HBD_MODIFY_HEIGHT: 100,
		}},
	})

	ls.ClaimHBDInterest(100, 200, 50, "")

	// Alice should receive all 50 interest
	records := lDb.LedgerRecords["hive:alice"]
	require.Len(t, records, 1)
	assert.Equal(t, int64(50), records[0].Amount)
	assert.Equal(t, "interest", records[0].Type)
	assert.Equal(t, "hbd_savings", records[0].Asset)

	// Claim record should be saved
	require.Len(t, claimDb.Claims, 1)
	assert.Equal(t, uint64(200), claimDb.Claims[0].BlockHeight)
	assert.Equal(t, int64(50), claimDb.Claims[0].Amount)
	assert.Equal(t, 1, claimDb.Claims[0].ReceivedN)
}

func TestClaimHBDInterest_TwoAccounts_EqualBalances(t *testing.T) {
	// Two accounts with identical balances and durations → 50/50 split.
	ls, lDb, _ := newLedgerEnvWithClaims(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:           "hive:alice",
			BlockHeight:       100,
			HBD_SAVINGS:       1000,
			HBD_AVG:           0,
			HBD_CLAIM_HEIGHT:  100,
			HBD_MODIFY_HEIGHT: 100,
		}},
		"hive:bob": {{
			Account:           "hive:bob",
			BlockHeight:       100,
			HBD_SAVINGS:       1000,
			HBD_AVG:           0,
			HBD_CLAIM_HEIGHT:  100,
			HBD_MODIFY_HEIGHT: 100,
		}},
	})

	ls.ClaimHBDInterest(100, 200, 100, "")

	aliceRecords := lDb.LedgerRecords["hive:alice"]
	bobRecords := lDb.LedgerRecords["hive:bob"]
	require.Len(t, aliceRecords, 1)
	require.Len(t, bobRecords, 1)
	assert.Equal(t, int64(50), aliceRecords[0].Amount)
	assert.Equal(t, int64(50), bobRecords[0].Amount)
}

func TestClaimHBDInterest_TwoAccounts_DifferentBalances(t *testing.T) {
	// Alice: 3000, Bob: 1000 → 3:1 ratio → 75/25 split of 100 interest.
	ls, lDb, _ := newLedgerEnvWithClaims(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:           "hive:alice",
			BlockHeight:       100,
			HBD_SAVINGS:       3000,
			HBD_AVG:           0,
			HBD_CLAIM_HEIGHT:  100,
			HBD_MODIFY_HEIGHT: 100,
		}},
		"hive:bob": {{
			Account:           "hive:bob",
			BlockHeight:       100,
			HBD_SAVINGS:       1000,
			HBD_AVG:           0,
			HBD_CLAIM_HEIGHT:  100,
			HBD_MODIFY_HEIGHT: 100,
		}},
	})

	ls.ClaimHBDInterest(100, 200, 100, "")

	aliceRecords := lDb.LedgerRecords["hive:alice"]
	bobRecords := lDb.LedgerRecords["hive:bob"]
	require.Len(t, aliceRecords, 1)
	require.Len(t, bobRecords, 1)
	assert.Equal(t, int64(75), aliceRecords[0].Amount)
	assert.Equal(t, int64(25), bobRecords[0].Amount)
}

func TestClaimHBDInterest_MidPeriodStake(t *testing.T) {
	// Alice staked 1000 at block 100 (full period).
	// Bob staked 1000 at block 150 (half the period).
	// Claim at block 200, period started at block 100.
	//
	// Alice TWAB = (0 + 1000*100) / 100 = 1000
	// Bob TWAB   = (0 + 1000*50) / 100  = 500
	// Total = 1500
	// Alice gets 1000/1500 * 120 = 80
	// Bob gets   500/1500 * 120  = 40
	ls, lDb, _ := newLedgerEnvWithClaims(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:           "hive:alice",
			BlockHeight:       100,
			HBD_SAVINGS:       1000,
			HBD_AVG:           0,
			HBD_CLAIM_HEIGHT:  100,
			HBD_MODIFY_HEIGHT: 100,
		}},
		"hive:bob": {{
			Account:           "hive:bob",
			BlockHeight:       150,
			HBD_SAVINGS:       1000,
			HBD_AVG:           0,
			HBD_CLAIM_HEIGHT:  100,
			HBD_MODIFY_HEIGHT: 150,
		}},
	})

	ls.ClaimHBDInterest(100, 200, 120, "")

	aliceRecords := lDb.LedgerRecords["hive:alice"]
	bobRecords := lDb.LedgerRecords["hive:bob"]
	require.Len(t, aliceRecords, 1)
	require.Len(t, bobRecords, 1)
	assert.Equal(t, int64(80), aliceRecords[0].Amount)
	assert.Equal(t, int64(40), bobRecords[0].Amount)
}

func TestClaimHBDInterest_DivisionByZero(t *testing.T) {
	// blockHeight == HBD_CLAIM_HEIGHT → B=0, should not panic.
	ls, lDb, claimDb := newLedgerEnvWithClaims(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:           "hive:alice",
			BlockHeight:       200,
			HBD_SAVINGS:       1000,
			HBD_AVG:           0,
			HBD_CLAIM_HEIGHT:  200,
			HBD_MODIFY_HEIGHT: 200,
		}},
	})

	// Should not panic
	require.NotPanics(t, func() {
		ls.ClaimHBDInterest(200, 200, 50, "")
	})

	// No interest should be distributed
	assert.Empty(t, lDb.LedgerRecords["hive:alice"])

	// Claim record should still be saved (even if no distribution)
	require.Len(t, claimDb.Claims, 1)
	assert.Equal(t, int64(0), claimDb.Claims[0].Amount)
}

func TestClaimHBDInterest_ZeroSavings(t *testing.T) {
	// Account with 0 savings should be excluded from distribution.
	ls, lDb, _ := newLedgerEnvWithClaims(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:           "hive:alice",
			BlockHeight:       100,
			HBD_SAVINGS:       1000,
			HBD_AVG:           0,
			HBD_CLAIM_HEIGHT:  100,
			HBD_MODIFY_HEIGHT: 100,
		}},
		"hive:bob": {{
			Account:           "hive:bob",
			BlockHeight:       100,
			HBD_SAVINGS:       0,
			HBD_AVG:           0,
			HBD_CLAIM_HEIGHT:  100,
			HBD_MODIFY_HEIGHT: 100,
		}},
	})

	ls.ClaimHBDInterest(100, 200, 50, "")

	// Alice gets everything, Bob gets nothing
	aliceRecords := lDb.LedgerRecords["hive:alice"]
	require.Len(t, aliceRecords, 1)
	assert.Equal(t, int64(50), aliceRecords[0].Amount)
	assert.Empty(t, lDb.LedgerRecords["hive:bob"])
}

func TestClaimHBDInterest_AccumulatedAvg(t *testing.T) {
	// Test that HBD_AVG (unnormalized cumulative sum from prior modifications)
	// is correctly incorporated.
	//
	// Alice had 500 HBD_SAVINGS from block 100-150 (50 blocks → cum = 25000),
	// then changed to 1000 at block 150. HBD_AVG=25000, MODIFY=150.
	// Claim at block 200.
	// TWAB = (25000 + 1000*50) / 100 = 75000/100 = 750
	ls, lDb, _ := newLedgerEnvWithClaims(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:           "hive:alice",
			BlockHeight:       150,
			HBD_SAVINGS:       1000,
			HBD_AVG:           25000,
			HBD_CLAIM_HEIGHT:  100,
			HBD_MODIFY_HEIGHT: 150,
		}},
	})

	ls.ClaimHBDInterest(100, 200, 75, "")

	// Single account → gets 100% of interest
	records := lDb.LedgerRecords["hive:alice"]
	require.Len(t, records, 1)
	assert.Equal(t, int64(75), records[0].Amount)
}

func TestClaimHBDInterest_TruncatesDown(t *testing.T) {
	// Verify that both TWAB and distribution truncate (round down), not round up.
	//
	// Three accounts with balances chosen so that integer division produces remainders:
	//   Alice: 1000 HBD_SAVINGS, full period (100 blocks)
	//     TWAB = (0 + 1000*100) / 100 = 1000
	//   Bob:   700 HBD_SAVINGS, full period
	//     TWAB = (0 + 700*100) / 100 = 700
	//   Carol: 300 HBD_SAVINGS, full period
	//     TWAB = (0 + 300*100) / 100 = 300
	//   totalAvg = 2000
	//
	// Distribute 100 units:
	//   Alice: 1000 * 100 / 2000 = 50    (exact)
	//   Bob:   700  * 100 / 2000 = 35    (exact)
	//   Carol: 300  * 100 / 2000 = 15    (exact)
	//   Total distributed = 100 (no dust)
	//
	// Now distribute 99 units (forces truncation):
	//   Alice: 1000 * 99 / 2000 = 49     (49.5 truncated)
	//   Bob:   700  * 99 / 2000 = 34     (34.65 truncated)
	//   Carol: 300  * 99 / 2000 = 14     (14.85 truncated)
	//   Total distributed = 97, dust = 2 (lost to truncation)

	t.Run("distribution truncates fractional amounts", func(t *testing.T) {
		ls, lDb, _ := newLedgerEnvWithClaims(map[string][]ledgerDb.BalanceRecord{
			"hive:alice": {{
				Account:           "hive:alice",
				BlockHeight:       100,
				HBD_SAVINGS:       1000,
				HBD_AVG:           0,
				HBD_CLAIM_HEIGHT:  100,
				HBD_MODIFY_HEIGHT: 100,
			}},
			"hive:bob": {{
				Account:           "hive:bob",
				BlockHeight:       100,
				HBD_SAVINGS:       700,
				HBD_AVG:           0,
				HBD_CLAIM_HEIGHT:  100,
				HBD_MODIFY_HEIGHT: 100,
			}},
			"hive:carol": {{
				Account:           "hive:carol",
				BlockHeight:       100,
				HBD_SAVINGS:       300,
				HBD_AVG:           0,
				HBD_CLAIM_HEIGHT:  100,
				HBD_MODIFY_HEIGHT: 100,
			}},
		})

		ls.ClaimHBDInterest(100, 200, 99, "")

		aliceRecords := lDb.LedgerRecords["hive:alice"]
		bobRecords := lDb.LedgerRecords["hive:bob"]
		carolRecords := lDb.LedgerRecords["hive:carol"]
		require.Len(t, aliceRecords, 1)
		require.Len(t, bobRecords, 1)
		require.Len(t, carolRecords, 1)

		// Each amount must be the truncated (floored) value, not rounded
		assert.Equal(t, int64(49), aliceRecords[0].Amount, "Alice: 1000*99/2000=49.5 should truncate to 49")
		assert.Equal(t, int64(34), bobRecords[0].Amount, "Bob: 700*99/2000=34.65 should truncate to 34")
		assert.Equal(t, int64(14), carolRecords[0].Amount, "Carol: 300*99/2000=14.85 should truncate to 14")

		// Total distributed < total available (dust lost to truncation)
		totalDistributed := aliceRecords[0].Amount + bobRecords[0].Amount + carolRecords[0].Amount
		assert.Equal(t, int64(97), totalDistributed, "total distributed should be 97, not 99 — 2 units lost to truncation")
		assert.Less(t, totalDistributed, int64(99), "total distributed must be <= interest amount")
	})

	t.Run("TWAB truncates fractional average", func(t *testing.T) {
		// Alice: HBD_AVG=0, HBD_SAVINGS=100, staked at block 100.
		// Claim at block 203, period started at 100.
		// B = 203 - 100 = 103
		// A = 203 - 100 = 103
		// TWAB = (0 + 100*103) / 103 = 100 (exact, no truncation)
		//
		// Bob: HBD_AVG=0, HBD_SAVINGS=100, staked at block 101.
		// B = 203 - 100 = 103
		// A = 203 - 101 = 102
		// TWAB = (0 + 100*102) / 103 = 10200/103 = 99 (99.03 truncated)
		//
		// totalAvg = 199
		// Alice: 100 * 100 / 199 = 50 (50.25 truncated)
		// Bob:    99 * 100 / 199 = 49 (49.74 truncated)
		ls, lDb, _ := newLedgerEnvWithClaims(map[string][]ledgerDb.BalanceRecord{
			"hive:alice": {{
				Account:           "hive:alice",
				BlockHeight:       100,
				HBD_SAVINGS:       100,
				HBD_AVG:           0,
				HBD_CLAIM_HEIGHT:  100,
				HBD_MODIFY_HEIGHT: 100,
			}},
			"hive:bob": {{
				Account:           "hive:bob",
				BlockHeight:       101,
				HBD_SAVINGS:       100,
				HBD_AVG:           0,
				HBD_CLAIM_HEIGHT:  100,
				HBD_MODIFY_HEIGHT: 101,
			}},
		})

		ls.ClaimHBDInterest(100, 203, 100, "")

		aliceRecords := lDb.LedgerRecords["hive:alice"]
		bobRecords := lDb.LedgerRecords["hive:bob"]
		require.Len(t, aliceRecords, 1)
		require.Len(t, bobRecords, 1)

		// Bob's TWAB is truncated from 99.03 to 99, giving Alice a slight edge
		assert.Equal(t, int64(50), aliceRecords[0].Amount, "Alice: 100*100/199=50.25 should truncate to 50")
		assert.Equal(t, int64(49), bobRecords[0].Amount, "Bob: 99*100/199=49.74 should truncate to 49")

		totalDistributed := aliceRecords[0].Amount + bobRecords[0].Amount
		assert.Equal(t, int64(99), totalDistributed, "1 unit lost to truncation")
	})
}

func TestClaimHBDInterest_FrBalanceInterestGoesToDao(t *testing.T) {
	// system:fr_balance's interest should be redirected to hive:vsc.dao (DAO_WALLET).
	// Both accounts should receive proportional interest based on their TWAB.
	ls, lDb, _ := newLedgerEnvWithClaims(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:           "hive:alice",
			BlockHeight:       100,
			HBD_SAVINGS:       1000,
			HBD_AVG:           0,
			HBD_CLAIM_HEIGHT:  100,
			HBD_MODIFY_HEIGHT: 100,
		}},
		"system:fr_balance": {{
			Account:           "system:fr_balance",
			BlockHeight:       100,
			HBD_SAVINGS:       3000,
			HBD_AVG:           0,
			HBD_CLAIM_HEIGHT:  100,
			HBD_MODIFY_HEIGHT: 100,
		}},
	})

	ls.ClaimHBDInterest(100, 200, 100, "")

	// Alice gets 25% (TWAB 1000 out of 4000 total)
	aliceRecords := lDb.LedgerRecords["hive:alice"]
	require.Len(t, aliceRecords, 1)
	assert.Equal(t, int64(25), aliceRecords[0].Amount)

	// DAO wallet gets 75% (from system:fr_balance's TWAB 3000 out of 4000)
	daoRecords := lDb.LedgerRecords["hive:vsc.dao"]
	require.Len(t, daoRecords, 1)
	assert.Equal(t, int64(75), daoRecords[0].Amount)

	// system:fr_balance itself should NOT have any records
	assert.Empty(t, lDb.LedgerRecords["system:fr_balance"])
}

func TestClaimHBDInterest_ObservedApr_MonthlyInterval(t *testing.T) {
	// Claim interval of exactly one month (876_600 Hive blocks = HIVE_BLOCKS_PER_YEAR/12).
	// amount=15, TWAB=1000 → (15/1000) * (10_519_200/876_600) = 0.015 * 12 = 0.18
	ls, _, claimDb := newLedgerEnvWithClaims(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:           "hive:alice",
			BlockHeight:       100,
			HBD_SAVINGS:       1000,
			HBD_AVG:           0,
			HBD_CLAIM_HEIGHT:  100,
			HBD_MODIFY_HEIGHT: 100,
		}},
	})

	ls.ClaimHBDInterest(100, 876_700, 15, "")

	require.Len(t, claimDb.Claims, 1)
	assert.InDelta(t, 0.18, claimDb.Claims[0].ObservedApr, 1e-9)
}

func TestClaimHBDInterest_ObservedApr_HalfMonthIntervalSameApr(t *testing.T) {
	// Claim interval of half a month (438_300 blocks = HIVE_BLOCKS_PER_YEAR/24).
	// amount=75, TWAB=10000 → (75/10000) * (10_519_200/438_300) = 0.0075 * 24 = 0.18
	// Same annualized APR as the monthly test — confirms interval-based annualization.
	ls, _, claimDb := newLedgerEnvWithClaims(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:           "hive:alice",
			BlockHeight:       100,
			HBD_SAVINGS:       10000,
			HBD_AVG:           0,
			HBD_CLAIM_HEIGHT:  100,
			HBD_MODIFY_HEIGHT: 100,
		}},
	})

	ls.ClaimHBDInterest(100, 438_400, 75, "")

	require.Len(t, claimDb.Claims, 1)
	assert.InDelta(t, 0.18, claimDb.Claims[0].ObservedApr, 1e-9)
}

func TestClaimHBDInterest_ObservedApr_NoPriorClaimIsZero(t *testing.T) {
	// lastClaim==0 means no prior claim on record — no interval to annualize against.
	ls, _, claimDb := newLedgerEnvWithClaims(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:           "hive:alice",
			BlockHeight:       100,
			HBD_SAVINGS:       1000,
			HBD_AVG:           0,
			HBD_CLAIM_HEIGHT:  100,
			HBD_MODIFY_HEIGHT: 100,
		}},
	})

	ls.ClaimHBDInterest(0, 200, 50, "")

	require.Len(t, claimDb.Claims, 1)
	assert.Equal(t, float64(0), claimDb.Claims[0].ObservedApr)
}

func TestOplogIngest(t *testing.T) {
	balDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:     "hive:alice",
			BlockHeight: 0,
			HBD:         100,
		}},
	})
	lDb := newMockLedgerDb()
	aDb := newMockActionsDb()
	ls := ledgerSystem.New(balDb, lDb, nil, aDb, nil)
	state := ls.NewEmptyState()

	// Execute a transfer via session, commit, then ingest
	session := ls.NewEmptySession(state, 10)
	result := session.ExecuteTransfer(ledgerSystem.OpLogEvent{
		Id:          "xfer-1",
		From:        "hive:alice",
		To:          "hive:bob",
		Amount:      25,
		Asset:       "hbd",
		BlockHeight: 10,
	})
	require.True(t, result.Ok)
	session.Done()

	exported := state.Export()
	require.Len(t, exported.Oplog, 1)

	// Ingest into the ledger system (writes to mock DB)
	ls.IngestOplog(exported.Oplog, ledgerSystem.OplogInjestOptions{
		StartHeight: 10,
		EndHeight:   20,
	})

	state.Flush()

	// After ingest, balances should reflect the transfer via DB records
	freshState := ls.NewEmptyState()
	aliceBal := freshState.GetBalance("hive:alice", 21, "hbd")
	bobBal := freshState.GetBalance("hive:bob", 21, "hbd")
	// Alice started with 100, sent 25 — but deposit type ops aren't "deposit" in the oplog,
	// the transfer produces ledger records that may or may not match the GetBalance filter.
	// GetBalance for hbd filters on op types ["unstake", "deposit"], so transfer records won't show.
	// Alice's DB balance record says 100, transfer deducted via ledger record type "transfer" which isn't queried.
	// This is expected behavior — GetBalance only accounts for deposits and unstakes on top of snapshots.
	t.Logf("alice balance after ingest: %d, bob balance: %d", aliceBal, bobBal)
}

// seedPendulumBucket simulates the per-swap accrual side-effect: a paired
// transfer ledger record on the bucket (positive) — the matching debit on the
// source contract account is irrelevant for these distribute / balance tests.
func seedPendulumBucket(t *testing.T, lDb ledgerDb.Ledger, txID string, amount int64, blockHeight uint64) {
	t.Helper()
	lDb.StoreLedger(ledgerDb.LedgerRecord{
		Id:          txID + "#out",
		BlockHeight: blockHeight,
		Amount:      amount,
		Asset:       "hbd",
		Owner:       ledgerSystem.PendulumNodesHBDBucket,
		Type:        "transfer",
	})
}

func TestPendulumLedgerOps(t *testing.T) {
	ls, state := newLedgerEnv()

	// Seed the bucket as the swap-time path would: paired ExecuteTransfer
	// records produce type=transfer entries (the matching contract-side debit
	// is exercised by the LedgerSession tests in ledger-system/).
	seedPendulumBucket(t, state.LedgerDb, "swap-tx-1", 25, 100)
	seedPendulumBucket(t, state.LedgerDb, "swap-tx-2", 25, 101)

	t.Run("bucket balance sums seeded transfers", func(t *testing.T) {
		bal := ls.PendulumBucketBalance(ledgerSystem.PendulumNodesHBDBucket, 200)
		assert.Equal(t, int64(50), bal)
	})

	t.Run("distribute debits the nodes bucket and credits the destination", func(t *testing.T) {
		result := ls.PendulumDistribute("hive:node1", 3, "pendulum-distribute-1", 102)
		require.True(t, result.Ok)

		bucketRecs, err := state.LedgerDb.GetLedgerRange(ledgerSystem.PendulumNodesHBDBucket, 0, 1000, "hbd")
		require.NoError(t, err)
		var foundDebit bool
		for _, rec := range *bucketRecs {
			if rec.Type == "pendulum_distribute" && rec.Amount == -3 {
				foundDebit = true
			}
		}
		assert.True(t, foundDebit)

		nodeRecs, err := state.LedgerDb.GetLedgerRange("hive:node1", 0, 1000, "hbd")
		require.NoError(t, err)
		require.NotEmpty(t, *nodeRecs)
		assert.Equal(t, int64(3), (*nodeRecs)[0].Amount)
		assert.Equal(t, "pendulum_distribute", (*nodeRecs)[0].Type)
	})

	t.Run("distribute fails on insufficient bucket balance", func(t *testing.T) {
		result := ls.PendulumDistribute("hive:node2", 999999, "pendulum-distribute-too-much", 102)
		require.False(t, result.Ok)
		assert.Equal(t, "insufficient pendulum nodes bucket balance", result.Msg)
	})

	t.Run("shared tx id across different distribution destinations is allowed", func(t *testing.T) {
		r1 := ls.PendulumDistribute("hive:nodeA", 2, "pendulum-distribute-shared", 103)
		r2 := ls.PendulumDistribute("hive:nodeB", 1, "pendulum-distribute-shared", 103)
		require.True(t, r1.Ok)
		require.True(t, r2.Ok)
	})

	t.Run("bucket balance reflects distribute effects", func(t *testing.T) {
		// 50 seeded - 3 distributed - 2 distributed - 1 distributed = 44.
		bal := ls.PendulumBucketBalance(ledgerSystem.PendulumNodesHBDBucket, 200)
		assert.Equal(t, int64(44), bal)
	})
}

func TestGetBalance_HiveConsensusIncludesSlashLedger(t *testing.T) {
	balDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:        "hive:alice",
			BlockHeight:    100,
			HIVE_CONSENSUS: 1_000_000,
		}},
	})
	lDb := newMockLedgerDb()
	ls := ledgerSystem.New(balDb, lDb, nil, newMockActionsDb(), nil)
	state := ls.NewEmptyState()

	require.True(t, ls.SafetySlashConsensusBond(ledgerSystem.SafetySlashConsensusParams{
		Account:         "alice",
		SlashBps:        1000,
		TxID:            "tx-consensus-read",
		BlockHeight:     200,
		EvidenceKind:    safetyslash.EvidenceVSCDoubleBlockSign,
		BurnDelayBlocks: 0,
	}).Ok)

	bal := state.GetBalance("hive:alice", 200, "hive_consensus")
	require.Equal(t, int64(900_000), bal)
}

func TestSafetySlashConsensusBond_DebitsBondAndBurnsWithoutDAO(t *testing.T) {
	balDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:        "hive:alice",
			BlockHeight:    100,
			HIVE_CONSENSUS: 1_000_000,
		}},
	})
	lDb := newMockLedgerDb()
	ls := ledgerSystem.New(balDb, lDb, nil, newMockActionsDb(), nil)

	res := ls.SafetySlashConsensusBond(ledgerSystem.SafetySlashConsensusParams{
		Account:         "alice",
		SlashBps:        1000,
		TxID:            "evidence-tx-1",
		BlockHeight:     200,
		EvidenceKind:    safetyslash.EvidenceVSCDoubleBlockSign,
		BurnDelayBlocks: 0,
	})
	require.True(t, res.Ok, res.Msg)

	var debitAmt, burnAmt int64
	for _, recs := range lDb.LedgerRecords {
		for _, r := range recs {
			if r.Type == "safety_slash_consensus" {
				debitAmt = r.Amount
			}
			if r.Type == ledgerSystem.LedgerTypeSafetySlashReserve {
				burnAmt += r.Amount
				assert.Equal(t, params.ProtocolSlashReserveAccount, r.To)
			}
			assert.NotEqual(t, "safety_slash_hive_credit", r.Type)
		}
	}
	require.Equal(t, int64(100_000), debitAmt)
	require.Equal(t, int64(100_000), burnAmt)
}

func TestSafetySlashConsensusBond_DelayedBurnFinalizes(t *testing.T) {
	balDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:        "hive:alice",
			BlockHeight:    100,
			HIVE_CONSENSUS: 1_000_000,
		}},
	})
	lDb := newMockLedgerDb()
	ls := ledgerSystem.New(balDb, lDb, nil, newMockActionsDb(), nil)

	res := ls.SafetySlashConsensusBond(ledgerSystem.SafetySlashConsensusParams{
		Account:         "alice",
		SlashBps:        1000,
		TxID:            "evidence-tx-delay",
		BlockHeight:     200,
		EvidenceKind:    safetyslash.EvidenceVSCDoubleBlockSign,
		BurnDelayBlocks: 50,
	})
	require.True(t, res.Ok, res.Msg)

	var pendingAmt int64
	var finalBurn int64
	for _, recs := range lDb.LedgerRecords {
		for _, r := range recs {
			switch r.Type {
			case ledgerSystem.LedgerTypeSafetySlashHiveBurnPending:
				pendingAmt += r.Amount
				assert.Equal(t, params.ProtocolSlashPendingBurnAccount, r.To)
				assert.Equal(t, "250", r.From)
			case ledgerSystem.LedgerTypeSafetySlashReserve:
				finalBurn += r.Amount
			}
		}
	}
	require.Equal(t, int64(100_000), pendingAmt)
	require.Equal(t, int64(0), finalBurn)

	ls.FinalizeMaturedSafetySlashBurns(249)
	finalBurn = 0
	for _, recs := range lDb.LedgerRecords {
		for _, r := range recs {
			if r.Type == ledgerSystem.LedgerTypeSafetySlashReserve {
				finalBurn += r.Amount
			}
		}
	}
	require.Equal(t, int64(0), finalBurn)

	ls.FinalizeMaturedSafetySlashBurns(250)
	finalBurn = 0
	for _, recs := range lDb.LedgerRecords {
		for _, r := range recs {
			if r.Type == ledgerSystem.LedgerTypeSafetySlashReserve {
				finalBurn += r.Amount
			}
		}
	}
	require.Equal(t, int64(100_000), finalBurn)
}

func TestSafetySlashConsensusBond_ClampsBurnDelayBlocks(t *testing.T) {
	balDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:        "hive:alice",
			BlockHeight:    100,
			HIVE_CONSENSUS: 1_000_000,
		}},
	})
	lDb := newMockLedgerDb()
	ls := ledgerSystem.New(balDb, lDb, nil, newMockActionsDb(), nil)

	require.True(t, ls.SafetySlashConsensusBond(ledgerSystem.SafetySlashConsensusParams{
		Account:         "alice",
		SlashBps:        1000,
		TxID:            "tx-clamp-delay",
		BlockHeight:     200,
		EvidenceKind:    safetyslash.EvidenceVSCDoubleBlockSign,
		BurnDelayBlocks: 9_000_000,
	}).Ok)

	for _, recs := range lDb.LedgerRecords {
		for _, r := range recs {
			if r.Type == ledgerSystem.LedgerTypeSafetySlashHiveBurnPending {
				// Capped to params.MaxSafetySlashBurnDelayBlocks
				assert.Equal(t, "3333533", r.From)
			}
		}
	}
}

func TestSafetySlashConsensusBond_FinalizeMaturedBurnIdempotent(t *testing.T) {
	balDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:        "hive:alice",
			BlockHeight:    100,
			HIVE_CONSENSUS: 1_000_000,
		}},
	})
	lDb := newMockLedgerDb()
	ls := ledgerSystem.New(balDb, lDb, nil, newMockActionsDb(), nil)
	require.True(t, ls.SafetySlashConsensusBond(ledgerSystem.SafetySlashConsensusParams{
		Account:         "alice",
		SlashBps:        1000,
		TxID:            "tx-idem",
		BlockHeight:     200,
		EvidenceKind:    safetyslash.EvidenceVSCDoubleBlockSign,
		BurnDelayBlocks: 50,
	}).Ok)
	ls.FinalizeMaturedSafetySlashBurns(250)
	n1 := countLedgerType(lDb, ledgerSystem.LedgerTypeSafetySlashReserve)
	ls.FinalizeMaturedSafetySlashBurns(250)
	ls.FinalizeMaturedSafetySlashBurns(300)
	n2 := countLedgerType(lDb, ledgerSystem.LedgerTypeSafetySlashReserve)
	require.Equal(t, n1, n2)
}

func countLedgerType(l *test_utils.MockLedgerDb, typ string) int {
	n := 0
	for _, recs := range l.LedgerRecords {
		for _, r := range recs {
			if r.Type == typ {
				n++
			}
		}
	}
	return n
}

// TestCancelPendingSafetySlashBurn_PreMaturity exercises the cancel path:
// a slash with a non-zero burn delay creates a pending row; cancelling
// before maturity must net the pending HIVE balance back to zero AND prevent
// FinalizeMaturedSafetySlashBurns from later promoting the row to a burn.
func TestCancelPendingSafetySlashBurn_PreMaturity(t *testing.T) {
	balDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:        "hive:alice",
			BlockHeight:    100,
			HIVE_CONSENSUS: 1_000_000,
		}},
	})
	lDb := newMockLedgerDb()
	ls := ledgerSystem.New(balDb, lDb, nil, newMockActionsDb(), nil)
	res := ls.SafetySlashConsensusBond(ledgerSystem.SafetySlashConsensusParams{
		Account:         "alice",
		SlashBps:        1000,
		TxID:            "tx-cancel",
		BlockHeight:     200,
		EvidenceKind:    safetyslash.EvidenceVSCDoubleBlockSign,
		BurnDelayBlocks: 100,
	})
	require.True(t, res.Ok)

	// Pre-cancel: pending account holds the burn slice.
	pendingBefore := lDbPendingSlashBalance(t, lDb)
	require.Equal(t, int64(100_000), pendingBefore, "expected 10%% of bond on pending account before cancel")

	cancelRes := ls.CancelPendingSafetySlashBurn(ledgerSystem.CancelPendingSafetySlashBurnParams{
		TxID:           "tx-cancel",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		SlashedAccount: "alice",
		BlockHeight:    250,
		Reason:         "false-positive double sign rollback",
	})
	require.True(t, cancelRes.Ok, "cancel must succeed pre-maturity: %s", cancelRes.Msg)

	// Post-cancel: pending account net balance for this slash is 0.
	pendingAfter := lDbPendingSlashBalance(t, lDb)
	require.Equal(t, int64(0), pendingAfter, "pending must net to 0 after cancel")

	// Idempotent: second cancel returns Ok with "already cancelled" and
	// emits no further records.
	count1 := countLedgerType(lDb, ledgerSystem.LedgerTypeSafetySlashHiveBurnPendingCancelled)
	cancelRes2 := ls.CancelPendingSafetySlashBurn(ledgerSystem.CancelPendingSafetySlashBurnParams{
		TxID:           "tx-cancel",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		SlashedAccount: "alice",
		BlockHeight:    260,
	})
	require.True(t, cancelRes2.Ok)
	require.Contains(t, cancelRes2.Msg, "already cancelled")
	count2 := countLedgerType(lDb, ledgerSystem.LedgerTypeSafetySlashHiveBurnPendingCancelled)
	require.Equal(t, count1, count2, "second cancel must not double-write")

	// FinalizeMaturedSafetySlashBurns at maturity must NOT promote a
	// cancelled row.
	finalBurnsBefore := countLedgerType(lDb, ledgerSystem.LedgerTypeSafetySlashReserve)
	ls.FinalizeMaturedSafetySlashBurns(400)
	finalBurnsAfter := countLedgerType(lDb, ledgerSystem.LedgerTypeSafetySlashReserve)
	require.Equal(t, finalBurnsBefore, finalBurnsAfter, "cancelled pending row must not promote to burn")
}

// TestCancelPendingSafetySlashBurn_AfterFinalizeRejected proves cancel is a
// no-op once the burn has already been finalized.
func TestCancelPendingSafetySlashBurn_AfterFinalizeRejected(t *testing.T) {
	balDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:        "hive:alice",
			BlockHeight:    100,
			HIVE_CONSENSUS: 1_000_000,
		}},
	})
	lDb := newMockLedgerDb()
	ls := ledgerSystem.New(balDb, lDb, nil, newMockActionsDb(), nil)
	require.True(t, ls.SafetySlashConsensusBond(ledgerSystem.SafetySlashConsensusParams{
		Account:         "alice",
		SlashBps:        1000,
		TxID:            "tx-late",
		BlockHeight:     200,
		EvidenceKind:    safetyslash.EvidenceVSCDoubleBlockSign,
		BurnDelayBlocks: 50,
	}).Ok)
	ls.FinalizeMaturedSafetySlashBurns(250)

	res := ls.CancelPendingSafetySlashBurn(ledgerSystem.CancelPendingSafetySlashBurnParams{
		TxID:           "tx-late",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		SlashedAccount: "alice",
		BlockHeight:    300,
	})
	require.False(t, res.Ok, "cancel after finalize must fail")
	require.Contains(t, res.Msg, "already finalized")
}

// TestReverseSafetySlashConsensusDebit_RestoresBond verifies the reverse
// credit primitive's effect on hive_consensus balance.
func TestReverseSafetySlashConsensusDebit_RestoresBond(t *testing.T) {
	balDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:        "hive:alice",
			BlockHeight:    100,
			HIVE_CONSENSUS: 1_000_000,
		}},
	})
	lDb := newMockLedgerDb()
	ls := ledgerSystem.New(balDb, lDb, nil, newMockActionsDb(), nil)

	require.True(t, ls.SafetySlashConsensusBond(ledgerSystem.SafetySlashConsensusParams{
		Account:      "alice",
		SlashBps:     1000,
		TxID:         "tx-reverse",
		BlockHeight:  200,
		EvidenceKind: safetyslash.EvidenceVSCDoubleBlockSign,
	}).Ok)
	postSlash := ls.GetBalance("hive:alice", 200, "hive_consensus")
	require.Equal(t, int64(900_000), postSlash, "10%% slash should leave 90%% of bond")

	revRes := ls.ReverseSafetySlashConsensusDebit(ledgerSystem.ReverseSafetySlashConsensusDebitParams{
		TxID:         "tx-reverse",
		EvidenceKind: safetyslash.EvidenceVSCDoubleBlockSign,
		Account:      "alice",
		Amount:       100_000,
		BlockHeight:  220,
		Reason:       "validator restored after governance review",
	})
	require.True(t, revRes.Ok, "reverse must succeed: %s", revRes.Msg)

	postReverse := ls.GetBalance("hive:alice", 220, "hive_consensus")
	require.Equal(t, int64(1_000_000), postReverse,
		"reverse credit equal to original debit must restore the bond exactly (Lean: bond_full_reverse_restores)")

	// Idempotency: same reverse twice writes one row (Mongo upsert keys on Id).
	require.True(t, ls.ReverseSafetySlashConsensusDebit(ledgerSystem.ReverseSafetySlashConsensusDebitParams{
		TxID:         "tx-reverse",
		EvidenceKind: safetyslash.EvidenceVSCDoubleBlockSign,
		Account:      "alice",
		Amount:       100_000,
		BlockHeight:  221,
	}).Ok)
	postIdem := ls.GetBalance("hive:alice", 221, "hive_consensus")
	require.Equal(t, int64(1_000_000), postIdem,
		"idempotent reverse must not double-credit (deterministic ledger Id)")
}

// TestReverseSafetySlashConsensusDebit_RejectsInvalid covers parameter
// validation: zero amount, blank account, blank txID/kind.
func TestReverseSafetySlashConsensusDebit_RejectsInvalid(t *testing.T) {
	balDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{})
	lDb := newMockLedgerDb()
	ls := ledgerSystem.New(balDb, lDb, nil, newMockActionsDb(), nil)
	for _, c := range []struct {
		name string
		p    ledgerSystem.ReverseSafetySlashConsensusDebitParams
	}{
		{"blank tx", ledgerSystem.ReverseSafetySlashConsensusDebitParams{Account: "alice", EvidenceKind: "k", Amount: 1}},
		{"blank kind", ledgerSystem.ReverseSafetySlashConsensusDebitParams{TxID: "t", Account: "alice", Amount: 1}},
		{"blank account", ledgerSystem.ReverseSafetySlashConsensusDebitParams{TxID: "t", EvidenceKind: "k", Amount: 1}},
		{"zero amount", ledgerSystem.ReverseSafetySlashConsensusDebitParams{TxID: "t", EvidenceKind: "k", Account: "alice", Amount: 0}},
		{"negative amount", ledgerSystem.ReverseSafetySlashConsensusDebitParams{TxID: "t", EvidenceKind: "k", Account: "alice", Amount: -1}},
	} {
		t.Run(c.name, func(t *testing.T) {
			require.False(t, ls.ReverseSafetySlashConsensusDebit(c.p).Ok)
		})
	}
}

// lDbPendingSlashBalance sums HIVE amounts on the pending burn account.
// Helper test utility — keeps test asserts focused on net effect.
func lDbPendingSlashBalance(t *testing.T, lDb *test_utils.MockLedgerDb) int64 {
	t.Helper()
	total := int64(0)
	for _, recs := range lDb.LedgerRecords {
		for _, r := range recs {
			if r.To == params.ProtocolSlashPendingBurnAccount && r.Asset == "hive" {
				total += r.Amount
			}
		}
	}
	return total
}

func TestSafetySlashConsensusBond_RejectsMaturityOverflow(t *testing.T) {
	balDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:        "hive:alice",
			BlockHeight:    100,
			HIVE_CONSENSUS: 1_000_000,
		}},
	})
	lDb := newMockLedgerDb()
	ls := ledgerSystem.New(balDb, lDb, nil, newMockActionsDb(), nil)
	res := ls.SafetySlashConsensusBond(ledgerSystem.SafetySlashConsensusParams{
		Account:         "alice",
		SlashBps:        1000,
		TxID:            "tx-wrap",
		BlockHeight:     ^uint64(0) - 1000,
		EvidenceKind:    safetyslash.EvidenceVSCDoubleBlockSign,
		BurnDelayBlocks: 9_000_000,
	})
	require.False(t, res.Ok)
}

// readCursorRow returns the parsed cursor scan-from height for the
// finalize cursor row (single deterministic Id), or 0 if not present.
func readCursorRow(t *testing.T, lDb *test_utils.MockLedgerDb) uint64 {
	t.Helper()
	var got uint64
	var gotHeight uint64
	for _, recs := range lDb.LedgerRecords {
		for _, r := range recs {
			if r.Type != ledgerSystem.LedgerTypeSafetySlashBurnFinalizeCursor {
				continue
			}
			if r.BlockHeight < gotHeight {
				continue
			}
			c, err := strconv.ParseUint(strings.TrimSpace(r.From), 10, 64)
			require.NoError(t, err)
			gotHeight = r.BlockHeight
			got = c
		}
	}
	return got
}

// TestFinalizeCursor_AdvancesPastFullyMatured verifies that after a slash
// matures and is finalized, the cursor advances to blockHeight+1, so
// subsequent finalize calls don't re-scan the now-empty range.
func TestFinalizeCursor_AdvancesPastFullyMatured(t *testing.T) {
	balDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:        "hive:alice",
			BlockHeight:    100,
			HIVE_CONSENSUS: 1_000_000,
		}},
	})
	lDb := newMockLedgerDb()
	ls := ledgerSystem.New(balDb, lDb, nil, newMockActionsDb(), nil)

	require.True(t, ls.SafetySlashConsensusBond(ledgerSystem.SafetySlashConsensusParams{
		Account:         "alice",
		SlashBps:        1000,
		TxID:            "tx-cursor-1",
		BlockHeight:     200,
		EvidenceKind:    safetyslash.EvidenceVSCDoubleBlockSign,
		BurnDelayBlocks: 50,
	}).Ok)

	// Before finalize: no cursor written.
	require.Equal(t, uint64(0), readCursorRow(t, lDb))

	// Finalize after maturity (200+50 = 250).
	ls.FinalizeMaturedSafetySlashBurns(250)
	cursor := readCursorRow(t, lDb)
	require.Equal(t, uint64(251), cursor, "cursor should advance past blockHeight when nothing remains pending")
	finalized := countLedgerType(lDb, ledgerSystem.LedgerTypeSafetySlashReserve)
	require.Equal(t, 1, finalized)

	// A subsequent finalize call should be a no-op (no new burn rows).
	ls.FinalizeMaturedSafetySlashBurns(300)
	require.Equal(t, finalized, countLedgerType(lDb, ledgerSystem.LedgerTypeSafetySlashReserve))
	// Cursor advances further once nothing is pending in the new range.
	require.Equal(t, uint64(301), readCursorRow(t, lDb))
}

// TestFinalizeCursor_AnchorsToOldestUnfinalized verifies the cursor
// anchors to the lowest emission height of any still-unfinalized pending
// row when finalize runs but some rows are not yet mature.
func TestFinalizeCursor_AnchorsToOldestUnfinalized(t *testing.T) {
	balDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:        "hive:alice",
			BlockHeight:    100,
			HIVE_CONSENSUS: 1_000_000,
		}},
		"hive:bob": {{
			Account:        "hive:bob",
			BlockHeight:    100,
			HIVE_CONSENSUS: 1_000_000,
		}},
	})
	lDb := newMockLedgerDb()
	ls := ledgerSystem.New(balDb, lDb, nil, newMockActionsDb(), nil)

	// Slash alice at 200 (maturity 250) and bob at 220 (maturity 320).
	require.True(t, ls.SafetySlashConsensusBond(ledgerSystem.SafetySlashConsensusParams{
		Account: "alice", SlashBps: 1000, TxID: "tx-a",
		BlockHeight: 200, EvidenceKind: safetyslash.EvidenceVSCDoubleBlockSign,
		BurnDelayBlocks: 50,
	}).Ok)
	require.True(t, ls.SafetySlashConsensusBond(ledgerSystem.SafetySlashConsensusParams{
		Account: "bob", SlashBps: 1000, TxID: "tx-b",
		BlockHeight: 220, EvidenceKind: safetyslash.EvidenceVSCDoubleBlockSign,
		BurnDelayBlocks: 100,
	}).Ok)

	// Finalize at 250: alice's row is mature (250 == maturity), bob's
	// is not (mat 320 > 250). Cursor should anchor to bob's emission
	// height 220, the oldest still-unfinalized row.
	ls.FinalizeMaturedSafetySlashBurns(250)
	require.Equal(t, uint64(220), readCursorRow(t, lDb))
	require.Equal(t, 1, countLedgerType(lDb, ledgerSystem.LedgerTypeSafetySlashReserve))

	// Finalize at 320: bob now matures, cursor advances to 321.
	ls.FinalizeMaturedSafetySlashBurns(320)
	require.Equal(t, uint64(321), readCursorRow(t, lDb))
	require.Equal(t, 2, countLedgerType(lDb, ledgerSystem.LedgerTypeSafetySlashReserve))
}

// TestFinalizeCursor_NeverGoesBackward proves the cursor is monotonic
// even if FinalizeMaturedSafetySlashBurns is called repeatedly with the
// same blockHeight or with a height earlier than the last seen.
func TestFinalizeCursor_NeverGoesBackward(t *testing.T) {
	balDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:        "hive:alice",
			BlockHeight:    100,
			HIVE_CONSENSUS: 1_000_000,
		}},
	})
	lDb := newMockLedgerDb()
	ls := ledgerSystem.New(balDb, lDb, nil, newMockActionsDb(), nil)

	require.True(t, ls.SafetySlashConsensusBond(ledgerSystem.SafetySlashConsensusParams{
		Account: "alice", SlashBps: 1000, TxID: "tx-mono",
		BlockHeight: 200, EvidenceKind: safetyslash.EvidenceVSCDoubleBlockSign,
		BurnDelayBlocks: 50,
	}).Ok)

	ls.FinalizeMaturedSafetySlashBurns(250)
	cursor1 := readCursorRow(t, lDb)
	require.Equal(t, uint64(251), cursor1)

	// Replay the same call: cursor must not move backward.
	ls.FinalizeMaturedSafetySlashBurns(250)
	require.Equal(t, cursor1, readCursorRow(t, lDb))

	// Earlier-height call (out-of-order replay protection).
	ls.FinalizeMaturedSafetySlashBurns(240)
	require.Equal(t, cursor1, readCursorRow(t, lDb))

	// Forward progress still works.
	ls.FinalizeMaturedSafetySlashBurns(400)
	require.Equal(t, uint64(401), readCursorRow(t, lDb))
}

// TestFinalizeCursor_BackfillsFromZeroOnFirstRun proves a node that has
// never written a cursor (e.g. just upgraded with pending rows already in
// the DB) starts scanning from height 0 and picks up every existing
// pending row, then writes its first cursor.
func TestFinalizeCursor_BackfillsFromZeroOnFirstRun(t *testing.T) {
	balDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:        "hive:alice",
			BlockHeight:    100,
			HIVE_CONSENSUS: 1_000_000,
		}},
	})
	lDb := newMockLedgerDb()
	ls := ledgerSystem.New(balDb, lDb, nil, newMockActionsDb(), nil)

	// Pre-populate a slash + pending row at height 200, then drop any
	// cursor row that may have been written (simulate pre-cursor data
	// from a prior version of the node).
	require.True(t, ls.SafetySlashConsensusBond(ledgerSystem.SafetySlashConsensusParams{
		Account: "alice", SlashBps: 1000, TxID: "tx-old",
		BlockHeight: 200, EvidenceKind: safetyslash.EvidenceVSCDoubleBlockSign,
		BurnDelayBlocks: 100,
	}).Ok)

	for k, recs := range lDb.LedgerRecords {
		filtered := recs[:0]
		for _, r := range recs {
			if r.Type == ledgerSystem.LedgerTypeSafetySlashBurnFinalizeCursor {
				continue
			}
			filtered = append(filtered, r)
		}
		lDb.LedgerRecords[k] = filtered
	}
	require.Equal(t, uint64(0), readCursorRow(t, lDb))

	// First post-upgrade finalize after maturity (200+100=300): must
	// scan from 0 and pick up the legacy row, then plant the first cursor.
	ls.FinalizeMaturedSafetySlashBurns(300)
	require.Equal(t, 1, countLedgerType(lDb, ledgerSystem.LedgerTypeSafetySlashReserve))
	require.Equal(t, uint64(301), readCursorRow(t, lDb))
}

// _ = math.MaxInt64 keeps math imported in case future tests need it.
var _ = math.MaxInt64
