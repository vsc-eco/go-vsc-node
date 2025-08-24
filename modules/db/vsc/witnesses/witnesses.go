package witnesses

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

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
	_, err := w.InsertOne(context.Background(), Witness{})
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
	cursor, err := w.Find(ctx, oldRecordsFilter, oldRecordsOptions)
	if err != nil {
		return err
	}
	var deletionHeight *uint64
	for cursor.Next(ctx) {
		var result bson.M
		if err := cursor.Decode(&result); err != nil {
			return fmt.Errorf("failed to decode block: %w", err)
		}
		delHeight := uint64(result["height"].(int64))
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

func (w *witnesses) GetLastestWitnesses() ([]Witness, error) {
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

//Get valid witnesses at block height
//

const maxSignedDiff = (3 * 24 * 60 * 20)

// Deterministically sorted (alphetical) list of witnesses at a given block height
func (w *witnesses) GetWitnessesAtBlockHeight(bh uint64) ([]Witness, error) {
	ctx := context.Background()

	var gte uint64
	if bh > maxSignedDiff {
		gte = bh - maxSignedDiff
	} else {
		gte = 0
	}

	distinctAccounts, err := w.Distinct(ctx, "account", bson.M{
		"height": bson.M{
			"$gte": gte,
			"$lt":  bh,
		},
	})

	if err != nil {
		return nil, err
	}

	findOptions := options.Find().SetSort(bson.D{{
		Key:   "height",
		Value: -1,
	}})
	cursor, err := w.Find(ctx, bson.M{
		"height": bson.M{
			"$gte": gte,
			"$lt":  bh,
		},

		"account": bson.M{"$in": distinctAccounts},
	}, findOptions)
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

	//TODO: add filtering options equivalent to the old VSC network

	witnessMap := make(map[string]*Witness)

	for _, witness := range witnesses {
		if witnessMap[witness.Account] == nil {
			witnessMap[witness.Account] = &witness
		}
	}

	outNames := make([]string, 0)
	for _, witness := range witnessMap {
		outNames = append(outNames, witness.Account)
	}

	sort.Strings(outNames)

	outWit := make([]Witness, 0)
	for _, name := range outNames {
		if witnessMap[name] == nil {
			return nil, fmt.Errorf("witness not found")
		}
		outWit = append(outWit, *witnessMap[name])
	}

	return outWit, nil
}

func (w *witnesses) GetWitnessesByPeerId(PeerIds ...string) ([]Witness, error) {
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

type EmptyWitnesses struct {
	witnesses []Witness
}

func NewEmptyWitnesses() *EmptyWitnesses {
	return &EmptyWitnesses{
		witnesses: make([]Witness, 0),
	}
}

func (w *EmptyWitnesses) GetLastestWitnesses() ([]Witness, error) {
	return w.witnesses, nil
}
