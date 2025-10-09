package tss_helpers

const (
	SigningAlgoSecp256k1 = SigningAlgo("secp256k1")
	SigningAlgoEd25519   = SigningAlgo("ed25519")
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
