package blockproducer

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
	"vsc-node/lib/hive"
	"vsc-node/lib/logger"
	a "vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/common/common_types"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/nonces"
	"vsc-node/modules/db/vsc/transactions"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	blockconsumer "vsc-node/modules/hive/block-consumer"
	ledgerSystem "vsc-node/modules/ledger-system"
	libp2p "vsc-node/modules/p2p"
	rcSystem "vsc-node/modules/rc-system"
	stateEngine "vsc-node/modules/state-processing"
	transactionpool "vsc-node/modules/transaction-pool"

	"github.com/chebyrash/promise"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multicodec"
	blsu "github.com/protolambda/bls12-381-util"
	"github.com/vsc-eco/hivego"
)

var CONSENSUS_SPECS = common.CONSENSUS_SPECS

type BlockProducer struct {
	a.Plugin

	config       common.IdentityConfig
	sconf        systemconfig.SystemConfig
	StateEngine  *stateEngine.StateEngine
	VscBlocks    vscBlocks.VscBlocks
	hiveConsumer *blockconsumer.HiveConsumer
	HiveCreator  hive.HiveTransactionCreator
	Datalayer    *datalayer.DataLayer
	TxDb         transactions.Transactions
	rcSystem     *rcSystem.RcSystem
	nonceDb      nonces.Nonces

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

func (bp *BlockProducer) BlockTick(bh uint64, headHeight *uint64) {
	if !bp._started {
		return
	}
	bp.bh = bh
	if headHeight == nil {
		fmt.Println("HeadHeight is nil")
		return
	}
	if bh < *headHeight-40 {
		return
	}

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
			// canProduce := bp.canProduce(bh)
			// fmt.Println("Can produce", canProduce)
			// if canProduce {
			bp.ProduceBlock(witnessSlot.SlotHeight)
			// }
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
		// fmt.Println("PrevBlock exists")
		prevBlockId = &prevBlock.BlockContent
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
	}

	oplog := bp.MakeOplog(slotHeight, daSession)

	if oplog != nil {
		offchainTxs = append(offchainTxs, *oplog)
	}

	//Make contract outputs
	contractOutputs := bp.MakeOutputs(daSession)
	if len(contractOutputs) > 0 {
		offchainTxs = append(offchainTxs, contractOutputs...)
	}

	for _, tx := range offchainTxs {
		outCids = append(outCids, cid.MustParse(tx.Id))
	}

	// rcMap := bp.MakeRcMap()

	// if len(offchainTxs) == 0 {
	// 	return nil, nil, errors.New("no transactions to include")
	// }

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

	// fmt.Println("vsc Block", blockData)

	blockCid, err := bp.Datalayer.PutObject(blockData)
	// fmt.Println("vsc Block", blockCid, err)

	if err != nil {
		return nil, nil, err
	}

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
		MerkleRoot: &mr,
		Block:      *blockCid,
	}

	daSession.Commit()

	return &blockHeader, outTxs, nil
}

func (bp *BlockProducer) generateTransactions(slotHeight uint64) []vscBlocks.VscBlockTx {
	txs := make([]vscBlocks.VscBlockTx, 0)
	//Get transactions here!

	prefilteredTxs, _ := bp.TxDb.FindUnconfirmedTransactions(slotHeight)

	fmt.Println("prefilteredTxs", prefilteredTxs)
	txRecords := make([]transactions.TransactionRecord, 0)

	nonceMap := make(map[string]int64, len(prefilteredTxs))

	for _, txRecord := range prefilteredTxs {
		keyId := transactionpool.HashKeyAuths(txRecord.RequiredAuths)
		if nonceMap[keyId] == 0 {
			nonceRecord, _ := bp.nonceDb.GetNonce(keyId)
			nonceMap[keyId] = int64(nonceRecord.Nonce)
		}
		if txRecord.Nonce >= nonceMap[keyId] && txRecord.RcLimit >= bp.sconf.ConsensusParams().MinRcLimit {
			txRecords = append(txRecords, txRecord)
		}
	}

	if len(txRecords) == 0 {
		return []vscBlocks.VscBlockTx{}
	}

	//Sequence transactions

	preOrder := make([]transactions.TransactionRecord, len(txRecords))
	copy(preOrder, txRecords)

	ids := make([]string, 0)

	for _, txRecord := range txRecords {
		ids = append(ids, txRecord.Id)
	}

	fmt.Println("txRecords", txRecords)
	fmt.Println("ids", ids)
	seedStr := []byte(transactionpool.HashKeyAuths(ids))

	data := binary.BigEndian.Uint64(seedStr[:])
	rando := rand.New(rand.NewSource(int64(data)))
	rando.Shuffle(len(preOrder), func(i, j int) {
		preOrder[i], preOrder[j] = preOrder[j], preOrder[i]
	})

	// nonceMap := make(map[string]uint64)

	txMap := make(map[string][]transactions.TransactionRecord)

	for _, txRecord := range txRecords {
		keyId := transactionpool.HashKeyAuths(txRecord.RequiredAuths)
		if txMap[keyId] == nil {
			txMap[keyId] = []transactions.TransactionRecord{}
		}
		txMap[keyId] = append(txMap[keyId], txRecord)
	}

	for k, _ := range txMap {
		slices.SortFunc(txMap[k], func(i, j transactions.TransactionRecord) int {
			return int(i.Nonce) - int(j.Nonce)
		})
	}

	ledgerSession := ledgerSystem.NewSession(&ledgerSystem.LedgerState{
		Oplog:           make([]ledgerSystem.OpLogEvent, 0),
		VirtualLedger:   make(map[string][]ledgerSystem.LedgerUpdate),
		GatewayBalances: make(map[string]uint64),

		BlockHeight: slotHeight,
		LedgerDb:    bp.StateEngine.LedgerState.LedgerDb,
		ActionDb:    bp.StateEngine.LedgerState.ActionDb,
		BalanceDb:   bp.StateEngine.LedgerState.BalanceDb,
	})
	// ledgerSession := bp.StateEngine.LedgerSystem.NewSession(slotHeight)
	rcSession := bp.rcSystem.NewSession(ledgerSession)

	sequencedTxs := make([]transactions.TransactionRecord, 0)
	for _, preRecord := range preOrder {
		keyId := transactionpool.HashKeyAuths(preRecord.RequiredAuths)
		payer := preRecord.RequiredAuths[0]

		tx := txMap[keyId][0]

		rcLimit := uint64(0)
		if tx.RcLimit == 0 {
			//Minimum of 0.05 hbd or 50 integer units
			for _, op := range tx.Ops {
				if op.Type == "transfer" {
					rcLimit += 100
				} else if op.Type == "stake_hbd" {
					rcLimit += 200
				} else if op.Type == "unstake_hbd" {
					rcLimit += 200
				} else if op.Type == "withdraw" {
					rcLimit += 200
				} else if op.Type == "call" {
					rcLimit += 100
				} else {
					rcLimit += 50
				}
			}
		}

		didConsume, _ := rcSession.Consume(payer, slotHeight, int64(tx.RcLimit))

		if didConsume {
			if nonceMap[keyId] == tx.Nonce {
				txMap[keyId] = txMap[keyId][1:]
				nonceMap[keyId]++
				sequencedTxs = append(sequencedTxs, tx)
			}
		}
	}
	// rcFinish := rcSession.Done()
	// fmt.Println("rcFinish", rcFinish)
	for _, txRecord := range sequencedTxs {
		op := txRecord.Ops[0].Type
		txs = append(txs, vscBlocks.VscBlockTx{
			Id:   txRecord.Id,
			Op:   &op,
			Type: int(common.BlockTypeTransaction),
		})
	}

	return txs
}

func (bp *BlockProducer) sortTransactions(slotHeight uint64) []transactions.TransactionRecord {

	return nil
}

func (bp *BlockProducer) ProduceBlock(bh uint64) {
	//For right now we will just produce a blank
	//This will allow us to test the e2e parsing

	fmt.Println("bp.bh", bp.bh)
	stTime := bp.bh + 1
	for i := 0; i < 5; i++ {
		if bh == stTime {
			break
		}

		time.Sleep(1 * time.Second)
	}

	genBlock, transactions, err := bp.GenerateBlock(bh, generateBlockParams{
		PopulateTxs: true,
	})

	if err != nil {
		fmt.Println("Error generating block", err)
		return
	}

	cid, _ := bp.Datalayer.HashObject(genBlock)

	electionResult, err := bp.electionsDb.GetElectionByHeight(bh)

	if err != nil {
		fmt.Println("Error generating block", err)
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

	if err != nil {
		fmt.Println("Error waiting for signatures", err)
		return
	}

	// fmt.Println("signedWeight", signedWeight, err)
	// fmt.Println("CircuitMap", circuit.CircuitMap())

	if !(signedWeight > (electionResult.TotalWeight * 2 / 3)) {
		fmt.Println("[bp] not enough signatures", "signedW="+strconv.Itoa(int(signedWeight)), "totalW="+strconv.Itoa(int(electionResult.TotalWeight*2/3)))
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
		"net_id":       bp.sconf.NetId(),
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

	if slot == nil {
		return "", errors.New("no slot found for height")
	}

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
		op := txRecord.Ops[0].Type
		transactions = append(transactions, vscBlocks.VscBlockTx{
			Id:   txRecord.Id,
			Op:   &op,
			Type: int(common.BlockTypeTransaction),
		})
	}

	// fmt.Println("Received msg to create block", msg)
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

	go func() {

	}()

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
				sigStr, ok := sig.Data["sig"].(string)
				if !ok {
					continue
				}
				account, ok := sig.Data["account"].(string)
				if !ok {
					continue
				}
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

				added, err := circuit.AddAndVerify(member, sigStr)

				fmt.Println("[bp] aggregating signature", sigStr, "from", account)
				fmt.Println("[bp] agg err", err)
				if added {
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
	// txRecords, _ := bp.TxDb.FindUnconfirmedTransactions(height)
	txRecords := bp.generateTransactions(height)

	if len(txRecords) > 0 {
		return true
	}

	if len(bp.StateEngine.LedgerState.Oplog) > 0 {
		return true
	}

	if len(bp.StateEngine.TxOutIds) > 0 {
		return true
	}

	return false
}

func (bp *BlockProducer) MakeOplog(bh uint64, session *datalayer.Session) *vscBlocks.VscBlockTx {
	// note: oplog is required, otherwise the StateEngine won't Flush()
	compileResult := bp.StateEngine.LedgerState.Compile(bh)
	opLog := make([]ledgerSystem.OpLogEvent, 0)

	if compileResult != nil {
		opLog = compileResult.OpLog
	}

	outputs := make([]stateEngine.OplogOutputEntry, 0)
	for _, txId := range bp.StateEngine.TxOutIds {
		output := bp.StateEngine.TxOutput[txId]
		// fmt.Println("Making oplog", output)

		LedgerIdx := make([]int, 0)
		for _, opId := range output.LedgerIds {
			idx := slices.IndexFunc(opLog, func(i ledgerSystem.OpLogEvent) bool {
				return i.Id == opId
			})
			LedgerIdx = append(LedgerIdx, idx)
		}

		outputs = append(outputs, stateEngine.OplogOutputEntry{
			Id:        txId,
			Ok:        output.Ok,
			LedgerIdx: LedgerIdx,
		})
	}

	//CLEAN Oplog
	oplogData := map[string]interface{}{
		"__t":     "vsc-oplog",
		"__v":     "0.1",
		"ledger":  opLog,
		"outputs": outputs,
	}

	// outputJson, _ := json.Marshal(oplogData)

	// fmt.Println("outputJson", string(outputJson))

	cborBytes, _ := common.EncodeDagCbor(oplogData)

	cid, _ := common.HashBytes(cborBytes, multicodec.DagCbor)

	session.Put(cborBytes, cid)

	return &vscBlocks.VscBlockTx{
		Id:   cid.String(),
		Type: int(common.BlockTypeOplog),
	}
}

func (bp *BlockProducer) MakeOutputs(session *datalayer.Session) []vscBlocks.VscBlockTx {

	contractOutputs := make([]vscBlocks.VscBlockTx, 0)
	for contractId, output := range bp.StateEngine.TempOutputs {
		var db datalayer.DataBin
		if output.Cid == "" {
			db = datalayer.NewDataBin(bp.Datalayer)
		} else {
			cidz := cid.MustParse(output.Cid)
			db = datalayer.NewDataBinFromCid(bp.Datalayer, cidz)
		}

		for key := range output.Deletions {
			db.Delete(key)
		}

		for key, value := range output.Cache {
			if output.Deletions[key] {
				continue
			}
			cidz, err := common.HashBytes(value, multicodec.Raw)
			if err != nil {
				continue
			}
			session.Put(value, cidz)

			//Required to prevent self blocking due to datalayer accessing the DS.
			//Temp fix, long term create a "databin" session that uses the underlying datalayer session
			bp.Datalayer.PutRaw(value, common_types.PutRawOptions{
				Codec: multicodec.Raw,
			})

			db.Set(key, cidz)
		}
		savedCid := db.Save()

		if len(bp.StateEngine.ContractResults[contractId]) == 0 {
			continue
		}

		// results := make([]struct {
		// 	Ret string `json:"ret" bson:"ret"`
		// 	Ok  bool   `json:"ok" bson:"ok"`
		// }, 0)
		results := make([]contracts.ContractOutputResult, 0)

		inputIds := make([]string, 0)

		for _, v := range bp.StateEngine.ContractResults[contractId] {
			var ret string
			if utf8.ValidString(v.Ret) {
				ret = v.Ret
			} else {
				ret = base64.RawStdEncoding.EncodeToString([]byte(v.Ret))
			}

			results = append(results, contracts.ContractOutputResult{
				Ret:    ret,
				Ok:     v.Success,
				Err:    v.Err,
				ErrMsg: v.ErrMsg,
				Logs:   v.Logs,
				TssOps: v.TssOps,
			})
			inputIds = append(inputIds, v.TxId)
		}

		outputObj := map[string]interface{}{
			"__t":          "vsc-output",
			"__v":          "0.1",
			"contract_id":  contractId,
			"metadata":     output.Metadata,
			"state_merkle": savedCid.String(),
			"inputs":       inputIds,
			"results":      results,
		}

		dagBytes, _ := common.EncodeDagCbor(outputObj)

		outputId, _ := cid.Prefix{
			Version:  1,
			Codec:    uint64(multicodec.DagCbor),
			MhType:   uint64(multicodec.Sha2_256),
			MhLength: -1,
		}.Sum(dagBytes)

		session.Put(dagBytes, outputId)

		contractOutputs = append(contractOutputs, vscBlocks.VscBlockTx{
			Id:   outputId.String(),
			Type: int(common.BlockTypeOutput),
		})
	}

	slices.SortFunc(contractOutputs, func(a, b vscBlocks.VscBlockTx) int {
		return strings.Compare(a.Id, b.Id)
	})

	return contractOutputs
}

func (bp *BlockProducer) MakeRcMap(session *datalayer.Session) *vscBlocks.VscBlockTx {
	rcMap := bp.StateEngine.RcMap

	var exists bool
	for key := range rcMap {
		if key != "" {
			exists = true
			continue
		}
	}

	if !exists {
		return nil
	}

	//CLEAN Oplog
	oplogData := map[string]interface{}{
		"__t":    "vsc-rc",
		"__v":    "0.1",
		"rc_map": rcMap,
	}

	cborBytes, _ := common.EncodeDagCbor(oplogData)

	cid, _ := common.HashBytes(cborBytes, multicodec.DagCbor)

	session.Put(cborBytes, cid)

	return &vscBlocks.VscBlockTx{
		Id:   cid.String(),
		Type: int(common.BlockTypeRcUpdate),
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
	bp.hiveConsumer.RegisterBlockTick("block-producer", bp.BlockTick, false)
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

func New(logger logger.Logger, p2p *libp2p.P2PServer, hiveConsumer *blockconsumer.HiveConsumer, se *stateEngine.StateEngine, conf common.IdentityConfig, sconf systemconfig.SystemConfig, hiveCreator hive.HiveTransactionCreator, da *datalayer.DataLayer, electionsDb elections.Elections, vscBlocks vscBlocks.VscBlocks, txDb transactions.Transactions, rcSystem *rcSystem.RcSystem, nonceDb nonces.Nonces) *BlockProducer {
	return &BlockProducer{
		log:          logger,
		sigChannels:  make(map[uint64]chan sigMsg),
		StateEngine:  se,
		hiveConsumer: hiveConsumer,
		config:       conf,
		sconf:        sconf,
		HiveCreator:  hiveCreator,
		p2p:          p2p,
		Datalayer:    da,
		electionsDb:  electionsDb,
		VscBlocks:    vscBlocks,
		TxDb:         txDb,
		rcSystem:     rcSystem,
		nonceDb:      nonceDb,
	}
}
