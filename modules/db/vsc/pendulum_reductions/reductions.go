package pendulum_reductions

import (
	"context"
	"errors"

	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const collectionName = "pendulum_reductions"

type reductionsDb struct {
	*db.Collection
}

func New(d *vsc.VscDb) PendulumReductions {
	return &reductionsDb{db.NewCollection(d.DbInstance, collectionName)}
}

func (r *reductionsDb) Init() error {
	if err := r.Collection.Init(); err != nil {
		return err
	}
	// {account, epoch}: the explorer's primary access path — all of one
	// account's reductions across epochs, newest first.
	if err := r.CreateIndexIfNotExist(mongo.IndexModel{
		Keys: bson.D{{Key: "account", Value: 1}, {Key: "epoch", Value: 1}},
	}); err != nil {
		return err
	}
	// {epoch}: whole-committee view for a single settlement, and HasEpoch.
	if err := r.CreateIndexIfNotExist(mongo.IndexModel{
		Keys: bson.D{{Key: "epoch", Value: 1}},
	}); err != nil {
		return err
	}
	// {id} unique: idempotent apply-time upserts (id == "epoch:account").
	return r.CreateIndexIfNotExist(mongo.IndexModel{
		Keys:    bson.D{{Key: "id", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
}

func (r *reductionsDb) SaveReductions(recs []PendulumReductionRecord) error {
	if len(recs) == 0 {
		return nil
	}
	models := make([]mongo.WriteModel, 0, len(recs))
	for _, rec := range recs {
		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"id": rec.Id}).
			SetUpdate(bson.M{"$set": rec}).
			SetUpsert(true))
	}
	_, err := r.BulkWrite(context.Background(), models)
	return err
}

func (r *reductionsDb) FindReductions(
	account *string,
	epoch *uint64,
	fromEpoch *uint64,
	toEpoch *uint64,
	offset int,
	limit int,
) ([]PendulumReductionRecord, error) {
	filter := bson.M{}
	if account != nil {
		filter["account"] = *account
	}
	if epoch != nil {
		filter["epoch"] = *epoch
	} else {
		epochRange := bson.M{}
		if fromEpoch != nil {
			epochRange["$gte"] = *fromEpoch
		}
		if toEpoch != nil {
			epochRange["$lte"] = *toEpoch
		}
		if len(epochRange) > 0 {
			filter["epoch"] = epochRange
		}
	}

	opts := options.Find().
		SetSort(bson.D{{Key: "epoch", Value: -1}, {Key: "account", Value: 1}}).
		SetSkip(int64(offset)).
		SetLimit(int64(limit))

	cursor, err := r.Find(context.Background(), filter, opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(context.Background())

	out := make([]PendulumReductionRecord, 0)
	for cursor.Next(context.Background()) {
		var rec PendulumReductionRecord
		if err := cursor.Decode(&rec); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

func (r *reductionsDb) HasEpoch(epoch uint64) (bool, error) {
	res := r.FindOne(context.Background(), bson.M{"epoch": epoch})
	if err := res.Err(); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
