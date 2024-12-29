package stateEngine_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"testing"
	"vsc-node/modules/aggregate"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/assert"
)

type MockBalanceDb struct {
	aggregate.Plugin
}

func (m *MockBalanceDb) GetBalanceRecord(account string, blockHeight int64, asset string) (int64, error) {
	if account == "test-account" && asset == "HIVE" {
		return 0, nil
	}
	return 0, nil
}

type MockLedgerDb struct {
	aggregate.Plugin

	LedgerRecords map[string][]ledgerDb.LedgerRecord
}

func (m *MockLedgerDb) InsertLedgerRecord(ledgerRecord ledgerDb.LedgerRecord) error {
	m.LedgerRecords[ledgerRecord.Owner] = append(m.LedgerRecords[ledgerRecord.Owner], ledgerRecord)
	return nil
}

func (m *MockLedgerDb) GetLedgerRecords1(account string, anchorHeight int64, blockHeight int64, asset string) ([]ledgerDb.LedgerRecord, error) {
	return m.LedgerRecords[account], nil
}
func (m *MockLedgerDb) GetLedgerAfterHeight(account string, blockHeight int64, asset string, limit *int64) (*[]ledgerDb.LedgerRecord, error) {
	das := m.LedgerRecords[account]
	return &das, nil
}

func (m *MockLedgerDb) StoreLedger(ledgerRecord ledgerDb.LedgerRecord) {
	m.LedgerRecords[ledgerRecord.Owner] = append(m.LedgerRecords[ledgerRecord.Owner], ledgerRecord)
}

type MockWithdrawsDb struct {
	aggregate.Plugin

	Withdraws map[string]ledgerDb.ActionRecord
}

func (m *MockWithdrawsDb) StoreWithdrawal(withdraw ledgerDb.ActionRecord) {
	withdraw.Status = "pending"
	m.Withdraws[withdraw.Id] = withdraw
}

func (m *MockWithdrawsDb) MarkComplete(id string) {
	withdraw := m.Withdraws[id]
	withdraw.Status = "completed"
	m.Withdraws[id] = withdraw
}

func TestInsertCheckBalance(t *testing.T) {

	ledgerDb := MockLedgerDb{
		LedgerRecords: make(map[string][]ledgerDb.LedgerRecord),
	}

	ledgerExecutor := stateEngine.LedgerExecutor{
		Ls: &stateEngine.LedgerSystem{
			BalanceDb: &MockBalanceDb{},
			LedgerDb:  &ledgerDb,
		},
	}
	ledgerExecutor.SetHeight(1000)

	ledgerExecutor.Deposit(stateEngine.Deposit{
		Id:     "tx0-1",
		Asset:  "HIVE",
		Amount: 100,
		From:   "hive:test-account",
		Memo:   "test",
		BIdx:   1,
		OpIdx:  0,
	})
	fmt.Println(ledgerExecutor)
	check1 := reflect.DeepEqual(ledgerExecutor.VirtualLedger["hive:test-account"][0], stateEngine.LedgerUpdate{
		Id:     "tx0-1",
		Asset:  "HIVE",
		Amount: 100,
		Owner:  "hive:test-account",
		Memo:   "",
		Type:   "deposit",

		BIdx:  1,
		OpIdx: 0,
	})

	assert.True(t, check1)

	bal := ledgerExecutor.SnapshotForAccount("hive:test-account", 0, "HIVE")

	fmt.Println(bal)
	assert.Equal(t, int64(100), bal)

	ledgerExecutor.Deposit(stateEngine.Deposit{
		Id:     "2",
		Asset:  "HIVE",
		Amount: 100,
		From:   "hive:test-account",
		Memo:   "to=0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045",
	})

	fmt.Println(ledgerExecutor.VirtualLedger)

	check2 := reflect.DeepEqual(ledgerExecutor.VirtualLedger["did:pkh:eip155:1:0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045"][0], stateEngine.LedgerUpdate{
		Id:     "2",
		Asset:  "HIVE",
		Amount: 100,
		Owner:  "did:pkh:eip155:1:0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045",
		Memo:   "",
		Type:   "deposit",
	})

	bbytes, _ := json.Marshal(ledgerExecutor.VirtualLedger["did:pkh:eip155:1:0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045"][0])
	fmt.Println("bbytes string()", string(bbytes))

	assert.True(t, check2)

	ledgerExecutor.Deposit(stateEngine.Deposit{
		Id:     "3",
		Asset:  "HIVE",
		Amount: 100,
		From:   "hive:test-account",
		Memo:   "to=vaultec",

		BIdx:  1,
		OpIdx: 0,
	})

	check3 := reflect.DeepEqual(ledgerExecutor.VirtualLedger["hive:vaultec"][0], stateEngine.LedgerUpdate{
		Id:     "3",
		Asset:  "HIVE",
		Amount: 100,
		Owner:  "hive:vaultec",
		Memo:   "",
		Type:   "deposit",

		BIdx:  1,
		OpIdx: 0,
	})
	assert.True(t, check3)

	result := ledgerExecutor.ExecuteTransfer(stateEngine.OpLogEvent{
		From:   "hive:test-account",
		To:     "hive:vaultec",
		Amount: 10,
		Asset:  "HIVE",

		BIdx:  2,
		OpIdx: 0,
	})

	bbytesResult, _ := json.Marshal(result)
	fmt.Println("bbytesResult", string(bbytesResult))

	assert.Equal(t, int64(110), ledgerExecutor.SnapshotForAccount("hive:vaultec", 0, "HIVE"))

	ledgerExecutor.Stake(stateEngine.StakeOp{
		stateEngine.OpLogEvent{
			Id:     "4",
			From:   "hive:test-account",
			To:     "hive:vaultec",
			Amount: 10,
			Type:   "stake",
			Asset:  "hbd",
			Memo:   "",
		},
		false,
	})

	ledgerExecutor.Stake(stateEngine.StakeOp{
		stateEngine.OpLogEvent{
			Id:     "4",
			From:   "hive:test-account",
			To:     "hive:vaultec",
			Amount: 10,
			Type:   "stake",
			Asset:  "hbd",
			Memo:   "",
		},
		true,
	})

	ledgerExecutor.Unstake(stateEngine.StakeOp{
		stateEngine.OpLogEvent{
			Id:     "5",
			From:   "hive:test-account",
			To:     "hive:vaultec",
			Amount: 10,
			Type:   "unstake",
			Asset:  "hbd",
			Memo:   "",
		},
		true,
	})
	ledgerExecutor.Unstake(stateEngine.StakeOp{
		stateEngine.OpLogEvent{
			Id:     "5",
			From:   "hive:test-account",
			To:     "hive:vaultec",
			Amount: 10,
			Type:   "unstake",
			Asset:  "hbd",
			Memo:   "",
		},
		false,
	})
}

func TestGatewayWithdrawal(t *testing.T) {
	ledgerDb := MockLedgerDb{
		LedgerRecords: make(map[string][]ledgerDb.LedgerRecord),
	}

	ledgerExecutor := stateEngine.LedgerExecutor{
		Ls: &stateEngine.LedgerSystem{
			BalanceDb: &MockBalanceDb{},
			LedgerDb:  &ledgerDb,
			ActionsDb: &MockWithdrawsDb{},
		},
	}
	ledgerExecutor.SetHeight(1000)
	ledgerExecutor.Deposit(stateEngine.Deposit{
		Id:     "tx0-1",
		Asset:  "hbd",
		Amount: 100,
		From:   "hive:test-account",
		Memo:   "test",
		BIdx:   1,
		OpIdx:  0,
	})

	result := ledgerExecutor.Withdraw(stateEngine.WithdrawParams{
		Id:     "1",
		Asset:  "hbd",
		Amount: 10,
		From:   "hive:test-account",
		To:     "hive:test-account",
		Memo:   "test",
	})

	fmt.Println("Result", result)
	fmt.Println("Ledger", ledgerExecutor.VirtualLedger)
	fmt.Println("Withdraws", ledgerExecutor.Oplog)

	result1 := ledgerExecutor.Withdraw(stateEngine.WithdrawParams{
		Id:     "1",
		Asset:  "hbd",
		Amount: 10,
		From:   "hive:test-account",
		To:     "hive:test-account",
		Memo:   "test",
	})

	fmt.Println("Result", result1)
	result2 := ledgerExecutor.Withdraw(stateEngine.WithdrawParams{
		Id:     "1",
		Asset:  "hbd",
		Amount: 150,
		From:   "hive:test-account",
		To:     "hive:test-account",
		Memo:   "test",
	})

	fmt.Println("Result", result2)

}

func TestFractionReserve(t *testing.T) {
	ledgerDb := MockLedgerDb{
		LedgerRecords: make(map[string][]ledgerDb.LedgerRecord),
	}

	ledgerExecutor := stateEngine.LedgerExecutor{
		Ls: &stateEngine.LedgerSystem{
			BalanceDb: &MockBalanceDb{},
			LedgerDb:  &ledgerDb,
			ActionsDb: &MockWithdrawsDb{},
		},
	}

	hbdBalanceMap := map[string]int64{
		"hive:test-account":     1000,
		"hive:vaultec":          1000,
		"hive:geo52rey":         500,
		"hive:geo52rey.dev":     100,
		"hive:theycallmedan":    65_000,
		"hive:starkerz":         10_000,
		"hive:taskmaster4450":   100_000,
		"hive:lordbutterfly":    50_000,
		"hive:aggroed":          10_000,
		"hive:themarkymark":     25_000,
		"hive:justinsunsteemit": 10_000,
		"hive:gtg":              10_000,
		"hive:pharesim":         100_000,
		"hive:ausbitbank":       100_000,
		"hive:smooth.witness":   100_000,
		"hive:roelandp":         100_000,
		"hive:smooth":           500_000,
	}

	hbdStakedBalance := map[string]int64{
		"hive:investor-420":  125_000,
		"hive:theycallmedan": 50_000,
	}

	ledgerExecutor.SetHeight(1000)
	x := 0
	idx := int64(0)
	for account, balance := range hbdBalanceMap {
		ledgerExecutor.Deposit(stateEngine.Deposit{
			Id:     "hbd-" + strconv.Itoa(x),
			Asset:  "hbd",
			Amount: balance,
			From:   account,
			Memo:   "Test Deposit",
			BIdx:   idx,
			OpIdx:  0,
		})

		x++
		idx++
	}
	x = 0
	for account, balance := range hbdStakedBalance {
		ledgerExecutor.Deposit(stateEngine.Deposit{
			Id:     "hbdstaked-" + strconv.Itoa(x),
			Asset:  "hbd_savings",
			Amount: balance,
			From:   account,
			Memo:   "Test Deposit",
			BIdx:   idx,
			OpIdx:  0,
		})
		idx++
		x++
	}

	accountList := []string{
		"hive:test-account",
		"hive:vaultec",
		"hive:geo52rey",
		"hive:geo52rey.dev",
		"hive:theycallmedan",
		"hive:starkerz",
		"hive:taskmaster4450",
		"hive:lordbutterfly",
		"hive:aggroed",
		"hive:themarkymark",
		"hive:justinsunsteemit",
		"hive:gtg",
		"hive:pharesim",
		"hive:ausbitbank",
		"hive:smooth.witness",
		"hive:roelandp",
		"hive:smooth",
		"hive:investor-420",
	}

	result := ledgerExecutor.CalculationFractStats(accountList, 1100)

	bbytes, _ := json.Marshal(result)

	fmt.Println("Fractional reserve calculation", string(bbytes))
}

func TestGatewaySync(t *testing.T) {

}
