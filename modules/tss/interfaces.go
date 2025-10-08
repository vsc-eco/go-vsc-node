package tss

import (
	stateEngine "vsc-node/modules/state-processing"
	tss_helpers "vsc-node/modules/tss/helpers"

	bc "github.com/bnb-chain/tss-lib/v2/common"
)

type KeySigner interface {
	SignMessage(msgToSign [][]byte, localStateItem KeygenLocalState, parties []string) ([]*bc.SignatureData, error)
}

type KeyGen interface {
}

type KeygenLocalState struct {
	PubKey          string   `json:"pub_key"`
	LocalData       []byte   `json:"local_data"`
	ParticipantKeys []string `json:"participant_keys"` // the participant of last key gen
	LocalPartyKey   string   `json:"local_party_key"`
}

type actionType string

const (
	KeyGenAction  actionType = "key_gen"
	SignAction    actionType = "sign"
	ReshareAction actionType = "reshare"
)

// DEPRECATE
type ActiveAction struct {
	Type      actionType
	KeyGen    *KeyGen
	KeySigner *KeySigner
}

type QueuedAction struct {
	Type  actionType
	KeyId string
	Args  [][]byte
	Algo  tss_helpers.SigningAlgo
}

type partyConfig struct {
	threshold int
}

type GetScheduler interface {
	GetSchedule(slotHeight uint64) []stateEngine.WitnessSlot
}
