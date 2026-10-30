package ledgerSystem_test

import (
	"fmt"
	"sync"
	"testing"

	"vsc-node/lib/test_utils"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	ledgerSystem "vsc-node/modules/ledger-system"
)

// TestVirtualLedgerConcurrentReadWrite drives the three block-processing writes
// of LedgerState.VirtualLedger (session Done, DropVirtual, Flush) on one
// goroutine while another reads balances through a fresh session, the way the
// getConsensusDelegation and simulateContractCalls resolvers do. Run with -race:
// an unguarded map shows up as a data race (and, outside the race detector, as a
// fatal "concurrent map read and map write" that kills the node).
func TestVirtualLedgerConcurrentReadWrite(t *testing.T) {
	state := &ledgerSystem.LedgerState{
		Oplog:           make([]ledgerSystem.OpLogEvent, 0),
		VirtualLedger:   make(map[string][]ledgerSystem.LedgerUpdate),
		GatewayBalances: make(map[string]uint64),
		BlockHeight:     100,
		LedgerDb: &test_utils.MockLedgerDb{
			LedgerRecords: make(map[string][]ledgerDb.LedgerRecord),
		},
		ActionDb: &test_utils.MockActionsDb{
			Actions: make(map[string]ledgerDb.ActionRecord),
		},
		BalanceDb: &test_utils.MockBalanceDb{
			BalanceRecords: map[string][]ledgerDb.BalanceRecord{
				"hive:alice": {{Account: "hive:alice", BlockHeight: 50, Hive: 1_000_000_000}},
			},
		},
	}

	const rounds = 2000
	var wg sync.WaitGroup
	wg.Add(2)

	// Writer: what block processing does each slot.
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			session := ledgerSystem.NewSession(state)
			res := session.ExecuteTransfer(ledgerSystem.OpLogEvent{
				Id:          fmt.Sprintf("tx-%d", i),
				From:        "hive:alice",
				To:          "hive:bob",
				Amount:      1,
				Asset:       "hive",
				BlockHeight: 100,
			})
			if !res.Ok {
				t.Errorf("transfer %d failed: %s", i, res.Msg)
				return
			}
			session.Done()
			state.DropVirtual("hive:bob", func(v ledgerSystem.LedgerUpdate) bool {
				return v.Type == "deposit"
			})
			if i%50 == 0 {
				state.Flush()
			}
		}
	}()

	// Reader: a read-only query on its own goroutine.
	go func() {
		defer wg.Done()
		for i := 0; i < rounds*5; i++ {
			bal := ledgerSystem.NewSession(state).GetBalance("hive:bob", 100, "hive")
			if bal < 0 {
				t.Errorf("negative balance read: %d", bal)
				return
			}
		}
	}()

	wg.Wait()
}
