package tss_db

import (
	"context"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type tssKeys struct {
	*db.Collection
}

func (tssKeys *tssKeys) InsertKey(id string, t TssKeyAlgo) error {
	opts := options.FindOneAndUpdate().SetUpsert(true)
	tssKeys.FindOneAndUpdate(context.Background(), bson.M{
		"id": id,
	}, bson.M{
		"$set": bson.M{
			"algo":   t,
			"status": "created",
		},
	}, opts)

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

func (tssKeys *tssKeys) SetKey(key TssKey) error {
	res := tssKeys.FindOneAndUpdate(context.Background(), bson.M{
		"id": key.Id,
	}, bson.M{
		"$set": bson.M{
			"status":         key.Status,
			"public_key":     key.PublicKey,
			"epoch":          key.Epoch,
			"created_height": key.CreatedHeight,
		},
	})

	return res.Err()
}

func (tssKeys *tssKeys) FindNewKeys(bh uint64) ([]TssKey, error) {
	findCursor, _ := tssKeys.Find(context.Background(), bson.M{
		"status": "created",
	})

	keys := make([]TssKey, 0)
	if findCursor.Next(context.Background()) {
		var k TssKey
		findCursor.Decode(&k)
		keys = append(keys, k)
	}
	return keys, nil
}

// Find keys with al ower
func (tssKeys *tssKeys) FindEpochKeys(epoch uint64) ([]TssKey, error) {
	findCursor, _ := tssKeys.Find(context.Background(), bson.M{
		"status": "active",
		"epoch": bson.M{
			"$lt": epoch,
		},
	})

	keys := make([]TssKey, 0)
	if findCursor.Next(context.Background()) {
		var k TssKey
		findCursor.Decode(&k)
		keys = append(keys, k)
	}
	return keys, nil
}

func NewKeys(d *vsc.VscDb) TssKeys {
	return &tssKeys{db.NewCollection(d.DbInstance, "tss_keys")}
}
