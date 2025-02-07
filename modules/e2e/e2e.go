package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/hive"
	"vsc-node/lib/utils"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/witnesses"

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
	steps       []func()
	HiveCreator hive.HiveTransactionCreator
	Datalayer   *datalayer.DataLayer
	Witnesses   witnesses.Witnesses
}

func (e2e *E2ERunner) Init() error {
	return nil
}

func (e2e *E2ERunner) Start() *promise.Promise[any] {

	go func() {
		for _, step := range e2e.steps {
			step()
		}
	}()
	return utils.PromiseResolve[any](nil)
}

func (e2e *E2ERunner) Stop() error {
	return nil
}

func (e2e *E2ERunner) SetSteps(steps []func()) {
	e2e.steps = steps
}

func (e2e *E2ERunner) Wait(blocks uint64) func() {
	return func() {

	}
}

func (e2e *E2ERunner) WaitToStart() func() {
	return func() {

	}
}

func (e2e *E2ERunner) Sleep(seconds uint64) func() {
	return func() {
		time.Sleep(time.Duration(seconds) * time.Second)
	}
}

// Creates and broadcasts a mock election using predefined list of validator user names
func (e2e *E2ERunner) BroadcastMockElection(witnessListS []string) func() {
	return func() {
		da := e2e.Datalayer

		members := []elections.ElectionMember{}

		witnessList := []witnesses.Witness{}
		//TODO detect current height
		for _, wStr := range witnessListS {
			w, err := e2e.Witnesses.GetWitnessAtHeight(wStr, nil)
			fmt.Println("w, err", w, err)
			if err != nil {
				continue
			}
			witnessList = append(witnessList, *w)
		}
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
		}

		fmt.Println("len(members)", len(members))
		fmt.Println("members", members)
		if len(members) == 0 {
			panic("No members found")
		}

		electionData := elections.ElectionData{
			ElectionCommonInfo: elections.ElectionCommonInfo{
				Epoch: 0,
				NetId: common.NETWORK_ID,
			},
			ElectionDataInfo: elections.ElectionDataInfo{
				Weights:         []uint64{},
				Members:         members,
				ProtocolVersion: 0,
			},
		}

		cid, err := da.PutObject(electionData)

		if err != nil {
			return
		}

		electionHeader := map[string]interface{}{
			"epoch":  0,
			"data":   cid.String(),
			"net_id": common.NETWORK_ID,
		}

		bbyes, _ := json.Marshal(electionHeader)

		op := e2e.HiveCreator.CustomJson([]string{"e2e.mocks"}, []string{}, "vsc.election_result", string(bbyes))

		tx := e2e.HiveCreator.MakeTransaction([]hivego.HiveOperation{op})

		e2e.HiveCreator.Broadcast(tx)
	}
}

// Produces X number of mock blocks
func (e2e *E2ERunner) Produce(blocks uint64) func() {
	return func() {

	}
}
