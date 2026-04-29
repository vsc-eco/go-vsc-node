package systemconfig

import (
	"encoding/json"
	"fmt"
	"os"
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
	OracleParams() params.OracleParams
	TssParams() params.TssParams
	PendulumPoolWhitelist() []string
	LoadOverrides(path string) error
}

type config struct {
	network               string
	bootstrapPeers        []string
	netId                 string
	hiveChainId           string
	gatewayWallet         string
	startHeight           uint64
	consensusParams       params.ConsensusParams
	oracleParams          params.OracleParams
	tssParams             params.TssParams
	pendulumPoolWhitelist []string
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

func (c *config) OracleParams() params.OracleParams {
	return c.oracleParams
}

func (c *config) TssParams() params.TssParams {
	return c.tssParams
}

// PendulumPoolWhitelist returns the per-network list of pool contract IDs that
// are eligible to participate in the Magi pendulum (CLP fee accrual + LP rewards),
// in addition to any DAO-owned pools matched by PendulumBolt.EnforceDAOOwnedPools.
func (c *config) PendulumPoolWhitelist() []string {
	if len(c.pendulumPoolWhitelist) == 0 {
		return nil
	}
	out := make([]string, len(c.pendulumPoolWhitelist))
	copy(out, c.pendulumPoolWhitelist)
	return out
}

// SysConfigOverrides is the JSON shape for the -sysconfig override file.
// Only fields present in the JSON are applied; the rest keep their
// network defaults.
type SysConfigOverrides struct {
	BootstrapPeers        []string                `json:"bootstrapPeers,omitempty"`
	NetId                 string                  `json:"netId,omitempty"`
	HiveChainId           string                  `json:"hiveChainId,omitempty"`
	GatewayWallet         string                  `json:"gatewayWallet,omitempty"`
	StartHeight           *uint64                 `json:"startHeight,omitempty"`
	ConsensusParams       *params.ConsensusParams `json:"consensusParams,omitempty"`
	OracleParams          *params.OracleParams    `json:"oracleParams,omitempty"`
	TssParams             *params.TssParams       `json:"tssParams,omitempty"`
	PendulumPoolWhitelist *[]string               `json:"pendulumPoolWhitelist,omitempty"`
}

func (c *config) LoadOverrides(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading sysconfig overrides: %w", err)
	}
	// Unmarshal into raw messages so we can apply each section
	// directly onto the existing config, preserving defaults for
	// fields not present in the JSON.
	var raw struct {
		BootstrapPeers        []string        `json:"bootstrapPeers,omitempty"`
		NetId                 string          `json:"netId,omitempty"`
		HiveChainId           string          `json:"hiveChainId,omitempty"`
		GatewayWallet         string          `json:"gatewayWallet,omitempty"`
		StartHeight           *uint64         `json:"startHeight,omitempty"`
		ConsensusParams       json.RawMessage `json:"consensusParams,omitempty"`
		OracleParams          json.RawMessage `json:"oracleParams,omitempty"`
		TssParams             json.RawMessage `json:"tssParams,omitempty"`
		PendulumPoolWhitelist *[]string       `json:"pendulumPoolWhitelist,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parsing sysconfig overrides: %w", err)
	}
	if raw.BootstrapPeers != nil {
		c.bootstrapPeers = raw.BootstrapPeers
	}
	if raw.NetId != "" {
		c.netId = raw.NetId
	}
	if raw.HiveChainId != "" {
		c.hiveChainId = raw.HiveChainId
	}
	if raw.GatewayWallet != "" {
		c.gatewayWallet = raw.GatewayWallet
	}
	if raw.StartHeight != nil {
		c.startHeight = *raw.StartHeight
	}
	if raw.ConsensusParams != nil {
		if err := json.Unmarshal(raw.ConsensusParams, &c.consensusParams); err != nil {
			return fmt.Errorf("applying consensus overrides: %w", err)
		}
	}
	if raw.OracleParams != nil {
		if err := json.Unmarshal(raw.OracleParams, &c.oracleParams); err != nil {
			return fmt.Errorf("applying oracle overrides: %w", err)
		}
	}
	if raw.TssParams != nil {
		if err := json.Unmarshal(raw.TssParams, &c.tssParams); err != nil {
			return fmt.Errorf("applying tss overrides: %w", err)
		}
	}
	if raw.PendulumPoolWhitelist != nil {
		c.pendulumPoolWhitelist = append([]string(nil), (*raw.PendulumPoolWhitelist)...)
	}
	return nil
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
			MinStake:             params.CONSENSUS_MINIMUM,
			MinMembers:           7,
			MinSpSigners:         6,
			MinRcLimit:           params.MINIMUM_RC_LIMIT,
			TssIndexHeight:       params.TSS_INDEX_HEIGHT,
			ElectionInterval:     params.ELECTION_INTERVAL,
			ElectionDupeFixEpoch: 1406,
		},
		oracleParams: params.OracleParams{
			ChainContracts: map[string]string{
				"BTC": "vsc1BdrQ6EtbQ64rq2PkPd21x4MaLnVRcJj85d",
				// "DASH": "vsc1...", // deploy dash-mapping-contract and add contract ID
				// "LTC":  "vsc1...", // deploy ltc-mapping-contract and add contract ID
			},
		},
		tssParams: params.DefaultTssParams,
		// Mainnet defaults to empty — eligibility falls back to PendulumBolt's DAO-owner check.
		pendulumPoolWhitelist: nil,
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
			MinStake:             params.CONSENSUS_MINIMUM,
			MinMembers:           3,
			MinSpSigners:         3,
			MinRcLimit:           params.MINIMUM_RC_LIMIT,
			TssIndexHeight:       1409500,
			ElectionInterval:     3600,
			ElectionDupeFixEpoch: 268,
		},
		oracleParams: params.OracleParams{
			ChainContracts: map[string]string{
				"BTC": "vsc1BkWohDf5fPcwn7V9B9ar6TyiWc3A2ZGJ4t",
				// "DASH": "vsc1...", // deploy dash-mapping-contract and add contract ID
				// "LTC":  "vsc1...", // deploy ltc-mapping-contract and add contract ID
			},
			ZKVerifierChains: map[string]string{
				"ETH": "vsc1BdjvsW9XtHZKKLXNscsiqBrPt2hhsbdZgp",
			},
		},
		tssParams: params.DefaultTssParams,
		// Populate with deployed pool contract IDs once they exist; operators
		// can override via -sysconfig pendulumPoolWhitelist. Listed pools bypass
		// the DAO-owner check in PendulumBolt.
		pendulumPoolWhitelist: nil,
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
			MinStake:             1000,
			MinMembers:           3,
			MinSpSigners:         3,
			MinRcLimit:           params.MINIMUM_RC_LIMIT,
			TssIndexHeight:       0,
			ElectionInterval:     40,
			ElectionDupeFixEpoch: 0,
		},
		tssParams: params.DefaultTssParams,
		// Devnet operators set via -sysconfig pendulumPoolWhitelist on each node.
		pendulumPoolWhitelist: nil,
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
			MinStake:             1,
			MinMembers:           3,
			MinRcLimit:           1,
			TssIndexHeight:       0,
			ElectionInterval:     1000,
			ElectionDupeFixEpoch: 0,
		},
		tssParams: params.MocknetTssParams,
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
