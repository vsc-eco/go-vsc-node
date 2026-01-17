package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/hive"
	"vsc-node/lib/utils"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/transactions"
	"vsc-node/modules/db/vsc/witnesses"
	election_proposer "vsc-node/modules/election-proposer"
	blockconsumer "vsc-node/modules/hive/block-consumer"
	libp2p "vsc-node/modules/p2p"

	a "vsc-node/modules/aggregate"

	"github.com/chebyrash/promise"
	"github.com/vsc-eco/hivego"
	"go.mongodb.org/mongo-driver/bson"
)

type TestFinalizer struct {
}

// func (tf *TestFinalizer) Finish() promise.Promise {
// 	return promise.New(func(resolve func(interface{}), reject func(error)) {
// 		resolve(nil)
// 	})
// }

func NukeDb(db *vsc.VscDb) {
	ctx := context.Background()

	colsNames, _ := db.ListCollectionNames(ctx, bson.M{})
	for _, colName := range colsNames {
		db.Collection(colName).DeleteMany(ctx, bson.M{})
	}
}

type DbNuker struct {
	a.Plugin
	Db *vsc.VscDb
}

func (dn *DbNuker) Init() error {
	ctx := context.Background()

	colsNames, _ := dn.Db.ListCollectionNames(ctx, bson.M{})
	for _, colName := range colsNames {
		dn.Db.Collection(colName).DeleteMany(ctx, bson.M{})
	}

	return nil
}

func (db *DbNuker) Start() *promise.Promise[any] {
	return utils.PromiseResolve[any](nil)
}
func (db *DbNuker) Stop() error {
	return nil
}

func NewDbNuker(db *vsc.VscDb) *DbNuker {
	return &DbNuker{
		Db: db,
	}
}

var _ a.Plugin = &DbNuker{}

type E2ERunner struct {
	steps            []func() error
	HiveCreator      hive.HiveTransactionCreator
	Datalayer        *datalayer.DataLayer
	Witnesses        witnesses.Witnesses
	ElectionProposer election_proposer.ElectionProposer
	P2pService       *libp2p.P2PServer
	IdentityConfig   common.IdentityConfig
	SystemConfig     systemconfig.SystemConfig
	TxDb             transactions.Transactions

	BlockHeight uint64

	HiveConsumer *blockconsumer.HiveConsumer
	BlockEvent   chan uint64
}

func (e2e *E2ERunner) Init() error {
	e2e.HiveConsumer.RegisterBlockTick("e2e.runner", e2e.BlockTick, true)
	return nil
}

func (e2e *E2ERunner) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		defer func() {
			if r := recover(); r != nil {
				reject(fmt.Errorf("recovered from panic: %v", r))
			}
		}()
		for idx, step := range e2e.steps {
			fmt.Println("Running step " + strconv.Itoa(idx))
			err := step()

			fmt.Println("Error in step: "+strconv.Itoa(idx), err)
			if err != nil {
				reject(err)
				return
			}
		}
		resolve(nil)
	})
}

func (e2e *E2ERunner) Stop() error {
	return nil
}

func (e2e *E2ERunner) SetSteps(steps []func() error) {
	e2e.steps = steps
}

func (e2e *E2ERunner) Wait(blocks uint64) Step {
	return Step{
		Name: "Wait for Blocks",
		TestFunc: func(ctx StepCtx) (EvaluateFunc, error) {

			// fmt.Println("e2e.BlockHeight", e2e.BlockHeight)

			bh := e2e.BlockHeight + blocks

			for {
				bhx := <-e2e.BlockEvent
				// fmt.Println("e2e.Wait Waiting for", bhx)
				if bhx >= bh {
					break
				}
			}

			// for e2e.BlockHeight < bh {
			// 	time.Sleep(500 * time.Millisecond)
			// }
			return nil, nil
		},
	}
}

func (e2e *E2ERunner) WaitToStart() Step {
	return Step{
		Name: "Wait To Start",
		TestFunc: func(ctx StepCtx) (EvaluateFunc, error) {

			//Wait till first block is produced
			<-e2e.BlockEvent
			return nil, nil
		},
	}
}

func (e2e *E2ERunner) Sleep(seconds uint64) func() error {
	return func() error {
		time.Sleep(time.Duration(seconds) * time.Second)
		return nil
	}
}

// Creates and broadcasts a mock election using predefined list of validator user names
func (e2e *E2ERunner) BroadcastElection() Step {
	return Step{
		Name: "Broadcast Election",
		TestFunc: func(ctx StepCtx) (EvaluateFunc, error) {
			witnessListS := ctx.Container.nodeNames

			da := e2e.Datalayer
			members := []elections.ElectionMember{}
			witnessList := []witnesses.Witness{}
			//TODO detect current height
			for _, wStr := range witnessListS {
				w, err := e2e.Witnesses.GetWitnessAtHeight(wStr, nil)
				if err != nil {
					continue
				}
				witnessList = append(witnessList, *w)
			}

			var weights []uint64
			//Finish this when I can!
			for _, w := range witnessList {
				var key string
				for _, dk := range w.DidKeys {
					if dk.Type == "consensus" {
						key = dk.Key
						break
					}
				}
				if key == "" {
					//Don't do witness
					continue
				}
				members = append(members, elections.ElectionMember{
					Key:     key,
					Account: w.Account,
				})
				weights = append(weights, 10)
			}

			if len(members) == 0 {
				panic("No members found")
			}
			electionData := elections.ElectionData{
				ElectionCommonInfo: elections.ElectionCommonInfo{
					Epoch: 0,
					NetId: e2e.SystemConfig.NetId(),
					Type:  "initial",
				},
				ElectionDataInfo: elections.ElectionDataInfo{
					Weights:         weights,
					Members:         members,
					ProtocolVersion: 0,
				},
			}
			cid, err := da.PutObject(electionData)
			if err != nil {
				return nil, err
			}
			electionHeader := map[string]interface{}{
				"epoch":  0,
				"data":   cid.String(),
				"net_id": e2e.SystemConfig.NetId(),
			}
			bbyes, _ := json.Marshal(electionHeader)
			op := e2e.HiveCreator.CustomJson([]string{"e2e.mocks"}, []string{}, "vsc.election_result", string(bbyes))
			tx := e2e.HiveCreator.MakeTransaction([]hivego.HiveOperation{op})
			e2e.HiveCreator.Broadcast(tx)

			return nil, nil
		},
	}
}

func (e2e *E2ERunner) DupElection(delay ...time.Duration) Step {
	return Step{
		Name: "Broadcast Election",
		TestFunc: func(ctx StepCtx) (EvaluateFunc, error) {
			if len(delay) > 0 {
				fmt.Println("Sleeping broadcast election")
				time.Sleep(delay[0])
			}
			electionResult, err := ctx.Container.runningNodes[0].electionDb.GetElectionByHeight(500_000_000)

			fmt.Println("DupElection", err)
			electionResult.BlockHeight = uint64(ctx.Container.runningNodes[0].StateEngine.BlockHeight + 20)

			for _, n := range ctx.Container.runningNodes {
				n.electionDb.StoreElection(elections.ElectionResult{
					ElectionCommonInfo: elections.ElectionCommonInfo{
						Epoch: electionResult.Epoch + 1,
						NetId: "vsc-mainnet",
						Type:  "initial",
					},
					ElectionHeaderInfo: elections.ElectionHeaderInfo{
						Data: electionResult.Data,
					},
					ElectionDataInfo: elections.ElectionDataInfo{
						Members:         electionResult.Members,
						Weights:         electionResult.Weights,
						ProtocolVersion: electionResult.ProtocolVersion,
					},
					TotalWeight: electionResult.TotalWeight,
					BlockHeight: electionResult.BlockHeight,
					Proposer:    "mockup",
				})

			}
			return nil, nil
		},
	}

}

func (e2e *E2ERunner) BlockTick(bh uint64, headHeight *uint64) {
	e2e.BlockEvent <- bh
	e2e.BlockHeight = bh
}

// Produces X number of mock blocks
func (e2e *E2ERunner) Produce(blocks uint64) func() error {
	return func() error {
		return nil
	}
}
