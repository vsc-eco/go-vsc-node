package systemconfig

import (
	"encoding/hex"
	"vsc-node/modules/common/params"
)

type SystemConfig interface {
	OnMainnet() bool
	OnMocknet() bool
	BootstrapPeers() []string
	NetId() string
	GatewayWallet() string
	ConsensusParams() params.ConsensusParams
	ChainId() []byte // nil = use library default (mainnet)
}

type config struct {
	network         string
	bootstrapPeers  []string
	netId           string
	gatewayWallet   string
	consensusParams params.ConsensusParams
	chainId         []byte
}

var testnetChainId, _ = hex.DecodeString("18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e")

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

func (c *config) ChainId() []byte {
	return c.chainId
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

// TESTNET_BOOTSTRAP can be populated with testnet peer multiaddrs as they become available
var TESTNET_BOOTSTRAP = []string{}

// TestnetConfig returns system config for Magi/VSC testnet (techcoderx).
// Chain ID differs from mainnet — required for correct transaction signing.
func TestnetConfig() SystemConfig {
	conf := &config{
		bootstrapPeers: TESTNET_BOOTSTRAP,
		network:        "testnet",
		netId:          "vsc-testnet",
		gatewayWallet:  "vsc.testnet",
		chainId:        testnetChainId,
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
