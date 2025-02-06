package witnesses

type Witness struct {
	Account         string            `json:"account"`
	Height          uint64            `json:"height"`
	DidKeys         []PostingJsonKeys `json:"did_keys"`
	Enabled         bool              `json:"enabled"`
	GitCommit       string            `json:"git_commit"`
	NetId           string            `json:"net_id"`
	PeerId          string            `json:"peer_id"`
	ProtocolVersion uint64            `json:"protocol_version"`
	Ts              string            `json:"ts"`
	VersionId       string            `json:"version_id"`
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
	NetId           string `json:"net_id"`
	PeerId          string `json:"peer_id"`
	Ts              string `json:"ts"`
	GitCommit       string `json:"git_commit"`
	VersionId       string `json:"version_id" bson:"version_id"`
	ProtocolVersion string `json:"protocol_version"`
	Witness         struct {
		Enabled bool `json:"enabled"`
		// Plugins     []string `json:"plugins"`
		// DelayNotch  int      `json:"delay_notch"`
		// SigningKeys []string `json:"signing_keys"`
	} `json:"witness"`
}

type SetWitnessUpdateType struct {
	Metadata PostingJsonMetadata
	Account  string
	Height   uint64
	TxId     string
	BlockId  string
}
