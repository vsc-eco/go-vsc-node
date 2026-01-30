package hive_test

import (
	"fmt"
	"testing"
	"vsc-node/lib/hive"

	"github.com/vsc-eco/hivego"
)

// Fill in with a test WIF for your test
// Use .env later on
const wif = ""

func TestSavingsWithdraw(t *testing.T) {
	// goenv := os.Getenv("GOENV")
	hiveClient := hivego.NewHiveRpc([]string{"https://api.hive.blog"})

	txCrafter := hive.TransactionCrafter{}
	txBroadcaster := hive.TransactionBroadcaster{
		Client: hiveClient,
	}

	// op := txCrafter.TransferFromSavings("vsc.node1", "vsc.node1", "0.100", "HBD", "test transfer", 3195778)
	op := txCrafter.CancelTransferFromSavings("vsc.node1", 100)

	kp, _ := hivego.KeyPairFromWif(wif)
	tx := txCrafter.MakeTransaction([]hivego.HiveOperation{op})
	txBroadcaster.PopulateSigningProps(&tx, nil)
	sig, err := tx.Sign(*kp, hiveClient.ChainID)
	fmt.Println("Signature for TX:", sig, "error", err)
	tx.AddSig(sig)

	txId, err := hiveClient.BroadcastRaw(tx)
	fmt.Println("TX ID:", txId, "error", err)
}

func TestAmountToString(t *testing.T) {
	// Test cases
	tests := []struct {
		amount int64
		want   string
	}{
		{10000, "10.000"},
		{10250, "10.250"},
		{1000, "1.000"},
		{100, "0.100"},
		{1, "0.001"},
		{0, "0.000"},
	}

	for _, test := range tests {
		got := hive.AmountToString(test.amount)
		if got != test.want {
			t.Errorf("AmountToString(%d) = %s; want %s", test.amount, got, test.want)
		}
	}
}
