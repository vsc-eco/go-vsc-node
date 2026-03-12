package mapper

import "vsc-node/modules/config"

type mappingBotConfig struct {
	ContractId           string
	ConnectedGraphQLAddr string
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
		ConnectedGraphQLAddr: "0.0.0.0:8080",
	}, dataDirPtr)}
}
