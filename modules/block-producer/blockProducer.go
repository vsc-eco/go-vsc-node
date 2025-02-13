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
	a "vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	libp2p "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	"vsc-node/modules/vstream"

	"github.com/chebyrash/promise"
	"github.com/ipfs/go-cid"
	blsu "github.com/protolambda/bls12-381-util"
	"github.com/vsc-eco/hivego"
)

var CONSENSUS_SPECS = common.CONSENSUS_SPECS

type BlockProducer struct {
	a.Plugin

	config      *common.IdentityConfig
	StateEngine *stateEngine.StateEngine
	VStream     *vstream.VStream
	HiveCreator hive.HiveTransactionCreator
	Datalayer   *datalayer.DataLayer

	p2p     *libp2p.P2PServer
	service libp2p.PubSubService[p2pMessage]

	sigChannels  map[uint64]chan p2pMessage
	blockSigning *signingInfo
	bh           uint64

	electionsDb elections.Elections
}

type signingInfo struct {
	cid        cid.Cid
	slotHeight uint64
	circuit    *dids.PartialBlsCircuit
}

func (bp *BlockProducer) BlockTick(bh uint64) {
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
			if witnessSlot.SlotHeight == 20 {
				bp.ProduceBlock(witnessSlot.SlotHeight)
			}
		}
	}
}

// This function should generate a deterministically generated block
// In the future we should apply protocol versioning to this
func (bp *BlockProducer) GenerateBlock(slotHeight uint64) (map[string]interface{}, error) {
	blockData := map[string]interface{}{
		"txs": []struct {
			Id   string `json:"id"`
			Op   string `json:"op"`
			Type int    `json:"type"`
		}{},
		"headers": map[string]interface{}{
			"prevb": nil,
			"br":    [2]uint64{0, slotHeight},
		},
	}

	cid, _ := bp.Datalayer.PutObject(blockData)

	signedBlock := map[string]interface{}{
		"__t": "vsc-bh",
		"__v": "0.1",

		"headers": map[string]interface{}{
			"br":    [2]int{0, int(slotHeight)},
			"prevb": nil,
		},

		"merkle_root": nil,
		"block":       cid.String(),
	}

	return signedBlock, nil
}

func (bp *BlockProducer) ProduceBlock(bh uint64) {
	//For right now we will just produce a blank
	//This will allow us to test the e2e parsing

	signedBlock, _ := bp.GenerateBlock(bh)

	cid, _ := bp.Datalayer.HashObject(signedBlock)

	electionResult, err := bp.electionsDb.GetElectionByHeight(bh)

	if err != nil || electionResult == nil {
		return
	}

	circuit, err := dids.NewBlsCircuitGenerator(electionResult.MemberKeys()).Generate(*cid)

	go func() {
		time.Sleep(4 * time.Millisecond)
		bp.service.Send(p2pMessage{
			Type:       "block",
			SlotHeight: bh,
			Data: map[string]interface{}{
				"producer": bp.config.Config.Get().HiveUsername,
			},
		})
	}()

	bp.sigChannels[bh] = make(chan p2pMessage)
	bp.blockSigning = &signingInfo{
		cid:        *cid,
		slotHeight: bh,
		circuit:    &circuit,
	}
	ctx, _ := context.WithTimeout(context.Background(), 20*time.Second)
	signedWeight, err := bp.waitForSigs(ctx, electionResult)

	fmt.Println("signedWeight", signedWeight)

	finalCircuit, _ := circuit.Finalize()

	serialized, _ := finalCircuit.Serialize()

	signedBlock["signature"] = serialized

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
	fmt.Println(msg.Data)
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
		for bp.bh > msg.SlotHeight {
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

	blockHeader, err := bp.GenerateBlock(msg.SlotHeight)

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

	weightTotal := uint64(0)
	for _, weight := range election.Weights {
		weightTotal += weight
	}

	select {
	case <-ctx.Done():
		return 0, ctx.Err() // Return error if canceled
	default:
		signedWeight := uint64(0)

		fmt.Println("[bp] default action")
		// Perform the operation
		if bp.blockSigning != nil {
			// slotHeight := bp.blockSigning.slotHeight

			fmt.Println(signedWeight, weightTotal)
			for signedWeight < (weightTotal * 8 / 10) {
				sig := <-bp.sigChannels[bp.blockSigning.slotHeight]

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

				fmt.Println("circuit.AddAndVerify err", err)
				if err == nil {
					signedWeight += election.Weights[index]
				}
			}
			fmt.Println("Done waittt")
		}
		fmt.Println("Returning nil")
		return signedWeight, nil
	}
}

func (bp *BlockProducer) MakeOplog() {
	// fmt.Println("Making the oplog")

	compileResult := bp.StateEngine.LedgerExecutor.Compile()

	oplogData := map[string]interface{}{
		"__t":   "vsc-oplog",
		"__v":   "0.1",
		"oplog": compileResult,
	}
	fmt.Println("oplogData", oplogData)

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

func New(p2p *libp2p.P2PServer, vstream *vstream.VStream, se *stateEngine.StateEngine, conf *common.IdentityConfig, hiveCreator hive.HiveTransactionCreator, da *datalayer.DataLayer, electionsDb elections.Elections) *BlockProducer {
	return &BlockProducer{
		sigChannels: make(map[uint64]chan p2pMessage),
		StateEngine: se,
		VStream:     vstream,
		config:      conf,
		HiveCreator: hiveCreator,
		p2p:         p2p,
		Datalayer:   da,
		electionsDb: electionsDb,
	}
}
