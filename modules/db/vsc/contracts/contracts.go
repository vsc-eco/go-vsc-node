package contracts

import (
	"context"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/hive_blocks"

	"go.mongodb.org/mongo-driver/bson"
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
func (c *contracts) ContractById(contractId string) (SetContractArgs, error) {
	res := SetContractArgs{}
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

func (c *contracts) FindContracts(contractId *string, code *string, offset int, limit int) ([]SetContractArgs, error) {
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
		return []SetContractArgs{}, err
	}
	defer cursor.Close(context.TODO())
	var results []SetContractArgs
	for cursor.Next(context.TODO()) {
		var elem SetContractArgs
		if err := cursor.Decode(&elem); err != nil {
			return []SetContractArgs{}, err
		}
		results = append(results, elem)
	}
	return results, nil
}

func (c *contracts) RegisterContract(contractId string, args SetContractArgs) {
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

func (ch *contractState) GetLastOutput(contractId string, height uint64) *ContractOutput {
	options := options.FindOne().SetSort(bson.M{"block_height": -1})
	findResult := ch.FindOne(context.Background(), bson.M{"contract_id": contractId, "block_height": bson.M{
		"$lte": height,
	}}, options)
	if findResult.Err() != nil {
		return nil
	}
	contractOutput := ContractOutput{}
	findResult.Decode(&contractOutput)
	return &contractOutput
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

func NewContractState(d *vsc.VscDb) ContractState {
	return &contractState{db.NewCollection(d.DbInstance, "contract_state")}
}
