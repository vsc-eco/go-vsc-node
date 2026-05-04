package transactionpool_test

import (
	"testing"

	"vsc-node/modules/db/vsc/transactions"
	transactionpool "vsc-node/modules/transaction-pool"

	"github.com/stretchr/testify/assert"
)

// callOp crafts a valid `call` VSCTransactionOp with the given rc_limit
// via the same SerializeVSC path IngestTx sees from real clients.
func callOp(t *testing.T, rcLimit uint) transactionpool.VSCTransactionOp {
	t.Helper()
	call := &transactionpool.VscContractCall{
		Caller:     "hive:alice",
		ContractId: "vsc1dontcare",
		Action:     "noop",
		Payload:    "{}",
		RcLimit:    rcLimit,
		NetId:      "vsc-mocknet",
	}
	op, err := call.SerializeVSC()
	if err != nil {
		t.Fatalf("SerializeVSC failed: %v", err)
	}
	return op
}

func TestStaticMaxRcCost(t *testing.T) {
	t.Run("Empty", func(t *testing.T) {
		assert.Equal(t, uint64(0), transactionpool.StaticMaxRcCost(nil))
		assert.Equal(t, uint64(0), transactionpool.StaticMaxRcCost([]transactionpool.VSCTransactionOp{}))
	})

	t.Run("CallUsesPerOpRcLimit", func(t *testing.T) {
		ops := []transactionpool.VSCTransactionOp{
			callOp(t, 1000),
			callOp(t, 500),
		}
		assert.Equal(t, uint64(1500), transactionpool.StaticMaxRcCost(ops))
	})

	t.Run("CallBelowMinFloor", func(t *testing.T) {
		// A call op declaring rc_limit=10 must be floored to 100 so the
		// cap can't be trivially bypassed by a malformed op.
		ops := []transactionpool.VSCTransactionOp{callOp(t, 10)}
		assert.Equal(t, uint64(100), transactionpool.StaticMaxRcCost(ops))
	})

	t.Run("FixedCostOps", func(t *testing.T) {
		ops := []transactionpool.VSCTransactionOp{
			{Type: "transfer"},
			{Type: "withdraw"},
			{Type: "stake_hbd"},
			{Type: "unstake_hbd"},
			{Type: "surprise"}, // default/unknown → 50
		}
		assert.Equal(t, uint64(100+200+200+200+50),
			transactionpool.StaticMaxRcCost(ops))
	})

	t.Run("Mixed", func(t *testing.T) {
		ops := []transactionpool.VSCTransactionOp{
			callOp(t, 700),
			{Type: "transfer"},
			{Type: "withdraw"},
		}
		assert.Equal(t, uint64(700+100+200), transactionpool.StaticMaxRcCost(ops))
	})
}

func TestStaticMaxRcCostFromRecord(t *testing.T) {
	t.Run("Empty", func(t *testing.T) {
		assert.Equal(t, uint64(0), transactionpool.StaticMaxRcCostFromRecord(nil))
	})

	t.Run("CallUsesDataRcLimit", func(t *testing.T) {
		ops := []transactions.TransactionOperation{
			{Type: "call", Data: map[string]interface{}{"rc_limit": uint(1000)}},
			{Type: "call", Data: map[string]interface{}{"rc_limit": int64(500)}},
			{Type: "call", Data: map[string]interface{}{"rc_limit": float64(300)}},
		}
		assert.Equal(t, uint64(1800),
			transactionpool.StaticMaxRcCostFromRecord(ops))
	})

	t.Run("CallMissingRcLimitFallsBackToFloor", func(t *testing.T) {
		ops := []transactions.TransactionOperation{
			{Type: "call", Data: map[string]interface{}{}},
			{Type: "call", Data: nil},
		}
		assert.Equal(t, uint64(200),
			transactionpool.StaticMaxRcCostFromRecord(ops))
	})

	t.Run("CallBelowMinFloor", func(t *testing.T) {
		ops := []transactions.TransactionOperation{
			{Type: "call", Data: map[string]interface{}{"rc_limit": int64(50)}},
		}
		assert.Equal(t, uint64(100),
			transactionpool.StaticMaxRcCostFromRecord(ops))
	})

	t.Run("FixedCostOps", func(t *testing.T) {
		ops := []transactions.TransactionOperation{
			{Type: "transfer"},
			{Type: "withdraw"},
			{Type: "stake_hbd"},
			{Type: "unstake_hbd"},
			{Type: "surprise"},
		}
		assert.Equal(t, uint64(100+200+200+200+50),
			transactionpool.StaticMaxRcCostFromRecord(ops))
	})
}

// TestRcLimitInvariant_Ingestion exercises the logic the ingestion gate
// uses. The gate in IngestTx is literally:
//
//	if StaticMaxRcCost(ops) > Headers.RcLimit { return err }
//
// so asserting the helper's arithmetic against realistic op lists covers
// both the reject and accept paths the real gate produces.
func TestRcLimitInvariant_Ingestion(t *testing.T) {
	tests := []struct {
		name    string
		rcLimit uint64
		ops     []transactionpool.VSCTransactionOp
		reject  bool
	}{
		{
			name:    "RejectsMultiTransfer",
			rcLimit: 150, // < 3*100
			ops: []transactionpool.VSCTransactionOp{
				{Type: "transfer"},
				{Type: "transfer"},
				{Type: "transfer"},
			},
			reject: true,
		},
		{
			name:    "AcceptsMultiTransfer",
			rcLimit: 350, // ≥ 3*100
			ops: []transactionpool.VSCTransactionOp{
				{Type: "transfer"},
				{Type: "transfer"},
				{Type: "transfer"},
			},
			reject: false,
		},
		{
			name:    "RejectsCallWithHighRcLimit",
			rcLimit: 500,
			ops:     []transactionpool.VSCTransactionOp{callOp(t, 10000)},
			reject:  true,
		},
		{
			name:    "AcceptsCallWithinBudget",
			rcLimit: 10000,
			ops:     []transactionpool.VSCTransactionOp{callOp(t, 5000)},
			reject:  false,
		},
		{
			name:    "RejectsMixedOverBudget",
			rcLimit: 500,
			ops: []transactionpool.VSCTransactionOp{
				callOp(t, 300),
				{Type: "withdraw"}, // 200
				{Type: "transfer"}, // 100 → sum 600
			},
			reject: true,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			staticMax := transactionpool.StaticMaxRcCost(tc.ops)
			rejects := staticMax > tc.rcLimit
			assert.Equal(t, tc.reject, rejects,
				"staticMax=%d rcLimit=%d", staticMax, tc.rcLimit)
		})
	}
}
