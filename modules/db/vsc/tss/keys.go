package tss_db

import (
	"context"
	"fmt"
	"vsc-node/lib/vsclog"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var log = vsclog.Module("tss")

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

	log.Verbose("key inserted", "keyId", id, "algo", t, "epochs", epochs)
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
		log.Warn("SetKey failed", "keyId", key.Id, "status", key.Status, "epoch", key.Epoch, "err", dbErr)
	} else {
		log.Verbose("SetKey OK", "keyId", key.Id, "status", key.Status, "epoch", key.Epoch)
	}
	return dbErr
}

// SetSignatureVerified marks the key's BRK-2 check-signature as
// consensus-verified. A dedicated single-field $set (see the interface note):
// SetKey does not include signature_verified in its $set, so it neither
// persists nor clobbers this flag — the two updaters are independent. Idempotent
// (the flag is monotonic true).
func (tssKeys *tssKeys) SetSignatureVerified(id string) error {
	res := tssKeys.FindOneAndUpdate(context.Background(), bson.M{
		"id": id,
	}, bson.M{
		"$set": bson.M{
			"signature_verified": true,
		},
	})
	dbErr := res.Err()
	if dbErr != nil {
		log.Warn("SetSignatureVerified failed", "keyId", id, "err", dbErr)
	} else {
		log.Info("BRK-2 check-signature verified for key", "keyId", id)
	}
	return dbErr
}

// collectKeys drains a TssKey cursor fail-CLOSED, and is the only way these
// four finders should build their result.
//
// A23/18k: every one of them used to drain the cursor the same unsafe way.
// The Decode error was discarded, so a row that failed to decode was appended
// as a ZERO-VALUE TssKey -- an empty key id, silently entering key scheduling.
// cursor.Err() was never checked, so a cursor dying mid-iteration returned a
// SHORT list with a nil error. The cursor was never closed. And FindNewKeys and
// FindEpochKeys additionally dropped the Find error with `_`, which left
// findCursor nil on a query failure -- calling Next() on it panics -- and
// otherwise made a failed read indistinguishable from "no keys".
//
// "No keys" is not a safe default here: it makes this node schedule no keygen
// or reshare at a rotate block where its peers schedule them, so a momentary
// Mongo blip turned into a node that quietly stopped doing its share of key
// management. Every one of these paths must now report the failure instead.
func collectKeys(ctx context.Context, cur *mongo.Cursor, where string) ([]TssKey, error) {
	defer cur.Close(ctx)

	keys := make([]TssKey, 0)
	for cur.Next(ctx) {
		var k TssKey
		if err := cur.Decode(&k); err != nil {
			return nil, fmt.Errorf("%s: decoding key row %d: %w", where, len(keys), err)
		}
		keys = append(keys, k)
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("%s: cursor failed after %d rows: %w", where, len(keys), err)
	}
	return keys, nil
}

// FindDeprecatingKeys returns active keys whose ExpiryEpoch has been reached (and ExpiryEpoch > 0).
func (tssKeys *tssKeys) FindDeprecatingKeys(epoch uint64) ([]TssKey, error) {
	ctx := context.Background()
	findCursor, err := tssKeys.Find(ctx, bson.M{
		"status":       TssKeyActive,
		"expiry_epoch": bson.M{"$gt": 0, "$lte": epoch},
	})
	if err != nil {
		return nil, fmt.Errorf("FindDeprecatingKeys query failed: %w", err)
	}
	return collectKeys(ctx, findCursor, "FindDeprecatingKeys")
}

// FindNewlyRetired returns deprecated keys whose grace period has elapsed at the given block height.
// Returns keys where deprecated_height + KeyDeprecationGracePeriod == blockHeight, ensuring
// cleanup runs exactly once per key.
func (tssKeys *tssKeys) FindNewlyRetired(blockHeight uint64) ([]TssKey, error) {
	ctx := context.Background()
	// $lte, not ==, so a key whose exact grace-end block was skipped is still
	// caught on a later block.
	findCursor, err := tssKeys.Find(ctx, bson.M{
		"status":            TssKeyDeprecated,
		"deprecated_height": bson.M{"$gt": 0, "$lte": int64(blockHeight) - int64(KeyDeprecationGracePeriod)},
	})
	if err != nil {
		return nil, fmt.Errorf("FindNewlyRetired query failed: %w", err)
	}
	return collectKeys(ctx, findCursor, "FindNewlyRetired")
}

func (tssKeys *tssKeys) FindNewKeys(bh uint64) ([]TssKey, error) {
	ctx := context.Background()
	findCursor, err := tssKeys.Find(ctx, bson.M{
		"status": "created",
	})
	if err != nil {
		return nil, fmt.Errorf("FindNewKeys query failed: %w", err)
	}
	return collectKeys(ctx, findCursor, "FindNewKeys")
}

// FindEpochKeys returns active keys from a lower epoch that have not yet expired.
func (tssKeys *tssKeys) FindEpochKeys(epoch uint64) ([]TssKey, error) {
	ctx := context.Background()
	findCursor, err := tssKeys.Find(ctx, bson.M{
		"status": "active",
		"epoch":  bson.M{"$lt": epoch},
		"$or": []bson.M{
			{"expiry_epoch": bson.M{"$exists": false}}, // pre-expiry keys
			{"expiry_epoch": 0},                        // no expiry
			{"expiry_epoch": bson.M{"$gt": epoch}},     // not yet expired
		},
	})
	if err != nil {
		return nil, fmt.Errorf("FindEpochKeys query failed: %w", err)
	}
	return collectKeys(ctx, findCursor, "FindEpochKeys")
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
		log.Warn("DeprecateLegacyKeys failed", "err", err)
	} else {
		log.Info("DeprecateLegacyKeys completed")
	}
	return err
}

func NewKeys(d *vsc.VscDb) TssKeys {
	return &tssKeys{db.NewCollection(d.DbInstance, "tss_keys")}
}

// review2 HIGH #27: tss_keys is looked up by id (FindKey) and scanned by
// status / expiry_epoch with only the _id index.
func (e *tssKeys) Init() error {
	if err := e.Collection.Init(); err != nil {
		return err
	}
	for _, m := range []mongo.IndexModel{
		{Keys: bson.D{{Key: "id", Value: 1}}},
		// FindDeprecatingKeys runs per key-lifecycle pass ({status, expiry_epoch}).
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "expiry_epoch", Value: 1}}},
		// FindNewlyRetired runs every block ({status, deprecated_height}).
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "deprecated_height", Value: 1}}},
	} {
		if err := e.CreateIndexIfNotExist(m); err != nil {
			return err
		}
	}
	return nil
}
