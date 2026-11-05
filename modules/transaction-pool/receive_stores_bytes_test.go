package transactionpool

import (
	"bytes"
	"encoding/base64"
	"testing"

	"vsc-node/modules/common"

	"github.com/ipfs/go-cid"
	cbornode "github.com/ipfs/go-ipld-cbor"
	"github.com/multiformats/go-multicodec"
	"github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"
)

// 18r regression.
//
// ReceiveTx (the gossip path) used to call indexTx WITHOUT PutRaw, while
// SubmitTx calls both. Every node except the original submitter therefore held
// the tx in Mongo as UNCONFIRMED with no bytes behind its CID. That stays
// invisible until the tx is anchored in a block: the apply path then resolves
// the CID through getDagOrBlock, which blocks rather than skip an op (skipping
// would fork the ledger), so if no reachable peer still has the bytes the node
// stalls indefinitely.
//
// The fix stores decodedTx in ReceiveTx. Its load-bearing claim is that
// decodedTx is byte-identical to the submitter's sTx.Tx and hashes to the same
// CID -- if that were wrong, the fix would store bytes under a CID nobody asks
// for and the stall would persist with no symptom change. This test pins that
// claim by walking the real submit -> broadcast -> receive encoding chain.

func txCidPrefix() cid.Prefix {
	// Identical to the prefix literal used in both SubmitTx and ReceiveTx.
	return cid.Prefix{
		Version:  1,
		Codec:    uint64(multicodec.DagCbor),
		MhType:   uint64(multihash.SHA2_256),
		MhLength: -1,
	}
}

func sampleShell(t *testing.T) VSCTransactionShell {
	t.Helper()
	payload, err := cbornode.WrapObject(map[string]interface{}{
		"to":     "hive:bob",
		"amount": 1234,
		"asset":  "hbd",
	}, multihash.SHA2_256, -1)
	require.NoError(t, err)

	op := VSCTransactionOp{Type: "transfer", Payload: payload.RawData()}
	op.RequiredAuths.Active = []string{"hive:alice"}

	return VSCTransactionShell{
		Type:    "vsc-tx",
		Version: "0.2",
		Headers: VSCTransactionHeader{
			Nonce:         7,
			RequiredAuths: []string{"hive:alice"},
			RcLimit:       500,
			NetId:         "vsc-mainnet",
		},
		Tx: []VSCTransactionOp{op},
	}
}

func TestTx18r_GossipedBytesHashToTheSameCidAsSubmitted(t *testing.T) {
	shell := sampleShell(t)

	// --- submitter: VSCTransaction.Serialize produces sTx.Tx ---------------
	submittedBytes, err := common.EncodeDagCbor(shell)
	require.NoError(t, err)
	require.NotEmpty(t, submittedBytes)

	// SubmitTx line ~85: the CID it indexes under, and PutRaw's key.
	submittedCid, err := txCidPrefix().Sum(submittedBytes)
	require.NoError(t, err)

	// --- the wire: Broadcast base64s sTx.Tx -------------------------------
	onTheWire := base64.StdEncoding.EncodeToString(submittedBytes)

	// --- receiver: ReceiveTx decodes it back ------------------------------
	decodedTx, err := base64.StdEncoding.DecodeString(onTheWire)
	require.NoError(t, err)

	// The claim, in two halves.
	require.True(t, bytes.Equal(submittedBytes, decodedTx),
		"gossip must deliver the submitter's bytes unchanged; PutRaw(decodedTx) depends on it")

	receivedCid, err := txCidPrefix().Sum(decodedTx)
	require.NoError(t, err)
	require.Equal(t, submittedCid.String(), receivedCid.String(),
		"the receiver must store under the same CID the apply path will request")

	// And the bytes really are a decodable tx, not an opaque blob that merely
	// happens to hash correctly.
	var roundTripped VSCTransactionShell
	require.NoError(t, common.DecodeCbor(decodedTx, &roundTripped))
	require.Equal(t, shell.Headers.NetId, roundTripped.Headers.NetId)
	require.Equal(t, shell.Headers.Nonce, roundTripped.Headers.Nonce)
	require.Equal(t, shell.Headers.RequiredAuths, roundTripped.Headers.RequiredAuths)
	require.Len(t, roundTripped.Tx, 1)
	require.Equal(t, "transfer", roundTripped.Tx[0].Type)
}

// A corrupted or truncated wire payload must NOT hash to the submitted CID --
// otherwise the equality above would be vacuous.
func TestTx18r_AlteredBytesDoNotMatchTheCid(t *testing.T) {
	shell := sampleShell(t)
	submittedBytes, err := common.EncodeDagCbor(shell)
	require.NoError(t, err)
	submittedCid, err := txCidPrefix().Sum(submittedBytes)
	require.NoError(t, err)

	altered := append([]byte(nil), submittedBytes...)
	altered[len(altered)-1] ^= 0xFF
	alteredCid, err := txCidPrefix().Sum(altered)
	require.NoError(t, err)

	require.NotEqual(t, submittedCid.String(), alteredCid.String(),
		"CID must be content-addressed; the equality assertion above would be meaningless otherwise")
}
