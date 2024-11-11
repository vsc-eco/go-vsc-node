package db

import (
	"context"
	a "vsc-node/modules/aggregate"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Db interface {
	Database(name string, opts ...*options.DatabaseOptions) *mongo.Database
}
type db struct {
	cancel context.CancelFunc
	err    chan error
	*mongo.Client
}

var _ a.Plugin = &db{}
var _ Db = &db{}

func New() *db {
	return &db{err: make(chan error, 1)}
}

func (db *db) Init() error {
	return nil
}

func (db *db) Start() error {
	ctx := context.Background()

	driver, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017"))
	if err != nil {
		return err
	}
	db.Client = driver
	return nil
}

func (db *db) Stop() error {
	db.cancel()
	err := <-db.err
	if err == context.Canceled {
		return nil
	}
	return err
}
