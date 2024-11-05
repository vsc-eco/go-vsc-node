package hive_blocks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type hiveBlocks struct {
	*db.Collection
	writeMutex sync.Mutex // mutex for synchronizing writes, else SQLite will be busy
}

// a unique UUID prefix to avoid collisions when we convert nested arrays
// into BSON-compatible structures and need to name fields with unique keys
const fieldPrefix = "69ba102f-c815-4ce9-8022-90e520fe8516_"

// transforms nested arrays into BSON-compatible structures with unique keys
func makeBSONCompatible(value interface{}) interface{} {
	switch v := value.(type) {
	case []interface{}:
		// check if the array contains other arrays
		isArrayOfArrays := false
		for _, item := range v {
			switch item.(type) {
			case []interface{}, primitive.A:
				isArrayOfArrays = true
			}
		}
		if isArrayOfArrays {
			// convert each inner array into a map with unique keys
			arr := make([]interface{}, len(v))
			for i, elem := range v {
				switch innerArray := elem.(type) {
				case []interface{}:
					innerMap := make(map[string]interface{})
					for idx, elem := range innerArray {
						innerMap[fmt.Sprintf("%s%d", fieldPrefix, idx)] = makeBSONCompatible(elem)
					}
					arr[i] = innerMap
				case primitive.A:
					innerArrayConverted := []interface{}(innerArray)
					innerMap := make(map[string]interface{})
					for idx, elem := range innerArrayConverted {
						innerMap[fmt.Sprintf("%s%d", fieldPrefix, idx)] = makeBSONCompatible(elem)
					}
					arr[i] = innerMap
				default:
					arr[i] = makeBSONCompatible(elem)
				}
			}
			return arr
		} else {
			// process elems recursively
			arr := make([]interface{}, len(v))
			for i, item := range v {
				arr[i] = makeBSONCompatible(item)
			}
			return arr
		}
	case primitive.A:
		// convert primitive.A to []interface{} and process recursively
		return makeBSONCompatible([]interface{}(v))
	case map[string]interface{}:
		m := make(map[string]interface{})
		for k, val := range v {
			m[k] = makeBSONCompatible(val)
		}
		return m
	case primitive.M:
		// convert primitive.M to map[string]interface{} and process recursively
		return makeBSONCompatible(map[string]interface{}(v))
	case primitive.D:
		// convert primitive.D to map[string]interface{} and process recursively
		m := make(map[string]interface{})
		for _, elem := range v {
			m[elem.Key] = makeBSONCompatible(elem.Value)
		}
		return m
	default:
		return v
	}
}

// restores bson-compatible structures back to the original nested arrays
func remakeOriginalNestedArrayStructure(value interface{}) interface{} {
	switch v := value.(type) {
	case []interface{}:
		// process elements recursively
		arr := make([]interface{}, len(v))
		for i, item := range v {
			arr[i] = remakeOriginalNestedArrayStructure(item)
		}
		return arr
	case primitive.A:
		// convert to []interface{} and process recursively
		return remakeOriginalNestedArrayStructure([]interface{}(v))
	case map[string]interface{}:
		if isFieldKeys(v) {
			// reconstruct array from map
			innerArr := []interface{}{}
			for idx := 0; ; idx++ {
				key := fmt.Sprintf("%s%d", fieldPrefix, idx)
				val, exists := v[key]
				if !exists {
					break
				}
				innerArr = append(innerArr, remakeOriginalNestedArrayStructure(val))
			}
			return innerArr
		} else {
			// process map elements recursively
			m := make(map[string]interface{})
			for k, val := range v {
				m[k] = remakeOriginalNestedArrayStructure(val)
			}
			return m
		}
	case primitive.M:
		// convert primitive.M to map[string]interface{} and process recursively
		return remakeOriginalNestedArrayStructure(map[string]interface{}(v))
	case primitive.D:
		// convert primitive.D to map[string]interface{} and process recursively
		m := make(map[string]interface{})
		for _, elem := range v {
			m[elem.Key] = remakeOriginalNestedArrayStructure(elem.Value)
		}
		return m
	default:
		return v
	}
}

// checks if a map has keys that start with the unique field prefix
func isFieldKeys(m map[string]interface{}) bool {
	if len(m) == 0 {
		return false
	}
	for key := range m {
		if !strings.HasPrefix(key, fieldPrefix) {
			return false
		}
	}
	return true
}

func New(d *vsc.VscDb) (HiveBlocks, error) {
	hiveBlocks := &hiveBlocks{db.NewCollection(d.DbInstance, "hive_blocks"), sync.Mutex{}}

	indexModel := mongo.IndexModel{
		Keys:    bson.D{{Key: "block.block_number", Value: 1}}, // ascending order
		Options: options.Index().SetUnique(false),              // not unique
	}

	// create index on block.block_number for faster queries
	_, err := hiveBlocks.Collection.Indexes().CreateOne(context.Background(), indexModel)
	if err != nil {
		return nil, fmt.Errorf("failed to create index: %w", err)
	}

	return hiveBlocks, nil
}

// stores a block and updates the last stored block atomically without txs
func (h *hiveBlocks) StoreBlock(block *HiveBlock) error {
	h.writeMutex.Lock()
	defer h.writeMutex.Unlock()

	if block == nil {
		return fmt.Errorf("block is nil")
	}

	// convert block to map[string]interface{}
	var blockMap map[string]interface{}
	blockJSON, err := json.Marshal(block)
	if err != nil {
		return fmt.Errorf("failed to marshal block to JSON: %w", err)
	}
	err = json.Unmarshal(blockJSON, &blockMap)
	if err != nil {
		return fmt.Errorf("failed to unmarshal block JSON: %w", err)
	}

	// process map to make it BSON compatible
	processedBlock := makeBSONCompatible(blockMap)

	// create ordered bulk write models
	models := []mongo.WriteModel{
		mongo.NewInsertOneModel().SetDocument(bson.M{
			"type":  "block",
			"block": processedBlock,
		}),
		mongo.NewUpdateOneModel().
			SetFilter(bson.M{"type": "metadata"}).
			SetUpdate(bson.M{"$set": bson.M{"last_stored_block": block.BlockNumber}}).
			SetUpsert(true),
	}

	bulkWriteOptions := options.BulkWrite().SetOrdered(true)

	// execute the bulk write op
	_, err = h.Collection.BulkWrite(context.Background(), models, bulkWriteOptions)
	if err == nil {
		return nil
	}
	return fmt.Errorf("failed to perform bulk write after retries: %w", err)
}

// retrieves blocks in a specified range, ordered by block number
func (h *hiveBlocks) FetchStoredBlocks(startBlock, endBlock int) ([]HiveBlock, error) {
	ctx := context.Background()
	filter := bson.M{
		"type": "block",
		"block.block_number": bson.M{
			"$gte": startBlock,
			"$lte": endBlock,
		},
	}

	findOptions := options.Find().SetSort(bson.D{{Key: "block.block_number", Value: 1}})
	cursor, err := h.Collection.Find(ctx, filter, findOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch blocks: %w", err)
	}
	defer cursor.Close(ctx)

	var blocks []HiveBlock

	for cursor.Next(ctx) {
		var result bson.M
		if err := cursor.Decode(&result); err != nil {
			return nil, fmt.Errorf("failed to decode block: %w", err)
		}

		// extract the stored block data
		blockDataRaw, ok := result["block"]
		if !ok {
			return nil, fmt.Errorf("invalid block data")
		}

		// reconstruct the original block data
		reconstructedData := remakeOriginalNestedArrayStructure(blockDataRaw)

		// convert our reconstructed data to JSON
		blockJSON, err := json.Marshal(reconstructedData)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal reconstructed block: %w", err)
		}

		// unmarshal JSON into HiveBlock struct
		var block HiveBlock
		if err := json.Unmarshal(blockJSON, &block); err != nil {
			return nil, fmt.Errorf("failed to unmarshal block JSON: %w", err)
		}

		blocks = append(blocks, block)
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("cursor error: %w", err)
	}

	return blocks, nil
}

// deletes all block and metadata documents from the collection
func (h *hiveBlocks) ClearBlocks() error {
	ctx := context.Background()
	h.writeMutex.Lock()
	defer h.writeMutex.Unlock()

	_, err := h.Collection.DeleteMany(ctx, bson.M{"type": "block"})
	if err != nil {
		return fmt.Errorf("failed to clear blocks: %w", err)
	}

	_, err = h.Collection.DeleteMany(ctx, bson.M{"type": "metadata"})
	if err != nil {
		return fmt.Errorf("failed to clear metadata: %w", err)
	}

	return nil
}

// stores the last processed block number
func (h *hiveBlocks) StoreLastProcessedBlock(blockNumber int) error {
	ctx := context.Background()
	h.writeMutex.Lock()
	defer h.writeMutex.Unlock()

	_, err := h.Collection.UpdateOne(ctx, bson.M{"type": "metadata"},
		bson.M{"$set": bson.M{"last_processed_block": blockNumber}},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("failed to store last processed block: %w", err)
	}
	return nil
}

// retrieves the last processed block number
//
// returns -1 if no blocks have been processed yet
func (h *hiveBlocks) GetLastProcessedBlock() (int, error) {
	var result bson.M
	err := h.Collection.FindOne(context.Background(), bson.M{"type": "metadata"}).Decode(&result)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return -1, nil
		}
		return 0, fmt.Errorf("failed to get last processed block: %w", err)
	}

	blockNumberRaw, exists := result["last_processed_block"]
	if !exists {
		// field does not exist! thus, no blocks have been processed yet
		return -1, nil
	}

	blockNumber, ok := blockNumberRaw.(int32)
	if !ok {
		return 0, fmt.Errorf("failed to parse block number")
	}

	return int(blockNumber), nil
}

// retrieves the last stored block number
func (h *hiveBlocks) GetLastStoredBlock() (int, error) {
	var result struct {
		BlockNumber int `bson:"last_stored_block"`
	}
	err := h.Collection.FindOne(context.Background(), bson.M{"type": "metadata"}).Decode(&result)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to get last stored block: %w", err)
	}
	return result.BlockNumber, nil
}

func (h *hiveBlocks) GetHighestBlock() (int, error) {
	findOptions := options.FindOne().SetSort(bson.D{{Key: "block.block_number", Value: -1}})
	var result bson.M
	err := h.Collection.FindOne(context.Background(), bson.M{"type": "block"}, findOptions).Decode(&result)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return 0, nil // no blocks stored yet
		}
		return 0, fmt.Errorf("failed to get highest block: %w", err)
	}

	// extracts block.block_number
	blockData, ok := result["block"].(bson.M)
	if !ok {
		// attempt other possible types
		blockDataMap, ok := result["block"].(map[string]interface{})
		if !ok {
			return 0, fmt.Errorf("failed to get highest block: 'block' field missing or invalid")
		}
		blockData = bson.M(blockDataMap)
	}

	blockNumberRaw, ok := blockData["block_number"]
	if !ok {
		return 0, fmt.Errorf("failed to get highest block: 'block_number' field missing in block")
	}

	blockNumber, ok := blockNumberRaw.(float64)
	if !ok {
		return 0, fmt.Errorf("failed to get highest block: 'block_number' field is not a float64")
	}

	return int(blockNumber), nil
}
