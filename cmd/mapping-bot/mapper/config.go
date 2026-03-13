package mapper

import "vsc-node/modules/config"

type mappingBotConfig struct {
	ContractId           string
	PrimaryPublicKey     string
	BackupPublicKey      string
	ConnectedGraphQLAddr string
	HttpPort             uint16
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
		ContractId:           "ADD_BTC_MAPPING_CONTRACT_ID",
		PrimaryPublicKey:     "",
		BackupPublicKey:      "",
		ConnectedGraphQLAddr: "0.0.0.0:8080",
		HttpPort:             8000,
	}, dataDirPtr)}
}

func (c *mappingBotConfigStruct) ContractId() string {
	return c.Get().ContractId
}

func (c *mappingBotConfigStruct) PrimaryKey() string {
	return c.Get().PrimaryPublicKey
}

func (c *mappingBotConfigStruct) BackupKey() string {
	return c.Get().BackupPublicKey
}

func (c *mappingBotConfigStruct) HttpPort() uint16 {
	return c.Get().HttpPort
}
