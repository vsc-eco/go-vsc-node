package ledgerSystem_test

import (
	"fmt"
	"testing"
	"vsc-node/lib/test_utils"
	ledger_db "vsc-node/modules/db/vsc/ledger"
	. "vsc-node/modules/ledger-system"
)

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
		LedgerDb:        &test_utils.MockLedgerDb{LedgerRecords: make(map[string][]ledger_db.LedgerRecord)},
		ActionDb:        &test_utils.MockActionsDb{Actions: make(map[string]ledger_db.ActionRecord)},
		BalanceDb: &test_utils.MockBalanceDb{BalanceRecords: map[string][]ledger_db.BalanceRecord{
			user: {{Account: user, BlockHeight: 0, HBD: startingHBD}},
		}},
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
