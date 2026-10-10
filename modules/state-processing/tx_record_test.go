package state_engine

import (
	"errors"
	"testing"

	"vsc-node/modules/db/vsc/transactions"
)

// flakyTxDb mirrors the real DB while a read is failing: GetTransaction returns
// nil and GetTransactionErr returns the error, for the first `fails` reads.
type flakyTxDb struct {
	transactions.Transactions
	fails int
	calls int
	rec   *transactions.TransactionRecord
}

func (f *flakyTxDb) GetTransaction(string) *transactions.TransactionRecord {
	f.calls++
	if f.calls <= f.fails {
		return nil
	}
	return f.rec
}

func (f *flakyTxDb) GetTransactionErr(string) (*transactions.TransactionRecord, error) {
	f.calls++
	if f.calls <= f.fails {
		return nil, errors.New("connection reset")
	}
	return f.rec, nil
}

// The apply path reads a transaction's record to skip one a prior block already
// handled. A failed read must not come back as "no record": the node would run
// the handled transaction again while its peers skip it.
func TestTxRecordRetriesInsteadOfReadingNil(t *testing.T) {
	want := &transactions.TransactionRecord{Id: "tx-a", Status: transactions.TransactionStatusConfirmed}
	f := &flakyTxDb{fails: 2, rec: want}
	se := &StateEngine{txDb: f}
	if got := se.txRecord("tx-a"); got != want {
		t.Fatalf("got %+v after transient read errors, want the record", got)
	}
	if f.calls != 3 {
		t.Fatalf("calls = %d, want 3 (2 failures + 1 success)", f.calls)
	}
}
