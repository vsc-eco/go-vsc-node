package libp2p

import (
	"fmt"
	"vsc-node/modules/config"
)

type p2pConfig struct {
	Port                   int
	ServerMode             bool
	AllowPrivate           bool
	PubsubBufferSize       int
	PubsubConcurrencyLimit int
	Bootnodes              []string
	AnnounceAddrs          []string

	// Pentest finding N-L6: operator-managed connection deny lists,
	// applied to the libp2p ConnectionGater at node start. Empty by
	// default (allow-all, matches prior behaviour). BlockedPeers is a
	// list of base58 peer IDs; BlockedSubnets is a list of CIDR
	// ranges. Edit the p2p config file and restart to ban.
	BlockedPeers   []string
	BlockedSubnets []string
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
		Port:                   10720,
		ServerMode:             false,
		AllowPrivate:           false,
		PubsubBufferSize:       512,
		PubsubConcurrencyLimit: 256,
		Bootnodes:              []string{},
		AnnounceAddrs:          []string{},
		BlockedPeers:           []string{},
		BlockedSubnets:         []string{},
	}, dataDirPtr)}
}

// Set p2p config options **for mocknet/e2e/unit tests only**
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
		pc.PubsubBufferSize = conf.PubsubBufferSize
		pc.PubsubConcurrencyLimit = conf.PubsubConcurrencyLimit
		pc.Bootnodes = conf.Bootnodes
		pc.AnnounceAddrs = conf.AnnounceAddrs
		pc.BlockedPeers = conf.BlockedPeers
		pc.BlockedSubnets = conf.BlockedSubnets
	})
}

func (pc *p2pConfigStruct) Init() error {
	if err := pc.Config.Init(); err != nil {
		return err
	}
	if pc.Get().PubsubBufferSize == 0 {
		pc.Update(func(c *p2pConfig) {
			c.PubsubBufferSize = pc.DefaultValue().PubsubBufferSize
		})
	}
	if pc.Get().PubsubConcurrencyLimit == 0 {
		pc.Update(func(c *p2pConfig) {
			c.PubsubConcurrencyLimit = pc.DefaultValue().PubsubConcurrencyLimit
		})
	}
	return nil
}

func (pc *p2pConfigStruct) SetBootnodes(bootnodes []string) error {
	return pc.Update(func(pc *p2pConfig) {
		pc.Bootnodes = bootnodes
	})
}
