package state_engine_test

import (
	"testing"

	"vsc-node/lib/test_utils"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	rcDb "vsc-node/modules/db/vsc/rcs"
	"vsc-node/modules/db/vsc/transactions"
	tss_db "vsc-node/modules/db/vsc/tss"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	"vsc-node/modules/db/vsc/witnesses"
	stateEngine "vsc-node/modules/state-processing"
)

// countingTssRequests records every signing request the state engine files.
type countingTssRequests struct {
	*test_utils.MockTssRequestsDb
	filed []tss_db.TssRequest
}

func (c *countingTssRequests) SetSignedRequest(req tss_db.TssRequest) error {
	c.filed = append(c.filed, req)
	return c.MockTssRequestsDb.SetSignedRequest(req)
}

// Applying a block's contract output with one failed and one successful
// result, each carrying a TSS sign operation. Below 0.16.0 both are filed as
// signing requests; from 0.16.0 only the successful result's is.
func TestFailedResultStartsNoTssSigningFrom0_16(t *testing.T) {
	for _, tc := range []struct {
		name      string
		consensus uint64
		wantMsgs  []string
	}{
		{"0.14: every result's sign op is filed, the failed one too", 14, []string{"failed-msg", "ok-msg"}},
		{"0.15: every result's sign op is still filed (the rule is on 0.16)", 15, []string{"failed-msg", "ok-msg"}},
		{"0.16: only the successful result's sign op is filed", 16, []string{"ok-msg"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var elec elections.ElectionResult
			elec.ProtocolVersion = tc.consensus // a promoted field
			electionDb := &test_utils.MockElectionDb{
				Elections:         make(map[uint64]*elections.ElectionResult),
				ElectionsByHeight: map[uint64]elections.ElectionResult{1: elec},
			}
			requests := &countingTssRequests{MockTssRequestsDb: &test_utils.MockTssRequestsDb{Requests: make(map[string]tss_db.TssRequest)}}
			se := stateEngine.New(
				systemconfig.MocknetConfig(), nil,
				&test_utils.MockWitnessDb{ByAccount: make(map[string]*witnesses.Witness), ByPeerId: make(map[string][]witnesses.Witness)},
				electionDb,
				&test_utils.MockContractDb{Contracts: make(map[string]contracts.Contract)},
				&test_utils.MockContractStateDb{Outputs: make(map[string]contracts.ContractOutput)},
				&test_utils.MockTxDb{Records: make(map[string]transactions.TransactionRecord)},
				&test_utils.MockLedgerDb{LedgerRecords: make(map[string][]ledgerDb.LedgerRecord)},
				&test_utils.MockBalanceDb{BalanceRecords: make(map[string][]ledgerDb.BalanceRecord)},
				nil,
				&test_utils.MockInterestClaimsDb{},
				&test_utils.MockVscBlocksDb{Blocks: make([]vscBlocks.VscHeaderRecord, 0)},
				&test_utils.MockActionsDb{Actions: make(map[string]ledgerDb.ActionRecord)},
				&test_utils.MockRcDb{Records: make(map[string][]rcDb.RcRecord)},
				&test_utils.MockNonceDb{Nonces: make(map[string]uint64)},
				&test_utils.MockTssKeysDb{Keys: make(map[string]tss_db.TssKey)},
				&test_utils.MockTssCommitmentsDb{Commitments: make(map[string]tss_db.TssCommitment)},
				requests,
				nil, nil, nil, nil, nil, nil,
			)
			code := contracts.LEDGER_ERROR
			out := &stateEngine.ContractOutput{
				Id:         "bafyreioutput",
				ContractId: "vsc1BtcMapping",
				Inputs:     []string{"txfailed-0", "txok-0"},
				Results: []contracts.ContractOutputResult{
					{Ok: false, Err: &code, TssOps: []tss_db.TssOp{{Type: "sign", KeyId: "vsc1BtcMapping-main", Args: "failed-msg"}}},
					{Ok: true, TssOps: []tss_db.TssOp{{Type: "sign", KeyId: "vsc1BtcMapping-main", Args: "ok-msg"}}},
				},
			}
			out.Ingest(se, stateEngine.TxSelf{TxId: "block", BlockHeight: 100}, 100)
			got := make([]string, 0, len(requests.filed))
			for _, r := range requests.filed {
				got = append(got, r.Msg)
			}
			if len(got) != len(tc.wantMsgs) {
				t.Fatalf("signing requests filed for %v, want %v", got, tc.wantMsgs)
			}
			for i := range got {
				if got[i] != tc.wantMsgs[i] {
					t.Fatalf("signing requests filed for %v, want %v", got, tc.wantMsgs)
				}
			}
		})
	}
}
