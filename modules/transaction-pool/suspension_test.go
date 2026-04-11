package transactionpool

import (
	"strings"
	"testing"
)

type suspendedGate struct{}

func (suspendedGate) ProcessingSuspendedForPool() bool { return true }

func TestIngestTxRejectedWhenSuspendedBeforeOtherValidation(t *testing.T) {
	tp := &TransactionPool{processingGate: suspendedGate{}}
	_, err := tp.IngestTx(SerializedVSCTransaction{}, IngestOptions{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "suspended") {
		t.Fatalf("unexpected error: %v", err)
	}
}
