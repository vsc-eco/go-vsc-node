package election_proposer

import (
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
	"vsc-node/modules/db/vsc/witnesses"
	libp2p "vsc-node/modules/p2p"

	"github.com/JustinKnueppel/go-result"
	"github.com/chebyrash/promise"
	"github.com/vsc-eco/hivego"
	"go.mongodb.org/mongo-driver/mongo"
)

type electionProposer struct {
	conf common.IdentityConfig

	p2p     *libp2p.P2PServer
	service libp2p.PubSubService[p2pMessage]

	witnesses witnesses.Witnesses
	elections elections.Elections

	da *datalayer.DataLayer

	txCreator hive.HiveTransactionCreator

	circuit dids.PartialBlsCircuit
}

type ElectionProposer = *electionProposer

var _ a.Plugin = &electionProposer{}

// TODO: Add a way to get the current consensus version
// TODO: Add a way to get the witness active score
func New(
	p2p *libp2p.P2PServer,
	witnesses witnesses.Witnesses,
	elections elections.Elections,
	da *datalayer.DataLayer,
	txCreator hive.HiveTransactionCreator,
	conf common.IdentityConfig,
) ElectionProposer {
	return &electionProposer{
		conf:      conf,
		p2p:       p2p,
		witnesses: witnesses,
		elections: elections,
		da:        da,
		txCreator: txCreator,
	}
}

// Init implements aggregate.Plugin.
func (e *electionProposer) Init() error {
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

func (e *electionProposer) GenerateElection() (elections.ElectionHeader, elections.ElectionData, error) {
	// TODO: get latest block
	blk := uint64(0)
	return e.GenerateElectionAtBlock(blk)
}

// Generates a raw election graph from local data
func (e *electionProposer) GenerateElectionAtBlock(blk uint64) (elections.ElectionHeader, elections.ElectionData, error) {
	witnesses, err := e.witnesses.GetWitnessesAtBlockHeight(blk)
	if err != nil {
		return elections.ElectionHeader{}, elections.ElectionData{}, err
	}
	electionResult, err := e.elections.GetElectionByHeight(blk - 1)
	if err != nil {
		return elections.ElectionHeader{}, elections.ElectionData{}, err
	}

	// TODO: Add a way to get the witness active score
	// const scoreChart = await this.self.witness.getWitnessActiveScore(blk)
	scoreChart := map[string]uint64{}

	return e.GenerateElectionWithWitnessesAndEpochAndConsensusVersionAndScoreChart(witnesses, electionResult.Epoch, electionResult.ProtocolVersion, scoreChart)
}

const DEFAULT_NEW_NODE_WEIGHT = uint64(10)
const MINIMUM_ELECTION_MEMBER_COUNT = int(8)

var REQUIRED_ELECTION_MEMBERS = []string{} // TODO: Set this to a list of required election members

const VSC_ELECTION_TX_ID = "vsc.election_result"

func (e *electionProposer) GenerateElectionWithWitnessesAndEpochAndConsensusVersionAndScoreChart(
	witnessList []witnesses.Witness,
	previousEpoch uint64,
	consensusVersion uint64,
	scoreChart map[string]uint64,
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

	optionalNodes := slices.DeleteFunc(witnessList, func(w witnesses.Witness) bool {
		return slices.Contains(REQUIRED_ELECTION_MEMBERS, w.Account)
	})

	totalOptionalWeight := utils.Sum(
		utils.Map(optionalNodes, func(w witnesses.Witness) uint64 {
			score, ok := scoreChart[w.Account]
			if ok {
				return score
			}

			return DEFAULT_NEW_NODE_WEIGHT
		}),
	)

	distWeight := uint64(math.Ceil((1 + float64(totalOptionalWeight)/2) / float64(len(REQUIRED_ELECTION_MEMBERS))))
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
			score, ok := scoreChart[member.Account]
			if ok {
				weights[i] = score
			} else {
				weights[i] = DEFAULT_NEW_NODE_WEIGHT
			}
		}
	}

	electionData := elections.ElectionData{}
	electionData.Epoch = previousEpoch + 1
	electionData.Members = members
	electionData.NetId = common.NETWORK_ID
	electionData.ProtocolVersion = consensusVersion
	electionData.Weights = weights
	cid, err := electionData.Cid()
	if err != nil {
		return elections.ElectionHeader{}, elections.ElectionData{}, err
	}

	electionHeader := elections.ElectionHeader{}
	electionHeader.Data = cid.String()
	electionHeader.Epoch = electionData.Epoch
	electionHeader.NetId = electionData.NetId

	return electionHeader, electionData, nil
}

type ElectionOptions struct {
	OverrideMinimumMemberCount int
}

func (e *electionProposer) HoldElection(blk uint64, options ...ElectionOptions) error {
	if e.circuit != nil {
		return errors.New("election already in progress")
	}

	electionResult, err := e.elections.GetElectionByHeight(blk - 1)
	firstElection := false
	if err == mongo.ErrNoDocuments {
		firstElection = true
	} else if err != nil {
		return err
	}

	electionHeader, electionData, err := e.GenerateElectionAtBlock(blk)
	if err != nil {
		return err
	}

	// console.log("electionData - holding election", electionData)

	var minimumMemberCount int
	if len(options) > 0 {
		minimumMemberCount = options[0].OverrideMinimumMemberCount
	} else {
		minimumMemberCount = MINIMUM_ELECTION_MEMBER_COUNT
	}

	if len(electionData.Members) < minimumMemberCount {
		return errors.New("Minimum network config not met for election. Skipping.")
	}

	cid, err := electionHeader.Cid()
	if err != nil {
		return err
	}

	var memberKeys []dids.BlsDID
	if firstElection {
		w, err := e.witnesses.GetWitnessesAtBlockHeight(blk)

		fmt.Println("GetWitnessesAtBlockHeight err", err)
		if err != nil {
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
		return err
	}

	e.circuit = circuit

	time.Sleep(20 * time.Second)

	finalCircuit, err := circuit.Finalize()
	if err != nil {
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

		_, err = e.txCreator.Broadcast(tx)

		if err != nil {
			return fmt.Errorf("failed to update account: %w", err)
		}
	}

	return nil
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
