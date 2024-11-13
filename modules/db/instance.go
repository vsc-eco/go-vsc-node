package db

import (
	a "vsc-node/modules/aggregate"

	"github.com/chebyrash/promise"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type DbInstance struct {
	*mongo.Database

	db   Db
	name string
	opts []*options.DatabaseOptions
}

var _ a.Plugin = &DbInstance{}

func NewDbInstance(db Db, name string, opts ...*options.DatabaseOptions) *DbInstance {
	return &DbInstance{
		nil,
		db,
		name,
		opts,
	}
}

// Init implements aggregate.Plugin.
func (d *DbInstance) Init() error {
	d.Database = d.db.Database(d.name, d.opts...)
	return nil
}

// Start implements aggregate.Plugin.
func (d *DbInstance) Start() *promise.Promise[any] {
	return nil
}

// Stop implements aggregate.Plugin.
func (d *DbInstance) Stop() error {
	return nil
}
