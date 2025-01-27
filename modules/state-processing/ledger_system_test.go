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

	BalanceRecords map[string][]ledgerDb.BalanceRecord
}

func (m *MockBalanceDb) GetBalanceRecord(account string, blockHeight int64, asset string) (int64, int64, error) {
	var latestRecord ledgerDb.BalanceRecord
	for _, record := range m.BalanceRecords[account] {
		if record.BlockHeight < blockHeight {
			latestRecord = record
		}
	}

	bal := int64(0)
	if asset == "hive" {
		bal = latestRecord.Hive
	} else if asset == "hbd" {
		bal = latestRecord.HBD
	} else if asset == "hbd_savings" {
		bal = latestRecord.HBD_SAVINGS
	}

	return bal, latestRecord.BlockHeight, nil
}

func (m *MockBalanceDb) UpdateBalanceRecord(account string, blockHeight int64, balances map[string]int64) error {
	previousRecord := m.GetLatestRecord(account, blockHeight)

	//HBD_MODIFY_HEIGHT = some value that is less than claim height aka % of the month interval
	//HBD_CLAIM_HEIGHT = time since last claim (caculated value)
	//HBD_AVG = average balance throughout the month using average calculation
	moreAvg := int64(0)
	HBD_AVG := int64(0)
	if previousRecord != nil {
		moreAvg = previousRecord.HBD_SAVINGS * previousRecord.HBD_MODIFY_HEIGHT / previousRecord.HBD_CLAIM_HEIGHT
		HBD_AVG = previousRecord.HBD_AVG

	}

	m.BalanceRecords[account] = append(m.BalanceRecords[account], ledgerDb.BalanceRecord{
		Account:           account,
		BlockHeight:       blockHeight,
		Hive:              balances["hive"],
		HBD:               balances["hbd"],
		HBD_SAVINGS:       balances["hbd_savings"],
		HBD_AVG:           HBD_AVG + moreAvg,
		HBD_MODIFY_HEIGHT: blockHeight,
		HBD_CLAIM_HEIGHT:  1,
	})
	return nil
}

func (m *MockBalanceDb) GetLatestRecord(account string, beforeHeight int64) *ledgerDb.BalanceRecord {
	var record ledgerDb.BalanceRecord

	found := false
	for _, records := range m.BalanceRecords[account] {
		if record.BlockHeight < records.BlockHeight {
			record = records
			found = true
		}
	}

	if !found {
		return nil
	}

	return &record
}

func (m *MockBalanceDb) GetAll(blockHeight int64) []ledgerDb.BalanceRecord {
	out := make([]ledgerDb.BalanceRecord, 0)
	for _, records := range m.BalanceRecords {
		out = append(out, records...)
	}
	return out
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
	filteredResults := make([]ledgerDb.LedgerRecord, 0)
	for _, record := range das {
		if record.BlockHeight >= blockHeight && record.Asset == asset {
			filteredResults = append(filteredResults, record)
		}
	}

	return &filteredResults, nil
}

func (m *MockLedgerDb) GetLedgerRange(account string, start int64, end int64, asset string) (*[]ledgerDb.LedgerRecord, error) {
	das := m.LedgerRecords[account]
	filteredResults := make([]ledgerDb.LedgerRecord, 0)
	for _, record := range das {
		if record.Asset == asset {
			if record.BlockHeight >= start && record.BlockHeight <= end {
				filteredResults = append(filteredResults, record)
			}
		}
	}

	return &filteredResults, nil

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

func (m *MockWithdrawsDb) ExecuteComplete(id string) {

}

func (m *MockWithdrawsDb) Get(id string) (*ledgerDb.ActionRecord, error) {
	d := m.Withdraws[id]
	if d.Id == "" {
		return nil, nil
	}
	return &d, nil
}

func (m *MockWithdrawsDb) SetStatus(id string, status string) {
	withdraw := m.Withdraws[id]
	withdraw.Status = status
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
		true,
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
		true,
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

// Tests HBD Savings
// Handles stake and unstake operations
// Will handle claiming in the future
func TestHBDSavings(t *testing.T) {

	ledgerDbA := MockLedgerDb{
		LedgerRecords: make(map[string][]ledgerDb.LedgerRecord),
	}
	actionsDb := &MockWithdrawsDb{
		Withdraws: make(map[string]ledgerDb.ActionRecord),
	}
	ledgerSystem := stateEngine.LedgerSystem{
		BalanceDb: &MockBalanceDb{
			BalanceRecords: make(map[string][]ledgerDb.BalanceRecord),
		},
		LedgerDb:  &ledgerDbA,
		ActionsDb: actionsDb,
	}
	ledgerExecutor := stateEngine.LedgerExecutor{
		Ls: &ledgerSystem,
	}
	ledgerExecutor.SetHeight(10)

	//Note this will record into "virtualLedger"
	ledgerExecutor.Deposit(stateEngine.Deposit{
		Id:     "deposit-tx-0",
		Asset:  "hbd",
		Amount: 100,
		From:   "hive:test-account",
		Memo:   "Deposit test",
		BIdx:   1,
		OpIdx:  0,
	})
	ledgerExecutor.Deposit(stateEngine.Deposit{
		Id:     "deposit-tx-0",
		Asset:  "hbd",
		Amount: 100,
		From:   "hive:vaultec",
		Memo:   "Deposit test",
		BIdx:   1,
		OpIdx:  0,
	})

	//Note: this pulls from the virtualLedger and not the actual ledger in the db
	bal := ledgerExecutor.SnapshotForAccount("hive:test-account", 9, "hbd")

	fmt.Println("Balance", bal)
	ledgerResult := ledgerExecutor.Stake(
		stateEngine.StakeOp{
			OpLogEvent: stateEngine.OpLogEvent{
				Id:     "stake-tx-0",
				From:   "hive:test-account",
				To:     "hive:test-account",
				Amount: 10,
				Type:   "stake",
				Asset:  "hbd",
				Memo:   "Stake test",
			},
		},
	)
	ledgerResult = ledgerExecutor.Stake(
		stateEngine.StakeOp{
			OpLogEvent: stateEngine.OpLogEvent{
				Id:     "stake-tx-0",
				From:   "hive:vaultec",
				To:     "hive:vaultec",
				Amount: 10,
				Type:   "stake",
				Asset:  "hbd",
				Memo:   "Stake test",
			},
		},
	)
	fmt.Println("Ledger Result", ledgerResult)
	fromBal := ledgerExecutor.SnapshotForAccount("hive:test-account", 11, "hbd")
	toBal := ledgerExecutor.SnapshotForAccount("hive:test-account", 11, "hbd_savings")
	fmt.Println("From Balance", fromBal, toBal)

	//First thing must happen! It must be finalized!
	//We are going to simulate that here. In place of a block finalization!

	//Pull out oplog
	exported := ledgerExecutor.Export()
	fmt.Println("exported.Oplog", exported.Oplog)

	//Flush oplog
	ledgerExecutor.Flush()

	//Theoretically agree and publish a signed block
	//This is what goes into blocks
	ledgerSystem.ExecuteOplog(exported.Oplog, 0, 20)

	// fmt.Println("ActionsDb", actionsDb.Withdraws)

	//This is where a stake/unstake operation should happen on chain and get back indexed
	ledgerSystem.ExecuteActions([]string{"stake-tx-0-1"}, stateEngine.ExtraInfo{
		//Hypothetically a few minutes
		BlockHeight: 30,
	})

	hbdBal := ledgerSystem.GetBalance("hive:vaultec", 31, "hbd_savings")
	ledgerSystem.SaveSnapshot("hive:vaultec", 32)

	fmt.Println("HBD savings Balance", hbdBal)
	// ledgerJson, _ := json.Marshal(ledgerDbA.LedgerRecords)
	// fmt.Println("Ledger Records", string(ledgerJson))
	// ledgerJson, _ = json.Marshal(ledgerSystem.ActionsDb)
	// fmt.Println("ActionsDb", string(ledgerJson))
	// ledgerJson, _ = json.Marshal(ledgerSystem.BalanceDb)
	// fmt.Println("BalanceDb", string(ledgerJson))

	ledgerSystem.ClaimHBDInterest(1, 40, 1)
	hbdBal = ledgerSystem.GetBalance("hive:vaultec", 45, "hbd_savings")

	fmt.Println("HBD savings Balance after claim", hbdBal)

	ledgerExecutor.SetHeight(40)
	ledgerResult = ledgerExecutor.Unstake(
		stateEngine.StakeOp{
			OpLogEvent: stateEngine.OpLogEvent{
				Id:     "unstake-tx-0",
				From:   "hive:vaultec",
				To:     "hive:vaultec",
				Amount: 5,
				Type:   "unstake",
				Asset:  "hbd",
				Memo:   "Stake test",
			},
		},
	)
	fmt.Println("Ledger Result", ledgerResult)
	hbdBal = ledgerSystem.GetBalance("hive:vaultec", 50, "hbd")
	fmt.Println("HBD Balance before unstake", hbdBal)
	exported = ledgerExecutor.Export()
	ledgerExecutor.Flush()

	ledgerSystem.ExecuteOplog(exported.Oplog, 40, 50)
	//ID = 0x0F8239B80720BA9367B19047c92924e7287b7A35-0-unstake
	ledgerSystem.ExecuteActions([]string{"unstake-tx-0-1"}, stateEngine.ExtraInfo{
		BlockHeight: 60,
	})

	fmt.Println(ledgerSystem.LedgerDb)

	hbdBal = ledgerSystem.GetBalance("hive:vaultec", 86459, "hbd")

	fmt.Println("HBD Balance after unstake", hbdBal)

	// ledgerJson, _ = json.Marshal(ledgerDbA.LedgerRecords)
	// fmt.Println("Ledger Records", string(ledgerJson))

}
