package tss

import (
	"vsc-node/modules/common/consensusversion"
	stateEngine "vsc-node/modules/state-processing"
	tss_helpers "vsc-node/modules/tss/helpers"

	bc "github.com/bnb-chain/tss-lib/v3/common"
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
	Args  []byte
	Algo  tss_helpers.SigningAlgo
}

type partyConfig struct {
	threshold int
}

type GetScheduler interface {
	GetSchedule(slotHeight uint64) []stateEngine.WitnessSlot
	// TssMinimumConsensusVersion is the minimum major/consensus triple for TSS at this Hive height.
	TssMinimumConsensusVersion(blockHeight uint64) consensusversion.Version
	// BtcKeysignHalted reports whether governance has frozen BTC TSS keysign via
	// vsc.tss_halt (Build Map §5b). Read by the solvency gate before issuing a sign.
	BtcKeysignHalted() bool
	// BtcTheftHalted reports whether the BTC mapping contract's deterministic theft-halt
	// flag ("th", M1.1b SPV-proven auto-trip) is set — mirrored into the consensus cache
	// each block. Read by the solvency gate alongside BtcKeysignHalted (either freezes).
	BtcTheftHalted() bool
	// EvmKeysignHalted reports whether governance has frozen the EVM bridges'
	// vault keysigns via vsc.tss_halt {"keyId":"evm"} (N2, from 0.13.0). Read by
	// the EVM sign gate (evm_halt.go).
	EvmKeysignHalted() bool
	// EvmBridgeHalted reports whether the EVM bridge contract's own theft flag
	// ("th") or custody flag ("si") is set, mirrored into the consensus cache at
	// the end of each block (CG-1). Read by the EVM sign gate with
	// EvmKeysignHalted (either freezes).
	EvmBridgeHalted(contract string) bool
	// BtcSolvencyHalted reports whether witnesses holding 2/3 of the weight voted
	// the BTC vault insolvent (N4, from 0.13.0; state-processing btc_solvency.go).
	// Read by the BTC sign gate with the other two BTC halts.
	BtcSolvencyHalted() bool
}
