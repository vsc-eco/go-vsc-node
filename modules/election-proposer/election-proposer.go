package election_proposer

import (
	"context"
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
	"vsc-node/modules/db/vsc/elections"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/witnesses"
	libp2p "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	"vsc-node/modules/vstream"

	"github.com/JustinKnueppel/go-result"
	"github.com/chebyrash/promise"
	"github.com/vsc-eco/hivego"
	"go.mongodb.org/mongo-driver/mongo"
)

// 6 hours of  hive blocks
var ELECTION_INTERVAL = uint64(6 * 60 * 20)

type electionProposer struct {
	conf common.IdentityConfig

	p2p     *libp2p.P2PServer
	service libp2p.PubSubService[p2pMessage]

	witnesses witnesses.Witnesses
	elections elections.Elections
	balanceDb ledgerDb.Balances

	da *datalayer.DataLayer

	txCreator hive.HiveTransactionCreator

	circuit    dids.PartialBlsCircuit
	vstream    *vstream.VStream
	se         *stateEngine.StateEngine
	bh         uint64
	headHeight *uint64

	sigChannels map[uint64]chan p2pMessage
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
	balanceDb ledgerDb.Balances,
	da *datalayer.DataLayer,
	txCreator hive.HiveTransactionCreator,
	conf common.IdentityConfig,
	se *stateEngine.StateEngine,
	vstream *vstream.VStream,
) ElectionProposer {
	return &electionProposer{
		conf:        conf,
		p2p:         p2p,
		witnesses:   witnesses,
		elections:   elections,
		balanceDb:   balanceDb,
		da:          da,
		txCreator:   txCreator,
		se:          se,
		vstream:     vstream,
		sigChannels: make(map[uint64]chan p2pMessage),
	}
}

// Init implements aggregate.Plugin.
func (e *electionProposer) Init() error {
	e.vstream.RegisterBlockTick("election-proposer", e.blockTick, false)
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

	if e.canHold() {
		// fmt.Println("canHold", e.canHold())

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

		if e.circuit == nil {
			fmt.Println("Holding new election!")
			err := e.HoldElection(witnessSlot.SlotHeight)
			fmt.Println("HoldElection err", err)
			if err != nil {
				// panic(err)
			}
		}

		if witnessSlot != nil {
			if witnessSlot.Account == e.conf.Get().HiveUsername && bh%common.CONSENSUS_SPECS.SlotLength == 0 {
			}
		}
	}
}

func (e *electionProposer) canHold() bool {
	if e.headHeight == nil {
		return false
	}
	if e.bh < *e.headHeight-50 {
		return false
	}

	result, _ := e.elections.GetElectionByHeight(e.bh)

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
			if balRecord.HIVE_CONSENSUS > common.CONSENSUS_STAKE_MIN {
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
	electionData.NetId = common.NETWORK_ID
	electionData.ProtocolVersion = consensusVersion
	electionData.Weights = weights
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
	electionHeader.Type = pType

	return electionHeader, electionData, nil
}

type ElectionOptions struct {
	OverrideMinimumMemberCount int
}

func (e *electionProposer) HoldElection(blk uint64, options ...ElectionOptions) error {
	if e.circuit != nil {
		fmt.Println("election already in progress")
		return errors.New("election already in progress")
	}

	electionResult, err := e.elections.GetElectionByHeight(blk - 1)
	firstElection := false
	if err == mongo.ErrNoDocuments {
		firstElection = true
	} else if err != nil {
		fmt.Println("election.err", err)
		return err
	}

	electionHeader, electionData, err := e.GenerateElectionAtBlock(blk)

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

		op := e.txCreator.CustomJson([]string{e.conf.Get().HiveUsername}, []string{}, VSC_ELECTION_TX_ID, string(jsonBytes))

		tx := e.txCreator.MakeTransaction([]hivego.HiveOperation{op})

		e.txCreator.PopulateSigningProps(&tx, nil)

		sig, err := e.txCreator.Sign(tx)
		if err != nil {
			fmt.Println("election.err", err)
			return fmt.Errorf("failed to update account: %w", err)
		}

		tx.AddSig(sig)

		// txId, err := e.txCreator.Broadcast(tx)
		// if err != nil {
		// 	return err
		// }

		// fmt.Println("Propose Election TxId", txId)

		return nil
	}

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
		w, err := e.witnesses.GetWitnessesAtBlockHeight(blk)

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

	e.circuit = circuit

	sig, err := signCid(e.conf, cid)

	fmt.Println("election.sig", sig, err)
	if err != nil {
		return err
	}

	signReq := signRequest{
		Epoch:       electionHeader.Epoch,
		BlockHeight: blk,
	}
	reqJson, _ := json.Marshal(signReq)
	err = e.service.Send(p2pMessage{
		Type: "hold_election",
		Data: string(reqJson),
	})
	fmt.Println("SEnding error", err)
	if err != nil {
		fmt.Println("election.err", err)
		return err
	}

	for i := 0; i < 20; i++ {
		time.Sleep(time.Second)
		// TODO check if necessary voting weight is achieved, and break early
	}

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

		op := e.txCreator.CustomJson([]string{e.conf.Get().HiveUsername}, []string{}, VSC_ELECTION_TX_ID, string(jsonBytes))

		tx := e.txCreator.MakeTransaction([]hivego.HiveOperation{op})

		e.txCreator.PopulateSigningProps(&tx, nil)

		sig, err := e.txCreator.Sign(tx)
		if err != nil {
			return fmt.Errorf("failed to update account: %w", err)
		}

		tx.AddSig(sig)

		// _, err = e.txCreator.Broadcast(tx)

		// if err != nil {
		// 	return fmt.Errorf("failed to update account: %w", err)
		// }
	}

	return nil
}

func (ep *electionProposer) waitForSigs(ctx context.Context, election *elections.ElectionResult) (uint64, error) {

	if ep.signingInfo == nil {
		return 0, errors.New("no block signing info")
	}
	go func() {
		time.Sleep(12 * time.Second)
		// ep.sigChannels[ep.signingInfo.epoch] <- sigMsg{
		// 	Type: "end",
		// }
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
			msg := <-ep.sigChannels[ep.signingInfo.epoch]

			if msg.Type == "sig" {
				// sig := msg.Msg
				// sigStr := sig.Data["sig"].(string)
				// account := sig.Data["account"].(string)

				// var member dids.Member
				// var index int
				// for i, data := range election.Members {
				// 	if data.Account == account {
				// 		member = dids.BlsDID(data.Key)
				// 		index = i
				// 		break
				// 	}
				// }

				// circuit := *bp.blockSigning.circuit

				// err := circuit.AddAndVerify(member, sigStr)

				// fmt.Println("[bp] aggregating signature", sigStr, "from", account)
				// fmt.Println("[bp] agg err", err)
				// if err == nil {
				// 	signedWeight += election.Weights[index]
				// }
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

// {drain} :=  this.self.p2pService.multicastChannel.call("hold_election", {
//     mode: "stream",
//     payload: {
//         block_height: blk
//     },
//     streamTimeout: 20_000
// })

//     let votedWeight = 0;
//     let totalWeight = 0;
//     if(electionResult?.weights) {
//         //Vote based off weight
//         for(let key of pubKeys) {
//             member := members.find(e => e.key === key)
//             if(member) {
//                 votedWeight += electionResult.weights[members.indexOf(member)]
//             }
//         }
//         totalWeight = electionResult.weight_total
//     } else {
//         //Vote based off signer count
//         votedWeight = pubKeys.length
//         totalWeight = members.length
//     }

//     voteMajority := minimalRequiredElectionVotes(electionHeader.epoch === 0 || !electionResult ? blk : blk - electionResult.block_height, totalWeight); //Hardcode for 0 until the future

//     if(((votedWeight >= voteMajority) || electionHeader.epoch === 0)) {
//         //Must be valid

//         if (!process.env.HIVE_ACCOUNT) {
//             throw new Error("no hive account... will not broadcast election result")
//         }

//         if (!process.env.HIVE_ACCOUNT_ACTIVE) {
//             throw new Error("no hive account active key... will not broadcast election result")
//         }

//          HiveClient.broadcast.json({
//             id: "vsc.election_result",
//             required_auths: [process.env.HIVE_ACCOUNT],
//             required_posting_auths: [],
//             json: JSON.stringify({
//                 ...electionHeader,
//                 signature: circuit.serialize(members.map(e => e.key))
//             })
//         }, PrivateKey.fromString(process.env.HIVE_ACCOUNT_ACTIVE))
//     }
// }

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
