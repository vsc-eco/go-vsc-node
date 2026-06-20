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
			ConsensusVersionFloorEpoch:     1698,
			ConsensusVersionFloorMajor:     0,
			ConsensusVersionFloorConsensus: 2,
			PendulumSeedEpoch:              1622,
			EvmAddressChecksumHeight:       106_907_500,
			// v0.2.0 release activation: the contract-update timelock, gateway-key
			// strict admission, gateway dao-removal, try/catch ICC and the pendulum
			// LP-floor all gate on the CHAIN-ACTIVE CONSENSUS VERSION reaching 0.2.0
			// (consensusversion.V0_2_0), driven by the floor below — NOT a dedicated
			// height. The floor is currently 0.1.0 (epoch 1623), so the whole v0.2.0
			// batch is inert. To roll out: raise ConsensusVersionFloorConsensus to 2
			// at a future epoch boundary (CP-1 devnet-proven first — see the gateway
			// dao-removal note in modules/gateway/multisig.go).
			// Bond inclusion window (CP-2): 86,400 Hive blocks = 3 days @ 3s.
			// Activation height 0 = INERT (no behavior change) until an operator
			// pins a future epoch-boundary height (>=3d lead) for rollout.
			BondInclusionWindowBlocks:     86_400,
			BondInclusionActivationHeight: 107454300,
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
			SafetySlashActivationHeight: 107454300,
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
			// v0.2.0 release activation: now driven by the chain-active consensus
			// version reaching 0.2.0 (the ConsensusVersionFloor* below), NOT a
			// dedicated height. The floor is 0.2.0 from epoch 662, so the whole
			// v0.2.0 batch is in force on testnet today (head is well past epoch 662).
			//
			// The election-build gate (WitnessKeyStrictActive) keys on the PRIOR
			// election's version, so it stays dormant for epoch 662 (prev = epoch 661
			// = 0.1.0) and bites from epoch 663 — automatically giving witnesses a
			// full epoch to re-announce gateway_key_pop, the same protection the old
			// 3_852_000 height was hand-positioned to provide (FIX election-stall
			// 2026-06-11), now without a magic number.
			//
			// REINDEX NOTE: testnet already activated v0.2.0 via the old 3_852_000
			// height. The version gate re-derives activation from the floor, which
			// differs from the height in the historical gap [epoch-662 anchor,
			// 3_852_000] — a reindex from genesis can diverge there if any
			// contract-update / gateway-rotation landed in that window. Forward
			// operation is unaffected (both say "active" now); confirm before relying
			// on a full-genesis testnet reindex.
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
			// Ephemeral network (fresh per run): pin the consensus-version floor to
			// 0.2.0 from epoch 1 so the whole v0.2.0 batch is active from genesis and
			// exercised by devnet/regression tests. Replaces the old
			// Version0_2_0Height=1; this is the same floor pin the try/catch and
			// LP-floor devnet tests already use (and pass with). No reindex concern —
			// devnet is fresh per run and every node runs the current 0.2.0 binary,
			// so the floor never excludes a witness.
			ConsensusVersionFloorEpoch:     1,
			ConsensusVersionFloorMajor:     0,
			ConsensusVersionFloorConsensus: 2,
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
			// (its own height, matching the 0.2.0 floor pin above) so the devnet
			// double-sign integration
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
			// Mocknet leaves the consensus-version floor UNPINNED (no 0.2.0 floor).
			// The in-process e2e harness wires v0.2.0 features it exercises directly
			// (e.g. ContractTest.TryCatchActive → WithTryCatch), not via the chain
			// version, and mocknet disables the contract-update timelock
			// (contractUpdateTimelockBlocks: 0). Pinning the floor here would also
			// make the version-floor filter exclude the version-less witness fixtures
			// several election-proposer unit tests build. (Replaces the old
			// Version0_2_0Height=1.)
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
