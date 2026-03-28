package db

import (
	"context"
	"vsc-node/lib/utils"
	a "vsc-node/modules/aggregate"

	"github.com/chebyrash/promise"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type DbInstance struct {
	*mongo.Database

	db   Db
	conf DbConfig
	opts []*options.DatabaseOptions
}

var _ a.Plugin = &DbInstance{}

func NewDbInstance(db Db, conf DbConfig, opts ...*options.DatabaseOptions) *DbInstance {
	return &DbInstance{
		nil,
		db,
		conf,
		opts,
	}
}

// Init implements aggregate.Plugin.
func (d *DbInstance) Init() error {
	d.Database = d.db.Database(d.conf.GetDbName(), d.opts...)
	return nil
}

// Start implements aggregate.Plugin.
func (d *DbInstance) Start() *promise.Promise[any] {
	return utils.PromiseResolve[any](nil)
}

// Stop implements aggregate.Plugin.
func (d *DbInstance) Stop() error {
	return nil
}

func (d *DbInstance) Clear() error {
	return d.Drop(context.TODO())
}
