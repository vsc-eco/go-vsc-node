package transactionpool_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"time"

	"encoding/base64"
	"fmt"
	"testing"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/transactions"
	"vsc-node/modules/db/vsc/witnesses"
	p2pInterface "vsc-node/modules/p2p"
	transactionpool "vsc-node/modules/transaction-pool"

	"github.com/ipfs/go-cid"

	"github.com/multiformats/go-multicodec"
	mh "github.com/multiformats/go-multihash"
)

func TestTest(t *testing.T) {
	tx := transactionpool.VSCTransfer{
		To:     "hive:vsc.account",
		From:   "hive:vsc.account",
		Amount: "0.005",
		Asset:  "hbd",
	}

	serialized, err := tx.SerializeVSC()

	fmt.Println(serialized, err)
	b64 := base64.StdEncoding.EncodeToString(serialized.Tx)

	fmt.Println("b64", b64)

	prefix := cid.Prefix{
		Version:  1,
		Codec:    uint64(multicodec.DagCbor),
		MhType:   uint64(mh.SHA2_256),
		MhLength: -1,
	}

	sumCid, _ := prefix.Sum(serialized.Tx)

	fmt.Println("cid", sumCid)

	cza := cid.MustParse("bafyreier73v5xdpfzar7bxwiwrjyia6crcz4vw6hed43wancva5aaqsvue")

	czPrefix := cza.Prefix()
	fmt.Println(czPrefix.Codec, czPrefix.MhLength, czPrefix.MhType, czPrefix.Version)
}

func TestTx(t *testing.T) {
	transferOp := &transactionpool.VSCTransfer{
		From:   "vsc.account",
		To:     "vsc.account",
		Amount: "0.001",
		Asset:  "hbd",
		NetId:  "vsc-mainnet",
	}

	txId, _ := transferOp.Hash()

	fmt.Println(txId)
	pubKey, privKey, _ := ed25519.GenerateKey(rand.Reader)

	didKey, _ := dids.NewKeyDID(pubKey)
	transactionCreator := transactionpool.TransactionCrafter{
		Identity: dids.NewKeyProvider(privKey),
		Did:      didKey,

		VSCBroadcast: &transactionpool.InternalBroadcast{
			TxPool: &transactionpool.TransactionPool{},
		},
	}

	sTx, _ := transactionCreator.SignFinal(transferOp)

	transactionCreator.Broadcast(sTx)
}

func TestFlow(t *testing.T) {
	dbConf := db.NewDbConfig()

	err := dbConf.SetDbURI("mongodb://localhost:27017")
	if err != nil {
		fmt.Println("error is", err)
		os.Exit(1)
	}

	db := db.New(dbConf)
	vscDb := vsc.New(db, "tx-pool")
	txDb := transactions.New(vscDb)
	witnessesDb := witnesses.New(vscDb)
	p2p := p2pInterface.New(witnessesDb)

	da := datalayer.New(p2p)

	txPool := transactionpool.New(p2p, txDb, da)

	plugins := []aggregate.Plugin{
		db,
		vscDb,
		witnessesDb,
		txDb,
		p2p,
		txPool,
	}

	a := aggregate.New(plugins)

	// go func() {
	// 	}()
	test_utils.RunPlugin(t, a, false)

	time.Sleep(30 * time.Second)

	pubKey, privKey, _ := ed25519.GenerateKey(rand.Reader)
	didKey, _ := dids.NewKeyDID(pubKey)

	transferOp := &transactionpool.VSCTransfer{
		From:   didKey.String(),
		To:     "vsc.account",
		Amount: "0.001",
		Asset:  "hbd",
		NetId:  "vsc-mainnet",
	}

	txId, _ := transferOp.Hash()

	fmt.Println("txId", txId)

	transactionCreator := transactionpool.TransactionCrafter{
		Identity: dids.NewKeyProvider(privKey),
		Did:      didKey,

		VSCBroadcast: &transactionpool.InternalBroadcast{
			TxPool: txPool,
		},
	}

	sTx, _ := transactionCreator.SignFinal(transferOp)

	transactionCreator.Broadcast(sTx)

	select {}
}

func TestEth(t *testing.T) {

}
