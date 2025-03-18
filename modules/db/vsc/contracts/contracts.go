package contracts

import (
	"context"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

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
			"inputs":       output.Inputs,
			"contract_id":  output.ContractId,
			"state_merkle": output.StateMerkle,
			"results":      output.Results,
			"block_height": output.AnchoredHeight,
		},
	}, options)

}

func (ch *contractState) GetLastOutput(contractId string, height int) *ContractOutput {
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
