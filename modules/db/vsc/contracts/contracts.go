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

	return nil
}

// ContractById implements Contracts.
func (c *contracts) ContractById(contractId string, height uint64) (Contract, error) {
	res := Contract{}
	filter := bson.M{
		"id": contractId,
		"creation_height": bson.M{
			"$lte": height,
		},
	}
	opts := options.FindOne().SetSort(bson.M{"creation_height": -1})
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
	findQuery := bson.M{
		"id":              contractId,
		"creation_height": args.CreationHeight,
	}
	updateQuery := bson.M{
		"$set": bson.M{
			"code":        args.Code,
			"name":        args.Name,
			"description": args.Description,
			"creator":     args.Creator,
			"owner":       args.Owner,
			"tx_id":       args.TxId,
			"runtime":     args.Runtime,
			"latest":      true,
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
