package db

import (
	"context"
	"os"
	a "vsc-node/modules/aggregate"

	"github.com/FerretDB/FerretDB/ferretdb"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Db interface {
	Database(name string, opts ...*options.DatabaseOptions) *mongo.Database
}
type db struct {
	db     *ferretdb.FerretDB
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

func (db *db) Start() error {
	ctx, cancel := context.WithCancel(context.Background())
	db.cancel = cancel
	go func() {
		db.err <- db.db.Run(ctx)
	}()
	driver, err := mongo.Connect(ctx, options.Client().ApplyURI(db.db.MongoDBURI()))
	if err != nil {
		cancel()
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
