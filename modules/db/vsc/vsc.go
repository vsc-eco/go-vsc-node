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

func New(d db.Db, suffix ...string) *VscDb {
	var dbPath string
	if len(suffix) > 0 && suffix[0] != "" {
		dbPath = "go-vsc-" + suffix[0]
	} else {
		dbPath = "go-vsc"
	}

	return &VscDb{db.NewDbInstance(d, dbPath)}
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
