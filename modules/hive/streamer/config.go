package streamer

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"vsc-node/modules/config"
)

var DefaultHiveURIs = []string{
	"https://api.hive.blog",
	"https://techcoderx.com",
	"https://hive-api.3speak.tv",
	"https://api.openhive.network",
	"https://api.deathwing.me",
}

type hiveConfig struct {
	HiveURIs []string `json:"HiveURIs,omitempty"`
	// Old field for backward compatibility
	HiveURI string `json:"HiveURI,omitempty"`
}

type hiveConfigStruct struct {
	*config.Config[hiveConfig]
}

type HiveConfig = *hiveConfigStruct

func NewHiveConfig(dataDir ...string) HiveConfig {
	var dataDirPtr *string
	if len(dataDir) > 0 {
		dataDirPtr = &dataDir[0]
	}

	return &hiveConfigStruct{config.New(hiveConfig{
		HiveURIs: DefaultHiveURIs,
	}, dataDirPtr)}
}

func (hc *hiveConfigStruct) Init() error {
	err := hc.Config.Init()
	if err != nil {
		return fmt.Errorf("Failed to init Hive config: %w", err)
	}

	config := hc.Get()

	// Migration: convert old single URI to array
	if config.HiveURI != "" && len(config.HiveURIs) == 0 {
		err = hc.Update(func(hc *hiveConfig) {
			// build new hive uris prioritizing the current setting
			hc.HiveURIs = func(current string, defaults []string) []string {
				result := make([]string, 0, len(defaults)+1)
				result = append(result, hc.HiveURI)
				for _, uri := range defaults {
					if uri != current {
						result = append(result, uri)
					}
				}
				return result
			}(hc.HiveURI, DefaultHiveURIs)
			hc.HiveURI = "" // clear old field
		})
		if err != nil {
			return err
		}
	}

	envURIs := os.Getenv("HIVE_API")
	if envURIs != "" {
		uris := strings.Split(envURIs, ",")
		for i := range uris {
			uris[i] = strings.TrimSpace(uris[i])
		}
		return hc.SetHiveURIs(uris)
	}
	return nil
}

func (hc *hiveConfigStruct) SetHiveURIs(uris []string) error {
	// Validate all URIs before updating
	for _, uri := range uris {
		if uri != "" {
			_, err := url.Parse(uri)
			if err != nil {
				return fmt.Errorf("Invalid Hive API URL '%s': %w", uri, err)
			}
		}
	}

	return hc.Update(func(hc *hiveConfig) {
		if len(uris) == 0 {
			hc.HiveURIs = DefaultHiveURIs
		} else {
			hc.HiveURIs = uris
		}
	})
}

// Helper method to add a single URI
func (hc *hiveConfigStruct) AddHiveURI(uri string) error {
	if uri != "" {
		_, err := url.Parse(uri)
		if err != nil {
			return fmt.Errorf("Invalid Hive API URL: %w", err)
		}
	}

	return hc.Update(func(hc *hiveConfig) {
		hc.HiveURIs = append(hc.HiveURIs, uri)
	})
}

// Helper method to get all URIs
func (hc *hiveConfigStruct) GetHiveURIs() []string {
	return hc.Get().HiveURIs
}
