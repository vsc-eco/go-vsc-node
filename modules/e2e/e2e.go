package e2e

import (
	"context"
	"vsc-node/lib/utils"
	"vsc-node/modules/db/vsc"

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
