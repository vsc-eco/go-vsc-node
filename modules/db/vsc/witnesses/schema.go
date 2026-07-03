package witnesses

type Witness struct {
	Account   string            `json:"account" bson:"account"`
	Height    uint64            `json:"height" bson:"height"`
	DidKeys   []PostingJsonKeys `json:"did_keys" bson:"did_keys"`
	Enabled   bool              `json:"enabled" bson:"enabled"`
	GitCommit string            `json:"git_commit" bson:"git_commit"`
	NetId     string            `json:"net_id" bson:"net_id"`
	PeerId    string            `json:"peer_id" bson:"peer_id"`
	// VersionMajor with ProtocolVersion (consensus) and VersionNonConsensus form major.consensus.non_consensus.
	VersionMajor        uint64 `json:"version_major" bson:"version_major,omitempty"`
	ProtocolVersion     uint64 `json:"protocol_version" bson:"protocol_version"`
	VersionNonConsensus uint64 `json:"version_non_consensus" bson:"version_non_consensus,omitempty"`
	Ts                  string `json:"ts" bson:"ts"`
	TxId                string `json:"tx_id" bson:"tx_id"`
	VersionId           string `json:"version_id" bson:"version_id"`
	GatewayKey          string `json:"gateway_key" bson:"gateway_key"`
	GatewayActiveKey    string `json:"gateway_active_key" bson:"gateway_active_key"`
	// GatewayKeyPoP is a hex secp256k1 proof-of-possession for GatewayKey, bound
	// to the announcing account (audit H-6, gateway companion to the consensus
	// key's PoP). Empty for witnesses that announced before gateway-PoP support.
	GatewayKeyPoP string   `json:"gateway_key_pop" bson:"gateway_key_pop,omitempty"`
	PeerAddrs     []string `json:"peer_addrs" bson:"peer_addrs"`
	// DelegationMode is the operator's announced consensus-delegation policy
	// (delegationmode.{Deactivated,Share,Custom}). Empty for witnesses that
	// announced before this field existed; callers normalize empty → Deactivated.
	// Consensus 0.5.0+ reads this to gate delegation acceptance and reward
	// sharing. omitempty keeps pre-0.5.0 records byte-identical.
	DelegationMode string `json:"delegation_mode,omitempty" bson:"delegation_mode,omitempty"`
	// DelegationModeEffective is the delegation mode IN FORCE at this row's height
	// while an adverse (leaving-Share) downgrade is timelocked (consensus 0.5.0+).
	// Readers return it until the chain epoch reaches DelegationModeMaturityEpoch,
	// then switch to DelegationMode (the announced target). Empty when no downgrade
	// is pending and on pre-0.5.0 rows. Set by the state engine at ingest
	// (StateEngine.computeDelegationTimelock), NOT from L1 metadata.
	DelegationModeEffective string `json:"delegation_mode_effective,omitempty" bson:"delegation_mode_effective,omitempty"`
	// DelegationModeMaturityEpoch is the election epoch at/after which DelegationMode
	// replaces DelegationModeEffective. 0 means "effective immediately" (pre-0.5.0
	// rows and non-adverse announcements), so readers ignore the timelock fields.
	DelegationModeMaturityEpoch uint64 `json:"delegation_mode_maturity_epoch,omitempty" bson:"delegation_mode_maturity_epoch,omitempty"`
}

type PostingJsonMetadata struct {
	Services []string                   `json:"services"`
	VscNode  PostingJsonMetadataVscNode `json:"vsc_node" bson:"vsc_node"`
	DidKeys  []PostingJsonKeys          `json:"did_keys"`
}

type PostingJsonKeys struct {
	CryptoType string `json:"ct" bson:"ct"`
	Type       string `json:"t" bson:"t"`
	Key        string `json:"key" bson:"key"`
	// PoP is a base64 (raw-url) BLS proof-of-possession for Key, bound to the
	// announcing account. Empty for witnesses that announced before PoP support.
	PoP string `json:"pop,omitempty" bson:"pop,omitempty"`
}

type PostingJsonMetadataVscNode struct {
	NetId           string   `json:"net_id"`
	PeerId          string   `json:"peer_id"`
	PeerAddrs       []string `json:"peer_addrs"`
	Ts              string   `json:"ts"`
	GitCommit       string   `json:"git_commit"`
	VersionId       string   `json:"version_id" bson:"version_id"`
	VersionMajor    uint64   `json:"version_major"`
	ProtocolVersion uint64   `json:"protocol_version"`
	// Non-consensus component; may differ across nodes without excluding them from committee.
	VersionNonConsensus uint64 `json:"version_non_consensus"`
	Witness             struct {
		Enabled bool `json:"enabled"`
		// Plugins     []string `json:"plugins"`
		// DelayNotch  int      `json:"delay_notch"`
		// SigningKeys []string `json:"signing_keys"`
	} `json:"witness"`
	GatewayKey       string `json:"gateway_key"`
	GatewayActiveKey string `json:"gateway_active_key"`
	GatewayKeyPoP    string `json:"gateway_key_pop"`
	// DelegationMode mirrors the announced operator delegation policy
	// (delegationmode.{Deactivated,Share,Custom}); empty when not announced.
	DelegationMode string `json:"delegation_mode"`
}

type SetWitnessUpdateType struct {
	Metadata         PostingJsonMetadata
	Account          string
	Height           uint64
	TxId             string
	BlockId          string
	GatewayKey       string
	GatewayActiveKey string
	// DelegationModeEffective / DelegationModeMaturityEpoch carry the timelock
	// resolution computed by the state engine (computeDelegationTimelock) into
	// SetWitnessUpdate. Zero-values mean "no pending downgrade" and are not
	// persisted. json tags are required: SetWitnessUpdate deep-copies this struct
	// via a JSON round-trip.
	DelegationModeEffective     string `json:"delegation_mode_effective"`
	DelegationModeMaturityEpoch uint64 `json:"delegation_mode_maturity_epoch"`
}
