package contracts

import (
	"context"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type contracts struct {
	*db.Collection
}

func New(d *vsc.VscDb) Contracts {
	return &contracts{db.NewCollection(d.DbInstance, "contracts")}
}

func (e *contracts) Init() error {
	err := e.Collection.Init()
	if err != nil {
		return err
	}

	return nil
}

func (c *contracts) RegisterContract(contractId string, args SetContractArgs) {
	findQuery := bson.M{
		"id": contractId,
	}
	updateQuery := bson.M{
		"$set": bson.M{
			"code":            args.Code,
			"name":            args.Name,
			"description":     args.Description,
			"creator":         args.Creator,
			"owner":           args.Owner,
			"tx_id":           args.TxId,
			"creation_height": args.CreationHeight,
		},
	}
	opts := options.FindOneAndUpdate().SetUpsert(true)
	c.FindOneAndUpdate(context.Background(), findQuery, updateQuery, opts)
}

type contractHistory struct {
	*db.Collection
}

func (ch *contractHistory) IngestOutput() {

}
