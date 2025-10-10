package tss_db

import (
	"context"
	"fmt"
	"time"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type tssRequests struct {
	*db.Collection
}

// FindRequests implements TssRequests.
// To get all msgHex associated with keyID, pass in nil for msgHex, or empty slice.
// Returns only the matching hex, if no matching element, nil value is returned.
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

func (tssReq *tssRequests) SetSignedRequest(req TssRequest) error {
	ctx := context.Background()
	singleResult := tssReq.FindOne(ctx, bson.M{
		"key_id": req.KeyId,
		"msg":    req.Msg,
	})

	if singleResult.Err() == nil {
		return nil
	}

	updateOptions := options.FindOneAndUpdate().SetUpsert(true)
	singeResult := tssReq.FindOneAndUpdate(context.Background(), bson.M{
		"key_id": req.KeyId,
		"msg":    req.Msg,
	}, bson.M{}, updateOptions)
	return singeResult.Err()
}

func (tss *tssRequests) FindUnsignedRequests(blockHeight uint64) ([]TssRequest, error) {
	return nil, nil
}

func NewRequests(d *vsc.VscDb) TssRequests {
	return &tssRequests{db.NewCollection(d.DbInstance, "tss_requests")}
}
