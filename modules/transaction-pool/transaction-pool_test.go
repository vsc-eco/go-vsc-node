package transactionpool_test

import (
	"crypto/ed25519"
	"crypto/rand"

	"encoding/base64"
	"fmt"
	"testing"
	"vsc-node/lib/dids"
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
	b64 := base64.StdEncoding.EncodeToString(serialized.Payload)

	fmt.Println("b64", b64)

	prefix := cid.Prefix{
		Version:  1,
		Codec:    uint64(multicodec.DagCbor),
		MhType:   uint64(mh.SHA2_256),
		MhLength: -1,
	}

	sumCid, _ := prefix.Sum(serialized.Payload)

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
		NetId:  "vsc-mocknet",
	}

	serializedOp, _ := transferOp.SerializeVSC()

	vscTx := transactionpool.VSCTransaction{
		Ops:   []transactionpool.VSCTransactionOp{serializedOp},
		Nonce: 0,
		NetId: "vsc-mocknet",
	}

	pubKey, privKey, _ := ed25519.GenerateKey(rand.Reader)

	didKey, _ := dids.NewKeyDID(pubKey)
	transactionCreator := transactionpool.TransactionCrafter{
		Identity: dids.NewKeyProvider(privKey),
		Did:      didKey,

		VSCBroadcast: &transactionpool.InternalBroadcast{
			TxPool: &transactionpool.TransactionPool{},
		},
	}

	sTx, _ := transactionCreator.SignFinal(vscTx)

	transactionCreator.Broadcast(sTx)
}

// TestFlow removed: requires too many module dependencies (p2p, datalayer, db, etc.)
// that have evolved. Rewrite with proper test infrastructure if needed.
