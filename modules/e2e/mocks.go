package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"vsc-node/modules/db/vsc/hive_blocks"

	"github.com/chebyrash/promise"
	"github.com/vsc-eco/hivego"
)

type MockHiveDbs struct {
	blocks map[uint64]*hive_blocks.HiveBlock

	HighestBlock uint64
}

func (m *MockHiveDbs) StoreBlocks(processedBlk uint64, blocks ...hive_blocks.HiveBlock) error {
	for _, block := range blocks {
		m.blocks[block.BlockNumber] = &block
	}
	return nil
}

func (m *MockHiveDbs) ClearBlocks() error {
	return nil
}

func (m *MockHiveDbs) StoreLastProcessedBlock(blockNumber uint64) error {
	return nil
}

func (m *MockHiveDbs) GetLastProcessedBlock() (uint64, error) {
	return 0, nil
}

func (m *MockHiveDbs) FetchStoredBlocks(startBlock uint64, endBlock uint64) ([]hive_blocks.HiveBlock, error) {
	return nil, nil
}

func (m *MockHiveDbs) ListenToBlockUpdates(ctx context.Context, startBlock uint64, listener func(block hive_blocks.HiveBlock, heightHead *uint64) error) (context.CancelFunc, <-chan error) {
	return nil, nil
}

func (m *MockHiveDbs) GetHighestBlock() (uint64, error) {
	return m.HighestBlock, nil
}

func (m *MockHiveDbs) GetBlock(blockNum uint64) (hive_blocks.HiveBlock, error) {
	if m.blocks[blockNum] == nil {
		return hive_blocks.HiveBlock{}, errors.New("block not found")
	}
	return *m.blocks[blockNum], nil
}

func (m *MockHiveDbs) GetMetadata() (hive_blocks.Document, error) {
	return hive_blocks.Document{}, nil
}

func (m *MockHiveDbs) SetMetadata(doc hive_blocks.Document) error {
	return nil
}

func (m *MockHiveDbs) Init() error {
	m.blocks = make(map[uint64]*hive_blocks.HiveBlock)
	return nil
}

func (m *MockHiveDbs) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(interface{}), reject func(error)) {
		resolve(nil)
	})
}

func (m *MockHiveDbs) Stop() error {
	return nil
}

var _ hive_blocks.HiveBlocks = &MockHiveDbs{}

func TransformTx(tx hivego.HiveTransaction) []hivego.Operation {
	var insertOps []hivego.Operation
	for _, op := range tx.Operations {
		opName := op.OpName()

		//Prepass, convert to flat map[string]interface{}
		var Value map[string]interface{}
		bval, _ := json.Marshal(op)
		json.Unmarshal(bval, &Value)

		// Do operation specific parsing to match hivego.Operation format
		if opName == "transfer" || opName == "transfer_from_savings" || opName == "transfer_to_savings" {
			rawAmount := Value["amount"].(string)

			splitAmt := strings.Split(rawAmount, " ")
			var nai string
			if splitAmt[1] == "HBD" {
				nai = "@@000000013"
			} else if splitAmt[1] == "HIVE" {
				nai = "@@000000021"
			}

			amtFloat, _ := strconv.ParseFloat(splitAmt[0], 64)

			//3 decimal places.
			amount := map[string]interface{}{
				"nai":       nai,
				"amount":    strconv.Itoa(int(amtFloat * 1000)),
				"precision": 3,
			}

			Value["amount"] = amount
		}
		//Probably not needed? Add more specific parsers as required

		insertOps = append(insertOps, hivego.Operation{
			Type:  opName,
			Value: Value,
		})
	}

	return insertOps
}
