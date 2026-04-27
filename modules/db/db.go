package db

import (
	"context"
	"vsc-node/lib/utils"
	a "vsc-node/modules/aggregate"

	"github.com/chebyrash/promise"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

type Db interface {
	Database(name string, opts ...*options.DatabaseOptions) *mongo.Database
	WithSlotTransaction(ctx context.Context, fn func(sessCtx mongo.SessionContext) error) error
}
type db struct {
	conf   DbConfig
	cancel context.CancelFunc
	*mongo.Client
}

var _ a.Plugin = &db{}
var _ Db = &db{}

func New(conf DbConfig) *db {
	return &db{conf: conf}
}

func (db *db) Init() error {
	ctx, cancel := context.WithCancel(context.Background())
	db.cancel = cancel

	c, err := mongo.Connect(ctx, options.Client().ApplyURI(db.conf.Get().DbURI))
	if err != nil {
		return err
	}
	err = c.Ping(ctx, nil)
	if err != nil {
		return err
	}
	db.Client = c

	return nil
}

func (db *db) Start() *promise.Promise[any] {
	return utils.PromiseResolve[any](nil)
}

func (db *db) Stop() error {
	db.cancel()
	return nil
}

// WithSlotTransaction runs fn inside a MongoDB transaction with snapshot read concern
// and majority write concern. Used to commit slot-end writes (balances, RC, nonces,
// TSS key transitions) atomically. Requires a replica set deployment.
func (db *db) WithSlotTransaction(ctx context.Context, fn func(sessCtx mongo.SessionContext) error) error {
	txnOpts := options.Transaction().
		SetReadConcern(readconcern.Snapshot()).
		SetWriteConcern(writeconcern.Majority())
	return db.Client.UseSession(ctx, func(sessCtx mongo.SessionContext) error {
		_, err := sessCtx.WithTransaction(sessCtx, func(sessCtx mongo.SessionContext) (interface{}, error) {
			return nil, fn(sessCtx)
		}, txnOpts)
		return err
	})
}
