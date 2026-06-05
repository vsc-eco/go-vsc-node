package contracts

import (
	"context"
	"fmt"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/hive_blocks"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type contracts struct {
	*db.Collection
}

func New(d *vsc.VscDb) Contracts {
	return &contracts{db.NewCollection(d.DbInstance, "contracts")}
}

func (e *contracts) Init() error {
	err := e.Collection.Init()
	if err != nil {
		return err
	}

	// Index for update history of a contract
	indexModel := mongo.IndexModel{
		Keys: bson.D{{Key: "id", Value: 1}, {Key: "creation_height", Value: -1}},
	}
	err = e.CreateIndexIfNotExist(indexModel)
	if err != nil {
		return fmt.Errorf("failed to create index: %w", err)
	}

	// Index for getting single latest contract by ID
	latestContractModel := mongo.IndexModel{
		Keys:    bson.D{{Key: "id", Value: 1}, {Key: "latest", Value: 1}},
		Options: options.Index().SetPartialFilterExpression(bson.M{"latest": true}),
	}
	err = e.CreateIndexIfNotExist(latestContractModel)
	if err != nil {
		return fmt.Errorf("failed to create index: %w", err)
	}

	// Index for activation-height lookups (active version + pending updates).
	activationIndex := mongo.IndexModel{
		Keys: bson.D{{Key: "id", Value: 1}, {Key: "activation_height", Value: -1}},
	}
	err = e.CreateIndexIfNotExist(activationIndex)
	if err != nil {
		return fmt.Errorf("failed to create index: %w", err)
	}

	// Backfill: rows written before the timelock feature have no activation_height.
	// Treat them as immediate (activation == creation) so the activation-aware
	// ContractById reproduces the pre-feature result exactly. Idempotent.
	_, err = e.UpdateMany(
		context.Background(),
		bson.M{"activation_height": bson.M{"$exists": false}},
		mongo.Pipeline{{{Key: "$set", Value: bson.M{"activation_height": "$creation_height"}}}},
	)
	if err != nil {
		return fmt.Errorf("failed to backfill activation_height: %w", err)
	}

	return nil
}

// ContractById returns the version of a contract that is ACTIVE at the given
// height: the newest one whose activation_height has been reached and that has
// not been cancelled. This is the single execution chokepoint, so a timelocked
// (queued) update is invisible here — the previously-active code keeps running —
// until its activation_height <= height. Pre-feature rows have
// activation_height == creation_height, so this matches the old behavior exactly.
func (c *contracts) ContractById(contractId string, height uint64) (Contract, error) {
	res := Contract{}
	filter := bson.M{
		"id": contractId,
		"activation_height": bson.M{
			"$lte": height,
		},
		"cancelled": bson.M{"$ne": true},
	}
	opts := options.FindOne().SetSort(bson.D{
		{Key: "activation_height", Value: -1},
		{Key: "creation_height", Value: -1},
	})
	qRes := c.FindOne(context.TODO(), filter, opts)
	if err := qRes.Err(); err != nil {
		return res, err
	}

	err := qRes.Decode(&res)
	return res, err
}

func (c *contracts) FindContracts(contractId *string, code *string, historical *bool, offset int, limit int) ([]Contract, error) {
	filters := bson.D{}
	if contractId != nil {
		filters = append(filters, bson.E{Key: "id", Value: *contractId})
	}
	if code != nil {
		filters = append(filters, bson.E{Key: "code", Value: *code})
	}
	if historical == nil || !*historical {
		filters = append(filters, bson.E{Key: "latest", Value: true})
	}
	pipe := hive_blocks.GetAggTimestampPipeline(filters, "creation_height", "creation_ts", offset, limit)
	cursor, err := c.Aggregate(context.TODO(), pipe)
	if err != nil {
		return []Contract{}, err
	}
	defer cursor.Close(context.TODO())
	var results []Contract
	for cursor.Next(context.TODO()) {
		var elem Contract
		if err := cursor.Decode(&elem); err != nil {
			return []Contract{}, err
		}
		results = append(results, elem)
	}
	return results, nil
}

func (c *contracts) RegisterContract(contractId string, args Contract) {
	// TODO: config to prune old versions

	// A version can never activate before it was submitted. Default to immediate
	// (activation == creation) so direct callers/tests that don't set
	// ActivationHeight, and backfilled rows, never resolve to a future-created
	// version in ContractById's activation_height <= height filter.
	activationHeight := args.ActivationHeight
	if activationHeight < args.CreationHeight {
		activationHeight = args.CreationHeight
	}

	findQuery := bson.M{
		"id":              contractId,
		"creation_height": args.CreationHeight,
	}
	updateQuery := bson.M{
		"$set": bson.M{
			"code":              args.Code,
			"name":              args.Name,
			"description":       args.Description,
			"creator":           args.Creator,
			"owner":             args.Owner,
			"tx_id":             args.TxId,
			"runtime":           args.Runtime,
			"activation_height": activationHeight,
			"proposer":          args.Proposer,
			"latest":            true,
		},
	}
	opts := options.FindOneAndUpdate().SetUpsert(true)

	oldVersionsFilter := bson.M{
		"id": contractId,
	}
	oldVersionsUpdate := bson.M{
		"$unset": bson.M{
			"latest": "",
		},
	}
	c.UpdateMany(context.Background(), oldVersionsFilter, oldVersionsUpdate)
	c.FindOneAndUpdate(context.Background(), findQuery, updateQuery, opts)
}

// FindActiveContracts returns, for each matching contract id, the version that is
// ACTIVE at head (newest non-cancelled version with activation_height <= head).
// Pending (timelocked) versions are excluded. Backs the default findContract.
func (c *contracts) FindActiveContracts(contractId *string, code *string, head uint64, offset int, limit int) ([]Contract, error) {
	match := bson.D{
		{Key: "activation_height", Value: bson.M{"$lte": head}},
		{Key: "cancelled", Value: bson.M{"$ne": true}},
	}
	if contractId != nil {
		match = append(match, bson.E{Key: "id", Value: *contractId})
	}
	if code != nil {
		match = append(match, bson.E{Key: "code", Value: *code})
	}
	pipe := mongo.Pipeline{
		{{Key: "$match", Value: match}},
		// Newest activated version first, then collapse to one row per id.
		{{Key: "$sort", Value: bson.D{
			{Key: "id", Value: 1},
			{Key: "activation_height", Value: -1},
			{Key: "creation_height", Value: -1},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$id"},
			{Key: "doc", Value: bson.M{"$first": "$$ROOT"}},
		}}},
		{{Key: "$replaceRoot", Value: bson.M{"newRoot": "$doc"}}},
		{{Key: "$sort", Value: bson.D{{Key: "id", Value: 1}}}},
	}
	if offset > 0 {
		pipe = append(pipe, bson.D{{Key: "$skip", Value: offset}})
	}
	if limit > 0 {
		pipe = append(pipe, bson.D{{Key: "$limit", Value: limit}})
	}
	pipe = append(pipe, creationTimestampLookup()...)
	return c.runContractPipeline(pipe)
}

// FindPendingUpdates returns queued contract updates that have NOT yet activated
// at head (activation_height > head, not cancelled). Each row exposes the new
// code, proposer, and activation_height — i.e. which contract gets what new code,
// by whom, and when it goes live. Backs findPendingContractUpdates.
func (c *contracts) FindPendingUpdates(contractId *string, head uint64, offset int, limit int) ([]Contract, error) {
	filters := bson.D{
		{Key: "activation_height", Value: bson.M{"$gt": head}},
		{Key: "cancelled", Value: bson.M{"$ne": true}},
	}
	if contractId != nil {
		filters = append(filters, bson.E{Key: "id", Value: *contractId})
	}
	// Soonest-to-activate first.
	pipe := hive_blocks.GetAggTimestampPipeline(filters, "creation_height", "creation_ts", offset, limit)
	return c.runContractPipeline(pipe)
}

// CancelPendingUpdate tombstones still-pending (activation_height > head, not yet
// cancelled) versions of a contract. When targetTx is non-nil only the version
// queued by that tx is cancelled; otherwise every pending version is. Returns the
// number of versions cancelled.
func (c *contracts) CancelPendingUpdate(contractId string, head uint64, cancelledHeight uint64, cancelledTx string, targetTx *string) (int, error) {
	filter := bson.M{
		"id":                contractId,
		"activation_height": bson.M{"$gt": head},
		"cancelled":         bson.M{"$ne": true},
	}
	if targetTx != nil {
		filter["tx_id"] = *targetTx
	}
	update := bson.M{
		"$set": bson.M{
			"cancelled":        true,
			"cancelled_height": cancelledHeight,
			"cancelled_tx":     cancelledTx,
		},
		"$unset": bson.M{
			"latest": "",
		},
	}
	res, err := c.UpdateMany(context.Background(), filter, update)
	if err != nil {
		return 0, err
	}
	return int(res.ModifiedCount), nil
}

// creationTimestampLookup joins hive_blocks to populate creation_ts from the
// creation_height block, matching the tail of hive_blocks.GetAggTimestampPipeline
// (used where a $group precedes pagination so the shared helper can't be reused
// wholesale).
func creationTimestampLookup() mongo.Pipeline {
	return mongo.Pipeline{
		{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: "hive_blocks"},
			{Key: "localField", Value: "creation_height"},
			{Key: "foreignField", Value: "block.block_number"},
			{Key: "as", Value: "block_info"},
		}}},
		{{Key: "$unwind", Value: "$block_info"}},
		{{Key: "$addFields", Value: bson.D{{Key: "creation_ts", Value: "$block_info.block.timestamp"}}}},
		{{Key: "$project", Value: bson.D{{Key: "block_info", Value: 0}}}},
	}
}

func (c *contracts) runContractPipeline(pipe mongo.Pipeline) ([]Contract, error) {
	cursor, err := c.Aggregate(context.TODO(), pipe)
	if err != nil {
		return []Contract{}, err
	}
	defer cursor.Close(context.TODO())
	var results []Contract
	for cursor.Next(context.TODO()) {
		var elem Contract
		if err := cursor.Decode(&elem); err != nil {
			return []Contract{}, err
		}
		results = append(results, elem)
	}
	return results, nil
}

type contractState struct {
	*db.Collection
}

func (ch *contractState) IngestOutput(output IngestOutputArgs) {
	options := options.FindOneAndUpdate().SetUpsert(true)
	ch.FindOneAndUpdate(context.Background(), bson.M{"id": output.Id}, bson.M{
		"$set": bson.M{
			"id":           output.Id,
			"contract_id":  output.ContractId,
			"state_merkle": output.StateMerkle,
			"block_height": output.AnchoredHeight,

			"metadata": output.Metadata,
			"inputs":   output.Inputs,
			"results":  output.Results,
		},
	}, options)

}

func (ch *contractState) GetLastOutput(contractId string, height uint64) (ContractOutput, error) {
	options := options.FindOne().SetSort(bson.M{"block_height": -1})
	findResult := ch.FindOne(context.Background(), bson.M{"contract_id": contractId, "block_height": bson.M{
		"$lte": height,
	}}, options)
	if findResult.Err() != nil {
		return ContractOutput{}, nil
	}
	contractOutput := ContractOutput{
		Metadata: ContractMetadata{},
	}
	err := findResult.Decode(&contractOutput)
	return contractOutput, err
}

func (ch *contractState) GetOutput(outputId string) *ContractOutput {
	findResult := ch.FindOne(context.Background(), bson.M{"id": outputId})
	if findResult.Err() != nil {
		return nil
	}

	contractOutput := ContractOutput{}
	findResult.Decode(&contractOutput)
	return &contractOutput
}

func (ch *contractState) FindOutputs(id *string, input *string, contract *string, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]ContractOutput, error) {
	filters := bson.D{}
	if id != nil {
		filters = append(filters, bson.E{Key: "id", Value: *id})
	}
	if input != nil {
		filters = append(filters, bson.E{Key: "inputs", Value: bson.D{{Key: "$elemMatch", Value: bson.D{{Key: "$eq", Value: *input}}}}})
	}
	if contract != nil {
		filters = append(filters, bson.E{Key: "contract_id", Value: *contract})
	}
	if fromBlock != nil {
		filters = append(filters, bson.E{Key: "block_height", Value: bson.D{{Key: "$gte", Value: *fromBlock}}})
	}
	if toBlock != nil {
		filters = append(filters, bson.E{Key: "block_height", Value: bson.D{{Key: "$lte", Value: *toBlock}}})
	}
	pipe := hive_blocks.GetAggTimestampPipeline(filters, "block_height", "timestamp", offset, limit)
	cursor, err := ch.Aggregate(context.TODO(), pipe)
	if err != nil {
		return []ContractOutput{}, err
	}
	defer cursor.Close(context.TODO())
	var results []ContractOutput
	for cursor.Next(context.TODO()) {
		var elem ContractOutput
		if err := cursor.Decode(&elem); err != nil {
			return []ContractOutput{}, err
		}
		results = append(results, elem)
	}
	return results, nil
}

func NewContractState(d *vsc.VscDb) ContractState {
	return &contractState{db.NewCollection(d.DbInstance, "contract_state")}
}
