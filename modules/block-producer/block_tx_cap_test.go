package blockproducer

import (
	"fmt"
	"strings"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/common"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	rcDb "vsc-node/modules/db/vsc/rcs"
	"vsc-node/modules/db/vsc/transactions"
	tss_db "vsc-node/modules/db/vsc/tss"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	"vsc-node/modules/db/vsc/witnesses"
	ledgerSystem "vsc-node/modules/ledger-system"
	rcSystem "vsc-node/modules/rc-system"
	stateEngine "vsc-node/modules/state-processing"
)

// capSlot is the slot every case in this file produces or signs.
const capSlot = 100

// capProducer is a block producer over in-memory stores on a chain whose
// election says consensus 0.<consensus>, with `pool` pending transfers in its
// pool, each from its own Hive account. The election seats one witness,
// "producer1", so it owns every slot.
func capProducer(t *testing.T, consensus uint64, pool int) *BlockProducer {
	t.Helper()
	var elec elections.ElectionResult
	elec.ProtocolVersion = consensus // a promoted field
	elec.Members = []elections.ElectionMember{{Account: "producer1", Key: "did:key:producer1"}}
	electionDb := &test_utils.MockElectionDb{
		Elections:         make(map[uint64]*elections.ElectionResult),
		ElectionsByHeight: map[uint64]elections.ElectionResult{1: elec},
	}
	txDb := &test_utils.MockTxDb{Records: make(map[string]transactions.TransactionRecord, pool)}
	for i := 0; i < pool; i++ {
		id := fmt.Sprintf("tx%05d", i)
		txDb.Records[id] = transactions.TransactionRecord{
			Id:            id,
			Status:        transactions.TransactionStatusUnconfirmed,
			RequiredAuths: []string{fmt.Sprintf("hive:user%05d", i)},
			Nonce:         0,
			RcLimit:       1000,
			Ops:           []transactions.TransactionOperation{{Type: "transfer"}},
		}
	}
	sconf := systemconfig.MocknetConfig()
	balanceDb := &test_utils.MockBalanceDb{BalanceRecords: make(map[string][]ledgerDb.BalanceRecord)}
	ledgerRecs := &test_utils.MockLedgerDb{LedgerRecords: make(map[string][]ledgerDb.LedgerRecord)}
	claims := &test_utils.MockInterestClaimsDb{}
	actions := &test_utils.MockActionsDb{Actions: make(map[string]ledgerDb.ActionRecord)}
	rcs := &test_utils.MockRcDb{Records: make(map[string][]rcDb.RcRecord)}
	nonceDb := &test_utils.MockNonceDb{Nonces: make(map[string]uint64)}
	se := stateEngine.New(
		sconf, nil,
		&test_utils.MockWitnessDb{ByAccount: make(map[string]*witnesses.Witness), ByPeerId: make(map[string][]witnesses.Witness)},
		electionDb,
		&test_utils.MockContractDb{Contracts: make(map[string]contracts.Contract)},
		&test_utils.MockContractStateDb{Outputs: make(map[string]contracts.ContractOutput)},
		txDb, ledgerRecs, balanceDb,
		&test_utils.MockHiveBlockDb{},
		claims,
		&test_utils.MockVscBlocksDb{Blocks: make([]vscBlocks.VscHeaderRecord, 0)},
		actions, rcs, nonceDb,
		&test_utils.MockTssKeysDb{Keys: make(map[string]tss_db.TssKey)},
		&test_utils.MockTssCommitmentsDb{Commitments: make(map[string]tss_db.TssCommitment)},
		&test_utils.MockTssRequestsDb{Requests: make(map[string]tss_db.TssRequest)},
		nil, nil, nil, nil, nil, nil,
	)
	identity := common.NewIdentityConfig(t.TempDir())
	if err := identity.Init(); err != nil { // writes the defaults, with a random BLS seed, to the temp dir
		t.Fatalf("identity config: %v", err)
	}
	bp := &BlockProducer{
		config:      identity,
		sconf:       sconf,
		StateEngine: se,
		TxDb:        txDb,
		rcSystem:    rcSystem.New(rcs, ledgerSystem.New(balanceDb, ledgerRecs, claims, actions, sconf), 10_000),
		nonceDb:     nonceDb,
	}
	bp.bh.Store(capSlot)
	return bp
}

// A producer takes at most params.MAX_BLOCK_L2_TXS pool transactions into one
// block from consensus 0.16.0, and every one it can before.
func TestBlockTxCap_ProducerTakesAtMost4096From0_16(t *testing.T) {
	if params.MAX_BLOCK_L2_TXS != 4096 {
		t.Fatalf("MAX_BLOCK_L2_TXS is %d; this test is written for 4096", params.MAX_BLOCK_L2_TXS)
	}
	for _, tc := range []struct {
		name      string
		consensus uint64
		pool      int
		want      int
	}{
		{"0.15: all 5,000 are taken", 15, 5000, 5000},
		{"0.16: 4,096 of 5,000 are taken", 16, 5000, 4096},
		{"0.16: a pool of exactly 4,096 is taken whole", 16, 4096, 4096},
		{"0.16: a pool under the cap is taken whole", 16, 300, 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bp := capProducer(t, tc.consensus, tc.pool)
			got := bp.generateTransactions(capSlot)
			if len(got) != tc.want {
				t.Fatalf("the block carries %d transactions, want %d", len(got), tc.want)
			}
			seen := make(map[string]bool, len(got))
			for _, tx := range got {
				if seen[tx.Id] {
					t.Fatalf("transaction %s is in the block twice", tx.Id)
				}
				seen[tx.Id] = true
			}
		})
	}
}

// A signer refuses a block that carries more than params.MAX_BLOCK_L2_TXS
// transactions from consensus 0.16.0, before it looks them up. Below 0.16.0 the
// count is not checked, so the same block gets as far as the lookup (and here
// fails it, since the ids are not in the signer's store).
func TestBlockTxCap_SignerRefusesABlockOver4096From0_16(t *testing.T) {
	msg := func(n int) p2pMessage {
		txs := make([]interface{}, n)
		for i := range txs {
			txs[i] = fmt.Sprintf("unknown%05d", i)
		}
		return p2pMessage{Type: "block", SlotHeight: capSlot, Data: map[string]interface{}{
			"producer":     "producer1",
			"transactions": txs,
		}}
	}
	const capErr = "over the 4096 limit"
	for _, tc := range []struct {
		name      string
		consensus uint64
		txs       int
		refused   bool
	}{
		{"0.16: 4,097 transactions are refused on the count", 16, 4097, true},
		{"0.16: 4,096 transactions pass the count", 16, 4096, false},
		{"0.15: 4,097 transactions pass the count", 15, 4097, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bp := capProducer(t, tc.consensus, 0)
			_, err := bp.HandleBlockMsg(msg(tc.txs))
			if err == nil {
				t.Fatalf("the block was signed; its transactions are not in the store")
			}
			if got := strings.Contains(err.Error(), capErr); got != tc.refused {
				t.Fatalf("error %q; refused on the count: %v, want %v", err, got, tc.refused)
			}
			if !tc.refused && err.Error() != "invalid transaction" {
				t.Fatalf("error %q, want the lookup's %q: the message did not reach the transactions", err, "invalid transaction")
			}
		})
	}
}
