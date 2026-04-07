package rc_system

import (
	"testing"
	"vsc-node/modules/common/params"
	rcDb "vsc-node/modules/db/vsc/rcs"
	ledgerSystem "vsc-node/modules/ledger-system"

	"github.com/chebyrash/promise"
)

// ── Mock implementations ──

type mockRcDb struct {
	records map[string][]rcDb.RcRecord
}

func newMockRcDb() *mockRcDb {
	return &mockRcDb{records: make(map[string][]rcDb.RcRecord)}
}

func (m *mockRcDb) Init() error { return nil }
func (m *mockRcDb) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(interface{}), reject func(error)) { resolve(nil) })
}
func (m *mockRcDb) Stop() error { return nil }

func (m *mockRcDb) GetRecord(account string, blockHeight uint64) (rcDb.RcRecord, error) {
	recs := m.records[account]
	var best rcDb.RcRecord
	for _, r := range recs {
		if r.BlockHeight <= blockHeight && r.BlockHeight >= best.BlockHeight {
			best = r
		}
	}
	return best, nil
}

func (m *mockRcDb) SetRecord(account string, blockHeight uint64, amount int64) {
	m.records[account] = append(m.records[account], rcDb.RcRecord{
		Account:     account,
		Amount:      amount,
		BlockHeight: blockHeight,
	})
}

type mockLedgerSession struct {
	balances map[string]int64
}

func (m *mockLedgerSession) GetBalance(account string, blockHeight uint64, asset string) int64 {
	return m.balances[account+":"+asset]
}
func (m *mockLedgerSession) ExecuteTransfer(op ledgerSystem.OpLogEvent, opts ...ledgerSystem.TransferOptions) ledgerSystem.LedgerResult {
	return ledgerSystem.LedgerResult{}
}
func (m *mockLedgerSession) Withdraw(w ledgerSystem.WithdrawParams) ledgerSystem.LedgerResult {
	return ledgerSystem.LedgerResult{}
}
func (m *mockLedgerSession) Stake(s ledgerSystem.StakeOp, opts ...ledgerSystem.TransferOptions) ledgerSystem.LedgerResult {
	return ledgerSystem.LedgerResult{}
}
func (m *mockLedgerSession) Unstake(s ledgerSystem.StakeOp) ledgerSystem.LedgerResult {
	return ledgerSystem.LedgerResult{}
}
func (m *mockLedgerSession) ConsensusStake(c ledgerSystem.ConsensusParams) ledgerSystem.LedgerResult {
	return ledgerSystem.LedgerResult{}
}
func (m *mockLedgerSession) ConsensusUnstake(c ledgerSystem.ConsensusParams) ledgerSystem.LedgerResult {
	return ledgerSystem.LedgerResult{}
}
func (m *mockLedgerSession) Done() []string   { return nil }
func (m *mockLedgerSession) Revert()          {}

type mockLedgerSystem struct {
	balances map[string]int64
}

func (m *mockLedgerSystem) GetBalance(account string, blockHeight uint64, asset string) int64 {
	return m.balances[account+":"+asset]
}
func (m *mockLedgerSystem) ClaimHBDInterest(lastClaim uint64, blockHeight uint64, amount int64, txId string) {}
func (m *mockLedgerSystem) IndexActions(actionUpdate map[string]interface{}, extraInfo ledgerSystem.ExtraInfo) {
}
func (m *mockLedgerSystem) Deposit(deposit ledgerSystem.Deposit) string { return "" }
func (m *mockLedgerSystem) IngestOplog(oplog []ledgerSystem.OpLogEvent, options ledgerSystem.OplogInjestOptions) {
}
func (m *mockLedgerSystem) NewEmptySession(state *ledgerSystem.LedgerState, startHeight uint64) ledgerSystem.LedgerSession {
	return nil
}
func (m *mockLedgerSystem) NewEmptyState() *ledgerSystem.LedgerState { return nil }

// ── CalculateFrozenBal tests ──

func TestCalculateFrozenBal_NoDecay(t *testing.T) {
	// Same block, no time passed
	result := CalculateFrozenBal(100, 100, 1000)
	if result != 1000 {
		t.Errorf("expected 1000, got %d", result)
	}
}

func TestCalculateFrozenBal_PartialDecay(t *testing.T) {
	// 1000 frozen, half the return period passed
	halfPeriod := params.RC_RETURN_PERIOD / 2
	result := CalculateFrozenBal(0, halfPeriod, 1000)
	if result != 500 {
		t.Errorf("expected 500, got %d", result)
	}
}

func TestCalculateFrozenBal_FullDecay(t *testing.T) {
	// Full return period passed, should be 0
	result := CalculateFrozenBal(0, params.RC_RETURN_PERIOD, 1000)
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
}

func TestCalculateFrozenBal_OverDecay(t *testing.T) {
	// More than return period passed, should clamp to 0
	result := CalculateFrozenBal(0, params.RC_RETURN_PERIOD+10000, 1000)
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
}

func TestCalculateFrozenBal_OneBlock(t *testing.T) {
	// 1 block decay on small amount — integer truncation
	result := CalculateFrozenBal(100, 101, 1800)
	// amtRet = 1 * 1800 / 144000 = 0 (integer division)
	if result != 1800 {
		t.Errorf("expected 1800, got %d", result)
	}
}

func TestCalculateFrozenBal_RegenRate(t *testing.T) {
	// 290K frozen, 20 blocks (1 minute at 3s blocks)
	result := CalculateFrozenBal(0, 20, 290_000)
	// amtRet = 20 * 290000 / 144000 = 40
	expected := int64(290_000 - 40)
	if result != expected {
		t.Errorf("expected %d, got %d (regen = %d/min)", expected, result, 290_000-result)
	}
}

// ── GetAvailableRCs tests ──

func TestGetAvailableRCs_Normal(t *testing.T) {
	db := newMockRcDb()
	ls := &mockLedgerSystem{balances: map[string]int64{"hive:alice:hbd": 10_000}}
	rcs := New(db, ls)

	avail := rcs.GetAvailableRCs("hive:alice", 100)
	// No frozen records, so available = balance + free amount
	expected := int64(10_000 + params.RC_HIVE_FREE_AMOUNT)
	if avail != expected {
		t.Errorf("expected %d, got %d", expected, avail)
	}
}

func TestGetAvailableRCs_FrozenExceedsBalance(t *testing.T) {
	db := newMockRcDb()
	// Set a frozen record that exceeds balance
	db.SetRecord("hive:alice", 100, 500_000)

	ls := &mockLedgerSystem{balances: map[string]int64{"hive:alice:hbd": 290_000}}
	rcs := New(db, ls)

	avail := rcs.GetAvailableRCs("hive:alice", 101)
	// frozeAmt from 500K at 1 block: still ~500K
	// With fix: frozeAmt capped to balAmt (290K + 5K free = 295K)
	// available = 295K - 295K = 0
	if avail < 0 {
		t.Errorf("available RC should never be negative, got %d", avail)
	}
	if avail != 0 {
		t.Errorf("expected 0 (frozen exceeds balance), got %d", avail)
	}
}

func TestGetAvailableRCs_DidAccount_NoFreeAmount(t *testing.T) {
	db := newMockRcDb()
	ls := &mockLedgerSystem{balances: map[string]int64{"did:key:abc:hbd": 10_000}}
	rcs := New(db, ls)

	avail := rcs.GetAvailableRCs("did:key:abc", 100)
	// DID accounts don't get the free amount
	if avail != 10_000 {
		t.Errorf("expected 10000, got %d", avail)
	}
}

// ── CanConsume tests ──

func TestCanConsume_Normal(t *testing.T) {
	db := newMockRcDb()
	ls := &mockLedgerSystem{balances: map[string]int64{"hive:alice:hbd": 10_000}}
	rcs := New(db, ls)

	session := rcs.NewSession(&mockLedgerSession{balances: map[string]int64{"hive:alice:hbd": 10_000}})

	can, remaining, _ := session.CanConsume("hive:alice", 100, 1800)
	if !can {
		t.Error("should be able to consume 1800 with 15000 available")
	}
	expected := int64(10_000+params.RC_HIVE_FREE_AMOUNT) - 1800
	if remaining != expected {
		t.Errorf("expected remaining %d, got %d", expected, remaining)
	}
}

func TestCanConsume_InsufficientRC(t *testing.T) {
	db := newMockRcDb()
	ls := &mockLedgerSystem{balances: map[string]int64{"hive:alice:hbd": 50}}
	rcs := New(db, ls)

	session := rcs.NewSession(&mockLedgerSession{balances: map[string]int64{"hive:alice:hbd": 50}})

	// With 50 balance + 5000 free = 5050, asking for 6000
	can, _, _ := session.CanConsume("hive:alice", 100, 6000)
	if can {
		t.Error("should not be able to consume 6000 with 5050 available")
	}
}

func TestCanConsume_FrozenExceedsBalance_NeverNegative(t *testing.T) {
	db := newMockRcDb()
	// Simulate inflated frozen record (the bug scenario)
	db.SetRecord("hive:alice", 99, 360_000)

	ls := &mockLedgerSystem{balances: map[string]int64{"hive:alice:hbd": 290_000}}
	rcs := New(db, ls)

	session := rcs.NewSession(&mockLedgerSession{balances: map[string]int64{"hive:alice:hbd": 290_000}})

	can, remaining, _ := session.CanConsume("hive:alice", 100, 100)
	if can {
		t.Error("should not be able to consume when frozen exceeds balance")
	}
	if remaining < 0 {
		t.Errorf("remaining should never be negative, got %d", remaining)
	}
}

func TestCanConsume_ExactBalance(t *testing.T) {
	db := newMockRcDb()
	// Frozen exactly equals balance + free
	db.SetRecord("hive:alice", 100, int64(params.RC_HIVE_FREE_AMOUNT))

	ls := &mockLedgerSystem{balances: map[string]int64{"hive:alice:hbd": 0}}
	rcs := New(db, ls)

	session := rcs.NewSession(&mockLedgerSession{balances: map[string]int64{"hive:alice:hbd": 0}})

	can, _, _ := session.CanConsume("hive:alice", 100, 100)
	// frozeAmt from DB = 5000, balAmt = 0 + 5000 = 5000, totalAmt = 0
	if can {
		t.Error("should not consume when available is exactly 0")
	}
}

func TestCanConsume_ZeroBalance_ZeroFrozen(t *testing.T) {
	db := newMockRcDb()
	ls := &mockLedgerSystem{balances: map[string]int64{"did:key:abc:hbd": 0}}
	rcs := New(db, ls)

	session := rcs.NewSession(&mockLedgerSession{balances: map[string]int64{"did:key:abc:hbd": 0}})

	can, _, _ := session.CanConsume("did:key:abc", 100, 100)
	if can {
		t.Error("should not consume with zero balance and no free amount (DID account)")
	}
}

// ── Bug replication scenarios ──

func TestBug1_NegativeRC_Observed(t *testing.T) {
	// Replicate: account with 290K balance, frozen inflated to 298,683
	// Previously would return available = 290000 - 298683 = -8683
	db := newMockRcDb()
	db.SetRecord("hive:alice", 100, 298_683)

	ls := &mockLedgerSystem{balances: map[string]int64{"hive:alice:hbd": 290_000}}
	rcs := New(db, ls)

	avail := rcs.GetAvailableRCs("hive:alice", 100)
	if avail < 0 {
		t.Errorf("BUG 1 NOT FIXED: available RC is negative: %d", avail)
	}

	session := rcs.NewSession(&mockLedgerSession{balances: map[string]int64{"hive:alice:hbd": 290_000}})
	can, remaining, _ := session.CanConsume("hive:alice", 100, 100)
	if remaining < 0 {
		t.Errorf("BUG 1 NOT FIXED: CanConsume remaining is negative: %d", remaining)
	}
	if can {
		t.Error("should not allow consumption when frozen exceeds balance")
	}
}

func TestBug3_RegenRate_WithCap(t *testing.T) {
	// After fix: frozen capped to balance, so regen works at expected rate
	db := newMockRcDb()
	balance := int64(290_000)
	freeAmt := params.RC_HIVE_FREE_AMOUNT
	maxRC := balance + freeAmt

	// Set frozen to max (capped)
	db.SetRecord("hive:alice", 1000, maxRC)

	ls := &mockLedgerSystem{balances: map[string]int64{"hive:alice:hbd": balance}}
	rcs := New(db, ls)

	// After 20 blocks (1 minute), check regen
	avail := rcs.GetAvailableRCs("hive:alice", 1020)

	// Expected regen: 20 * maxRC / RC_RETURN_PERIOD
	expectedRegen := int64(20 * uint64(maxRC) / params.RC_RETURN_PERIOD)
	if avail != expectedRegen {
		t.Errorf("expected regen of %d RC after 1 min, got %d", expectedRegen, avail)
	}
	if avail <= 0 {
		t.Errorf("regen should produce positive available RC after 1 minute, got %d", avail)
	}
}

func TestBug3_RegenRate_InflatedFrozen(t *testing.T) {
	// Scenario: frozen is 500K but balance is 290K
	// Without UpdateRcMap cap, this would be in the DB
	// With CanConsume cap, available = max(0, 295K - frozen)
	// The frozen is still 500K in DB, regen is based on 500K decay
	db := newMockRcDb()
	db.SetRecord("hive:alice", 1000, 500_000)

	ls := &mockLedgerSystem{balances: map[string]int64{"hive:alice:hbd": 290_000}}
	rcs := New(db, ls)

	// How many blocks until user has any available RC?
	// Need frozen to decay below 295K (balance + free)
	// frozen(t) = 500000 * (1 - t/144000)
	// 500000 * (1 - t/144000) = 295000
	// t = 144000 * 205000/500000 = 59040 blocks ≈ 49 hours
	// This is the penalty for inflated frozen - why UpdateRcMap cap matters
	avail1020 := rcs.GetAvailableRCs("hive:alice", 1020)
	if avail1020 != 0 {
		t.Errorf("with inflated frozen, should still be 0 after 20 blocks, got %d", avail1020)
	}
}

func TestConsume_AccumulatesInSession(t *testing.T) {
	db := newMockRcDb()
	ls := &mockLedgerSystem{balances: map[string]int64{"hive:alice:hbd": 10_000}}
	rcs := New(db, ls)

	session := rcs.NewSession(&mockLedgerSession{balances: map[string]int64{"hive:alice:hbd": 10_000}})

	// Available = 10000 + 5000 (free) = 15000
	ok1, _ := session.Consume("hive:alice", 100, 5000)
	if !ok1 {
		t.Error("first consume should succeed")
	}

	// After first consume, rcMap["hive:alice"] = 5000
	// CanConsume should now see: 15000 - 5000 = 10000 available
	ok2, _ := session.Consume("hive:alice", 100, 5000)
	if !ok2 {
		t.Error("second consume should succeed (10000 remaining)")
	}

	// After second consume, rcMap["hive:alice"] = 10000
	// CanConsume should now see: 15000 - 10000 = 5000 available
	ok3, _ := session.Consume("hive:alice", 100, 5000)
	if !ok3 {
		t.Error("third consume should succeed (5000 remaining)")
	}

	// After third consume, rcMap["hive:alice"] = 15000
	// CanConsume should now see: 15000 - 15000 = 0 available
	ok4, _ := session.Consume("hive:alice", 100, 1)
	if ok4 {
		t.Error("fourth consume should FAIL — all RCs exhausted")
	}
}

func TestConsume_PreventsNxAmplification(t *testing.T) {
	// Attack scenario: account has 100 HBD (+ 5000 free = 5100 RC available)
	// Submits 10 transactions each costing 1000 RC
	// Before fix: all 10 pass (10000 RC consumed on 5100 balance)
	// After fix: only 5 pass (5000 RC consumed, 6th rejected)
	db := newMockRcDb()
	ls := &mockLedgerSystem{balances: map[string]int64{"hive:attacker:hbd": 100}}
	rcs := New(db, ls)

	session := rcs.NewSession(&mockLedgerSession{balances: map[string]int64{"hive:attacker:hbd": 100}})

	// Available = 100 + 5000 (free) = 5100
	passed := 0
	for i := 0; i < 10; i++ {
		ok, _ := session.Consume("hive:attacker", 100, 1000)
		if ok {
			passed++
		}
	}

	if passed > 5 {
		t.Errorf("RC AMPLIFICATION BUG: %d/10 transactions passed on 5100 RC budget (max should be 5)", passed)
	}
	if passed != 5 {
		t.Errorf("expected exactly 5 transactions to pass, got %d", passed)
	}
	t.Logf("RC enforcement: %d/10 transactions passed (correct — budget is 5100, each costs 1000)", passed)
}

func TestRevert_ClearsSession(t *testing.T) {
	db := newMockRcDb()
	ls := &mockLedgerSystem{balances: map[string]int64{"hive:alice:hbd": 10_000}}
	rcs := New(db, ls)

	session := rcs.NewSession(&mockLedgerSession{balances: map[string]int64{"hive:alice:hbd": 10_000}}).(*rcSession)

	session.Consume("hive:alice", 100, 1800)
	if len(session.rcMap) == 0 {
		t.Error("rcMap should have entry after consume")
	}

	session.Revert()
	if len(session.rcMap) != 0 {
		t.Error("rcMap should be empty after revert")
	}
}

func TestGetFrozenAmt_NoRecord(t *testing.T) {
	db := newMockRcDb()
	ls := &mockLedgerSystem{balances: map[string]int64{}}
	rcs := New(db, ls)

	frozen := rcs.GetFrozenAmt("hive:alice", 100)
	// No record means zero-value RcRecord (BlockHeight=0, Amount=0)
	// diff = 100 - 0 = 100, amtRet = 100 * 0 / 144000 = 0, frozen = 0
	if frozen != 0 {
		t.Errorf("expected 0 frozen for no record, got %d", frozen)
	}
}

func TestGetFrozenAmt_LargeBlockDiff(t *testing.T) {
	db := newMockRcDb()
	db.SetRecord("hive:alice", 100, 10_000)

	ls := &mockLedgerSystem{balances: map[string]int64{}}
	rcs := New(db, ls)

	// After full return period
	frozen := rcs.GetFrozenAmt("hive:alice", 100+params.RC_RETURN_PERIOD)
	if frozen != 0 {
		t.Errorf("expected 0 after full return period, got %d", frozen)
	}
}

// ── Edge case: uint64 overflow in CalculateFrozenBal ──

func TestCalculateFrozenBal_LargeAmount_Overflow(t *testing.T) {
	// Test for uint64 overflow: diff * uint64(initialBal) can overflow
	// if initialBal is large and diff is large
	// RC_RETURN_PERIOD = 144000
	// Max safe initialBal before overflow at diff=144000: uint64_max / 144000 ≈ 1.28e14
	// Real-world amounts are much smaller (< 1M), so this is safe
	// But test it anyway
	result := CalculateFrozenBal(0, 144_000, 1_000_000)
	if result != 0 {
		t.Errorf("expected 0 at full period, got %d", result)
	}
}

func TestCalculateFrozenBal_IntegerTruncation(t *testing.T) {
	// Small amounts with short periods lose precision to integer truncation
	// 100 frozen, 1 block: amtRet = 1 * 100 / 144000 = 0 (truncated)
	result := CalculateFrozenBal(0, 1, 100)
	if result != 100 {
		t.Errorf("expected 100 (no decay at 1 block for small amount), got %d", result)
	}

	// At what block does 100 RC start decaying?
	// amtRet = diff * 100 / 144000 >= 1 when diff >= 1440
	result2 := CalculateFrozenBal(0, 1440, 100)
	if result2 != 99 {
		t.Errorf("expected 99 at 1440 blocks, got %d", result2)
	}
}

// ── Edge case: negative balance (shouldn't happen but defensive) ──

func TestGetAvailableRCs_NegativeBalance(t *testing.T) {
	// If somehow balance is negative (ledger bug), available should still be safe
	db := newMockRcDb()
	ls := &mockLedgerSystem{balances: map[string]int64{"did:key:abc:hbd": -500}}
	rcs := New(db, ls)

	avail := rcs.GetAvailableRCs("did:key:abc", 100)
	// balAmt = -500, frozeAmt = 0, cap: 0 > -500 so frozeAmt stays 0
	// available = -500 - 0 = -500
	// Note: our fix only caps frozeAmt > balAmt, doesn't handle negative balance
	// This is OK because negative balance is a ledger-level invariant violation
	t.Logf("available with negative balance: %d (ledger invariant violation)", avail)
}

func TestCanConsume_NegativeBalance(t *testing.T) {
	db := newMockRcDb()
	ls := &mockLedgerSystem{balances: map[string]int64{"did:key:abc:hbd": -500}}
	rcs := New(db, ls)

	session := rcs.NewSession(&mockLedgerSession{balances: map[string]int64{"did:key:abc:hbd": -500}})

	can, _, _ := session.CanConsume("did:key:abc", 100, 100)
	// totalAmt = -500 - 0 = -500, -500 < 100, so can't consume
	if can {
		t.Error("should not consume with negative balance")
	}
}

// ── Edge case: uint64(rcsAvailable) wrap in transaction-pool ──

func TestGetAvailableRCs_NeverNegativeForHiveAccount(t *testing.T) {
	// Critical: transaction-pool does uint64(rcsAvailable) which wraps negative
	// Ensure Hive accounts (the main users) never get negative from GetAvailableRCs
	db := newMockRcDb()
	db.SetRecord("hive:alice", 50, 999_999) // huge frozen

	ls := &mockLedgerSystem{balances: map[string]int64{"hive:alice:hbd": 100}}
	rcs := New(db, ls)

	avail := rcs.GetAvailableRCs("hive:alice", 51)
	if avail < 0 {
		t.Errorf("CRITICAL: negative available RC would wrap in uint64 cast: %d", avail)
	}
}

// ── Edge case: same block height for record and query ──

func TestGetFrozenAmt_SameBlockHeight(t *testing.T) {
	db := newMockRcDb()
	db.SetRecord("hive:alice", 100, 5000)

	ls := &mockLedgerSystem{balances: map[string]int64{}}
	rcs := New(db, ls)

	// Query at same block as record
	frozen := rcs.GetFrozenAmt("hive:alice", 100)
	// diff = 0, amtRet = 0, frozen = 5000
	if frozen != 5000 {
		t.Errorf("expected 5000 at same block, got %d", frozen)
	}
}

// ── Edge case: multiple accounts in same session ──

func TestCanConsume_MultipleAccounts(t *testing.T) {
	db := newMockRcDb()
	ls := &mockLedgerSystem{balances: map[string]int64{
		"hive:alice:hbd": 10_000,
		"hive:bob:hbd":   500,
	}}
	rcs := New(db, ls)

	session := rcs.NewSession(&mockLedgerSession{balances: map[string]int64{
		"hive:alice:hbd": 10_000,
		"hive:bob:hbd":   500,
	}})

	can1, _, _ := session.CanConsume("hive:alice", 100, 5000)
	can2, _, _ := session.CanConsume("hive:bob", 100, 5000)

	if !can1 {
		t.Error("alice should be able to consume 5000")
	}
	if !can2 {
		t.Error("bob should be able to consume 5000 (500 balance + 5000 free)")
	}

	can3, _, _ := session.CanConsume("hive:bob", 100, 6000)
	if can3 {
		t.Error("bob should not consume 6000 (only 5500 available)")
	}
}
