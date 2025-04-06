package hive_blocks

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
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

	return hiveBlocks, nil
}

// CompareIndexOptions compares index options between two IndexModels
func compareIndexOptions(existingOptions *mongo.IndexSpecification, newOptions *options.IndexOptions) bool {
	// Compare options like Unique, Sparse, etc.
	// If any of the options don't match, return false
	if existingOptions == nil && newOptions == nil {
		return true
	}

	if existingOptions == nil || newOptions == nil {
		return false
	}

	// Compare relevant fields (add more as needed)
	if existingOptions.Unique != nil && newOptions.Unique != nil && *existingOptions.Unique != *newOptions.Unique {
		return false
	}

	if existingOptions.Sparse != nil && newOptions.Sparse != nil && *existingOptions.Sparse != *newOptions.Sparse {
		return false
	}

	if existingOptions.ExpireAfterSeconds != nil && newOptions.ExpireAfterSeconds != nil && *existingOptions.ExpireAfterSeconds != *newOptions.ExpireAfterSeconds {
		return false
	}

	// Add more option comparisons as needed

	return true
}

func createIndexIfNotExist(collection *mongo.Collection, indexModel mongo.IndexModel) error {
	// List all index specifications
	indexes, err := collection.Indexes().ListSpecifications(context.Background())
	if err != nil {
		return fmt.Errorf("failed to list index specifications: %v", err)
	}

	keys, err := bson.Marshal(indexModel.Keys)
	if err != nil {
		return fmt.Errorf("failed to marshal index model: %v", err)
	}

	// Check if the index already exists and matches the options
	var indexExists bool
	for _, existingIndex := range indexes {
		// Compare the index keys
		existingKeys, err := bson.Marshal(existingIndex.KeysDocument)
		if err != nil {
			return fmt.Errorf("failed to marshal existing index key doc: %v", err)
		}

		if slices.Equal(existingKeys, keys) {
			// Compare the options of the index
			if compareIndexOptions(existingIndex, indexModel.Options) {
				indexExists = true
				break
			}
		}
	}

	// If the index doesn't exist, create it
	if !indexExists {
		_, err := collection.Indexes().CreateOne(context.Background(), indexModel)
		if err != nil {
			return fmt.Errorf("failed to create index: %v", err)
		}
	}

	return nil
}

func (blocks *hiveBlocks) Init() error {
	err := blocks.Collection.Init()
	if err != nil {
		return err
	}

	indexModel := mongo.IndexModel{
		Keys:    bson.D{{Key: "block.block_number", Value: 1}}, // ascending order
		Options: options.Index().SetUnique(true),               // not unique
	}

	// create index on block.block_number for faster queries
	err = createIndexIfNotExist(blocks.Collection.Collection, indexModel)
	if err != nil {
		return fmt.Errorf("failed to create index: %w", err)
	}

	return nil
}

// stores a block and updates the last stored block atomically without txs
func (h *hiveBlocks) StoreBlocks(headBlock uint64, blocks ...HiveBlock) error {

	if len(blocks) == 0 {
		return fmt.Errorf("empty blocks")
	}
	models := make([]mongo.WriteModel, len(blocks)+1) // space for all hive blocks + the metadata doc

	h.writeMutex.Lock()
	defer h.writeMutex.Unlock()
	for i, block := range blocks {
		models[i] = mongo.NewUpdateOneModel().SetFilter(bson.M{
			"block.block_number": block.BlockNumber,
		}).SetUpdate(bson.M{
			"$set": Document{
				Type:  DocumentTypeHiveBlock,
				Block: &block,
			},
		}).SetUpsert(true)
	}

	models[len(models)-1] = mongo.NewUpdateOneModel().
		SetFilter(Document{Type: DocumentTypeMetadata}).
		SetUpdate(bson.M{"$set": Document{
			Type:            DocumentTypeMetadata,
			LastStoredBlock: &blocks[len(blocks)-1].BlockNumber,
			HeadHeight:      &headBlock,
		}}).
		SetUpsert(true)
	bulkWriteOptions := options.BulkWrite().SetOrdered(true)

	// execute the bulk write op
	_, err := h.Collection.BulkWrite(context.Background(), models, bulkWriteOptions)
	if err == nil {
		return nil
	}
	return fmt.Errorf("failed to perform bulk write after retries: %w", err)
}

// retrieves blocks in a specified range, ordered by block number
func (h *hiveBlocks) FetchStoredBlocks(startBlock, endBlock uint64) ([]HiveBlock, error) {
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

func (h *hiveBlocks) FetchNextBlocks(startBlock uint64) (<-chan HiveBlock, error) {
	ctx := context.Background()
	filter := Document{
		Type: DocumentTypeHiveBlock,
	}

	findOptions := options.Find().SetSort(bson.D{{Key: "block.block_number", Value: 1}})
	cursor, err := h.Collection.Find(ctx, filter, findOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch blocks: %w", err)
	}

	blocks := make(chan HiveBlock)

	go func() {
		defer cursor.Close(ctx)

		for cursor.Next(ctx) {
			var result Document
			if err := cursor.Decode(&result); err != nil {
				break
			}

			blocks <- *result.Block
		}
	}()
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("cursor error: %w", err)
	}

	return blocks, nil
}

// deletes all block and metadata documents from the collection
func (h *hiveBlocks) ClearBlocks() error {
	ctx := context.Background()

	_, err := h.Collection.DeleteMany(ctx, bson.M{})
	if err != nil {
		return fmt.Errorf("failed to clear blocks: %w", err)
	}

	return nil
}

// stores the last processed block number
func (h *hiveBlocks) StoreLastProcessedBlock(blockNumber uint64) error {
	ctx := context.Background()

	_, err := h.Collection.UpdateOne(ctx, Document{Type: DocumentTypeMetadata},
		bson.M{"$set": Document{Type: DocumentTypeMetadata, LastProcessedBlock: &blockNumber}},
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
func (h *hiveBlocks) GetLastProcessedBlock() (uint64, error) {
	var result Document
	err := h.Collection.FindOne(context.Background(), Document{Type: DocumentTypeMetadata}).Decode(&result)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to get last processed block: %w", err)
	}

	if result.LastProcessedBlock == nil {
		return 0, nil
	}
	return *result.LastProcessedBlock, nil
}

// retrieves the last stored block number
func (h *hiveBlocks) GetLastStoredBlock() (uint64, error) {
	var result Document
	err := h.Collection.FindOne(context.Background(), Document{Type: DocumentTypeMetadata}).Decode(&result)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to get last stored block: %w", err)
	}
	return *result.LastStoredBlock, nil
}

func (h *hiveBlocks) GetHighestBlock() (uint64, error) {
	findOptions := options.FindOne().SetSort(bson.D{{Key: "block.block_number", Value: -1}})
	var result Document
	err := h.Collection.FindOne(context.Background(), Document{Type: DocumentTypeHiveBlock}, findOptions).Decode(&result)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return 0, nil // no blocks stored yet
		}
		return 0, fmt.Errorf("failed to get highest block: %w", err)
	}

	return result.Block.BlockNumber, nil
}

func (h *hiveBlocks) ListenToBlockUpdates(ctx context.Context, startBlock uint64, listener func(block HiveBlock, headBlock uint64) error) (context.CancelFunc, <-chan error) {
	startBlock--
	ctx, cancel := context.WithCancel(ctx)
	errChan := make(chan error)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:

				metadata := Document{}
				metadataFind := h.FindOne(ctx, Document{Type: DocumentTypeMetadata})

				err := metadataFind.Decode(&metadata)
				if err != nil {
					errChan <- err
					return
				}

				// TODO mongo.ErrNoDocuments
				cur, err := h.Find(ctx, bson.M{
					"type":               DocumentTypeHiveBlock,
					"block.block_number": bson.M{"$gt": startBlock},
				})
				if err != nil {
					errChan <- err
					return
				}

				for cur.Next(ctx) {
					doc := Document{}
					err = cur.Decode(&doc)
					if err != nil {
						errChan <- err
						return
					}
					startBlock = doc.Block.BlockNumber
					err = listener(*doc.Block, *metadata.HeadHeight)
					if err != nil {
						errChan <- err
						return
					}
				}
				err = cur.Err()
				if err != nil {
					errChan <- err
					return
				}
			}
		}
	}()
	return cancel, errChan
}

func (h *hiveBlocks) GetBlock(blockNum uint64) (HiveBlock, error) {
	var result Document
	err := h.FindOne(context.Background(), bson.M{
		"block.block_number": blockNum,
	}).Decode(&result)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return HiveBlock{}, mongo.ErrNoDocuments
		}
		return HiveBlock{}, fmt.Errorf("failed to get block: %w", err)
	}

	return *result.Block, nil
}

func (h *hiveBlocks) SetMetadata(doc Document) error {
	ctx := context.Background()

	_, err := h.Collection.UpdateOne(ctx, Document{Type: DocumentTypeMetadata},
		bson.M{"$set": doc},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("failed to set metadata: %w", err)
	}
	return nil
}

func (h *hiveBlocks) GetMetadata() (Document, error) {
	var result Document
	err := h.Collection.FindOne(context.Background(), Document{Type: DocumentTypeMetadata}).Decode(&result)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return Document{}, nil
		}
		return Document{}, fmt.Errorf("failed to get metadata: %w", err)
	}

	return result, nil
}
