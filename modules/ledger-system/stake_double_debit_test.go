package ledgerSystem

import (
	"fmt"
	"testing"
	ledger_db "vsc-node/modules/db/vsc/ledger"

	"github.com/chebyrash/promise"
)

// ---------------------------------------------------------------------------
// Minimal mock implementations of the DB interfaces required by LedgerState
// ---------------------------------------------------------------------------

// mockLedger satisfies ledger_db.Ledger
type mockLedger struct{}

func (m *mockLedger) Init() error                        { return nil }
func (m *mockLedger) Start() *promise.Promise[any]       { return nil }
func (m *mockLedger) Stop() error                        { return nil }
func (m *mockLedger) StoreLedger(...ledger_db.LedgerRecord) {}
func (m *mockLedger) GetLedgerAfterHeight(account string, blockHeight uint64, asset string, limit *int64) (*[]ledger_db.LedgerRecord, error) {
	empty := make([]ledger_db.LedgerRecord, 0)
	return &empty, nil
}
func (m *mockLedger) GetLedgerRange(account string, start uint64, end uint64, asset string, options ...ledger_db.LedgerOptions) (*[]ledger_db.LedgerRecord, error) {
	empty := make([]ledger_db.LedgerRecord, 0)
	return &empty, nil
}
func (m *mockLedger) GetLedgersTsRange(account *string, txId *string, txTypes []string, asset *ledger_db.Asset, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]ledger_db.LedgerRecord, error) {
	return nil, nil
}
func (m *mockLedger) GetRawLedgerRange(account *string, txId *string, txTypes []string, asset *ledger_db.Asset, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]ledger_db.LedgerRecord, error) {
	return nil, nil
}
func (m *mockLedger) GetDistinctAccountsRange(startBlock, endBlock uint64) ([]string, error) {
	return nil, nil
}

// mockBalances satisfies ledger_db.Balances -- seeds an account with a given HBD balance
type mockBalances struct {
	hbd int64
}

func (m *mockBalances) Init() error                  { return nil }
func (m *mockBalances) Start() *promise.Promise[any] { return nil }
func (m *mockBalances) Stop() error                  { return nil }
func (m *mockBalances) GetBalanceRecord(account string, blockHeight uint64) (*ledger_db.BalanceRecord, error) {
	return &ledger_db.BalanceRecord{
		Account:     account,
		BlockHeight: 0,
		HBD:         m.hbd,
	}, nil
}
func (m *mockBalances) UpdateBalanceRecord(record ledger_db.BalanceRecord) error {
	return nil
}
func (m *mockBalances) GetAll(blockHeight uint64) []ledger_db.BalanceRecord {
	return nil
}

// mockActions satisfies ledger_db.BridgeActions
type mockActions struct{}

func (m *mockActions) Init() error                  { return nil }
func (m *mockActions) Start() *promise.Promise[any] { return nil }
func (m *mockActions) Stop() error                  { return nil }
func (m *mockActions) StoreAction(withdraw ledger_db.ActionRecord) {}
func (m *mockActions) ExecuteComplete(actionId *string, ids ...string) {}
func (m *mockActions) Get(id string) (*ledger_db.ActionRecord, error) { return nil, nil }
func (m *mockActions) SetStatus(id string, status string) {}
func (m *mockActions) GetPendingActions(bh uint64, t ...string) ([]ledger_db.ActionRecord, error) {
	return nil, nil
}
func (m *mockActions) GetPendingActionsByEpoch(epoch uint64, t ...string) ([]ledger_db.ActionRecord, error) {
	return nil, nil
}
func (m *mockActions) GetActionsRange(txId *string, actionId *string, account *string, byTypes []string, asset *ledger_db.Asset, status *string, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]ledger_db.ActionRecord, error) {
	return nil, nil
}
func (m *mockActions) GetAccountPendingConsensusUnstake(account string) (int64, error) {
	return 0, nil
}
func (m *mockActions) GetActionsByTxId(txId string) ([]ledger_db.ActionRecord, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// Test: Prove Stake() double-debits the user's HBD balance
// ---------------------------------------------------------------------------

func TestStakeDoubleDebit(t *testing.T) {
	const startingHBD = int64(1000) // user starts with 1000 HBD
	const stakeAmount = int64(400)  // stake 400 HBD
	const blockHeight = uint64(100)
	const user = "hive:alice"

	// Build a LedgerState with mock DBs seeded at 1000 HBD
	state := &LedgerState{
		Oplog:           make([]OpLogEvent, 0),
		VirtualLedger:   make(map[string][]LedgerUpdate),
		GatewayBalances: make(map[string]uint64),
		LedgerDb:        &mockLedger{},
		ActionDb:        &mockActions{},
		BalanceDb:       &mockBalances{hbd: startingHBD},
	}

	session := NewSession(state)

	// Confirm starting balance
	preBal := session.GetBalance(user, blockHeight, "hbd")
	fmt.Printf("[PRE-STAKE]  Balance: %d HBD (expected %d)\n", preBal, startingHBD)
	if preBal != startingHBD {
		t.Fatalf("unexpected starting balance: got %d, want %d", preBal, startingHBD)
	}

	// Perform the stake
	result := session.Stake(StakeOp{
		OpLogEvent: OpLogEvent{
			Id:          "stake-1",
			From:        user,
			To:          user,
			Asset:       "hbd",
			Amount:      stakeAmount,
			BlockHeight: blockHeight,
		},
	})
	fmt.Printf("[STAKE]      Result: ok=%v msg=%q\n", result.Ok, result.Msg)
	if !result.Ok {
		t.Fatalf("Stake() failed unexpectedly: %s", result.Msg)
	}

	// Check balance after stake
	postBal := session.GetBalance(user, blockHeight, "hbd")
	expectedCorrect := startingHBD - stakeAmount   // 600 -- what it should be
	expectedBuggy := startingHBD - 2*stakeAmount   // 200 -- double-debit

	fmt.Printf("[POST-STAKE] Balance: %d HBD\n", postBal)
	fmt.Printf("             Expected (correct):  %d\n", expectedCorrect)
	fmt.Printf("             Expected (bug):      %d\n", expectedBuggy)

	if postBal == expectedBuggy {
		fmt.Println("\n*** BUG CONFIRMED: Stake() double-debited the balance! ***")
		fmt.Printf("    Lost %d HBD instead of %d HBD\n", startingHBD-postBal, stakeAmount)
	} else if postBal == expectedCorrect {
		fmt.Println("\n--- Balance is correct; no double-debit detected ---")
	} else {
		fmt.Printf("\n??? Unexpected balance: %d (neither %d nor %d)\n", postBal, expectedCorrect, expectedBuggy)
	}

	// Now try a transfer of 500 HBD -- should succeed with correct balance (600)
	// but will fail with double-debited balance (200)
	transferResult := session.ExecuteTransfer(OpLogEvent{
		Id:          "transfer-1",
		From:        user,
		To:          "hive:bob",
		Asset:       "hbd",
		Amount:      500,
		BlockHeight: blockHeight,
	})
	fmt.Printf("\n[TRANSFER 500 HBD] Result: ok=%v msg=%q\n", transferResult.Ok, transferResult.Msg)

	if !transferResult.Ok {
		fmt.Println("*** Transfer of 500 HBD FAILED -- user was robbed of funds by Stake() double-debit ***")
	} else {
		fmt.Println("--- Transfer of 500 HBD succeeded (balance was correct) ---")
	}

	// Final assertion: the balance SHOULD be 600 after staking 400 from 1000
	if postBal != expectedCorrect {
		t.Errorf("DOUBLE-DEBIT BUG: after staking %d from %d, balance is %d (want %d)",
			stakeAmount, startingHBD, postBal, expectedCorrect)
	}
}
