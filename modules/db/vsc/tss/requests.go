package tss_db

import (
	"context"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type tssRequests struct {
	*db.Collection
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
