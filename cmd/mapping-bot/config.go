package main

import "vsc-node/modules/config"

type mappingBotConfig struct {
	ContractId string
}

type mappingBotConfigStruct struct {
	*config.Config[mappingBotConfig]
}

func newMappingBotConfig(dataDir ...string) *mappingBotConfigStruct {
	var dataDirPtr *string
	if len(dataDir) > 0 {
		dataDirPtr = &dataDir[0]
	}
	return &mappingBotConfigStruct{config.New(mappingBotConfig{
		ContractId: "ADD_BTC_MAPPING_CONTRACT_ID",
	}, dataDirPtr)}
}
