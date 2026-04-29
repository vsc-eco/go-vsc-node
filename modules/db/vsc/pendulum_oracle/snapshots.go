package pendulum_oracle

import (
	"context"
	"errors"

	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const collectionName = "pendulum_oracle_snapshots"

type snapshotsDb struct {
	*db.Collection
}

func New(d *vsc.VscDb) PendulumOracleSnapshots {
	return &snapshotsDb{db.NewCollection(d.DbInstance, collectionName)}
}

func (s *snapshotsDb) Init() error {
	if err := s.Collection.Init(); err != nil {
		return err
	}
	indexModel := mongo.IndexModel{
		Keys:    bson.D{{Key: "tick_block_height", Value: 1}},
		Options: options.Index().SetUnique(true),
	}
	return s.CreateIndexIfNotExist(indexModel)
}

func (s *snapshotsDb) SaveSnapshot(rec SnapshotRecord) error {
	filter := bson.M{"tick_block_height": rec.TickBlockHeight}
	update := bson.M{"$set": rec}
	opts := options.Update().SetUpsert(true)
	_, err := s.UpdateOne(context.Background(), filter, update, opts)
	return err
}

func (s *snapshotsDb) GetSnapshot(tickBlockHeight uint64) (*SnapshotRecord, bool, error) {
	res := s.FindOne(context.Background(), bson.M{"tick_block_height": tickBlockHeight})
	if err := res.Err(); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var out SnapshotRecord
	if err := res.Decode(&out); err != nil {
		return nil, false, err
	}
	return &out, true, nil
}

func (s *snapshotsDb) GetSnapshotAtOrBefore(blockHeight uint64) (*SnapshotRecord, bool, error) {
	filter := bson.M{"tick_block_height": bson.M{"$lte": blockHeight}}
	opts := options.FindOne().SetSort(bson.D{{Key: "tick_block_height", Value: -1}})
	res := s.FindOne(context.Background(), filter, opts)
	if err := res.Err(); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var out SnapshotRecord
	if err := res.Decode(&out); err != nil {
		return nil, false, err
	}
	return &out, true, nil
}
