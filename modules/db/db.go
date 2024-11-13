package db

import (
	"context"
	a "vsc-node/modules/aggregate"
	"vsc-node/modules/config"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Db interface {
	Database(name string, opts ...*options.DatabaseOptions) *mongo.Database
}
type db struct {
	conf   *config.Config[dbConfig]
	cancel context.CancelFunc
	*mongo.Client
}

var _ a.Plugin = &db{}
var _ Db = &db{}

func New(conf *config.Config[dbConfig]) *db {
	return &db{conf: conf}
}

func (db *db) Init() error {
	ctx, cancel := context.WithCancel(context.Background())
	db.cancel = cancel

	c, err := mongo.Connect(ctx, options.Client().ApplyURI(db.conf.Get().DbURI))
	if err != nil {
		return err
	}
	db.Client = c

	return nil
}

func (db *db) Start() error {
	return nil
}

func (db *db) Stop() error {
	db.cancel()
	return nil
}
