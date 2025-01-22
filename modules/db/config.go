package db

import (
	"errors"
	"vsc-node/modules/config"
)

var ErrEmptyURI = errors.New("empty MongoDB URI")

type dbConfig struct {
	DbURI string
}

type DbConfig struct {
	*config.Config[dbConfig]
}

func NewDbConfig() DbConfig {
	return DbConfig{config.New(dbConfig{
		DbURI: "mongodb://localhost:27017",
	})}
}

func (dc *DbConfig) SetDbURI(uri string) error {
	if uri == "" {
		return ErrEmptyURI
	}
	return dc.Update(func(dc *dbConfig) {
		dc.DbURI = uri
	})
}
