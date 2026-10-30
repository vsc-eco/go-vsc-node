package db

import (
	"errors"
	"fmt"
	"os"
	"vsc-node/modules/config"
)

var ErrEmptyURI = errors.New("empty MongoDB URI")

const DefaultDbName = "go-vsc"

type dbConfig struct {
	DbURI  string
	DbName string
	// HafDbURI is a PostgreSQL connection string (e.g.
	// postgresql://user@host:5432/haf_block_log) pointing at a HAF database
	// holding the prefiltered Hive block log. When non-empty, blocks are pulled
	// directly from HAF's hive.irreversible_*_view instead of streamed from a
	// Hive API node; when empty, the classic Hive API streamer is used.
	HafDbURI string
}

type dbConfigStruct struct {
	*config.Config[dbConfig]
}

type DbConfig = *dbConfigStruct

func NewDbConfig(dataDir ...string) DbConfig {
	var dataDirPtr *string
	if len(dataDir) > 0 {
		dataDirPtr = &dataDir[0]
	}

	return &dbConfigStruct{config.New(dbConfig{
		DbURI:  "mongodb://localhost:27017",
		DbName: DefaultDbName,
	}, dataDirPtr)}
}

func (dc *dbConfigStruct) Init() error {
	err := dc.Config.Init()
	if err != nil {
		return err
	}

	url := os.Getenv("MONGO_URL")
	if url != "" {
		err = dc.SetDbURI(url)
	}

	if hafUrl := os.Getenv("HAF_DB_URL"); hafUrl != "" {
		err = dc.SetHafDbURI(hafUrl)
	}

	if dc.GetDbName() == "" {
		err = dc.SetDbName(DefaultDbName)
	}

	if err != nil {
		return err
	}

	return nil
}

func (dc *dbConfigStruct) SetDbURI(uri string) error {
	if uri == "" {
		return ErrEmptyURI
	}
	return dc.Update(func(dc *dbConfig) {
		dc.DbURI = uri
	})
}

func (dc *dbConfigStruct) SetDbName(name string) error {
	if name == "" {
		return fmt.Errorf("empty db name")
	}
	return dc.Update(func(dc *dbConfig) {
		dc.DbName = name
	})
}

func (dc *dbConfigStruct) GetDbName() string {
	return dc.Get().DbName
}

// SetHafDbURI points the node at a HAF (PostgreSQL) database. Setting it to a
// non-empty connection string switches block ingestion to the HAF source.
func (dc *dbConfigStruct) SetHafDbURI(uri string) error {
	return dc.Update(func(dc *dbConfig) {
		dc.HafDbURI = uri
	})
}

// GetHafDbURI returns the HAF PostgreSQL connection string, or "" when the
// node should stream blocks from the Hive API instead.
func (dc *dbConfigStruct) GetHafDbURI() string {
	return dc.Get().HafDbURI
}
