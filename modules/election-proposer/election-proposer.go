package election_proposer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
	"vsc-node/lib/hive"
	"vsc-node/lib/utils"
	a "vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/elections"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	"vsc-node/modules/db/vsc/witnesses"
	blockconsumer "vsc-node/modules/hive/block-consumer"
	libp2p "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/JustinKnueppel/go-result"
	"github.com/chebyrash/promise"
	blsu "github.com/protolambda/bls12-381-util"
	"github.com/vsc-eco/hivego"
	"go.mongodb.org/mongo-driver/mongo"
)

// 6 hours of  hive blocks
var ELECTION_INTERVAL = uint64(6 * 60 * 20)

type electionProposer struct {
	conf  common.IdentityConfig
	sconf systemconfig.SystemConfig

	p2p     *libp2p.P2PServer
	service libp2p.PubSubService[p2pMessage]

	witnesses witnesses.Witnesses
	elections elections.Elections
	balanceDb ledgerDb.Balances
	vscBlocks vscBlocks.VscBlocks

	da *datalayer.DataLayer

	txCreator hive.HiveTransactionCreator

	hiveConsumer *blockconsumer.HiveConsumer
	se           *stateEngine.StateEngine
	bh           uint64
	headHeight   *uint64

	sigChannels map[uint64]chan *signResponse
	signingInfo *struct {
		epoch   uint64
		circuit *dids.PartialBlsCircuit
	}
}

type ElectionProposer = *electionProposer

var _ a.Plugin = &electionProposer{}

// TODO: Add a way to get the current consensus version
// TODO: Add a way to get the witness active score
func New(
	p2p *libp2p.P2PServer,
	witnesses witnesses.Witnesses,
	elections elections.Elections,
	vscBlocks vscBlocks.VscBlocks,
	balanceDb ledgerDb.Balances,
	da *datalayer.DataLayer,
	txCreator hive.HiveTransactionCreator,
	conf common.IdentityConfig,
	sconf systemconfig.SystemConfig,
	se *stateEngine.StateEngine,
	hiveConsumer *blockconsumer.HiveConsumer,
) ElectionProposer {
	return &electionProposer{
		conf:         conf,
		p2p:          p2p,
		witnesses:    witnesses,
		elections:    elections,
		vscBlocks:    vscBlocks,
		balanceDb:    balanceDb,
		da:           da,
		txCreator:    txCreator,
		sconf:        sconf,
		se:           se,
		hiveConsumer: hiveConsumer,
		sigChannels:  make(map[uint64]chan *signResponse),
	}
}

// Init implements aggregate.Plugin.
func (e *electionProposer) Init() error {
	e.hiveConsumer.RegisterBlockTick("election-proposer", e.blockTick, true)
	return nil
}

// Start implements aggregate.Plugin.
func (e *electionProposer) Start() *promise.Promise[any] {
	err := e.startP2P()
	if err != nil {
		return utils.PromiseReject[any](err)
	}
	return utils.PromiseResolve[any](nil)
}

// Stop implements aggregate.Plugin.
func (e *electionProposer) Stop() error {
	return e.stopP2P()
}

func (e *electionProposer) blockTick(bh uint64, headHeight *uint64) {
	e.bh = bh
	e.headHeight = headHeight

	// scoreMap, err := e.scoreMap()
	// fmt.Println(err, scoreMap.BannedNodes)
	if e.canHold() {

		slotInfo := stateEngine.CalculateSlotInfo(bh)

		schedule := e.se.GetSchedule(slotInfo.StartHeight)

		//Select current slot as per consensus algorithm
		var witnessSlot *stateEngine.WitnessSlot
		for _, slot := range schedule {
			if slot.SlotHeight == slotInfo.StartHeight {
				witnessSlot = &slot
				break
			}
		}

		if witnessSlot != nil {
			if witnessSlot.Account == e.conf.Get().HiveUsername && bh%common.CONSENSUS_SPECS.SlotLength == 0 {
				e.HoldElection(bh)
			}
		}
	}
}

func (e *electionProposer) canHold() bool {
	if e.bh%10 == 0 {
		// fmt.Println("caHold()", e.bh)
	}
	if e.headHeight == nil {
		return false
	}
	if e.bh < *e.headHeight-50 {
		return false
	}

	result, _ := e.elections.GetElectionByHeight(e.bh)

	// fmt.Println("Last check", result.BlockHeight < e.bh-ELECTION_INTERVAL)
	return result.BlockHeight < e.bh-ELECTION_INTERVAL
}

func (e *electionProposer) GenerateElection() (elections.ElectionHeader, elections.ElectionData, error) {
	// TODO: get latest block

	return e.GenerateElectionAtBlock(e.bh)
}

// Generates a raw election graph from local data
func (e *electionProposer) GenerateElectionAtBlock(blk uint64) (elections.ElectionHeader, elections.ElectionData, error) {
	witnesses, err := e.witnesses.GetWitnessesAtBlockHeight(blk)
	if err != nil {
		return elections.ElectionHeader{}, elections.ElectionData{}, err
	}
	electionResult, err := e.elections.GetElectionByHeight(blk - 1)
	if err != nil && err != mongo.ErrNoDocuments {
		return elections.ElectionHeader{}, elections.ElectionData{}, err
	}

	// TODO: Add a way to get the witness active score
	// const scoreChart = await this.self.witness.getWitnessActiveScore(blk)
	// scoreChart := map[string]uint64{}

	return e.GenerateFullElection(witnesses, electionResult.Epoch, electionResult.ProtocolVersion, blk)
}

const DEFAULT_NEW_NODE_WEIGHT = uint64(10)
const MINIMUM_ELECTION_MEMBER_COUNT = int(7)

var REQUIRED_ELECTION_MEMBERS = []string{
	// "vaultec.vsc",
} // TODO: Set this to a list of required election members

const VSC_ELECTION_TX_ID = "vsc.election_result"

func (e *electionProposer) GenerateFullElection(
	witnessList []witnesses.Witness,
	previousEpoch uint64,
	consensusVersion uint64,
	blockHeight uint64,
) (elections.ElectionHeader, elections.ElectionData, error) {
	witnessList = slices.DeleteFunc(witnessList, func(w witnesses.Witness) bool {
		return w.ProtocolVersion < consensusVersion
	})

	// ensure the list is in a deterministic order
	slices.SortFunc(
		witnessList,
		func(a witnesses.Witness, b witnesses.Witness) int {
			return strings.Compare(a.Account, b.Account)
		},
	)

	previousElection := e.elections.GetElection(previousEpoch)

	var etype string
	var firstElection bool
	if previousElection != nil {
		etype = previousElection.Type
	} else {
		etype = "initial"
		firstElection = true
	}

	stakedMap := map[string]uint64{}
	defaultWeightMap := map[string]uint64{}
	nodesWithStake := uint64(0)
	for _, w := range witnessList {
		// electionResult.
		// if etype == "initial" {
		// 	weightMap[w.Account] = 10
		// } else {
		// 	balRecord, _ := e.balanceDb.GetBalanceRecord("hive:"+w.Account, blockHeight)
		// 	weightMap[w.Account] = uint64(balRecord.HIVE_CONSENSUS)
		// }
		balRecord, err := e.balanceDb.GetBalanceRecord("hive:"+w.Account, blockHeight)
		if err != nil {
			return elections.ElectionHeader{}, elections.ElectionData{}, err
		}
		if balRecord != nil {
			if balRecord.HIVE_CONSENSUS >= e.sconf.ConsensusParams().MinStake {
				nodesWithStake++
				stakedMap[w.Account] = uint64(balRecord.HIVE_CONSENSUS)
			}
		}
		defaultWeightMap[w.Account] = DEFAULT_NEW_NODE_WEIGHT
	}

	var pType string
	weightMap := map[string]uint64{}
	if nodesWithStake >= uint64(MINIMUM_ELECTION_MEMBER_COUNT) || etype == "staked" {
		pType = "staked"
		weightMap = stakedMap
	} else {
		pType = "initial"
		weightMap = defaultWeightMap
	}

	witnessList = slices.DeleteFunc(witnessList, func(w witnesses.Witness) bool {
		weight, included := weightMap[w.Account]
		return !included || weight == 0
	})

	optionalNodes := slices.DeleteFunc(witnessList, func(w witnesses.Witness) bool {
		return slices.Contains(REQUIRED_ELECTION_MEMBERS, w.Account)
	})

	totalOptionalWeight := utils.Sum(
		utils.Map(optionalNodes, func(w witnesses.Witness) uint64 {
			return weightMap[w.Account]
		}),
	)

	distWeight := uint64(0)
	if len(REQUIRED_ELECTION_MEMBERS) > 0 {
		distWeight = uint64(math.Ceil((1 + float64(totalOptionalWeight)/2) / float64(len(REQUIRED_ELECTION_MEMBERS))))
	}

	// fmt.Println("witnessList", witnessList)
	members := utils.Map(witnessList, func(w witnesses.Witness) elections.ElectionMember {
		key, err := w.ConsensusKey()

		if err != nil {
			panic(err)
		}
		return elections.ElectionMember{
			Key:     key.String(),
			Account: w.Account,
		}
	})

	weights := make([]uint64, len(members))
	for i, member := range members {
		if slices.Contains(REQUIRED_ELECTION_MEMBERS, member.Account) {
			weights[i] = distWeight
		} else {
			weights[i] = weightMap[member.Account]
		}
	}

	electionData := elections.ElectionData{}

	if firstElection {
		electionData.Epoch = 0
	} else {
		electionData.Epoch = previousEpoch + 1
	}
	electionData.Members = members
	electionData.NetId = e.sconf.NetId()
	electionData.ProtocolVersion = consensusVersion
	electionData.Weights = weights
	electionData.Type = pType
	cid, err := electionData.Cid()
	if err != nil {
		return elections.ElectionHeader{}, elections.ElectionData{}, err
	}

	cborNode, _ := electionData.Node()
	ses := datalayer.NewSession(e.da)

	ses.Put(cborNode.RawData(), cid)
	ses.Commit()

	electionHeader := elections.ElectionHeader{}
	electionHeader.Data = cid.String()
	electionHeader.Epoch = electionData.Epoch
	electionHeader.NetId = electionData.NetId
	electionHeader.Type = electionData.Type

	return electionHeader, electionData, nil
}

type ElectionOptions struct {
	OverrideMinimumMemberCount int
}

func (ep *electionProposer) HoldElection(blk uint64, options ...ElectionOptions) error {
	if ep.signingInfo != nil {
		fmt.Println("election already in progress")
		return errors.New("election already in progress")
	}

	electionResult, err := ep.elections.GetElectionByHeight(blk - 1)
	firstElection := false
	if err == mongo.ErrNoDocuments {
		firstElection = true
	} else if err != nil {
		fmt.Println("election.err", err)
		return err
	}

	electionHeader, electionData, err := ep.GenerateElectionAtBlock(blk)

	if err != nil {
		fmt.Println("election.err", err)
		return err
	}

	if firstElection {
		electionResultJson := struct {
			elections.ElectionHeader
		}{
			electionHeader,
		}
		jsonBytes, err := json.Marshal(electionResultJson)
		if err != nil {
			fmt.Println("election.err", err)
			return err
		}

		op := ep.txCreator.CustomJson([]string{ep.conf.Get().HiveUsername}, []string{}, VSC_ELECTION_TX_ID, string(jsonBytes))

		tx := ep.txCreator.MakeTransaction([]hivego.HiveOperation{op})

		ep.txCreator.PopulateSigningProps(&tx, nil)

		sig, err := ep.txCreator.Sign(tx)
		if err != nil {
			fmt.Println("election.err", err)
			return fmt.Errorf("failed to update account: %w", err)
		}

		tx.AddSig(sig)

		txId, err := ep.txCreator.Broadcast(tx)
		if err != nil {
			return err
		}

		fmt.Println("Propose Election TxId", txId)

		return nil
	} else {

		// console.log("electionData - holding election", electionData)

		var minimumMemberCount int
		if len(options) > 0 {
			minimumMemberCount = options[0].OverrideMinimumMemberCount
		} else {
			minimumMemberCount = MINIMUM_ELECTION_MEMBER_COUNT
		}

		if len(electionData.Members) < minimumMemberCount {
			fmt.Println("election minimum member count not met", len(electionData.Members), minimumMemberCount)
			return errors.New("Minimum network config not met for election. Skipping.")
		}

		cid, err := electionHeader.Cid()
		if err != nil {
			fmt.Println("election.err", err)
			return err
		}

		var memberKeys []dids.BlsDID
		if firstElection {
			w, err := ep.witnesses.GetWitnessesAtBlockHeight(blk)

			fmt.Println("GetWitnessesAtBlockHeight err", err)
			if err != nil {
				fmt.Println("election.err", err)
				return err
			}
			res := resultJoin(utils.Map(w, func(w witnesses.Witness) result.Result[dids.BlsDID] {
				return resultWrap(w.ConsensusKey())
			})...)

			if res.IsErr() {
				return res.UnwrapErr()
			}
			memberKeys = res.Unwrap()
		} else {
			memberKeys = electionResult.MemberKeys()
		}

		circuit, err := dids.NewBlsCircuitGenerator(memberKeys).Generate(cid)
		if err != nil {
			fmt.Println("election.err", err)
			return err
		}

		ep.signingInfo = &struct {
			epoch   uint64
			circuit *dids.PartialBlsCircuit
		}{
			epoch:   electionHeader.Epoch,
			circuit: &circuit,
		}

		// sig, err := signCid(ep.conf, cid)

		// if err != nil {
		// 	return err
		// }

		signReq := signRequest{
			Epoch:       electionHeader.Epoch,
			BlockHeight: blk,
		}

		reqJson, _ := json.Marshal(signReq)
		go func() {
			time.Sleep(4 * time.Millisecond)
			ep.service.Send(p2pMessage{
				Type: "sign_request",
				Op:   "hold_election",
				Data: string(reqJson),
			})
		}()

		ep.sigChannels[ep.signingInfo.epoch] = make(chan *signResponse)

		fmt.Println("[ep] waiting for signatures", blk, electionHeader.Epoch)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		signedWeight, err := ep.waitForSigs(ctx, &electionResult)

		if err != nil {
			return err
		}

		fmt.Println("signedWeight", signedWeight)
		ep.signingInfo = nil

		finalCircuit, err := circuit.Finalize()
		if err != nil {
			fmt.Println("election.err", err)
			return err
		}

		bv := finalCircuit.RawBitVector()
		votedWeight := uint64(0)
		totalWeight := uint64(0)
		for i := 0; i < bv.BitLen(); i++ {
			if bv.Bit(i) == 1 {
				votedWeight += electionResult.Weights[i]
			}
			totalWeight += electionResult.Weights[i]
		}

		blocksSinceLastElection := blk
		if !firstElection {
			blocksSinceLastElection = blk - electionResult.BlockHeight
		}
		voteMajority := elections.MinimalRequiredElectionVotes(blocksSinceLastElection, totalWeight)

		if (votedWeight >= voteMajority) || firstElection {
			// Send out Election to Hive

			circuit, err := finalCircuit.Serialize()
			if err != nil {
				fmt.Println("election.err", err)
				return err
			}

			electionResultJson := struct {
				elections.ElectionHeader
				Signature dids.SerializedCircuit `json:"signature"`
			}{
				electionHeader,
				*circuit,
			}

			jsonBytes, err := json.Marshal(electionResultJson)
			if err != nil {
				return err
			}

			op := ep.txCreator.CustomJson([]string{ep.conf.Get().HiveUsername}, []string{}, VSC_ELECTION_TX_ID, string(jsonBytes))

			tx := ep.txCreator.MakeTransaction([]hivego.HiveOperation{op})

			ep.txCreator.PopulateSigningProps(&tx, nil)

			sig, err := ep.txCreator.Sign(tx)
			if err != nil {
				return fmt.Errorf("failed to update account: %w", err)
			}

			tx.AddSig(sig)

			_, err = ep.txCreator.Broadcast(tx)

			if err != nil {
				return fmt.Errorf("failed to update account: %w", err)
			}
		}
		return nil
	}
}

type ScoreMap struct {
	Map         map[string]uint64
	BannedNodes []string
	Members     []string
	Samples     uint64
}

func (ep *electionProposer) scoreMap() (ScoreMap, error) {
	const electionCount = 4

	elections := make([]elections.ElectionResult, electionCount)
	election, err := ep.elections.GetElectionByHeight(math.MaxInt64)
	if err != nil {
		return ScoreMap{}, err
	}

	elections = append(elections, election)

	for i := uint64(1); i < electionCount; i++ {

		prevElection := ep.elections.GetElection(election.Epoch - i)

		if prevElection == nil {
			break
		}
		elections = append(elections, *prevElection)

	}
	samples := uint64(0)
	witnessMap := map[string]bool{}
	scoreMap := map[string]uint64{}

	for _, election := range elections {
		blocks, err := ep.vscBlocks.GetBlocksByElection(election.Epoch)
		if err != nil {
			return ScoreMap{}, err
		}

		for _, block := range blocks {
			for _, member := range block.Signers {
				scoreMap[member] += 1
			}
		}
		for _, mbr := range election.Members {
			witnessMap[mbr.Account] = true
		}
		samples += uint64(len(blocks))
	}

	members := make([]string, 0, len(witnessMap))

	for member := range witnessMap {
		members = append(members, member)
	}

	bannedNodes := []string{}
	for _, member := range members {
		if scoreMap[member] < samples*75/100 {
			bannedNodes = append(bannedNodes, member)
		}
	}

	return ScoreMap{
		Map:         scoreMap,
		Members:     members,
		Samples:     samples,
		BannedNodes: bannedNodes,
	}, nil
}

func (ep *electionProposer) makeElection(blk uint64) (elections.ElectionHeader, error) {
	electionResult, err := ep.elections.GetElectionByHeight(blk - 1)

	if err != nil {
		return elections.ElectionHeader{}, err
	}

	if blk-electionResult.BlockHeight < ELECTION_INTERVAL {
		return elections.ElectionHeader{}, errors.New("next election not ready")
	}
	electionHeader, _, err := ep.GenerateElectionAtBlock(blk)

	return electionHeader, err
}

func (ep *electionProposer) waitForSigs(ctx context.Context, election *elections.ElectionResult) (uint64, error) {
	if ep.signingInfo == nil {
		return 0, errors.New("no block signing info")
	}
	// go func() {
	// 	time.Sleep(30 * time.Second)
	// 	if ep.signingInfo != nil {
	// 		ep.sigChannels[ep.signingInfo.epoch] <- nil
	// 	}
	// }()

	weightTotal := uint64(0)
	for _, weight := range election.Weights {
		weightTotal += weight
	}

	end := make(chan struct{})

	var err error
	var signedWeight uint64
	go func() {
		signedWeight := uint64(0)

		for signedWeight < (weightTotal * 8 / 10) {
			signResp := <-ep.sigChannels[ep.signingInfo.epoch]

			if signResp == nil {
				fmt.Println("[ep] timed out waiting")
				break
			}

			sigBytes, err := base64.URLEncoding.DecodeString(signResp.Sig)

			if err != nil {
				continue
			}

			sig := blsu.Signature{}
			sig96 := [96]byte{}
			copy(sig96[:], sigBytes[:])
			err = sig.Deserialize(&sig96)

			if err != nil {
				continue
			}

			sigStr := signResp.Sig
			account := signResp.Account

			var member dids.Member
			var index int
			for i, data := range election.Members {
				if data.Account == account {
					member = dids.BlsDID(data.Key)
					index = i
					break
				}
			}

			circuit := *ep.signingInfo.circuit

			added, err := circuit.AddAndVerify(member, sigStr)

			fmt.Println("[ep] aggregating signature", sigStr, "from", account)
			fmt.Println("[ep] agg err", err)
			if added {
				signedWeight += election.Weights[index]
			}
		}
		fmt.Println("Done waittt")
		// res = signedWeight
		end <- struct{}{}
	}()

	select {
	case <-ctx.Done():
		fmt.Println("[ep] collect sigs timeout")
		return signedWeight, nil // Return error if canceled
	case <-end:
		return signedWeight, err
	}
}

func resultWrap[T any](res T, err error) result.Result[T] {
	if err != nil {
		return result.Err[T](err)
	}
	return result.Ok(res)
}

func resultJoin[T any](results ...result.Result[T]) (res result.Result[[]T]) {
	for _, r := range results {
		if r.IsOk() {
			res = result.Ok(append(res.Unwrap(), r.Unwrap()))
		} else {
			return result.Map(r, func(T) []T {
				return nil
			})
		}
	}
	return res
}
