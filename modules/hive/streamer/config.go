package streamer

import (
	"encoding/json"
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
	// Try to read from new array format first
	uri, err := hc.readFirstURIFromArrayFormat()
	if err == nil && uri != "" {
		// Successfully read from new format, set it in memory
		hc.Config.Update(func(config *hiveConfig) {
			config.HiveURI = uri
		})
		// Mark as loaded so Init doesn't try to create a new file
		// We need to use reflection or just call the parent Init and ignore the error
	}

	err = hc.Config.Init()
	if err != nil {
		return fmt.Errorf("Failed to init Hive config: %w", err)
	}

	url := os.Getenv("HIVE_API")
	if url != "" {
		return hc.SetHiveURI(url)
	}
	return nil
}

// readFirstURIFromArrayFormat tries to read the first URI from the new array format
func (hc *hiveConfigStruct) readFirstURIFromArrayFormat() (string, error) {
	f, err := os.Open(hc.FilePath())
	if err != nil {
		return "", err
	}
	defer f.Close()

	var newFormat struct {
		HiveURIs []string `json:"HiveURIs,omitempty"`
	}

	decoder := json.NewDecoder(f)
	if err := decoder.Decode(&newFormat); err != nil {
		return "", err
	}

	if len(newFormat.HiveURIs) > 0 {
		return newFormat.HiveURIs[0], nil
	}

	return "", fmt.Errorf("no URIs found in array format")
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
