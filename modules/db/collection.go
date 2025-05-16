package db

import (
	"context"
	"fmt"
	"slices"
	"vsc-node/lib/utils"
	a "vsc-node/modules/aggregate"

	"github.com/chebyrash/promise"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Collection struct {
	*mongo.Collection

	db   *DbInstance
	name string
	opts []*options.CollectionOptions
}

var _ a.Plugin = &Collection{}

func NewCollection(db *DbInstance, name string, opts ...*options.CollectionOptions) *Collection {
	return &Collection{
		nil,
		db,
		name,
		opts,
	}
}

// Init implements aggregate.Plugin.
func (c *Collection) Init() error {
	c.Collection = c.db.Collection(c.name, c.opts...)
	return nil
}

// Start implements aggregate.Plugin.
func (c *Collection) Start() *promise.Promise[any] {
	return utils.PromiseResolve[any](nil)
}

// Stop implements aggregate.Plugin.
func (c *Collection) Stop() error {
	return nil
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

func (collection *Collection) CreateIndexIfNotExist(indexModel mongo.IndexModel) error {
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
