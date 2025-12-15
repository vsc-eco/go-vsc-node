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

	return nil
}

// ContractById implements Contracts.
func (c *contracts) ContractById(contractId string) (Contract, error) {
	res := Contract{}
	filter := bson.M{
		"id": contractId,
	}
	qRes := c.FindOne(context.TODO(), filter)
	if err := qRes.Err(); err != nil {
		return res, err
	}

	err := qRes.Decode(&res)
	return res, err
}

func (c *contracts) FindContracts(contractId *string, code *string, offset int, limit int) ([]Contract, error) {
	filters := bson.D{}
	if contractId != nil {
		filters = append(filters, bson.E{Key: "id", Value: *contractId})
	}
	if code != nil {
		filters = append(filters, bson.E{Key: "code", Value: *code})
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
	findQuery := bson.M{
		"id": contractId,
	}
	updateQuery := bson.M{
		"$set": bson.M{
			"code":            args.Code,
			"name":            args.Name,
			"description":     args.Description,
			"creator":         args.Creator,
			"owner":           args.Owner,
			"tx_id":           args.TxId,
			"creation_height": args.CreationHeight,
			"runtime":         args.Runtime,
		},
	}
	opts := options.FindOneAndUpdate().SetUpsert(true)
	c.FindOneAndUpdate(context.Background(), findQuery, updateQuery, opts)
}

func (c *contracts) UpdateContract(contractId string, args Contract) {
	findQuery := bson.M{
		"id": contractId,
	}
	updateQuery := bson.M{
		"$set": bson.M{
			"code":        args.Code,
			"name":        args.Name,
			"description": args.Description,
			"owner":       args.Owner,
			"runtime":     args.Runtime,
		},
	}
	opts := options.FindOneAndUpdate().SetUpsert(true)
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

func (ch *contractState) FindOutputs(id *string, input *string, contract *string, offset int, limit int) ([]ContractOutput, error) {
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

type contractUpdates struct {
	*db.Collection
}

func NewContractUpdates(d *vsc.VscDb) ContractUpdates {
	return &contractUpdates{db.NewCollection(d.DbInstance, "contract_updates")}
}

func (c *contractUpdates) Init() error {
	err := c.Collection.Init()
	if err != nil {
		return err
	}

	indexModel := mongo.IndexModel{
		Keys: bson.D{{Key: "contract_id", Value: 1}, {Key: "block_height", Value: -1}},
	}

	// create index on block.block_number for faster queries
	err = c.CreateIndexIfNotExist(indexModel)
	if err != nil {
		return fmt.Errorf("failed to create index: %w", err)
	}

	return nil
}

func (c *contractUpdates) Append(contractId string, txId string, height int64, owner string, code string) {
	// TODO: provide an option in config to skip this for most nodes that don't need to query it
	findQuery := bson.M{
		"_id": txId,
	}
	updateQuery := bson.M{
		"$set": bson.M{
			"contract_id":  contractId,
			"block_height": height,
			"owner":        owner,
			"code":         code,
		},
	}
	opts := options.FindOneAndUpdate().SetUpsert(true)
	c.FindOneAndUpdate(context.Background(), findQuery, updateQuery, opts)
}

func (c *contractUpdates) GetUpdatesByContractId(contractId string, offset int, limit int) ([]ContractUpdate, error) {
	if contractId == "" {
		return nil, fmt.Errorf("contract id must be specified")
	}
	filters := bson.D{{Key: "contract_id", Value: contractId}}
	pipe := hive_blocks.GetAggTimestampPipeline(filters, "block_height", "ts", offset, limit)
	cursor, err := c.Aggregate(context.TODO(), pipe)
	if err != nil {
		return []ContractUpdate{}, err
	}
	defer cursor.Close(context.TODO())
	var results []ContractUpdate
	for cursor.Next(context.TODO()) {
		var elem ContractUpdate
		if err := cursor.Decode(&elem); err != nil {
			return []ContractUpdate{}, err
		}
		results = append(results, elem)
	}
	return results, nil
}
