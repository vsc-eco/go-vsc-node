package witnesses

import (
	"context"
	"encoding/json"
	"fmt"

	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type witnesses struct {
	*db.Collection
}

func New(d *vsc.VscDb) Witnesses {
	return &witnesses{db.NewCollection(d.DbInstance, "witnesses")}
}

func (e *witnesses) Init() error {
	err := e.Collection.Init()
	if err != nil {
		return err
	}

	return nil
}

// StoreNodeAnnouncement implements Witnesses.
func (w *witnesses) StoreNodeAnnouncement(nodeId string) error {
	_, err := w.InsertOne(context.Background(), Witness{NodeId: nodeId})
	return err
}

func (w *witnesses) SetWitnessUpdate(requestIn SetWitnessUpdateType) error {
	ctx := context.Background()
	findOptions := options.FindOneAndUpdate().SetUpsert(true)

	bbytes, _ := json.Marshal(requestIn)
	request := SetWitnessUpdateType{}
	json.Unmarshal(bbytes, &request)
	query := bson.M{
		"account": request.Account,
		"height":  request.Height,
	}
	update := bson.M{
		"$set": bson.M{
			"peerId": request.Metadata.VscNode.UnsignedProof.PeerId,
			// "signing_keys": request.Metadata.DidKeys,
			// "version_id":   request.Metadata.VscNode.UnsignedProof.VersionId,
			"git_commit": request.Metadata.VscNode.UnsignedProof.GitCommit,
			"net_id":     request.Metadata.VscNode.UnsignedProof.NetId,
		},
	}
	w.FindOneAndUpdate(ctx, query, update, findOptions)

	// if result.Err() != nil {
	// 	return result.Err()
	// }
	oldRecordsFilter := bson.M{
		"account": request.Account,
	}
	//Maximum of 5 old records on a node should be maintained
	oldRecordsOptions := options.Find().SetLimit(6).SetSort(bson.D{{
		Key:   "height",
		Value: -1,
	}})
	cursor, _ := w.Find(ctx, oldRecordsFilter, oldRecordsOptions)
	var deletionHeight *int32
	for cursor.Next(ctx) {
		var result bson.M
		if err := cursor.Decode(&result); err != nil {
			return fmt.Errorf("failed to decode block: %w", err)
		}
		delHeight := result["height"].(int32)
		deletionHeight = &delHeight

	}

	if deletionHeight != nil {
		filter := bson.M{
			"account": request.Account,
			"height": bson.M{
				"$lt": *deletionHeight,
			},
		}
		w.DeleteMany(ctx, filter)
	}

	return nil
}

func (w *witnesses) GetWitnessNode(account string, blockHeight *int64) {
	ctx := context.Background()
	var filter bson.M
	//Get at specific block interval. More safe
	if blockHeight != nil {
		filter = bson.M{
			"account": account,
			"height": bson.M{
				"$lte": blockHeight,
			},
		}
	} else {
		//Get most latest record
		filter = bson.M{
			"account": account,
		}
	}
	findOptions := options.FindOne().SetSort(bson.D{{
		Key:   "height",
		Value: -1,
	}})
	mongoResult := w.FindOne(ctx, filter, findOptions)

	result := bson.M{}
	mongoResult.Decode(&result)
	// fmt.Println("Vaultec decoded data from mongo query result", result)
}
