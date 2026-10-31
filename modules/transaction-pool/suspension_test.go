package transactionpool

import (
	"strings"
	"testing"

	"vsc-node/modules/common"
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

// A18: a transaction signed for another network is refused at admission,
// before its nonce or any RC is touched (its execution refused only its
// effect and spent both); one for this network passes the check.
func TestA18_WrongNetworkIsRefusedAtAdmission(t *testing.T) {
	tx := func(net string) SerializedVSCTransaction {
		shell, err := common.EncodeDagCbor(VSCTransactionShell{Type: "vsc-tx", Version: "0.2",
			Headers: VSCTransactionHeader{Nonce: 0, RequiredAuths: []string{"did:pkh:eip155:1:0x0000000000000000000000000000000000000001"}, NetId: net},
			Tx:      []VSCTransactionOp{}})
		if err != nil {
			t.Fatal(err)
		}
		sig, _ := common.EncodeDagCbor(SignaturePackage{Type: "vsc-sig", Sigs: []common.Sig{}})
		return SerializedVSCTransaction{Tx: shell, Sig: sig}
	}
	tp := &TransactionPool{netId: "vsc-mainnet"}
	if _, err := tp.IngestTx(tx("vsc-testnet"), IngestOptions{}); err == nil || !strings.Contains(err.Error(), `is for network "vsc-testnet"`) {
		t.Fatalf("testnet tx on mainnet: %v", err)
	}
	// This network's: past the check (it then needs the nonce store, absent here).
	func() {
		defer func() { _ = recover() }()
		if _, err := tp.IngestTx(tx("vsc-mainnet"), IngestOptions{}); err != nil && strings.Contains(err.Error(), "is for network") {
			t.Fatalf("this network's tx refused as another's: %v", err)
		}
	}()
}
