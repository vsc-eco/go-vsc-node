package wasm_e2e

import (
	"fmt"
	"testing"

	"vsc-node/lib/test_utils"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	rcDb "vsc-node/modules/db/vsc/rcs"
	"vsc-node/modules/db/vsc/transactions"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	"vsc-node/modules/db/vsc/witnesses"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/vsc-eco/hivego"
)

// A transaction whose first operation is a contract call that succeeds and asks
// for a TSS operation, and whose second operation (a transfer of more HBD than
// the account holds) fails, run through the node's block path: ProcessBlock
// queues it, the next slot's ExecuteBatch runs it. The transaction fails and its
// ledger is reverted at every version. Below 0.16.0 the first call's successful
// result, with its TSS operation, is still published in the contract's output;
// from 0.16.0 a failed transaction publishes none of its calls' successful
// results.
func TestFailedTx_BlockPathPublishesNoSuccessfulResultFrom0_16(t *testing.T) {
	const contract = "vscfailtx"
	for _, tc := range []struct {
		name          string
		consensus     uint64
		wantPublished bool
	}{
		{"0.15: the reverted call's result and TSS op are published", 15, true},
		{"0.16: the reverted call's result is not published", 16, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ct := newPriceChain(t)
			ct.RegisterContract(contract, "hive:owner", compiledContract(t))
			sconf := systemconfig.MocknetConfig()
			electionDb := &test_utils.MockElectionDb{
				Elections:         make(map[uint64]*elections.ElectionResult),
				ElectionsByHeight: map[uint64]elections.ElectionResult{1: electionAt(tc.consensus)},
			}
			se := stateEngine.New(
				sconf, ct.DataLayer,
				&test_utils.MockWitnessDb{ByAccount: make(map[string]*witnesses.Witness), ByPeerId: make(map[string][]witnesses.Witness)},
				electionDb, ct.ContractDb, ct.ContractStateDb,
				&test_utils.MockTxDb{Records: make(map[string]transactions.TransactionRecord)},
				&test_utils.MockLedgerDb{LedgerRecords: make(map[string][]ledgerDb.LedgerRecord)},
				&test_utils.MockBalanceDb{BalanceRecords: make(map[string][]ledgerDb.BalanceRecord)},
				nil,
				&test_utils.MockInterestClaimsDb{},
				&test_utils.MockVscBlocksDb{Blocks: make([]vscBlocks.VscHeaderRecord, 0)},
				&test_utils.MockActionsDb{Actions: make(map[string]ledgerDb.ActionRecord)},
				&test_utils.MockRcDb{Records: make(map[string][]rcDb.RcRecord)},
				&test_utils.MockNonceDb{Nonces: make(map[string]uint64)},
				ct.Tss.Keys, ct.Tss.Commitments, ct.Tss.Requests,
				nil, test_utils.NewMockPoaSeatsDb(), nil, nil, nil, nil,
			)

			const hiveTx = "failedtxbatch0000000000000000000000000000"
			op := func(id, js string) hivego.Operation {
				return hivego.Operation{Type: "custom_json", Value: map[string]interface{}{
					"id":                     id,
					"json":                   js,
					"required_auths":         []interface{}{"alice"},
					"required_posting_auths": []interface{}{},
				}}
			}
			net := sconf.NetId()
			se.ProcessBlock(hive_blocks.HiveBlock{
				BlockNumber: 101,
				BlockID:     "0000006500000000000000000000000000000000",
				Timestamp:   "2026-11-18T00:00:00",
				Transactions: []hive_blocks.Tx{{
					Index:         0,
					TransactionID: hiveTx,
					Operations: []hivego.Operation{
						op("vsc.call", fmt.Sprintf(`{"net_id":%q,"contract_id":%q,"action":"createKey","payload":"x","rc_limit":5000,"intents":[]}`, net, contract)),
						op("vsc.transfer", fmt.Sprintf(`{"net_id":%q,"from":"hive:alice","to":"hive:bob","amount":"999999.000","asset":"hbd"}`, net)),
					},
				}},
			})
			queued := 0
			for _, p := range se.TxBatch {
				if p.TxId == hiveTx {
					queued = len(p.Ops)
				}
			}
			if queued != 2 {
				t.Fatalf("the transaction was queued with %d operations, want 2", queued)
			}
			// The next slot's first block runs the batch.
			se.ProcessBlock(hive_blocks.HiveBlock{
				BlockNumber: 110,
				BlockID:     "0000006e00000000000000000000000000000000",
				Timestamp:   "2026-11-18T00:00:27",
			})

			if out, ran := se.TxOutput[hiveTx]; !ran || out.Ok {
				t.Fatalf("the transaction ran %v with ok=%v, want it run and failed", ran, out.Ok)
			}
			var okResults, tssOps int
			for _, r := range se.ContractResults[contract] {
				if r.Success {
					okResults++
				}
				tssOps += len(r.TssOps)
			}
			t.Logf("published for %s: %d results, %d successful, %d TSS operations", contract, len(se.ContractResults[contract]), okResults, tssOps)
			if tc.wantPublished {
				// Below 0.16 the call must have run and succeeded, or the 0.16
				// case proves nothing.
				if okResults != 1 || tssOps == 0 {
					t.Fatalf("want the call's successful result with its TSS operation published: %d successful, %d TSS operations", okResults, tssOps)
				}
				return
			}
			if okResults != 0 || tssOps != 0 {
				t.Fatalf("a failed transaction published %d successful results and %d TSS operations, want none", okResults, tssOps)
			}
		})
	}
}
