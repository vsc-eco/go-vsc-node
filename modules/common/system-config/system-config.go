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
	OnDevnet() bool
	OnMocknet() bool
	BootstrapPeers() []string
	PubSubTopicPrefix() string
	NetId() string
	HiveChainId() string
	GatewayWallet() string
	StartHeight() uint64
	ConsensusParams() params.ConsensusParams
	// ContractUpdateTimelockBlocks is the network's contract-update timelock in
	// Hive L1 blocks (3s/block). 0 means updates activate immediately. Network-
	// baked and NOT overridable via -sysconfig: a consensus rule every node must
	// share so no single operator can shorten it.
	ContractUpdateTimelockBlocks() uint64
	OracleParams() params.OracleParams
	TssParams() params.TssParams
	PendulumPoolWhitelist() []string
	LoadOverrides(path string) error
}

type config struct {
	network         string
	bootstrapPeers  []string
	netId           string
	hiveChainId     string
	gatewayWallet   string
	startHeight     uint64
	consensusParams params.ConsensusParams
	// Network-baked contract-update timelock length (Hive L1 blocks). Deliberately
	// absent from SysConfigOverrides so it cannot be changed per-operator.
	contractUpdateTimelockBlocks uint64
	oracleParams                 params.OracleParams
	tssParams                    params.TssParams
	pendulumPoolWhitelist        []string
}

func (c *config) OnMainnet() bool {
	return c.network == "mainnet"
}

func (c *config) OnTestnet() bool {
	return c.network == "testnet"
}

func (c *config) OnDevnet() bool {
	return c.network == "devnet"
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

func (c *config) ContractUpdateTimelockBlocks() uint64 {
	return c.contractUpdateTimelockBlocks
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
		// 48h timelock on contract updates (see params.CONTRACT_UPDATE_TIMELOCK_BLOCKS).
		contractUpdateTimelockBlocks: params.CONTRACT_UPDATE_TIMELOCK_BLOCKS,
		consensusParams: params.ConsensusParams{
			MinStake:                       params.CONSENSUS_MINIMUM,
			MinMembers:                     8, // H-5: must be >= the gateway floor (8, gateway/multisig.go) — a valid 7-member election otherwise silently wedges keyRotation forever
			MinSpSigners:                   6,
			MinRcLimit:                     params.MINIMUM_RC_LIMIT,
			TssIndexHeight:                 params.TSS_INDEX_HEIGHT,
			ElectionInterval:               params.ELECTION_INTERVAL,
			ElectionDupeFixEpoch:           1406,
			ConsensusVersionFloorEpoch:     1623,
			ConsensusVersionFloorMajor:     0,
			ConsensusVersionFloorConsensus: 1,
			PendulumSeedEpoch:              1622,
			EvmAddressChecksumHeight:       106_907_500,
			// v0.2.0 release activation gate (see ConsensusParams.Version0_2_0Height).
			// Gates the contract-update timelock and every other consensus change
			// shipping in v0.2.0. 0 == unpinned/inert. PIN to a future mainnet height
			// (strictly above the chain head at deploy) before the v0.2.0 rollout.
			Version0_2_0Height: 0,
			// Bond inclusion window (CP-2): 86,400 Hive blocks = 3 days @ 3s.
			// Activation height 0 = INERT (no behavior change) until an operator
			// pins a future epoch-boundary height (>=3d lead) for rollout.
			BondInclusionWindowBlocks:     86_400,
			BondInclusionActivationHeight: 0,
			BondInclusionSampleCount:      8,
			// F6 churn cap: 0 = disabled (no per-election new-member cap). Pin
			// together with the bond activation height to bound atomic cohort
			// entry once the gate is live.
			MaxNewMembersPerElection: 0,
			// Established-member exception (operator requirement): the stake an
			// account was already ratified for stays exempt from the window
			// through the per-network absence grace set on the next line, even if
			// it drops out. Only meaningful when the bond gate is active. (mainnet
			// 403,200 ≈ 2 weeks @ 3s.)
			BondInclusionEstablishedGraceBlocks: 403_200,
			// Principal (HIVE_CONSENSUS bond) safety slashing. 0 = INERT
			// (detectors log but never debit). PIN a future mainnet height
			// (strictly above chain head, every witness upgraded first) before
			// turning slashing on — same reindex/upgrade-window footgun as the
			// other height gates. Stage AFTER the v0.2.0 batch has soaked.
			SafetySlashActivationHeight: 0,
		},
		oracleParams: params.OracleParams{
			ChainContracts: map[string]string{
				"BTC": "vsc1BdrQ6EtbQ64rq2PkPd21x4MaLnVRcJj85d",
				// "DASH": "vsc1...", // deploy dash-mapping-contract and add contract ID
				// "LTC":  "vsc1...", // deploy ltc-mapping-contract and add contract ID
			},
		},
		tssParams: params.DefaultTssParams,
		// Seeded with the deployed pool contract IDs; operators can override via
		// -sysconfig pendulumPoolWhitelist. Listed pools bypass the DAO-owner
		// check in PendulumBolt.
		pendulumPoolWhitelist: []string{
			"vsc1BoaniA5HW56GuQy6pVdoZfMcVaaDfnC8kp",
			"vsc1BVb95YKRHAEy24XgRSaW4L6d9vB88AdwjM",
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
		// Short (~90s) timelock so the mechanism is testable on testnet.
		contractUpdateTimelockBlocks: params.CONTRACT_UPDATE_TIMELOCK_BLOCKS_TESTNET,
		consensusParams: params.ConsensusParams{
			MinStake:                       params.CONSENSUS_MINIMUM,
			MinMembers:                     3,
			MinSpSigners:                   3,
			MinRcLimit:                     params.MINIMUM_RC_LIMIT,
			TssIndexHeight:                 1409500,
			ElectionInterval:               3600,
			ElectionDupeFixEpoch:           268,
			ConsensusVersionFloorEpoch:     662,
			ConsensusVersionFloorMajor:     0,
			ConsensusVersionFloorConsensus: 2,
			// Last pre-rollout testnet epoch. Seeds latestSettledEpoch=515 on
			// upgrade so the first post-rollout election (epoch 516) can fire;
			// epoch 515's settlement is skipped as stale and its bucket HBD
			// rolls into 516's settlement.
			PendulumSeedEpoch: 554,
			// Set to a future testnet height before rollout (same reindex-
			// divergence rule as mainnet). 0 = disabled until then.
			EvmAddressChecksumHeight: 3467200,
			// v0.2.0 release activation gate. Testnet has persistent history, so
			// PIN a future testnet height (above chain head) before rollout — not 1.
			// 0 = inert until then.
			//
			// FIX(election-stall 2026-06-11): the prior 323_250 was BELOW the testnet
			// head (~3.84M), so the H-6 gateway-PoP gate (+ contract-update timelock)
			// went active the instant nodes ran the binary — against witness records
			// the upgrade never backfilled with gateway_key_pop → the epoch-662
			// election formed with member_count=0 and the chain stopped rotating
			// committees. Re-pinned ~8h above the head (3_842_080 @ 2026-06-11) so every
			// witness re-announces (its record gains gateway_key_pop) before the gate
			// activates. protocol_version was already stored by the old indexer, so the
			// 0.2.0 floor at ConsensusVersionFloorEpoch=662 already passes.
			Version0_2_0Height: 3_852_000,
			// Bond inclusion window (CP-2): 7,200 blocks (~6h) for faster testnet
			// iteration. Activation 0 = inert until pinned.
			BondInclusionWindowBlocks:     7_200,
			BondInclusionActivationHeight: 3_870_000,
			BondInclusionSampleCount:      8,
			// F6 churn cap: 0 = disabled (no per-election new-member cap). Pin
			// together with the bond activation height to bound atomic cohort
			// entry once the gate is live.
			MaxNewMembersPerElection: 0,
			// Established-member exception (operator requirement): the stake an
			// account was already ratified for stays exempt from the window
			// through the per-network absence grace set on the next line, even if
			// it drops out. Only meaningful when the bond gate is active. (mainnet
			// 403,200 ≈ 2 weeks @ 3s.)
			BondInclusionEstablishedGraceBlocks: 33_600,
			// Principal (HIVE_CONSENSUS bond) safety slashing. 0 = INERT until
			// pinned. Set a future testnet height (above chain head) to soak
			// slashing before mainnet — same above-head rule as mainnet.
			SafetySlashActivationHeight: 3_870_000,
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
		pendulumPoolWhitelist: []string{
			"vsc1BbGEc5XXqptJj7dC6AkToRZb4tJ6vi44Rn",
			"vsc1BYVWLFJRb13GuGZd721LoJ4suxnZZghLV7",
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
		// Short (~90s) timelock so the mechanism is testable on devnet.
		contractUpdateTimelockBlocks: params.CONTRACT_UPDATE_TIMELOCK_BLOCKS_TESTNET,
		consensusParams: params.ConsensusParams{
			MinStake:                      1000,
			MinMembers:                    3,
			MinSpSigners:                  3,
			MinRcLimit:                    params.MINIMUM_RC_LIMIT,
			TssIndexHeight:                0,
			ElectionInterval:              40,
			ElectionDupeFixEpoch:          0,
			ConsensusVersionActivationNum: 4,
			ConsensusVersionActivationDen: 5,
			// Ephemeral network (fresh per run): pin at 1 so v0.2.0 behavior is
			// active from genesis and exercised by devnet/regression tests.
			Version0_2_0Height: 1,
			// Bond inclusion window (CP-2): tiny 80-block window for fast devnet
			// tests. Activation 0 = inert; devnet test harness pins a low height
			// to exercise the gate.
			BondInclusionWindowBlocks:     80,
			BondInclusionActivationHeight: 0,
			BondInclusionSampleCount:      8,
			// F6 churn cap: 0 = disabled (no per-election new-member cap). Pin
			// together with the bond activation height to bound atomic cohort
			// entry once the gate is live.
			MaxNewMembersPerElection: 0,
			// Established-member exception (operator requirement): the stake an
			// account was already ratified for stays exempt from the window
			// through the per-network absence grace set on the next line, even if
			// it drops out. Only meaningful when the bond gate is active. (mainnet
			// 403,200 ≈ 2 weeks @ 3s.)
			BondInclusionEstablishedGraceBlocks: 400,
			// Principal safety slashing ACTIVE from genesis on this ephemeral net
			// (matches Version0_2_0Height=1) so the devnet double-sign integration
			// test (tests/devnet/malicious_doublesign_test.go) can observe a real
			// bond slash. Honest nodes never equivocate / propose invalid blocks,
			// so no slash fires in normal runs; internal unit tests still pin their
			// own height via an sconf override.
			SafetySlashActivationHeight: 1,
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
		// Disabled (0): the in-process e2e harness updates then immediately
		// executes a contract and relies on updates taking effect at once.
		contractUpdateTimelockBlocks: 0,
		consensusParams: params.ConsensusParams{
			MinStake:                      1,
			MinMembers:                    3,
			MinSpSigners:                  3,
			MinRcLimit:                    1,
			TssIndexHeight:                0,
			ElectionInterval:              1000,
			ElectionDupeFixEpoch:          0,
			ConsensusVersionActivationNum: 4,
			ConsensusVersionActivationDen: 5,
			// Ephemeral network: pin at 1 so the in-process e2e harness runs with
			// v0.2.0 behavior active from genesis.
			Version0_2_0Height:            1,
			BondInclusionWindowBlocks:     80,
			BondInclusionActivationHeight: 0,
			BondInclusionSampleCount:      8,
			// F6 churn cap: 0 = disabled (no per-election new-member cap). Pin
			// together with the bond activation height to bound atomic cohort
			// entry once the gate is live.
			MaxNewMembersPerElection: 0,
			// Established-member exception (operator requirement): the stake an
			// account was already ratified for stays exempt from the window
			// through the per-network absence grace set on the next line, even if
			// it drops out. Only meaningful when the bond gate is active. (mainnet
			// 403,200 ≈ 2 weeks @ 3s.)
			BondInclusionEstablishedGraceBlocks: 400,
			// Principal safety slashing. 0 = INERT; the in-process e2e harness
			// and internal unit tests pin their own height via an sconf override.
			SafetySlashActivationHeight: 0,
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
