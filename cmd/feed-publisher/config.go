package main

import (
	"vsc-node/modules/config"
)

type feedPublisherConfig struct {
	ChainID         string `json:"ChainID"`
	IntervalSeconds int    `json:"IntervalSeconds"`
	Base            string `json:"Base"`
	Quote           string `json:"Quote"`
}

func newFeedPublisherConfig(dataDir string) *config.Config[feedPublisherConfig] {
	return config.New(feedPublisherConfig{
		ChainID:         "18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e",
		IntervalSeconds: 240,
		Base:            "1.000 TBD",
		Quote:           "1.000 TESTS",
	}, &dataDir)
}
