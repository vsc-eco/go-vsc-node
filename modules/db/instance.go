package db

import (
	a "vsc-node/modules/aggregate"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type DbInstance struct {
	*mongo.Database
}

var _ a.Plugin = &DbInstance{}

func NewDbInstance(db Db, name string, opts ...*options.DatabaseOptions) *DbInstance {
	return &DbInstance{
		db.Database(name, opts...),
	}
}

// Init implements aggregate.Plugin.
func (d *DbInstance) Init() error {
	return nil
}

// Start implements aggregate.Plugin.
func (d *DbInstance) Start() error {
	return nil
}

// Stop implements aggregate.Plugin.
func (d *DbInstance) Stop() error {
	return nil
}
