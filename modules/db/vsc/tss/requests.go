package tss_db

import (
	"context"
	"fmt"
	"time"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type tssRequests struct {
	*db.Collection
}

// Init runs the base collection init, performs the one-time backoff migration,
// and creates the composite index used by FindUnsignedRequests.
//
// The migration resets any status=failed document back to unsigned with a
// reset backoff state. This revives legacy documents that were hard-failed
// by the old 1000-block expiry so they get fresh retries under the new
// backoff system. Migration is idempotent: on subsequent boots the filter
// matches no rows.
func (tssReqs *tssRequests) Init() error {
	if err := tssReqs.Collection.Init(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Migration: failed → unsigned, reset backoff, clear sig.
	// Uses an aggregation pipeline so next_attempt_height can reference the
	// document's own created_height field.
	if _, err := tssReqs.UpdateMany(ctx, bson.M{
		"status": string(SignFailed),
	}, mongo.Pipeline{
		{{Key: "$set", Value: bson.M{
			"status":              string(SignPending),
			"attempt_count":       0,
			"next_attempt_height": "$created_height",
			"sig":                 "",
		}}},
	}); err != nil {
		return fmt.Errorf("failed→unsigned migration: %w", err)
	}

	// Backfill next_attempt_height/attempt_count on any existing docs that
	// predate the new fields. These default to the doc's created_height.
	if _, err := tssReqs.UpdateMany(ctx, bson.M{
		"next_attempt_height": bson.M{"$exists": false},
	}, mongo.Pipeline{
		{{Key: "$set", Value: bson.M{
			"attempt_count":       0,
			"next_attempt_height": "$created_height",
		}}},
	}); err != nil {
		return fmt.Errorf("next_attempt_height backfill: %w", err)
	}

	if err := tssReqs.CreateIndexIfNotExist(mongo.IndexModel{
		Keys: bson.D{
			{Key: "status", Value: 1},
			{Key: "next_attempt_height", Value: 1},
			{Key: "created_height", Value: 1},
		},
	}); err != nil {
		return fmt.Errorf("failed to create sign-dispatch index: %w", err)
	}

	return nil
}

// FindRequests returns every request for keyID whose Msg is in msgHex.
// Passing an empty msgHex matches everything; passing a nil slice returns
// no rows.
func (tssReq *tssRequests) FindRequests(
	keyID string,
	msgHex []string,
) ([]TssRequest, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	filter := bson.D{{Key: "key_id", Value: keyID}}

	if len(msgHex) != 0 {
		filter = append(filter, bson.E{
			Key:   "msg",
			Value: bson.M{"$in": msgHex},
		})
	}

	result, err := tssReq.Find(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("failed query: %w", err)
	}

	var tssRequest []TssRequest
	if err := result.All(ctx, &tssRequest); err != nil {
		return nil, fmt.Errorf("failed to decode to result: %w", err)
	}

	if len(tssRequest) == 0 {
		return nil, nil
	}

	return tssRequest, nil
}

// SetSignedRequest handles a new or resubmitted vsc.tss sign operation.
//
// - Not found                 → insert as unsigned with NextAttemptHeight=CreatedHeight.
// - Existing unsigned/complete → no-op.
// - Existing failed           → reset to unsigned, AttemptCount=0,
//   NextAttemptHeight=req.CreatedHeight, new CreatedHeight, clear Sig.
//
// The "failed → unsigned" reset is the mechanism users drive retries after a
// key-deprecation failure: they resubmit the same (key_id, msg) and the request
// re-enters the retry schedule.
func (tssReqs *tssRequests) SetSignedRequest(req TssRequest) error {
	ctx := context.Background()
	existing := tssReqs.FindOne(ctx, bson.M{
		"key_id": req.KeyId,
		"msg":    req.Msg,
	})

	if existing.Err() == mongo.ErrNoDocuments {
		nextAttempt := req.NextAttemptHeight
		if nextAttempt == 0 {
			nextAttempt = req.CreatedHeight
		}
		_, err := tssReqs.InsertOne(ctx, bson.M{
			"key_id":              req.KeyId,
			"msg":                 req.Msg,
			"status":              string(SignPending),
			"sig":                 "",
			"created_height":      req.CreatedHeight,
			"attempt_count":       req.AttemptCount,
			"next_attempt_height": nextAttempt,
		})
		return err
	}
	if existing.Err() != nil {
		return existing.Err()
	}

	var current TssRequest
	if err := existing.Decode(&current); err != nil {
		return fmt.Errorf("decode existing request: %w", err)
	}

	if current.Status != SignFailed {
		return nil
	}

	res := tssReqs.FindOneAndUpdate(ctx, bson.M{
		"key_id": req.KeyId,
		"msg":    req.Msg,
	}, bson.M{
		"$set": bson.M{
			"status":              string(SignPending),
			"sig":                 "",
			"attempt_count":       uint(0),
			"next_attempt_height": req.CreatedHeight,
			"created_height":      req.CreatedHeight,
		},
	})
	if res.Err() == mongo.ErrNoDocuments {
		return nil
	}
	return res.Err()
}

// FindUnsignedRequests returns up to `limit` unsigned requests that are due
// for another dispatch at `blockHeight`. Due = NextAttemptHeight <= blockHeight.
// Results are sorted by (next_attempt_height, created_height, key_id, msg) so
// every node picks the same subset when `limit` is smaller than the eligible
// set.
func (tssReqs *tssRequests) FindUnsignedRequests(blockHeight uint64, limit int64) ([]TssRequest, error) {
	ctx := context.Background()

	opts := options.Find().
		SetSort(bson.D{
			{Key: "next_attempt_height", Value: 1},
			{Key: "created_height", Value: 1},
			{Key: "key_id", Value: 1},
			{Key: "msg", Value: 1},
		}).
		SetLimit(limit)

	cursor, err := tssReqs.Find(ctx, bson.M{
		"status":              string(SignPending),
		"next_attempt_height": bson.M{"$lte": blockHeight},
	}, opts)
	if err != nil {
		return nil, err
	}

	var requests []TssRequest
	if err := cursor.All(ctx, &requests); err != nil {
		return nil, err
	}
	return requests, nil
}

// UpdateRequest updates the sig/status of the request identified by
// (key_id, msg). Callers for signing completion use this to persist the
// signature. AttemptCount and NextAttemptHeight are untouched — use
// BumpAttempt for those.
func (tssReqs *tssRequests) UpdateRequest(req TssRequest) error {
	ctx := context.Background()
	singleResult := tssReqs.FindOne(ctx, bson.M{
		"key_id": req.KeyId,
		"msg":    req.Msg,
	})

	if singleResult.Err() == mongo.ErrNoDocuments {
		return nil
	}

	res := tssReqs.FindOneAndUpdate(context.Background(), bson.M{
		"key_id": req.KeyId,
		"msg":    req.Msg,
	}, bson.M{
		"$set": bson.M{
			"sig":    req.Sig,
			"status": req.Status,
		},
	})

	return res.Err()
}

// BumpAttempt records that the request identified by (keyId, msg) was
// dispatched and sets its new backoff state. Called once per dispatch from
// RunActions so determinism holds across nodes.
func (tssReqs *tssRequests) BumpAttempt(keyId, msg string, attemptCount uint, nextAttemptHeight uint64) error {
	ctx := context.Background()
	_, err := tssReqs.UpdateOne(ctx, bson.M{
		"key_id": keyId,
		"msg":    msg,
	}, bson.M{
		"$set": bson.M{
			"attempt_count":       attemptCount,
			"next_attempt_height": nextAttemptHeight,
		},
	})
	return err
}

// MarkFailedByKey transitions every unsigned request for keyId to failed.
// Invoked when a key transitions to deprecated so its pending requests stop
// being retried.
func (tssReqs *tssRequests) MarkFailedByKey(keyId string) error {
	ctx := context.Background()
	_, err := tssReqs.UpdateMany(ctx, bson.M{
		"key_id": keyId,
		"status": string(SignPending),
	}, bson.M{
		"$set": bson.M{"status": string(SignFailed)},
	})
	return err
}

func NewRequests(d *vsc.VscDb) TssRequests {
	return &tssRequests{db.NewCollection(d.DbInstance, "tss_requests")}
}
