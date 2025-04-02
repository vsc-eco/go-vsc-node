package streamer

import (
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
		return err
	}

	url := os.Getenv("HIVE_API")
	if url != "" {
		return hc.SetHiveURI(url)
	} else {
		return hc.SetHiveURI(DefaultHiveURI)
	}
}

func (hc *hiveConfigStruct) SetHiveURI(uri string) error {
	return hc.Update(func(hc *hiveConfig) {
		if uri == "" {
			hc.HiveURI = DefaultHiveURI
		} else {
			hc.HiveURI = uri
		}
	})
}
