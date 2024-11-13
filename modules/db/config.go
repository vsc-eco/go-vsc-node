package db

import "vsc-node/modules/config"

type dbConfig struct {
	DbURI string
}

func NewDbConfig() *config.Config[dbConfig] {
	return config.New(dbConfig{
		DbURI: "mongodb://localhost:27017",
	})
}
