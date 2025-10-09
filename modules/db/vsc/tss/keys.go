package tss_db

import (
	"context"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
)

type tssKeys struct {
	*db.Collection
}

func (tssKeys *tssKeys) InsertKey(id string, t TssKeyType, owner string) error {
	tssKeys.FindOneAndUpdate(context.Background(), bson.M{
		"id": id,
	}, bson.M{
		"type":  t,
		"owner": owner,
	})
	return nil
}

func (tssKeys *tssKeys) FindKey(id string) (TssKey, error) {
	ctx := context.Background()
	result := tssKeys.FindOne(ctx, bson.M{
		"id": id,
	})

	if result.Err() != nil {
		return TssKey{}, result.Err()
	}

	tssKey := TssKey{}
	err := result.Decode(&tssKey)

	if err != nil {
		return TssKey{}, nil
	}

	return tssKey, nil
}

func (tssKeys) SetKey(id string, publicKey string) error {

	return nil
}

func NewKeys(d *vsc.VscDb) TssKeys {
	return &tssKeys{db.NewCollection(d.DbInstance, "tss_keys")}
}
