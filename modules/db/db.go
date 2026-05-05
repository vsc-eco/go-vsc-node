package db

import (
	"context"
	"vsc-node/lib/utils"
	a "vsc-node/modules/aggregate"

	"github.com/chebyrash/promise"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

const DefaultReplicaSet = "rs0"

type Db interface {
	Database(name string, opts ...*options.DatabaseOptions) *mongo.Database
	// StartSession exposes the underlying client so the state engine can
	// open a session for slot-scoped multi-document transactions.
	StartSession(opts ...*options.SessionOptions) (mongo.Session, error)
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

	clientOpts := options.Client().ApplyURI(db.conf.Get().DbURI)
	// Slot processing relies on multi-document transactions, which require
	// the deployment to be a replica set. Default to "rs0" unless the URI
	// already specifies one. Majority write concern keeps committed slot
	// state durable even on stepdown of a future multi-node deployment.
	if clientOpts.ReplicaSet == nil {
		rs := DefaultReplicaSet
		clientOpts.ReplicaSet = &rs
	}
	if clientOpts.WriteConcern == nil {
		clientOpts.SetWriteConcern(writeconcern.Majority())
	}

	c, err := mongo.Connect(ctx, clientOpts)
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
