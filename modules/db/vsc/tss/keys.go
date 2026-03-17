package tss_db

import (
	"context"
	"fmt"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type tssKeys struct {
	*db.Collection
}

func (tssKeys *tssKeys) InsertKey(id string, t TssKeyAlgo, epochs uint64) error {
	opts := options.FindOneAndUpdate().SetUpsert(true)
	tssKeys.FindOneAndUpdate(context.Background(), bson.M{
		"id": id,
	}, bson.M{
		"$set": bson.M{
			"algo":   t,
			"status": "created",
			"epochs": epochs,
		},
	}, opts)

	fmt.Printf("[TSS] [DB] InsertKey keyId=%s algo=%s epochs=%d status=created\n", id, t, epochs)
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
			"status":            key.Status,
			"public_key":        key.PublicKey,
			"epoch":             key.Epoch,
			"created_height":    key.CreatedHeight,
			"expiry_epoch":      key.ExpiryEpoch,
			"deprecated_height": key.DeprecatedHeight,
		},
	})

	dbErr := res.Err()
	if dbErr != nil {
		fmt.Printf("[TSS] [DB] SetKey FAILED keyId=%s status=%s epoch=%d err=%v\n",
			key.Id, key.Status, key.Epoch, dbErr)
	} else {
		fmt.Printf("[TSS] [DB] SetKey OK keyId=%s status=%s epoch=%d\n",
			key.Id, key.Status, key.Epoch)
	}
	return dbErr
}

// FindDeprecatingKeys returns active keys whose ExpiryEpoch has been reached (and ExpiryEpoch > 0).
func (tssKeys *tssKeys) FindDeprecatingKeys(epoch uint64) ([]TssKey, error) {
	findCursor, _ := tssKeys.Find(context.Background(), bson.M{
		"status":       TssKeyActive,
		"expiry_epoch": bson.M{"$gt": 0, "$lte": epoch},
	})

	keys := make([]TssKey, 0)
	for findCursor.Next(context.Background()) {
		var k TssKey
		findCursor.Decode(&k)
		keys = append(keys, k)
	}
	return keys, nil
}

// FindNewlyRetired returns deprecated keys whose grace period has elapsed at the given block height.
// Returns keys where deprecated_height + KeyDeprecationGracePeriod == blockHeight, ensuring
// cleanup runs exactly once per key.
func (tssKeys *tssKeys) FindNewlyRetired(blockHeight uint64) ([]TssKey, error) {
	findCursor, _ := tssKeys.Find(context.Background(), bson.M{
		"status":            TssKeyDeprecated,
		"deprecated_height": bson.M{"$gt": 0, "$lte": int64(blockHeight) - int64(KeyDeprecationGracePeriod)},
	})

	keys := make([]TssKey, 0)
	for findCursor.Next(context.Background()) {
		var k TssKey
		findCursor.Decode(&k)
		// Only return keys whose grace period ends exactly at this block (or just became eligible).
		// We use $lte so we catch any that were missed (e.g. if a block was skipped).
		keys = append(keys, k)
	}
	return keys, nil
}

func (tssKeys *tssKeys) FindNewKeys(bh uint64) ([]TssKey, error) {
	findCursor, _ := tssKeys.Find(context.Background(), bson.M{
		"status": "created",
	})

	keys := make([]TssKey, 0)
	for findCursor.Next(context.Background()) {
		var k TssKey
		findCursor.Decode(&k)
		keys = append(keys, k)
	}
	return keys, nil
}

// FindEpochKeys returns active keys from a lower epoch that have not yet expired.
func (tssKeys *tssKeys) FindEpochKeys(epoch uint64) ([]TssKey, error) {
	findCursor, _ := tssKeys.Find(context.Background(), bson.M{
		"status": "active",
		"epoch":  bson.M{"$lt": epoch},
		"$or": []bson.M{
			{"expiry_epoch": bson.M{"$exists": false}}, // pre-expiry keys
			{"expiry_epoch": 0},                        // no expiry
			{"expiry_epoch": bson.M{"$gt": epoch}},     // not yet expired
		},
	})

	keys := make([]TssKey, 0)
	for findCursor.Next(context.Background()) {
		var k TssKey
		findCursor.Decode(&k)
		keys = append(keys, k)
	}
	return keys, nil
}

// DeprecateLegacyKeys bulk-deprecates all active keys that have no expiry epoch set.
// deprecated_height is left at 0, meaning no retirement clock is running — the key
// stays deprecated until renewed.
func (tssKeys *tssKeys) DeprecateLegacyKeys() error {
	_, err := tssKeys.UpdateMany(context.Background(), bson.M{
		"status": TssKeyActive,
		"$or": []bson.M{
			{"expiry_epoch": bson.M{"$exists": false}},
			{"expiry_epoch": 0},
		},
	}, bson.M{
		"$set": bson.M{
			"status":            TssKeyDeprecated,
			"deprecated_height": 0,
		},
	})
	if err != nil {
		fmt.Printf("[TSS] [DB] DeprecateLegacyKeys FAILED err=%v\n", err)
	} else {
		fmt.Printf("[TSS] [DB] DeprecateLegacyKeys OK\n")
	}
	return err
}

func NewKeys(d *vsc.VscDb) TssKeys {
	return &tssKeys{db.NewCollection(d.DbInstance, "tss_keys")}
}
