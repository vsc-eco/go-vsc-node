package witnesses

import (
	"context"
	"encoding/json"
	"fmt"

	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const WITNESS_EXPIRE_BLOCKS = 86_400

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

	indexes := []mongo.IndexModel{
		{Keys: bson.D{{Key: "account", Value: 1}, {Key: "height", Value: -1}}},
		{Keys: bson.D{{Key: "height", Value: 1}}},
	}
	for _, idx := range indexes {
		if err := e.CreateIndexIfNotExist(idx); err != nil {
			return fmt.Errorf("failed to create witnesses index: %w", err)
		}
	}
	return nil
}

// StoreNodeAnnouncement implements Witnesses.
func (w *witnesses) StoreNodeAnnouncement(nodeId string) error {
	_, err := w.InsertOne(context.Background(), Witness{})
	return err
}

func (w *witnesses) SetWitnessUpdate(requestIn SetWitnessUpdateType) error {
	ctx := context.Background()
	findOptions := options.FindOneAndUpdate().SetUpsert(true)

	bbytes, _ := json.Marshal(requestIn)
	request := SetWitnessUpdateType{}
	json.Unmarshal(bbytes, &request)

	//Find duplicate registered node

	var consensusKey string

	for _, key := range request.Metadata.DidKeys {
		if key.Type == "consensus" {
			consensusKey = key.Key
			break
		}
	}

	if consensusKey == "" {
		return fmt.Errorf("no consensus key found in did keys")
	}

	query := bson.M{
		"account": request.Account,
		"height":  request.Height,
	}
	update := bson.M{
		"$set": bson.M{
			"peer_id":    request.Metadata.VscNode.PeerId,
			"peer_addrs": request.Metadata.VscNode.PeerAddrs,
			//timestamp
			"ts":               request.Metadata.VscNode.Ts,
			"tx_id":            request.TxId,
			"version_id":       request.Metadata.VscNode.VersionId,
			"git_commit":       request.Metadata.VscNode.GitCommit,
			"net_id":           request.Metadata.VscNode.NetId,
			"protocol_version": request.Metadata.VscNode.ProtocolVersion,
			"enabled":          request.Metadata.VscNode.Witness.Enabled,
			"did_keys":         request.Metadata.DidKeys,
			"gateway_key":      request.Metadata.VscNode.GatewayKey,
		},
	}
	w.FindOneAndUpdate(ctx, query, update, findOptions)

	return nil
}

// PruneOlderThan deletes witness records with height < cutoff. Caller must pass
// cutoff <= currentHeight - WITNESS_EXPIRE_BLOCKS so records still in the
// GetWitnessesAtBlockHeight lookback window survive.
func (w *witnesses) PruneOlderThan(ctx context.Context, cutoff uint64) (int64, error) {
	if cutoff == 0 {
		return 0, nil
	}
	res, err := w.DeleteMany(ctx, bson.M{"height": bson.M{"$lt": cutoff}})
	if err != nil {
		return 0, err
	}
	return res.DeletedCount, nil
}

func (w *witnesses) GetLastestWitnesses(searchOptions ...SearchOption) ([]Witness, error) {
	ctx := context.Background()
	findOptions := options.Find().SetSort(bson.D{{
		Key:   "height",
		Value: -1,
	}})
	cursor, err := w.Find(ctx, bson.M{}, findOptions)
	if err != nil {
		return nil, err
	}

	var witnesses []Witness
	for cursor.Next(ctx) {
		var result Witness
		if err := cursor.Decode(&result); err != nil {
			return nil, fmt.Errorf("failed to decode witness: %w", err)
		}
		witnesses = append(witnesses, result)
	}

	return witnesses, nil
}

// Deterministically sorted (alphabetical) list of witnesses at a given block height.
// Single-aggregation pipeline: $match → $sort by height desc → $group keeps first (latest)
// per account → $replaceRoot → $sort by account asc.
func (w *witnesses) GetWitnessesAtBlockHeight(bh uint64, opts ...SearchOption) ([]Witness, error) {
	ctx := context.Background()

	var gte uint64
	if bh > WITNESS_EXPIRE_BLOCKS {
		gte = bh - WITNESS_EXPIRE_BLOCKS
	} else {
		gte = 0
	}

	matchFilter := bson.M{
		"height": bson.M{
			"$gte": gte,
			"$lt":  bh,
		},
	}
	for _, opt := range opts {
		if err := opt(&matchFilter); err != nil {
			return nil, err
		}
	}

	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: matchFilter}},
		{{Key: "$sort", Value: bson.D{{Key: "height", Value: -1}}}},
		{{Key: "$group", Value: bson.M{
			"_id": "$account",
			"doc": bson.M{"$first": "$$ROOT"},
		}}},
		{{Key: "$replaceRoot", Value: bson.M{"newRoot": "$doc"}}},
		{{Key: "$sort", Value: bson.D{{Key: "account", Value: 1}}}},
	}

	cursor, err := w.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	out := make([]Witness, 0)
	for cursor.Next(ctx) {
		var result Witness
		if err := cursor.Decode(&result); err != nil {
			return nil, fmt.Errorf("failed to decode witness: %w", err)
		}
		out = append(out, result)
	}
	return out, nil
}

func (w *witnesses) GetWitnessesByPeerId(PeerIds []string, options ...SearchOption) ([]Witness, error) {
	findResult, err := w.Find(context.Background(), bson.M{
		"peer_id": bson.M{
			"$in": PeerIds,
		},
	})

	if err != nil {
		return nil, err
	}

	witnessSet := make(map[string]Witness)
	for findResult.Next(context.Background()) {
		var result Witness
		if err := findResult.Decode(&result); err != nil {
			return nil, fmt.Errorf("failed to decode witness: %w", err)
		}

		var witness Witness
		err := findResult.Decode(&witness)
		if err != nil {
			return nil, err
		}

		witnessSet[witness.Account] = witness
	}

	witnesses := make([]Witness, 0)
	for _, witness := range witnessSet {
		witnesses = append(witnesses, witness)
	}

	return witnesses, nil
}

func (w *witnesses) GetWitnessAtHeight(account string, bh *uint64) (*Witness, error) {
	ctx := context.Background()

	findOptions := options.FindOne().SetSort(bson.D{{Key: "height", Value: -1}})
	findQuery := bson.M{
		"account": account,
	}

	if bh != nil {
		findQuery["height"] = bson.M{
			"$lt": *bh,
		}
	}

	findResult := w.FindOne(ctx, findQuery, findOptions)

	if findResult.Err() != nil {
		return nil, findResult.Err()
	}

	witness := Witness{}
	err := findResult.Decode(&witness)

	if err != nil {
		return nil, err
	}

	return &witness, nil
}
