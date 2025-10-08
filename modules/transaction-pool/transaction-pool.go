package transactionpool

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/db/vsc/nonces"
	"vsc-node/modules/db/vsc/transactions"
	ledgerSystem "vsc-node/modules/ledger-system"
	libp2p "vsc-node/modules/p2p"
	rcSystem "vsc-node/modules/rc-system"

	"github.com/chebyrash/promise"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	cbornode "github.com/ipfs/go-ipld-cbor"
	"github.com/multiformats/go-multicodec"
	"github.com/multiformats/go-multihash"
	"go.mongodb.org/mongo-driver/mongo"
)

type TransactionPool struct {
	TxDb             transactions.Transactions
	nonceDb          nonces.Nonces
	rcs              *rcSystem.RcSystem
	hiveBlocks       hive_blocks.HiveBlocks
	p2p              *libp2p.P2PServer
	service          libp2p.PubSubService[p2pMessage]
	datalayer        *datalayer.DataLayer
	electionDataInfo elections.ElectionDataInfo
	// TODO: adding election members

	conf common.IdentityConfig
}

type IngestOptions struct {
	Broadcast bool
}

var MAX_TX_SIZE = 16384

// Ingests and verifies a transaction
func (tp *TransactionPool) IngestTx(sTx SerializedVSCTransaction, options ...IngestOptions) (*cid.Cid, error) {
	if sTx.Sig == nil {
		return nil, errors.New("no signature provided")
	}

	prefix := cid.Prefix{
		Version:  1,
		Codec:    uint64(multicodec.DagCbor),
		MhType:   uint64(multihash.SHA2_256),
		MhLength: -1,
	}

	if len(sTx.Tx) > MAX_TX_SIZE {
		return nil, fmt.Errorf("transaction size too big %d > %d", len(sTx.Tx), MAX_TX_SIZE)
	}

	cidz, err := prefix.Sum(sTx.Tx)
	if err != nil {
		return nil, err
	}

	sigPack := SignaturePackage{}

	err = cbornode.DecodeInto(sTx.Sig, &sigPack)
	fmt.Println("decode error", err)
	if err != nil {
		return nil, err
	}

	txShell := VSCTransactionShell{}
	if err := common.DecodeCbor(sTx.Tx, &txShell); err != nil {
		fmt.Println("decode error2", err)
		return nil, err
	}

	// // We throw away `b` here to ensure that we canonicalize the encoded
	// // CBOR object.
	// node, err := cbornode.WrapObject(m, multihash.SHA2_256, -1)
	// if err != nil {
	// 	return nil, err
	// }

	ops := make([]VSCTransactionSignOp, 0, len(txShell.Tx))

	for _, op := range txShell.Tx {
		payload := make(map[string]interface{})
		if err := cbornode.DecodeInto(op.Payload, &payload); err != nil {
			fmt.Println("decode error3", err)
			return nil, err
		}

		block, _ := cbornode.Decode(op.Payload, uint64(multihash.SHA2_256), -1)

		jsonBytes, _ := block.MarshalJSON()

		ops = append(ops, VSCTransactionSignOp{
			Type:    op.Type,
			Payload: string(jsonBytes),
		})
	}

	txSignStruct := VSCTransactionSignStruct{
		Type:    txShell.Type,
		Version: txShell.Version,
		Headers: txShell.Headers,
		Tx:      ops,
	}

	// ssbytes, _ := json.Marshal(txSignStruct)
	// fmt.Println("signingShell2", string(ssbytes))

	bytes, err := common.EncodeDagCbor(txSignStruct)
	cidz1, _ := cid.Prefix{
		Version:  1,
		Codec:    uint64(multicodec.DagCbor),
		MhType:   uint64(multihash.SHA2_256),
		MhLength: -1,
	}.Sum(bytes)
	blk, _ := blocks.NewBlockWithCid(bytes, cidz1)

	if err != nil {
		return nil, err
	}

	hashAuths := HashKeyAuths(txShell.Headers.RequiredAuths)
	nonceRecord, err := tp.nonceDb.GetNonce(hashAuths)

	if err != mongo.ErrNoDocuments && err != nil {
		return nil, fmt.Errorf("failed to get nonce: %w", err)
	}

	nonce := nonceRecord.Nonce

	if txShell.Headers.Nonce < nonce && nonce != 0 {
		return nil, fmt.Errorf("nonce too low: %d < %d", txShell.Headers.Nonce, nonce)
	}

	if txShell.Headers.Nonce > nonce+100 {
		return nil, fmt.Errorf("nonce incrementing too fast: %d > %d", txShell.Headers.Nonce, nonce+100)
	}

	fmt.Println("sigPack.Sigs", sigPack.Sigs)

	// make DIDs + verify signatures
	didBuf, hasVscDID, err := makeDIDs(
		tp.electionDataInfo.Members,
		tp.electionDataInfo.Weights,
		txShell.Headers.RequiredAuths,
		sigPack.Sigs,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to parse DIDs: %w", err)
	}

	verified, err := common.VerifySignatures(didBuf, blk, sigPack.Sigs)
	if err != nil {
		return nil, err
	}

	fmt.Println("Verification?", verified)
	if !verified {
		return nil, errors.New("missing required auth")
	}

	latestBlk, err := tp.hiveBlocks.GetHighestBlock()

	if err != nil {
		return nil, fmt.Errorf("failed to get latest block: %w", err)
	}

	// if transaction is signed by VSC DID, then ignore RCs
	if !hasVscDID {
		rcsAvailable := tp.rcs.GetAvailableRCs(txShell.Headers.RequiredAuths[0], latestBlk)

		fmt.Println("RCS available for", txShell.Headers.RequiredAuths[0], ":", rcsAvailable)
		//Note: RcLimit is user defined input
		if uint64(rcsAvailable) < txShell.Headers.RcLimit || txShell.Headers.RcLimit == 0 {
			return nil, fmt.Errorf("not enough RCS available: %d < %d", rcsAvailable, txShell.Headers.RcLimit)
		}
	}

	//VALIDATION COMPLETE

	err = tp.indexTx(cidz.String(), txShell)
	if err != nil {
		return nil, err
	}
	cidc, err := tp.datalayer.PutRaw(sTx.Tx, datalayer.PutRawOptions{
		Codec: multicodec.DagCbor,
	})
	if err != nil {
		return nil, err
	}

	fmt.Println("tx CID", cidz.String(), "cidc", cidc.String())

	fmt.Println("Options", options)
	if len(options) == 0 || options[0].Broadcast {
		err = tp.Broadcast(cidz.String(), sTx)
		fmt.Println("Broadcasting transaction", cidz.String(), err)
		if err != nil {
			return nil, err
		}
	}

	return &cidz, nil
}

func (tp *TransactionPool) Broadcast(id string, serializedTx SerializedVSCTransaction) error {
	b64tx := base64.StdEncoding.EncodeToString(serializedTx.Tx)
	b64sig := base64.StdEncoding.EncodeToString(serializedTx.Sig)

	return tp.service.Send(p2pMessage{
		Type: "announce_tx",
		Data: map[string]interface{}{
			"id":  id,
			"tx":  b64tx,
			"sig": b64sig,
		},
	})
}

func (tp *TransactionPool) ReceiveTx(p2pMsg p2pMessage) {

	if p2pMsg.Type != "announce_tx" {
		return
	}

	formattedData := struct {
		Tx  string `json:"tx"`
		Sig string `json:"sig"`
	}{}

	bmh, _ := json.Marshal(p2pMsg.Data)
	err := json.Unmarshal(bmh, &formattedData)

	if err != nil {
		return
	}

	decodedTx, _ := base64.StdEncoding.DecodeString(formattedData.Tx)
	decodedSig, _ := base64.StdEncoding.DecodeString(formattedData.Sig)

	if len(decodedTx) > MAX_TX_SIZE {
		return
	}

	prefix := cid.Prefix{
		Version:  1,
		Codec:    uint64(multicodec.DagCbor),
		MhType:   uint64(multihash.SHA2_256),
		MhLength: -1,
	}

	cidz, _ := prefix.Sum(decodedTx)

	txShell := VSCTransactionShell{}

	if err := common.DecodeCbor(decodedTx, &txShell); err != nil {
		fmt.Println("decode error2", err)
		return
	}

	if err != nil {
		fmt.Println("decode error", err)
		return
	}

	ops := make([]VSCTransactionSignOp, 0, len(txShell.Tx))

	for _, op := range txShell.Tx {
		payload := make(map[string]interface{})
		if err := cbornode.DecodeInto(op.Payload, &payload); err != nil {
			return
		}

		block, _ := cbornode.Decode(op.Payload, uint64(multihash.SHA2_256), -1)

		jsonBytes, _ := block.MarshalJSON()

		ops = append(ops, VSCTransactionSignOp{
			Type:    op.Type,
			Payload: string(jsonBytes),
		})
	}

	txSignStruct := VSCTransactionSignStruct{
		Type:    txShell.Type,
		Version: txShell.Version,
		Headers: txShell.Headers,
		Tx:      ops,
	}

	bytes, err := common.EncodeDagCbor(txSignStruct)
	cidz1, _ := cid.Prefix{
		Version:  1,
		Codec:    uint64(multicodec.DagCbor),
		MhType:   uint64(multihash.SHA2_256),
		MhLength: -1,
	}.Sum(bytes)
	blk, _ := blocks.NewBlockWithCid(bytes, cidz1)

	sigPack := SignaturePackage{}

	sigNode, err := cbornode.Decode(decodedSig, multihash.SHA2_256, -1)
	sigJson, _ := sigNode.MarshalJSON()

	json.Unmarshal(sigJson, &sigPack)

	didBuf, hasVscDID, err := makeDIDs(
		tp.electionDataInfo.Members,
		tp.electionDataInfo.Weights,
		txShell.Headers.RequiredAuths,
		sigPack.Sigs,
	)
	if err != nil {
		fmt.Println(fmt.Errorf("failed to parse DIDs: %w", err))
		return
	}

	verified, err := common.VerifySignatures(didBuf, blk, sigPack.Sigs)

	latestBlk, err := tp.hiveBlocks.GetHighestBlock()

	if err != nil {
		return
	}

	// if transaction is signed by VSC DID, then ignore RCs
	if !hasVscDID {
		rcsAvailable := tp.rcs.GetAvailableRCs(txShell.Headers.RequiredAuths[0], latestBlk)

		//Note: RcLimit is user defined input
		if uint64(rcsAvailable) < txShell.Headers.RcLimit || txShell.Headers.RcLimit == 0 {
			fmt.Println(errors.New("insufficient RCs"))
			return
		}
	}

	// fmt.Println("broadcast verify result", verified)
	if err != nil {
		return
	}

	if verified {
		tp.indexTx(cidz.String(), txShell)
	}
}

func (tp *TransactionPool) indexTx(txId string, txShell VSCTransactionShell) error {
	if len(txShell.Tx) == 0 {
		return errors.New("transaction has no operations")
	}

	opTypesM := make(map[string]bool, 0)
	ops := make([]transactions.TransactionOperation, 0)
	for idx, op := range txShell.Tx {
		opTypesM[op.Type] = true

		opData := make(map[string]interface{})
		err := cbornode.DecodeInto(op.Payload, &opData)

		if err != nil {
			return err
		}

		ops = append(ops, transactions.TransactionOperation{
			RequiredAuths: txShell.Headers.RequiredAuths,
			Type:          op.Type,
			Idx:           int64(idx),
			Data:          opData,
		})
	}

	opTypes := make([]string, 0, len(opTypesM))
	for opType := range opTypesM {
		opTypes = append(opTypes, opType)
	}

	return tp.TxDb.Ingest(transactions.IngestTransactionUpdate{
		Id:            txId,
		Status:        "UNCONFIRMED",
		RequiredAuths: txShell.Headers.RequiredAuths,
		Type:          "vsc",
		Version:       txShell.Version,
		Nonce:         txShell.Headers.Nonce,
		OpTypes:       opTypes,
		Ops:           ops,
		RcLimit:       txShell.Headers.RcLimit,
		Ledger:        make([]ledgerSystem.OpLogEvent, 0),
	})
}

func (tp *TransactionPool) Init() error {
	return nil
}

func (tp *TransactionPool) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		err := tp.startP2P()
		if err != nil {
			reject(err)
			return
		}

		<-tp.service.Context().Done()
		resolve(nil)
	})
}

func (tp *TransactionPool) Stop() error {
	return tp.stopP2P()
}

func New(p2p *libp2p.P2PServer, txDb transactions.Transactions, nonceDb nonces.Nonces, hiveBlocks hive_blocks.HiveBlocks, da *datalayer.DataLayer, conf common.IdentityConfig, rcSystem *rcSystem.RcSystem) *TransactionPool {
	return &TransactionPool{
		TxDb:       txDb,
		nonceDb:    nonceDb,
		p2p:        p2p,
		datalayer:  da,
		conf:       conf,
		hiveBlocks: hiveBlocks,
		rcs:        rcSystem,
	}
}

// MakeDIDs parses the requiredAuths into DIDs.
// Returns slice of parsed DIDs and a bool for if any of the signatures were
// signed by a VSC DID
func makeDIDs(
	electionMembers []elections.ElectionMember,
	electionMemberWeights []uint64,
	requiredAuths []string,
	signatures []common.Sig,
) ([]dids.DID, bool, error) {
	if len(electionMembers) == 0 {
		return nil, false, errors.New("no election member provided")
	}

	if len(electionMembers) != len(electionMemberWeights) {
		return nil, false, errors.New("invalid number of member weights provided")
	}

	// collecting member BlsDID
	var (
		electionMemberDIDs = make([]dids.BlsDID, len(electionMembers))
		err                error
	)

	for i, member := range electionMembers {
		electionMemberDIDs[i], err = dids.ParseBlsDID(member.Key)

		if err != nil {
			return nil, false, fmt.Errorf(
				"failed to parse election member DID: Account [%s], Key [%s], err [%w]",
				member.Account, member.Key, err,
			)
		}
	}

	// make network DID
	var (
		threshold      = uint64(0)
		signedByVscDID = false
		didBuf         = make([]dids.DID, len(requiredAuths))
	)

	// get threshold
	for _, w := range electionMemberWeights {
		threshold += w
	}
	threshold = uint64(math.Floor(float64(threshold) * (2.0 / 3.0)))

	for i, authKID := range requiredAuths {
		isVscDID := strings.HasPrefix(authKID, dids.VscDIDPrefix)

		if !isVscDID {
			didBuf[i], err = dids.Parse(authKID)
			if err != nil {
				return nil, signedByVscDID, fmt.Errorf("failed to parse DID: %w", err)
			}
			continue
		}

		// has VSC DID
		signedByVscDID = true

		// matching authKID with sig.Kid
		sigIndex := slices.IndexFunc(signatures, func(s common.Sig) bool { return authKID == s.Kid })
		notFound := sigIndex == -1
		if notFound {
			return nil, signedByVscDID, errors.New("valid signature not found")
		}

		sig := &signatures[sigIndex]

		if len(sig.Bv) == 0 {
			return nil, signedByVscDID, errors.New("signature's bitvector not found")
		}

		didBuf[i], err = dids.NewVscDID(
			electionMemberDIDs,
			electionMemberWeights,
			sig.Bv,
			threshold,
		)

		if err != nil {
			return nil, signedByVscDID, fmt.Errorf("failed to parse VSC DID: %w", err)
		}
	}

	return didBuf, signedByVscDID, nil
}
