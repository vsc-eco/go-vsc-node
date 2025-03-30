package nonces

import (
	"context"
	"fmt"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type nonceDb struct {
	*db.Collection
}

func (n *nonceDb) GetNonce(account string) (NonceRecord, error) {
	nonceRecord := NonceRecord{}
	findResult := n.FindOne(context.Background(), bson.M{"account": account})

	err := findResult.Decode(&nonceRecord)

	if err != nil {
		return NonceRecord{}, err
	}

	return nonceRecord, nil
}

func (n *nonceDb) SetNonce(account string, nonce uint64) error {

	options := options.FindOneAndUpdate().SetUpsert(true)
	singleResult := n.FindOneAndUpdate(context.Background(), bson.M{
		"account": account,
	}, bson.M{
		"$set": bson.M{
			"nonce": nonce,
		},
	}, options)
	fmt.Println(singleResult)

	return nil
}

func (n *nonceDb) Init() error {
	err := n.Collection.Init()
	if err != nil {
		return err
	}

	indexModel := mongo.IndexModel{
		Keys:    bson.D{{Key: "account", Value: 1}}, // ascending order
		Options: options.Index().SetUnique(true),    // not unique
	}
	n.Collection.Collection.Indexes().CreateOne(context.Background(), indexModel)

	// // create index on block.block_number for faster queries
	// err = createIndexIfNotExist(n.Collection.Collection, indexModel)
	// if err != nil {
	// 	return fmt.Errorf("failed to create index: %w", err)
	// }

	return nil
}

func New(d *vsc.VscDb) Nonces {
	return &nonceDb{db.NewCollection(d.DbInstance, "nonces")}
}
