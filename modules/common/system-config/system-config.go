package systemconfig

import (
	"fmt"
	"vsc-node/modules/common/params"
)

type SystemConfig interface {
	OnMainnet() bool
	OnMocknet() bool
	BootstrapPeers() []string
	NetId() string
	GatewayWallet() string
	ConsensusParams() params.ConsensusParams
}

type config struct {
	network         string
	bootstrapPeers  []string
	netId           string
	gatewayWallet   string
	consensusParams params.ConsensusParams
}

func (c *config) OnMainnet() bool {
	return c.network == "mainnet"
}

func (c *config) OnMocknet() bool {
	return c.network == "mocknet"
}

func (c *config) BootstrapPeers() []string {
	return c.bootstrapPeers
}

func (c *config) NetId() string {
	return c.netId
}

func (c *config) GatewayWallet() string {
	return c.gatewayWallet
}
func (c *config) ConsensusParams() params.ConsensusParams {
	return c.consensusParams
}

func MainnetConfig() SystemConfig {
	conf := &config{
		bootstrapPeers: MAINNET_BOOTSTRAP,
		network:        "mainnet",
		netId:          "vsc-mainnet",
		gatewayWallet:  "vsc.gateway",
		consensusParams: params.ConsensusParams{
			MinStake:       params.MAINNET_CONSENSUS_MINIMUM,
			MinRcLimit:     params.MINIMUM_RC_LIMIT,
			TssIndexHeight: params.TSS_INDEX_HEIGHT,
		},
	}
	return conf
}

// TODO: Define a testnet config
func TestnetConfig() SystemConfig {
	conf := &config{
		network:       "testnet",
		netId:         "vsc-testnet",
		gatewayWallet: "vsc.testnet",
		consensusParams: params.ConsensusParams{
			MinStake:       params.TESTNET_CONSENSUS_MINIMUM,
			MinRcLimit:     params.MINIMUM_RC_LIMIT,
			TssIndexHeight: 0,
		},
	}
	return conf
}

func MocknetConfig() SystemConfig {
	conf := &config{
		network:       "mocknet",
		netId:         "vsc-mocknet",
		gatewayWallet: "vsc.mocknet",
		consensusParams: params.ConsensusParams{
			MinStake:       1,
			MinRcLimit:     1,
			TssIndexHeight: 0,
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
	case "mocknet":
		return MocknetConfig()
	default:
		panic(fmt.Errorf("invalid network"))
	}
}
