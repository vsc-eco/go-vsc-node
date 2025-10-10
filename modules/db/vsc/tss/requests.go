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

// FindRequest implements TssRequests.
func (tssReq *tssRequests) FindRequest(keyID string, msgHex string) (*TssRequest, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	filter := bson.D{
		{Key: "id", Value: keyID},
		{Key: "msg", Value: msgHex},
	}

	result := tssReq.FindOne(ctx, filter)
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("failed query: %w", err)
	}

	var tssRequest TssRequest
	if err := result.Decode(&tssRequest); err != nil {
		return nil, fmt.Errorf("failed to decode to struct: %w", err)
	}

	return &tssRequest, nil
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
