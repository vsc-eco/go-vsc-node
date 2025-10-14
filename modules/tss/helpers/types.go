package tss_helpers

const (
	SigningAlgoEcdsa = SigningAlgo("ecdsa")
	SigningAlgoEddsa = SigningAlgo("eddsa")
)

type SigningAlgo string

// Type of authority used to generate the TSS key
// In the future we will introduce the concept of fractional authority to split a set of TSS keys across an equally weighted partitions of nodes.
const ConsensusAuthority = AuthorityType("consensus")

type AuthorityType string

type KeygenLocalState struct {
	PubKey          string   `json:"pub_key"`
	LocalData       []byte   `json:"local_data"`
	ParticipantKeys []string `json:"participant_keys"` // the participant of last key gen
	LocalPartyKey   string   `json:"local_party_key"`
}

type BaseCommitment struct {
	Type        string              `json:"type"`
	SessionId   string              `json:"session_id"`
	KeyId       string              `json:"key_id"`
	Commitment  string              `json:"commitment"`
	PublicKey   *string             `json:"pub_key"`
	Metadata    *CommitmentMetadata `json:"-"`
	BlockHeight uint64              `json:"block_height"`
	Epoch       uint64              `json:"epoch"`
}

type CommitmentMetadata struct {
	Error  *string `json:"err"`
	Reason *string `json:"reason"`
}

type SignedCommitment struct {
	BaseCommitment
	Signature string `json:"signature"`
	BitSet    string `json:"bv"`
}
