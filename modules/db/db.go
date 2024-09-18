package db

import (
	"context"
	"os"
	a "vsc-node/modules/aggregate"

	"github.com/FerretDB/FerretDB/ferretdb"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Db struct {
	db     *ferretdb.FerretDB
	cancel context.CancelFunc
	*mongo.Client
}

var _ a.Plugin = &Db{}

func New() *Db {
	return &Db{}
}

func (db *Db) Init() error {
	err := os.MkdirAll("data/", os.ModeDir)
	if err != nil {
		return err
	}
	d, err := ferretdb.New(&ferretdb.Config{
		Handler:   "sqlite",
		SQLiteURL: "file:data/",
		Listener: ferretdb.ListenerConfig{
			TCP: "127.0.0.1:9999",
		},
	})
	if err != nil {
		return err
	}
	db.db = d
	return nil
}

func (db *Db) Start() error {
	ctx, cancel := context.WithCancel(context.Background())
	db.cancel = cancel
	go db.db.Run(ctx)
	driver, err := mongo.Connect(ctx, options.Client().ApplyURI(db.db.MongoDBURI()))
	if err != nil {
		cancel()
		return err
	}
	db.Client = driver
	return nil
}

func (db *Db) Stop() error {
	db.cancel()
	return nil // TODO grab error from db.Run()
}
