package gql

import "vsc-node/modules/config"

type gqlConfig struct {
	HostAddr string
}

type gqlConfigStruct struct {
	*config.Config[gqlConfig]
}

type GqlConfig = *gqlConfigStruct

func NewGqlConfig(dataDir ...string) GqlConfig {
	var dataDirPtr *string
	if len(dataDir) > 0 {
		dataDirPtr = &dataDir[0]
	}

	return &gqlConfigStruct{config.New(gqlConfig{
		HostAddr: "0.0.0.0:8080",
	}, dataDirPtr)}
}

func (gc *gqlConfigStruct) SetHostAddr(addr string) error {
	return gc.Update(func(dc *gqlConfig) {
		dc.HostAddr = addr
	})
}

func (gc *gqlConfigStruct) GetHostAddr() string {
	return gc.Get().HostAddr
}
