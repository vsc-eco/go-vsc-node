package pendulum_settlements

import (
	"context"
	"errors"

	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const collectionName = "pendulum_settlements"

type settlementsDb struct {
	*db.Collection
}

func New(d *vsc.VscDb) PendulumSettlements {
	return &settlementsDb{db.NewCollection(d.DbInstance, collectionName)}
}

func (s *settlementsDb) Init() error {
	if err := s.Collection.Init(); err != nil {
		return err
	}
	indexModel := mongo.IndexModel{
		Keys:    bson.D{{Key: "epoch", Value: 1}},
		Options: options.Index().SetUnique(true),
	}
	return s.CreateIndexIfNotExist(indexModel)
}

func (s *settlementsDb) SaveMarker(rec SettlementMarker) error {
	filter := bson.M{"epoch": rec.Epoch}
	update := bson.M{"$set": rec}
	opts := options.Update().SetUpsert(true)
	_, err := s.UpdateOne(context.Background(), filter, update, opts)
	return err
}

func (s *settlementsDb) GetLatestSettledEpoch() (uint64, bool) {
	opts := options.FindOne().SetSort(bson.D{{Key: "epoch", Value: -1}})
	res := s.FindOne(context.Background(), bson.M{}, opts)
	if err := res.Err(); err != nil {
		return 0, false
	}
	var rec SettlementMarker
	if err := res.Decode(&rec); err != nil {
		return 0, false
	}
	return rec.Epoch, true
}

func (s *settlementsDb) GetMarker(epoch uint64) (*SettlementMarker, bool, error) {
	res := s.FindOne(context.Background(), bson.M{"epoch": epoch})
	if err := res.Err(); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var rec SettlementMarker
	if err := res.Decode(&rec); err != nil {
		return nil, false, err
	}
	return &rec, true, nil
}
