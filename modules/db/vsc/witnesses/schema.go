package witnesses

type Witness struct {
	Account          string            `json:"account" bson:"account"`
	Height           uint64            `json:"height" bson:"height"`
	DidKeys          []PostingJsonKeys `json:"did_keys" bson:"did_keys"`
	Enabled          bool              `json:"enabled" bson:"enabled"`
	GitCommit        string            `json:"git_commit" bson:"git_commit"`
	NetId            string            `json:"net_id" bson:"net_id"`
	PeerId           string            `json:"peer_id" bson:"peer_id"`
	// VersionMajor with ProtocolVersion (consensus) and VersionNonConsensus form major.consensus.non_consensus.
	VersionMajor        uint64 `json:"version_major" bson:"version_major,omitempty"`
	ProtocolVersion     uint64 `json:"protocol_version" bson:"protocol_version"`
	VersionNonConsensus uint64 `json:"version_non_consensus" bson:"version_non_consensus,omitempty"`
	Ts               string            `json:"ts" bson:"ts"`
	TxId             string            `json:"tx_id" bson:"tx_id"`
	VersionId        string            `json:"version_id" bson:"version_id"`
	GatewayKey       string            `json:"gateway_key" bson:"gateway_key"`
	GatewayActiveKey string            `json:"gateway_active_key" bson:"gateway_active_key"`
	PeerAddrs        []string          `json:"peer_addrs" bson:"peer_addrs"`
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
	Witness         struct {
		Enabled bool `json:"enabled"`
		// Plugins     []string `json:"plugins"`
		// DelayNotch  int      `json:"delay_notch"`
		// SigningKeys []string `json:"signing_keys"`
	} `json:"witness"`
	GatewayKey       string `json:"gateway_key"`
	GatewayActiveKey string `json:"gateway_active_key"`
}

type SetWitnessUpdateType struct {
	Metadata         PostingJsonMetadata
	Account          string
	Height           uint64
	TxId             string
	BlockId          string
	GatewayKey       string
	GatewayActiveKey string
}
