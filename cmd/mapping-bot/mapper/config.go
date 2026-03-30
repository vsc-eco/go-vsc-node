package mapper

import "vsc-node/modules/config"

type mappingBotConfig struct {
	ContractId           string
	ConnectedGraphQLAddr string
	HttpPort             uint16
	// SignApiKey authenticates requests to the /sign endpoint.
	// If empty, /sign is disabled for safety.
	SignApiKey string
}

type mappingBotConfigStruct struct {
	*config.Config[mappingBotConfig]
}

type MappingBotConfig = *mappingBotConfigStruct

func NewMappingBotConfig(dataDir ...string) *mappingBotConfigStruct {
	var dataDirPtr *string
	if len(dataDir) > 0 {
		dataDirPtr = &dataDir[0]
	}
	return &mappingBotConfigStruct{config.New(mappingBotConfig{
		ContractId:           "ADD_MAPPING_CONTRACT_ID",
		ConnectedGraphQLAddr: "0.0.0.0:8080",
		HttpPort:             8000,
	}, dataDirPtr)}
}

func (c *mappingBotConfigStruct) ContractId() string {
	return c.Get().ContractId
}

func (c *mappingBotConfigStruct) HttpPort() uint16 {
	return c.Get().HttpPort
}

func (c *mappingBotConfigStruct) SignApiKey() string {
	return c.Get().SignApiKey
}

func (c *mappingBotConfigStruct) SetHttpPort(port uint16) {
	c.Update(func(cfg *mappingBotConfig) {
		cfg.HttpPort = port
	})
}
