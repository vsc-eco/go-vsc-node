package hive_blocks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type hiveBlocks struct {
	*db.Collection
}

// a unique UUID prefix to avoid collisions for when we convert nested arrays
// into BSON-compatible structures and need to name fields and have unique keys
const fieldPrefix = "69ba102f-c815-4ce9-8022-90e520fe8516_"

// transforms nested arrays into BSON-compatible structures with unique keys
//
// we need to do this because mongoDB freaks out if you try to insert nested arrays which
// some blocks seem to have
func makeBSONCompatible(value interface{}) interface{} {
	switch v := value.(type) {
	case []interface{}:
		// check if elem is an array
		isArrayOfArrays := false
		for _, item := range v {
			if _, ok := item.([]interface{}); ok {
				isArrayOfArrays = true
				break
			}
		}
		if isArrayOfArrays {
			// convert each inner array into a document with unique keys due to our UUID prefix
			// to avoid collisions
			arr := make([]interface{}, len(v))
			for i, item := range v {
				if innerArray, ok := item.([]interface{}); ok {
					innerMap := make(map[string]interface{})
					for idx, elem := range innerArray {
						innerMap[fmt.Sprintf("%s%d", fieldPrefix, idx)] = makeBSONCompatible(elem)
					}
					arr[i] = innerMap
				} else {
					// not an array, we can process it recursively
					arr[i] = makeBSONCompatible(item)
				}
			}
			return arr
		} else {
			// process elements recursively
			arr := make([]interface{}, len(v))
			for i, item := range v {
				arr[i] = makeBSONCompatible(item)
			}
			return arr
		}
	case map[string]interface{}:
		m := make(map[string]interface{})
		for k, val := range v {
			m[k] = makeBSONCompatible(val)
		}
		return m
	default:
		return v
	}
}

// restores BSON-compatible structures back to the original nested arrays
// after we had to convert them away from the nested arrays to store them in mongoDB
func remakeOriginalNestedArrayStructure(value interface{}) interface{} {
	switch v := value.(type) {
	case []interface{}:
		if len(v) > 0 {
			if m, ok := toMap(v[0]); ok && isFieldKeys(m) {
				// re-make array of arrays
				arr := make([]interface{}, len(v))
				for i, item := range v {
					if m, ok := toMap(item); ok {
						innerArr := []interface{}{}
						for idx := 0; ; idx++ {
							key := fmt.Sprintf("%s%d", fieldPrefix, idx)
							val, exists := m[key]
							if !exists {
								break
							}
							innerArr = append(innerArr, remakeOriginalNestedArrayStructure(val))
						}
						arr[i] = innerArr
					} else {
						arr[i] = remakeOriginalNestedArrayStructure(item)
					}
				}
				return arr
			}
		}
		// process elems recursively
		arr := make([]interface{}, len(v))
		for i, item := range v {
			arr[i] = remakeOriginalNestedArrayStructure(item)
		}
		return arr
	case primitive.A:
		// convert to []interface{} and process recursively
		return remakeOriginalNestedArrayStructure([]interface{}(v))
	case map[string]interface{}, primitive.M:
		m := make(map[string]interface{})
		switch vv := v.(type) {
		case map[string]interface{}:
			for k, val := range vv {
				m[k] = remakeOriginalNestedArrayStructure(val)
			}
		case primitive.M:
			for k, val := range vv {
				m[k] = remakeOriginalNestedArrayStructure(val)
			}
		}
		return m
	case primitive.D:
		// convert doc primitive to map[string]interface{}
		m := map[string]interface{}{}
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
	// if map is empty, then not field keys
	if len(m) == 0 {
		return false
	}
	// else, for each key, check if it starts with the field prefix
	for key := range m {
		if !strings.HasPrefix(key, fieldPrefix) {
			return false
		}
	}
	return true
}

// func to convert various map types to map[string]interface{}
func toMap(value interface{}) (map[string]interface{}, bool) {
	switch v := value.(type) {
	case map[string]interface{}:
		return v, true
	case bson.M:
		return map[string]interface{}(v), true
	case primitive.D:
		m := make(map[string]interface{})
		for _, elem := range v {
			m[elem.Key] = elem.Value
		}
		return m, true
	default:
		return nil, false
	}
}

func New(d *vsc.VscDb) (HiveBlocks, error) {
	hiveBlocks := &hiveBlocks{db.NewCollection(d.DbInstance, "hive_blocks")}

	indexModel := mongo.IndexModel{
		Keys:    bson.D{{Key: "block.block_number", Value: 1}}, // ascending order
		Options: options.Index().SetUnique(false),              // not unique though!
	}

	// this index will make it (way) faster to query blocks by block number
	_, err := hiveBlocks.Collection.Indexes().CreateOne(context.Background(), indexModel)
	if err != nil {
		return nil, fmt.Errorf("failed to create index: %w", err)
	}

	return hiveBlocks, nil
}

// stores a block and updates the last processed block atomically without transactions
func (h *hiveBlocks) StoreBlock(ctx context.Context, block *HiveBlock) error {
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
			SetUpdate(bson.M{"$set": bson.M{"block_number": block.BlockNumber}}).
			SetUpsert(true),
	}

	bulkWriteOptions := options.BulkWrite().SetOrdered(true)

	// execute the bulk write operation
	_, err = h.Collection.BulkWrite(ctx, models, bulkWriteOptions)
	if err != nil {
		return fmt.Errorf("failed to perform bulk write: %w", err)
	}

	return nil
}

// retrieves blocks in a specified range, ordered by block number
func (h *hiveBlocks) FetchStoredBlocks(ctx context.Context, startBlock, endBlock int) ([]HiveBlock, error) {
	filter := bson.M{
		"type": "block",
		"block.block_number": bson.M{
			// "greater than or equal to..."
			"$gte": startBlock,
			// "less than or equal to..."
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
func (h *hiveBlocks) ClearBlocks(ctx context.Context) error {
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
func (h *hiveBlocks) StoreLastProcessedBlock(ctx context.Context, blockNumber int) error {
	_, err := h.Collection.UpdateOne(ctx, bson.M{"type": "metadata"},
		bson.M{"$set": bson.M{"block_number": blockNumber}},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("failed to store last processed block: %w", err)
	}
	return nil
}

// retrieves the last processed block number
func (h *hiveBlocks) GetLastProcessedBlock(ctx context.Context) (int, error) {
	var result struct {
		BlockNumber int `bson:"block_number"`
	}
	err := h.Collection.FindOne(ctx, bson.M{"type": "metadata"}).Decode(&result)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to get last processed block: %w", err)
	}
	return result.BlockNumber, nil
}
