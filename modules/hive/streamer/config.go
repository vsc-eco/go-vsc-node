package streamer

import (
	"fmt"
	"net/url"
	"os"
	"vsc-node/modules/config"
)

const DefaultHiveURI = "https://api.hive.blog"

type hiveConfig struct {
	HiveURI string
}

type hiveConfigStruct struct {
	*config.Config[hiveConfig]
}

type HiveConfig = *hiveConfigStruct

func NewHiveConfig() HiveConfig {
	return &hiveConfigStruct{config.New(hiveConfig{
		HiveURI: DefaultHiveURI,
	}, nil)}
}

func (hc *hiveConfigStruct) Init() error {
	err := hc.Config.Init()
	if err != nil {
		return fmt.Errorf("Failed to init Hive config: %w", err)
	}

	url := os.Getenv("HIVE_API")
	if url != "" {
		return hc.SetHiveURI(url)
	}
	return nil
}

func (hc *hiveConfigStruct) SetHiveURI(uri string) error {
	if uri != "" {
		_, err := url.Parse(uri)
		if err != nil {
			return fmt.Errorf("Invalid Hive API URL: %w", err)
		}
	}
	return hc.Update(func(hc *hiveConfig) {
		if uri == "" {
			hc.HiveURI = DefaultHiveURI
		} else {
			hc.HiveURI = uri
		}
	})
}
