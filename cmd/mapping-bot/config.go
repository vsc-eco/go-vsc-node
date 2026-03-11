package main

import "vsc-node/modules/config"

const mappingBotDataDir = "data/mapping-bot"

type mappingBotConfig struct {
	ContractId string
}

type mappingBotConfigStruct struct {
	*config.Config[mappingBotConfig]
}

func newMappingBotConfig() *mappingBotConfigStruct {
	dataDir := mappingBotDataDir
	return &mappingBotConfigStruct{config.New(mappingBotConfig{}, &dataDir)}
}

func (c *mappingBotConfigStruct) GetContractId() string {
	return c.Get().ContractId
}
