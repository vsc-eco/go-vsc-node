package vsc

import (
	"context"
	a "vsc-node/modules/aggregate"
	"vsc-node/modules/db"

	"go.mongodb.org/mongo-driver/bson"
)

type VscDb struct {
	*db.DbInstance
}

var _ a.Plugin = &VscDb{}

func New(d db.Db, dbName ...string) *VscDb {
	name := "go-vsc"
	if len(dbName) > 0 && dbName[0] != "" {
		name = dbName[0]
	}

	return &VscDb{db.NewDbInstance(d, name)}
}

func (db *VscDb) Nuke() error {
	ctx := context.Background()

	colsNames, err := db.ListCollectionNames(ctx, bson.M{})
	if err != nil {
		return err
	}

	for _, colName := range colsNames {
		_, err := db.Collection(colName).DeleteMany(ctx, bson.M{})
		if err != nil {
			return err
		}
	}

	return nil
}
