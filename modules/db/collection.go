package db

import (
	a "vsc-node/modules/aggregate"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Collection struct {
	*mongo.Collection
}

var _ a.Plugin = &Collection{}

func NewCollection(db *DbInstance, name string, opts ...*options.CollectionOptions) *Collection {
	return &Collection{
		db.Collection(name, opts...),
	}
}

// Init implements aggregate.Plugin.
func (c *Collection) Init() error {
	return nil
}

// Start implements aggregate.Plugin.
func (c *Collection) Start() error {
	return nil
}

// Stop implements aggregate.Plugin.
func (c *Collection) Stop() error {
	return nil
}
