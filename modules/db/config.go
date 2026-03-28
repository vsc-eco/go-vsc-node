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
