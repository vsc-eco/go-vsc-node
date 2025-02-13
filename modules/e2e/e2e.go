package e2e

import (
	"context"
	"time"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/hive"
	"vsc-node/lib/utils"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/witnesses"
	election_proposer "vsc-node/modules/election-proposer"
	"vsc-node/modules/vstream"

	a "vsc-node/modules/aggregate"

	"github.com/chebyrash/promise"
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

	BlockHeight uint64

	VStream    *vstream.VStream
	BlockEvent chan uint64
}

func (e2e *E2ERunner) Init() error {
	e2e.VStream.RegisterBlockTick("e2e.runner", e2e.BlockTick, true)
	return nil
}

func (e2e *E2ERunner) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		for _, step := range e2e.steps {
			err := step()
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

func (e2e *E2ERunner) Wait(blocks uint64) func() error {
	return func() error {

		bh := e2e.BlockHeight + blocks

		for {
			bhx := <-e2e.BlockEvent
			if bhx >= bh {
				break
			}
		}

		// for e2e.BlockHeight < bh {
		// 	time.Sleep(500 * time.Millisecond)
		// }
		return nil
	}
}

func (e2e *E2ERunner) WaitToStart() func() error {
	return func() error {
		//Wait till first block is produced
		<-e2e.BlockEvent
		return nil
	}
}

func (e2e *E2ERunner) Sleep(seconds uint64) func() error {
	return func() error {
		time.Sleep(time.Duration(seconds) * time.Second)
		return nil
	}
}

// Creates and broadcasts a mock election using predefined list of validator user names
func (e2e *E2ERunner) BroadcastMockElection(blk uint64) func() error {
	return func() error {
		return e2e.ElectionProposer.HoldElection(blk)
	}
}

func (e2e *E2ERunner) BlockTick(bh uint64) {
	e2e.BlockEvent <- bh
	e2e.BlockHeight = bh
}

// Produces X number of mock blocks
func (e2e *E2ERunner) Produce(blocks uint64) func() error {
	return func() error {
		return nil
	}
}
