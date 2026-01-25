package libp2p

import (
	"fmt"
	"vsc-node/modules/config"
)

type p2pConfig struct {
	Port         int
	ServerMode   bool
	AllowPrivate bool
}

type p2pConfigStruct struct {
	*config.Config[p2pConfig]
}

type P2POpts = p2pConfig
type P2PConfig = *p2pConfigStruct

func NewConfig(dataDir ...string) P2PConfig {
	var dataDirPtr *string
	if len(dataDir) > 0 {
		dataDirPtr = &dataDir[0]
	}

	return &p2pConfigStruct{config.New(p2pConfig{
		Port:         10720,
		ServerMode:   false,
		AllowPrivate: false,
	}, dataDirPtr)}
}

func (pc *p2pConfigStruct) SetOptions(conf p2pConfig) error {
	if conf.Port < 0 || conf.Port > 65535 {
		return fmt.Errorf("port must be between 1024 and 65535")
	} else if conf.Port > 0 && conf.Port < 1024 {
		return fmt.Errorf("cannot listen to privileged ports")
	}
	return pc.Update(func(pc *p2pConfig) {
		pc.Port = conf.Port
		pc.ServerMode = conf.ServerMode
		pc.AllowPrivate = conf.AllowPrivate
	})
}
