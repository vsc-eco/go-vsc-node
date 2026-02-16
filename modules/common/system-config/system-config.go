package systemconfig

import (
	"fmt"
	"vsc-node/modules/common/params"
)

type SystemConfig interface {
	OnMainnet() bool
	OnTestnet() bool
	OnMocknet() bool
	BootstrapPeers() []string
	PubSubTopicPrefix() string
	NetId() string
	HiveChainId() string
	GatewayWallet() string
	StartHeight() uint64
	ConsensusParams() params.ConsensusParams
}

type config struct {
	network         string
	bootstrapPeers  []string
	netId           string
	hiveChainId     string
	gatewayWallet   string
	startHeight     uint64
	consensusParams params.ConsensusParams
}

func (c *config) OnMainnet() bool {
	return c.network == "mainnet"
}

func (c *config) OnTestnet() bool {
	return c.network == "testnet" || c.network == "devnet"
}

func (c *config) OnMocknet() bool {
	return c.network == "mocknet"
}

func (c *config) BootstrapPeers() []string {
	return c.bootstrapPeers
}

func (c *config) PubSubTopicPrefix() string {
	return "/vsc/" + c.network
}

func (c *config) NetId() string {
	return c.netId
}

func (c *config) HiveChainId() string {
	return c.hiveChainId
}

func (c *config) GatewayWallet() string {
	return c.gatewayWallet
}
func (c *config) ConsensusParams() params.ConsensusParams {
	return c.consensusParams
}

func (c *config) StartHeight() uint64 {
	return c.startHeight
}

func MainnetConfig() SystemConfig {
	conf := &config{
		bootstrapPeers: MAINNET_BOOTSTRAP,
		network:        "mainnet",
		netId:          "vsc-mainnet",
		hiveChainId:    "beeab0de00000000000000000000000000000000000000000000000000000000",
		gatewayWallet:  "vsc.gateway",
		startHeight:    94601000,
		consensusParams: params.ConsensusParams{
			MinStake:         params.CONSENSUS_MINIMUM,
			MinMembers:       7,
			MinRcLimit:       params.MINIMUM_RC_LIMIT,
			TssIndexHeight:   params.TSS_INDEX_HEIGHT,
			ElectionInterval: params.ELECTION_INTERVAL,
		},
	}
	return conf
}

func TestnetConfig() SystemConfig {
	conf := &config{
		bootstrapPeers: TESTNET_BOOTSTRAP,
		network:        "testnet",
		netId:          "vsc-testnet",
		hiveChainId:    "18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e",
		gatewayWallet:  "vsc.gateway",
		startHeight:    2,
		consensusParams: params.ConsensusParams{
			MinStake:         params.CONSENSUS_MINIMUM,
			MinMembers:       3,
			MinRcLimit:       params.MINIMUM_RC_LIMIT,
			TssIndexHeight:   0,
			ElectionInterval: params.ELECTION_INTERVAL,
		},
	}
	return conf
}

func DevnetConfig() SystemConfig {
	conf := &config{
		network:       "devnet",
		netId:         "vsc-devnet",
		hiveChainId:   "18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e",
		gatewayWallet: "vsc.gateway",
		startHeight:   2,
		consensusParams: params.ConsensusParams{
			MinStake:         1000,
			MinMembers:       3,
			MinRcLimit:       params.MINIMUM_RC_LIMIT,
			TssIndexHeight:   0,
			ElectionInterval: 40,
		},
	}
	return conf
}

func MocknetConfig() SystemConfig {
	conf := &config{
		network:       "mocknet",
		netId:         "vsc-mocknet",
		hiveChainId:   "123456789abcdef000000000000000000000000000000000000000000000000",
		gatewayWallet: "vsc.mocknet",
		startHeight:   0,
		consensusParams: params.ConsensusParams{
			MinStake:         1,
			MinMembers:       3,
			MinRcLimit:       1,
			TssIndexHeight:   0,
			ElectionInterval: 1000,
		},
	}
	return conf
}

func FromNetwork(network string) SystemConfig {
	switch network {
	case "mainnet":
		return MainnetConfig()
	case "testnet":
		return TestnetConfig()
	case "devnet":
		return DevnetConfig()
	case "mocknet":
		return MocknetConfig()
	default:
		panic(fmt.Errorf("invalid network"))
	}
}
