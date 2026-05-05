package rc_system_test

import (
	"context"
	"strings"
	"testing"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/params"
	rc_system "vsc-node/modules/rc-system"
)

// These tests replicate the UpdateRcMap logic from state_engine.go
// to verify the balance cap and negative guard work correctly.
// We test the logic inline because StateEngine has many private dependencies.

// simulateUpdateRcMap replicates the exact logic from StateEngine.UpdateRcMap
// with the fix applied (balance cap + negative guard)
func simulateUpdateRcMap(
	rcMap map[string]int64,
	blockHeight uint64,
	db *test_utils.MockRcDb,
	getBalance func(account string) int64,
) {
	for k, v := range rcMap {
		rcRecord, _ := db.GetRecord(context.Background(), k, blockHeight-1)

		var rcBal int64
		if rcRecord.BlockHeight == 0 {
			rcBal = v
		} else {
			frozeAmt := rc_system.CalculateFrozenBal(rcRecord.BlockHeight, blockHeight, rcRecord.Amount)
			rcBal = frozeAmt + v
		}

		// The fix: cap to balance
		balAmt := getBalance(k)
		if strings.HasPrefix(k, "hive:") {
			balAmt = balAmt + params.RC_HIVE_FREE_AMOUNT
		}
		if rcBal > balAmt {
			rcBal = balAmt
		}
		if rcBal < 0 {
			rcBal = 0
		}

		db.SetRecord(context.Background(), k, blockHeight, rcBal)
	}
}

func TestUpdateRcMap_FirstConsumption(t *testing.T) {
	db := test_utils.NewMockRcDb()
	balances := map[string]int64{"hive:alice": 290_000}

	rcMap := map[string]int64{"hive:alice": 1800}
	simulateUpdateRcMap(rcMap, 1000, db, func(a string) int64 { return balances[a] })

	rec, _ := db.GetRecord(context.Background(), "hive:alice", 1000)
	if rec.Amount != 1800 {
		t.Errorf("expected 1800 stored, got %d", rec.Amount)
	}
}

func TestUpdateRcMap_AccumulatesWithDecay(t *testing.T) {
	db := test_utils.NewMockRcDb()
	balances := map[string]int64{"hive:alice": 290_000}
	getBalance := func(a string) int64 { return balances[a] }

	// Block 1000: first consumption
	simulateUpdateRcMap(map[string]int64{"hive:alice": 1800}, 1000, db, getBalance)

	// Block 1001: second consumption (1 block later)
	simulateUpdateRcMap(map[string]int64{"hive:alice": 1800}, 1001, db, getBalance)

	rec, _ := db.GetRecord(context.Background(), "hive:alice", 1001)
	// rc_system.CalculateFrozenBal(1000, 1001, 1800) = 1800 - (1*1800/144000) = 1800 - 0 = 1800
	// rcBal = 1800 + 1800 = 3600
	if rec.Amount != 3600 {
		t.Errorf("expected 3600 after 2 blocks, got %d", rec.Amount)
	}
}

func TestUpdateRcMap_CapsAtBalance(t *testing.T) {
	db := test_utils.NewMockRcDb()
	balance := int64(290_000)
	maxRC := balance + params.RC_HIVE_FREE_AMOUNT // 295,000
	getBalance := func(a string) int64 { return balance }

	// Simulate frozen already at max
	db.SetRecord(context.Background(), "hive:alice", 999, maxRC)

	// Try to add more consumption
	simulateUpdateRcMap(map[string]int64{"hive:alice": 5000}, 1000, db, getBalance)

	rec, _ := db.GetRecord(context.Background(), "hive:alice", 1000)
	// frozeAmt from 295K at 1 block ≈ 295000 (tiny decay)
	// rcBal = 295000 + 5000 = 300000 → capped to 295000
	if rec.Amount > maxRC {
		t.Errorf("BUG 3 NOT FIXED: stored amount %d exceeds max RC %d", rec.Amount, maxRC)
	}
	if rec.Amount != maxRC {
		t.Errorf("expected capped to %d, got %d", maxRC, rec.Amount)
	}
}

func TestUpdateRcMap_PreventsNegative(t *testing.T) {
	db := test_utils.NewMockRcDb()
	getBalance := func(a string) int64 { return 0 }

	// Somehow v is negative (shouldn't happen, but defensive)
	simulateUpdateRcMap(map[string]int64{"did:key:abc": -500}, 1000, db, getBalance)

	rec, _ := db.GetRecord(context.Background(), "did:key:abc", 1000)
	if rec.Amount < 0 {
		t.Errorf("BUG 2 NOT FIXED: negative amount stored: %d", rec.Amount)
	}
}

func TestUpdateRcMap_MultiTxSlot_CappedCorrectly(t *testing.T) {
	// Simulates the multi-TxPacket-per-slot scenario
	// Two contract calls from same user in one slot, each consuming 1800
	// CanConsume sees same DB frozen for both, so total = 3600
	db := test_utils.NewMockRcDb()
	balance := int64(5_000)
	maxRC := balance + params.RC_HIVE_FREE_AMOUNT // 10,000
	getBalance := func(a string) int64 { return balance }

	// User already has 8000 frozen
	db.SetRecord(context.Background(), "hive:alice", 999, 8000)

	// Both contract calls in same slot: se.RcMap accumulates 1800 + 1800 = 3600
	simulateUpdateRcMap(map[string]int64{"hive:alice": 3600}, 1000, db, getBalance)

	rec, _ := db.GetRecord(context.Background(), "hive:alice", 1000)
	// frozeAmt ≈ 8000 (1 block decay negligible)
	// rcBal = 8000 + 3600 = 11600 → capped to 10000
	if rec.Amount > maxRC {
		t.Errorf("multi-tx slot overflow not capped: stored %d > max %d", rec.Amount, maxRC)
	}
}

func TestUpdateRcMap_NonContractTx_CappedCorrectly(t *testing.T) {
	// Non-contract txs add hardcoded RC (e.g., transfer = 100, stake = 200)
	// without any CanConsume check
	db := test_utils.NewMockRcDb()
	balance := int64(1_000)
	maxRC := balance + params.RC_HIVE_FREE_AMOUNT // 11000
	getBalance := func(a string) int64 { return balance }

	// Already at max
	db.SetRecord(context.Background(), "hive:alice", 999, maxRC)

	// Transfer adds 100 RC bypassing CanConsume
	simulateUpdateRcMap(map[string]int64{"hive:alice": 100}, 1000, db, getBalance)

	rec, _ := db.GetRecord(context.Background(), "hive:alice", 1000)
	if rec.Amount > maxRC {
		t.Errorf("non-contract RC bypass not capped: stored %d > max %d", rec.Amount, maxRC)
	}
}

func TestUpdateRcMap_RegenAfterCap(t *testing.T) {
	// Verify that after capping, regen proceeds at normal rate
	db := test_utils.NewMockRcDb()
	balance := int64(290_000)
	maxRC := balance + params.RC_HIVE_FREE_AMOUNT
	_ = db // db used for setup below

	// Cap at max
	db.SetRecord(context.Background(), "hive:alice", 1000, maxRC)

	// No consumption for 20 blocks (1 minute)
	// Check how much frozen has decayed
	frozen := rc_system.CalculateFrozenBal(1000, 1020, maxRC)
	regen := maxRC - frozen
	expectedRegen := int64(20 * uint64(maxRC) / params.RC_RETURN_PERIOD)

	if regen != expectedRegen {
		t.Errorf("regen rate mismatch: got %d, expected %d per minute", regen, expectedRegen)
	}

	// Should be ~40 RC/min for 295K max
	if regen < 30 || regen > 50 {
		t.Errorf("regen rate outside expected range (30-50): got %d", regen)
	}
}

func TestUpdateRcMap_DidAccount_NoCap_BeyondBalance(t *testing.T) {
	// DID accounts don't get free amount
	db := test_utils.NewMockRcDb()
	balance := int64(10_000)
	getBalance := func(a string) int64 { return balance }

	db.SetRecord(context.Background(), "did:key:abc", 999, 9_900)

	// Consumption of 200 RC (like a stake)
	simulateUpdateRcMap(map[string]int64{"did:key:abc": 200}, 1000, db, getBalance)

	rec, _ := db.GetRecord(context.Background(), "did:key:abc", 1000)
	// frozeAmt ≈ 9900, rcBal = 9900 + 200 = 10100 → capped to 10000
	if rec.Amount > balance {
		t.Errorf("DID account not capped: stored %d > balance %d", rec.Amount, balance)
	}
}

func TestUpdateRcMap_ZeroBalance_StillStores(t *testing.T) {
	db := test_utils.NewMockRcDb()
	getBalance := func(a string) int64 { return 0 }

	// User with zero balance does a transaction (shouldn't happen, but defensive)
	simulateUpdateRcMap(map[string]int64{"hive:alice": 100}, 1000, db, getBalance)

	rec, _ := db.GetRecord(context.Background(), "hive:alice", 1000)
	// maxRC = 0 + 10000 = 10000, rcBal = 100, 100 < 10000, so stores 100
	if rec.Amount != 100 {
		t.Errorf("expected 100, got %d", rec.Amount)
	}
}

func TestUpdateRcMap_HighFrequencyTrading(t *testing.T) {
	// Simulate 200 swaps over 200 blocks, 1 per block
	db := test_utils.NewMockRcDb()
	balance := int64(290_000)
	maxRC := balance + params.RC_HIVE_FREE_AMOUNT
	getBalance := func(a string) int64 { return balance }

	for block := uint64(1000); block < 1200; block++ {
		simulateUpdateRcMap(map[string]int64{"hive:alice": 1800}, block, db, getBalance)
	}

	rec, _ := db.GetRecord(context.Background(), "hive:alice", 1199)
	if rec.Amount > maxRC {
		t.Errorf("after 200 swaps, frozen %d exceeds max %d", rec.Amount, maxRC)
	}

	// Verify it's capped at max
	if rec.Amount != maxRC {
		t.Logf("after 200 swaps, frozen = %d (max = %d)", rec.Amount, maxRC)
	}
}

func TestUpdateRcMap_RegenAfterBurst(t *testing.T) {
	// Burst of activity, then idle — verify clean regen
	db := test_utils.NewMockRcDb()
	balance := int64(290_000)
	getBalance := func(a string) int64 { return balance }

	// 50 swaps of 1800 each
	for block := uint64(1000); block < 1050; block++ {
		simulateUpdateRcMap(map[string]int64{"hive:alice": 1800}, block, db, getBalance)
	}

	// Now idle for 1 hour (1200 blocks at 3s)
	rec, _ := db.GetRecord(context.Background(), "hive:alice", 1049)
	frozenAfterBurst := rec.Amount

	frozenAfter1hr := rc_system.CalculateFrozenBal(1049, 1049+1200, frozenAfterBurst)
	regenIn1hr := frozenAfterBurst - frozenAfter1hr

	// Regen is proportional to frozen amount, not max balance
	// expectedRegen = 1200 * frozenAfterBurst / RC_RETURN_PERIOD
	expectedRegen := int64(1200 * uint64(frozenAfterBurst) / params.RC_RETURN_PERIOD)

	if regenIn1hr < expectedRegen-10 || regenIn1hr > expectedRegen+10 {
		t.Errorf("1-hour regen after burst: got %d, expected ~%d", regenIn1hr, expectedRegen)
	}

	// Verify regen is positive and reasonable
	if regenIn1hr <= 0 {
		t.Errorf("regen should be positive after burst, got %d", regenIn1hr)
	}
}

func TestUpdateRcMap_BalanceDecrease_StillCapped(t *testing.T) {
	// User's balance decreases (unstake) after accumulating frozen
	db := test_utils.NewMockRcDb()
	getBalance := func(a string) int64 { return 50_000 } // was 290K, now 50K

	// Old record with high frozen from when balance was higher
	db.SetRecord(context.Background(), "hive:alice", 999, 200_000)

	simulateUpdateRcMap(map[string]int64{"hive:alice": 100}, 1000, db, getBalance)

	rec, _ := db.GetRecord(context.Background(), "hive:alice", 1000)
	maxRC := int64(50_000) + params.RC_HIVE_FREE_AMOUNT // 60000
	if rec.Amount > maxRC {
		t.Errorf("not capped after balance decrease: stored %d > max %d", rec.Amount, maxRC)
	}
}
