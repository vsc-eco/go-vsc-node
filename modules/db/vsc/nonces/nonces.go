package nonces

import (
	"context"
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
	findResult := n.FindOne(context.Background(), bson.M{"_id": account})

	err := findResult.Decode(&nonceRecord)

	if err != nil {
		return NonceRecord{}, err
	}

	return nonceRecord, nil
}

func (n *nonceDb) SetNonce(account string, nonce uint64) error {

	options := options.FindOneAndUpdate().SetUpsert(true)
	n.FindOneAndUpdate(context.Background(), bson.M{
		"_id": account,
	}, bson.M{
		"$set": bson.M{
			"nonce": nonce,
		},
	}, options)

	return nil
}

func (n *nonceDb) BulkSetNonces(ctx context.Context, updates map[string]uint64) error {
	if len(updates) == 0 {
		return nil
	}
	models := make([]mongo.WriteModel, 0, len(updates))
	for account, nonce := range updates {
		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"_id": account}).
			SetUpdate(bson.M{"$set": bson.M{"nonce": nonce}}).
			SetUpsert(true))
	}
	_, err := n.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
	return err
}

func (n *nonceDb) Init() error {
	return n.Collection.Init()
}

func New(d *vsc.VscDb) Nonces {
	return &nonceDb{db.NewCollection(d.DbInstance, "nonces")}
}
