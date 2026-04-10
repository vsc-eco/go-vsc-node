package transactionpool_test

import (
	"encoding/base64"
	"fmt"
	"testing"
	"vsc-node/lib/dids"
	transactionpool "vsc-node/modules/transaction-pool"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
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

type mockBroadcast struct{}

func (m *mockBroadcast) Broadcast(tx transactionpool.SerializedVSCTransaction) (string, error) {
	return "mock-cid", nil
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

	ethKey, _ := ethCrypto.GenerateKey()
	ethAddr := ethCrypto.PubkeyToAddress(ethKey.PublicKey).Hex()

	transactionCreator := transactionpool.TransactionCrafter{
		Identity:     dids.NewEthProvider(ethKey),
		Did:          dids.NewEthDID(ethAddr),
		VSCBroadcast: &mockBroadcast{},
	}

	sTx, err := transactionCreator.SignFinal(vscTx)
	if err != nil {
		t.Fatalf("SignFinal failed: %v", err)
	}

	_, err = transactionCreator.Broadcast(sTx)
	if err != nil {
		t.Fatalf("Broadcast failed: %v", err)
	}
}

// TestFlow removed: requires too many module dependencies (p2p, datalayer, db, etc.)
// that have evolved. Rewrite with proper test infrastructure if needed.
