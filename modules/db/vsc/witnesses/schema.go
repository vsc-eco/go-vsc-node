package witnesses

type Witness struct {
	NodeId string
	Key    Key
}

type Key interface {
	Bytes() []byte
}

type PostingJsonMetadata struct {
	VscNode PostingJsonMetadataVscNode `json:"vsc_node" bson:"vsc_node"`
	DidKeys []PostingJsonKeys          `json:"did_keys"`
}

type PostingJsonKeys struct {
	CryptoType string `json:"ct" bson:"ct"`
	Type       string `json:"t" bson:"t"`
	Key        string `json:"key" bson:"key"`
}

type PostingJsonMetadataVscNode struct {
	Did           string `json:"did"`
	UnsignedProof struct {
		NetId     string `json:"net_id"`
		PeerId    string `json:"ipfs_peer_id"`
		Ts        string `json:"ts"`
		GitCommit string `json:"git_commit"`
		VersionId string `json:"version_id" bson:"version_id"`
		Witness   struct {
			Enabled     bool     `json:"enabled"`
			Plugins     []string `json:"plugins"`
			DelayNotch  int      `json:"delay_notch"`
			SigningKeys []string `json:"signing_keys"`
		} `json:"witness"`
	} `json:"unsigned_proof"`
}

type SetWitnessUpdateType struct {
	Metadata PostingJsonMetadata
	Account  string
	Height   int
	TxId     string
	BlockId  string
}
