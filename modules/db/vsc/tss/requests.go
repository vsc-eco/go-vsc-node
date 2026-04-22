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

// Init runs the base collection init, applies the one-time schema migration,
// and ensures the composite index used by FindUnsignedRequests exists.
//
// Migration: pre-queue rows have neither last_attempt nor attempt_count.
// Detect them by the absence of last_attempt and:
//
//   - set last_attempt = created_height so they enter the new FIFO queue at
//     their original submission position
//   - set attempt_count = 0
//   - if the row was status=failed (under the pre-queue 1000-block expiry
//     scheme), revive it to pending and clear sig — this is a one-time
//     revival on the migration boot only
//
// Rows failed AFTER the queue migration (by MarkFailed for cap exhaustion or
// MarkFailedByKey for key deprecation) already have last_attempt set, so they
// do not match this filter and stay failed across restarts. Subsequent boots
// match no rows and are no-ops.
func (tssReqs *tssRequests) Init() error {
	if err := tssReqs.Collection.Init(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// One-time revival: pre-queue failed rows lack last_attempt and need
	// status/sig reset alongside the backfill. Done first so the second
	// pass below catches only the remaining (non-failed) pre-queue rows.
	if _, err := tssReqs.UpdateMany(ctx, bson.M{
		"status":       string(SignFailed),
		"last_attempt": bson.M{"$exists": false},
	}, mongo.Pipeline{
		{{Key: "$set", Value: bson.M{
			"status":        string(SignPending),
			"sig":           "",
			"last_attempt":  "$created_height",
			"attempt_count": 0,
		}}},
	}); err != nil {
		return fmt.Errorf("revive pre-queue failed rows: %w", err)
	}

	// Backfill last_attempt (= created_height) and attempt_count for any
	// remaining pre-queue rows so they enter the new queue at their
	// original CreatedHeight position. Preserves created_height verbatim.
	if _, err := tssReqs.UpdateMany(ctx, bson.M{
		"last_attempt": bson.M{"$exists": false},
	}, mongo.Pipeline{
		{{Key: "$set", Value: bson.M{
			"last_attempt":  "$created_height",
			"attempt_count": 0,
		}}},
	}); err != nil {
		return fmt.Errorf("backfill last_attempt: %w", err)
	}

	if err := tssReqs.CreateIndexIfNotExist(mongo.IndexModel{
		Keys: bson.D{
			{Key: "status", Value: 1},
			{Key: "last_attempt", Value: 1},
		},
	}); err != nil {
		return fmt.Errorf("create sign-queue index: %w", err)
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

// SetSignedRequest inserts or revives a signing request for (key_id, msg).
//
//   - Not found                  → insert as pending, LastAttempt = CreatedHeight.
//   - Existing pending/complete  → no-op (preserves reservations and signatures).
//   - Existing failed            → reset to pending, LastAttempt = req.CreatedHeight,
//                                  AttemptCount = 0, clear sig.
//
// The reset path is how users drive retries after key-deprecation or
// attempt-cap failure: resubmit the same (key_id, msg) and it re-enters the
// queue at the resubmit block.
func (tssReqs *tssRequests) SetSignedRequest(req TssRequest) error {
	ctx := context.Background()
	existing := tssReqs.FindOne(ctx, bson.M{
		"key_id": req.KeyId,
		"msg":    req.Msg,
	})

	if existing.Err() == mongo.ErrNoDocuments {
		lastAttempt := req.LastAttempt
		if lastAttempt == 0 {
			lastAttempt = req.CreatedHeight
		}
		_, err := tssReqs.InsertOne(ctx, bson.M{
			"key_id":         req.KeyId,
			"msg":            req.Msg,
			"status":         string(SignPending),
			"sig":            "",
			"created_height": req.CreatedHeight,
			"last_attempt":   lastAttempt,
			"attempt_count":  req.AttemptCount,
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
			"status":         string(SignPending),
			"sig":            "",
			"last_attempt":   req.CreatedHeight,
			"attempt_count":  uint(0),
			"created_height": req.CreatedHeight,
		},
	})
	if res.Err() == mongo.ErrNoDocuments {
		return nil
	}
	return res.Err()
}

// FindUnsignedRequests returns up to `limit` unsigned requests whose
// reservation has elapsed at `blockHeight`:
//
//   - status == pending
//   - last_attempt <= blockHeight
//   - created_height < blockHeight
//
// The `created_height < blockHeight` gate is the cross-node determinism
// anchor. BlockTick is registered async on the L1 consumer: at block N, the
// TSS tick goroutine races the state engine's same-block writes. State
// engine inserts for block N have created_height == N and may or may not
// be visible when the tick reads the queue. Excluding rows inserted at the
// current block forces every honest node to use the same cutoff — rows only
// become eligible at block N+1, by which point the state engine for block N
// has committed on every node. Without this gate, two nodes can select
// different subsets at block N, advance LastAttempt to different values, and
// permanently disagree about which slot the request fires in.
//
// The attempt cap is NOT enforced here — at-cap rows surface to the caller,
// which calls MarkFailed on them at selection time. This avoids a periodic
// sweep.
//
// Results are sorted by (last_attempt, created_height, key_id, msg) ascending,
// guaranteeing every node selects the same subset when `limit` < eligible
// count. Served by the (status, last_attempt) composite index.
func (tssReqs *tssRequests) FindUnsignedRequests(blockHeight uint64, limit int64) ([]TssRequest, error) {
	ctx := context.Background()

	opts := options.Find().
		SetSort(bson.D{
			{Key: "last_attempt", Value: 1},
			{Key: "created_height", Value: 1},
			{Key: "key_id", Value: 1},
			{Key: "msg", Value: 1},
		}).
		SetLimit(limit)

	cursor, err := tssReqs.Find(ctx, bson.M{
		"status":         string(SignPending),
		"last_attempt":   bson.M{"$lte": blockHeight},
		"created_height": bson.M{"$lt": blockHeight},
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
// signature. AttemptCount and LastAttempt are untouched — use ReserveAttempt
// for those.
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

// ReserveAttempt advances LastAttempt to bh + SignAttemptReservation and
// increments AttemptCount for (keyId, msg) — but only if last_attempt <= bh.
// The filter guard makes the write idempotent within a reservation window,
// so replaying a block after a restart applies the update at most once.
//
// Called once per selected request from BlockTick on every node. Because
// BlockTick runs per-block via the L1 consumer and the selection inputs are
// all on-chain-derived, every node writes the same update.
func (tssReqs *tssRequests) ReserveAttempt(keyId, msg string, bh uint64) error {
	ctx := context.Background()
	_, err := tssReqs.UpdateOne(ctx, bson.M{
		"key_id":       keyId,
		"msg":          msg,
		"status":       string(SignPending),
		"last_attempt": bson.M{"$lte": bh},
	}, bson.M{
		"$set": bson.M{"last_attempt": bh + SignAttemptReservation},
		"$inc": bson.M{"attempt_count": 1},
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

// MarkFailed transitions the pending request identified by (keyId, msg) to
// failed. Called at selection time when the caller encounters a row with
// AttemptCount >= MaxSignAttempts. The pending guard prevents clobbering a
// completed row in a concurrent-resubmission race.
func (tssReqs *tssRequests) MarkFailed(keyId, msg string) error {
	ctx := context.Background()
	_, err := tssReqs.UpdateOne(ctx, bson.M{
		"key_id": keyId,
		"msg":    msg,
		"status": string(SignPending),
	}, bson.M{
		"$set": bson.M{"status": string(SignFailed)},
	})
	return err
}

func NewRequests(d *vsc.VscDb) TssRequests {
	return &tssRequests{db.NewCollection(d.DbInstance, "tss_requests")}
}
