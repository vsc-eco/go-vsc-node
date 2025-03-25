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
	hiveClient := hivego.NewHiveRpc("https://api.hive.blog")

	txCrafter := hive.TransactionCrafter{}
	txBroadcaster := hive.TransactionBroadcaster{
		Client: hiveClient,
	}

	// op := txCrafter.TransferFromSavings("vsc.node1", "vsc.node1", "0.100", "HBD", "test transfer", 3195778)
	op := txCrafter.CancelTransferFromSavings("vsc.node1", 100)

	kp, _ := hivego.KeyPairFromWif(wif)
	tx := txCrafter.MakeTransaction([]hivego.HiveOperation{op})
	txBroadcaster.PopulateSigningProps(&tx, nil)
	sig, err := tx.Sign(*kp)
	fmt.Println("Signature for TX:", sig, "error", err)
	tx.AddSig(sig)

	txId, err := hiveClient.BroadcastRaw(tx)
	fmt.Println("TX ID:", txId, "error", err)
}
