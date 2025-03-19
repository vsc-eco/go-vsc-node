package blockproducer

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
	"vsc-node/lib/hive"
	"vsc-node/lib/logger"
	a "vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/transactions"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	libp2p "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	"vsc-node/modules/vstream"

	"github.com/chebyrash/promise"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multicodec"
	blsu "github.com/protolambda/bls12-381-util"
	"github.com/vsc-eco/hivego"
)

var CONSENSUS_SPECS = common.CONSENSUS_SPECS

type BlockProducer struct {
	a.Plugin

	config      *common.IdentityConfig
	StateEngine *stateEngine.StateEngine
	VscBlocks   vscBlocks.VscBlocks
	VStream     *vstream.VStream
	HiveCreator hive.HiveTransactionCreator
	Datalayer   *datalayer.DataLayer
	TxDb        transactions.Transactions

	p2p     *libp2p.P2PServer
	service libp2p.PubSubService[p2pMessage]

	sigChannels  map[uint64]chan sigMsg
	blockSigning *signingInfo
	bh           uint64

	electionsDb elections.Elections

	_started bool

	log logger.Logger
}

type signingInfo struct {
	cid        cid.Cid
	slotHeight uint64
	circuit    *dids.PartialBlsCircuit
}

func (bp *BlockProducer) BlockTick(bh uint64) {
	if !bp._started {
		return
	}
	bp.bh = bh

	slotInfo := stateEngine.CalculateSlotInfo(bh)

	schedule := bp.StateEngine.GetSchedule(slotInfo.StartHeight)

	//Select current slot as per consensus algorithm
	var witnessSlot *stateEngine.WitnessSlot
	for _, slot := range schedule {
		if slot.SlotHeight == slotInfo.StartHeight {
			witnessSlot = &slot
			break
		}
	}

	if witnessSlot != nil {
		if witnessSlot.Account == bp.config.Get().HiveUsername && bh%CONSENSUS_SPECS.SlotLength == 0 {
			canProduce := bp.canProduce(bh)
			if canProduce {
				bp.ProduceBlock(witnessSlot.SlotHeight)
			}
		}
	}
}

type generateBlockParams struct {
	Transactions []vscBlocks.VscBlockTx
	PopulateTxs  bool
}

// This function should generate a deterministically generated block
// In the future we should apply protocol versioning to this
func (bp *BlockProducer) GenerateBlock(slotHeight uint64, options ...generateBlockParams) (*vscBlocks.VscHeader, []string, error) {
	prevBlock, err := bp.VscBlocks.GetBlockByHeight(slotHeight)
	daSession := datalayer.NewSession(bp.Datalayer)

	var prevBlockId *string
	var prevRange [2]int
	if prevBlock != nil {
		fmt.Println("PrevBlock exists")
		prevBlockId = &prevBlock.Id
		prevRange = [2]int{prevBlock.EndBlock, int(slotHeight)}
	} else {
		prevBlockId = nil
		prevRange = [2]int{0, int(slotHeight)}
	}

	offchainTxs := []vscBlocks.VscBlockTx{}
	outTxs := []string{}
	outCids := []cid.Cid{}
	if len(options) > 0 && options[0].PopulateTxs {
		fmt.Println("Populating transactions")
		offchainTxs = bp.generateTransactions(slotHeight)

	} else if len(options) > 0 && len(options[0].Transactions) > 0 {
		offchainTxs = options[0].Transactions
	}
	for _, tx := range offchainTxs {
		outTxs = append(outTxs, tx.Id)
		outCids = append(outCids, cid.MustParse(tx.Id))
	}

	oplog := bp.MakeOplog(slotHeight, daSession)

	if oplog != nil {
		offchainTxs = append(offchainTxs, *oplog)
	}

	mr, err := MerklizeCids(outCids)

	if err != nil {
		return nil, nil, err
	}

	blockData := vscBlocks.VscBlock{
		Headers: struct {
			Prevb *string "refmt:\"prevb\""
		}{
			Prevb: prevBlockId,
		},
		Transactions: offchainTxs,
		MerkleRoot:   &mr,
	}

	blockCid, _ := bp.Datalayer.PutObject(blockData)

	blockHeader := vscBlocks.VscHeader{
		Type:    "vsc-bh",
		Version: "0.1",
		Headers: struct {
			Br    [2]int  "refmt:\"br\""
			Prevb *string "refmt:\"prevb\""
		}{
			Br:    prevRange,
			Prevb: prevBlockId,
		},
		MerkleRoot: nil,
		Block:      *blockCid,
	}

	daSession.Commit()

	return &blockHeader, outTxs, nil
}

func (bp *BlockProducer) generateTransactions(slotHeight uint64) []vscBlocks.VscBlockTx {
	txs := make([]vscBlocks.VscBlockTx, 0)
	//Get transactions here!

	txRecords, _ := bp.TxDb.FindUnconfirmedTransactions(slotHeight)

	for _, txRecord := range txRecords {
		op := txRecord.Data["type"].(string)
		txs = append(txs, vscBlocks.VscBlockTx{
			Id:   txRecord.Id,
			Op:   &op,
			Type: 1,
		})
	}

	return txs
}

func (bp *BlockProducer) ProduceBlock(bh uint64) {
	//For right now we will just produce a blank
	//This will allow us to test the e2e parsing

	genBlock, transactions, _ := bp.GenerateBlock(bh, generateBlockParams{
		PopulateTxs: true,
	})

	cid, _ := bp.Datalayer.HashObject(genBlock)

	electionResult, err := bp.electionsDb.GetElectionByHeight(bh)

	if err != nil {
		return
	}

	circuit, err := dids.NewBlsCircuitGenerator(electionResult.MemberKeys()).Generate(*cid)

	go func() {
		// 4 ms
		time.Sleep(4 * time.Millisecond)
		bp.service.Send(p2pMessage{
			Type:       "block",
			SlotHeight: bh,
			Data: map[string]interface{}{
				"producer":     bp.config.Config.Get().HiveUsername,
				"transactions": transactions,
			},
		})
	}()

	bp.sigChannels[bh] = make(chan sigMsg)
	bp.blockSigning = &signingInfo{
		cid:        *cid,
		slotHeight: bh,
		circuit:    &circuit,
	}

	signedWeight, err := bp.waitForSigs(context.Background(), &electionResult)

	// fmt.Println("signedWeight", signedWeight, err)
	// fmt.Println("CircuitMap", circuit.CircuitMap())

	if !(signedWeight > (electionResult.TotalWeight * 2 / 3)) {
		fmt.Println("[bp] not enough signatures")
		return
	}

	finalCircuit, _ := circuit.Finalize()

	serialized, _ := finalCircuit.Serialize()

	// signedBlock["signature"] = serialized

	signedBlock := map[string]interface{}{
		"__t":         genBlock.Type,
		"__v":         genBlock.Version,
		"headers":     genBlock.Headers,
		"merkle_root": genBlock.MerkleRoot,
		"block":       genBlock.Block.String(),
		"signature":   serialized,
	}

	blockHeader := map[string]interface{}{
		"net_id": common.NETWORK_ID,

		"signed_block": signedBlock,
	}
	bbytes, _ := json.Marshal(blockHeader)

	op := bp.HiveCreator.CustomJson([]string{bp.config.Config.Get().HiveUsername}, []string{}, "vsc.produce_block", string(bbytes))

	tx := bp.HiveCreator.MakeTransaction([]hivego.HiveOperation{op})

	bp.HiveCreator.PopulateSigningProps(&tx, nil)

	sig, _ := bp.HiveCreator.Sign(tx)

	tx.AddSig(sig)

	id, err := bp.HiveCreator.Broadcast(tx)

	fmt.Println("BlockID!", id, err)
}

func (bp *BlockProducer) HandleBlockMsg(msg p2pMessage) (string, error) {
	if reflect.TypeOf(msg.Data["producer"]) != reflect.TypeOf("") {
		return "", errors.New("invalid input data")
	}

	if msg.SlotHeight+common.CONSENSUS_SPECS.SlotLength < bp.bh {
		return "", errors.New("invalid slot height (1)")
	}

	// fmt.Println("[bp] too ahead in future", msg.SlotHeight-common.CONSENSUS_SPECS.SlotLength, bp.bh)
	// fmt.Println("[bp] is block ahead?", msg.SlotHeight, bp.bh)

	if msg.SlotHeight > bp.bh {
		//Local node is out of sync perhaps
		for msg.SlotHeight > bp.bh {
			time.Sleep(1 * time.Second)
		}
	} else if msg.SlotHeight-common.CONSENSUS_SPECS.SlotLength > bp.bh {
		return "", errors.New("invalid slot height (2)")
	}

	producer := msg.Data["producer"].(string)

	slot := bp.getSlot(msg.SlotHeight)

	// fmt.Println("[bp] maybe bad producer", producer, slot.Account, producer == slot.Account)
	if producer != slot.Account {
		return "", errors.New("invalid producer")
	}

	blsPrivKey := blsu.SecretKey{}
	var arr [32]byte
	blsPrivSeedHex := bp.config.Get().BlsPrivKeySeed
	blsPrivSeed, err := hex.DecodeString(blsPrivSeedHex)
	if err != nil {
		return "", fmt.Errorf("failed to decode bls priv seed: %w", err)
	}
	if len(blsPrivSeed) != 32 {
		return "", fmt.Errorf("bls priv seed must be 32 bytes")
	}

	copy(arr[:], blsPrivSeed)
	if err = blsPrivKey.Deserialize(&arr); err != nil {
		return "", fmt.Errorf("failed to deserialize bls priv key: %w", err)
	}

	txStrs := []string{}
	for _, v := range msg.Data["transactions"].([]interface{}) {
		txStrs = append(txStrs, v.(string))
	}

	transactions := []vscBlocks.VscBlockTx{}
	for _, txStr := range txStrs {
		txRecord := bp.TxDb.GetTransaction(txStr)
		if txRecord == nil {
			return "", errors.New("invalid transaction")
		}
		op := txRecord.Data["type"].(string)
		transactions = append(transactions, vscBlocks.VscBlockTx{
			Id:   txRecord.Id,
			Op:   &op,
			Type: 1,
		})
	}

	blockHeader, _, err := bp.GenerateBlock(msg.SlotHeight, generateBlockParams{
		Transactions: transactions,
	})

	if err != nil {
		return "", err
	}

	// fmt.Println("[bp] maybe blockHeader", blockHeader, err)

	cid, err := bp.Datalayer.HashObject(blockHeader)

	if err != nil {
		return "", err
	}

	// fmt.Println("[bp] maybe cid", cid, err)

	sig := blsu.Sign(&blsPrivKey, cid.Bytes())

	sigBytes := sig.Serialize()

	sigStr := base64.URLEncoding.EncodeToString(sigBytes[:])
	// fmt.Println("[bp] maybe signed!", sigStr)

	return sigStr, nil
}

func (bp *BlockProducer) waitForSigs(ctx context.Context, election *elections.ElectionResult) (uint64, error) {
	if bp.blockSigning == nil {
		return 0, errors.New("no block signing info")
	}
	go func() {
		time.Sleep(12 * time.Second)
		bp.sigChannels[bp.blockSigning.slotHeight] <- sigMsg{
			Type: "end",
		}
	}()
	weightTotal := uint64(0)
	for _, weight := range election.Weights {
		weightTotal += weight
	}

	select {
	case <-ctx.Done():
		fmt.Println("[bp] ctx done")
		return 0, ctx.Err() // Return error if canceled
	default:
		signedWeight := uint64(0)

		fmt.Println("[bp] default action")
		// Perform the operation

		// slotHeight := bp.blockSigning.slotHeight

		for signedWeight < (weightTotal * 9 / 10) {
			msg := <-bp.sigChannels[bp.blockSigning.slotHeight]

			if msg.Type == "sig" {
				sig := msg.Msg
				sigStr := sig.Data["sig"].(string)
				account := sig.Data["account"].(string)

				var member dids.Member
				var index int
				for i, data := range election.Members {
					if data.Account == account {
						member = dids.BlsDID(data.Key)
						index = i
						break
					}
				}

				circuit := *bp.blockSigning.circuit

				err := circuit.AddAndVerify(member, sigStr)

				fmt.Println("[bp] aggregating signature", sigStr, "from", account)
				fmt.Println("[bp] agg err", err)
				if err == nil {
					signedWeight += election.Weights[index]
				}

			}
			if msg.Type == "end" {
				fmt.Println("Ending wait for sig")
				break
			}
		}
		fmt.Println("Done waittt")
		return signedWeight, nil
	}
}

func (bp *BlockProducer) canProduce(height uint64) bool {
	txRecords, _ := bp.TxDb.FindUnconfirmedTransactions(height)

	if len(txRecords) > 0 {
		return true
	}

	if len(bp.StateEngine.LedgerExecutor.Oplog) > 0 {
		return true
	}
	return false
}

func (bp *BlockProducer) MakeOplog(bh uint64, session *datalayer.Session) *vscBlocks.VscBlockTx {
	compileResult := bp.StateEngine.LedgerExecutor.Compile(bh)

	if compileResult == nil {
		return nil
	}

	oplogData := map[string]interface{}{
		"__t":   "vsc-oplog",
		"__v":   "0.1",
		"oplog": compileResult.OpLog,
	}

	cborBytes, _ := common.EncodeDagCbor(oplogData)

	cid, _ := common.HashBytes(cborBytes, multicodec.DagCbor)

	session.Put(cborBytes, cid)

	return &vscBlocks.VscBlockTx{
		Id:   cid.String(),
		Type: 6,
	}
}

func (bp *BlockProducer) getSlot(bh uint64) *stateEngine.WitnessSlot {
	schedule := bp.StateEngine.GetSchedule(bh)

	for _, slot := range schedule {
		if slot.SlotHeight == bh {
			return &slot
		}
	}

	return nil
}

func (bp *BlockProducer) Init() error {
	bp.VStream.RegisterBlockTick("block-producer", bp.BlockTick, false)
	return nil
}

func (bp *BlockProducer) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		err := bp.startP2P()
		bp._started = true
		if err != nil {
			reject(err)
			return
		}
		<-bp.service.Context().Done()
		resolve(nil)
	})
}

func (bp *BlockProducer) Stop() error {
	return bp.stopP2P()
}

func New(logger logger.Logger, p2p *libp2p.P2PServer, vstream *vstream.VStream, se *stateEngine.StateEngine, conf *common.IdentityConfig, hiveCreator hive.HiveTransactionCreator, da *datalayer.DataLayer, electionsDb elections.Elections, vscBlocks vscBlocks.VscBlocks, txDb transactions.Transactions) *BlockProducer {
	return &BlockProducer{
		log:         logger,
		sigChannels: make(map[uint64]chan sigMsg),
		StateEngine: se,
		VStream:     vstream,
		config:      conf,
		HiveCreator: hiveCreator,
		p2p:         p2p,
		Datalayer:   da,
		electionsDb: electionsDb,
		VscBlocks:   vscBlocks,
		TxDb:        txDb,
	}
}
