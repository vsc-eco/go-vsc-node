package tss_db

import (
	a "vsc-node/modules/aggregate"
)

// MaxKeyEpochs is the maximum number of epochs a key may be created or renewed for at once.
// about 3 months
const MaxKeyEpochs = uint64(365)

// KeyDeprecationGracePeriod is the number of blocks a key stays in "deprecated" status before
// being permanently retired (keystore entry deleted). At ~3s/block this is roughly 2 weeks.
const KeyDeprecationGracePeriod = uint64(403200)

// KeyRetirementEnabled controls whether deprecated keys ever advance to retired.
// When false, all newly deprecated keys receive deprecated_height=0, meaning no retirement
// clock is started and the key stays deprecated indefinitely until renewed.
const KeyRetirementEnabled = false

type TssKeys interface {
	a.Plugin
	InsertKey(id string, t TssKeyAlgo, epochs uint64) error
	FindKey(id string) (TssKey, error)
	SetKey(key TssKey) error
	FindNewKeys(blockHeight uint64) ([]TssKey, error)
	FindEpochKeys(epoch uint64) ([]TssKey, error)
	// FindDeprecatingKeys returns active keys whose ExpiryEpoch has been reached.
	FindDeprecatingKeys(epoch uint64) ([]TssKey, error)
	// FindNewlyRetired returns deprecated keys whose grace period has just elapsed.
	FindNewlyRetired(blockHeight uint64) ([]TssKey, error)
	// DeprecateLegacyKeys marks all active keys with no expiry as deprecated (deprecated_height=0).
	// Called once at node startup so pre-expiry-system keys are no longer reshared or signed.
	// Keys with deprecated_height=0 are not subject to the retirement grace period — they stay
	// deprecated until explicitly renewed.
	DeprecateLegacyKeys() error
}

type TssRequests interface {
	a.Plugin
	SetSignedRequest(req TssRequest) error
	FindUnsignedRequests(blockHeight uint64) ([]TssRequest, error)
	FindRequests(keyID string, msgHex []string) ([]TssRequest, error)
	UpdateRequest(req TssRequest) error
}

type TssCommitments interface {
	a.Plugin
	SetCommitmentData(commitment TssCommitment) error
	GetCommitment(keyId string, epoch uint64) (TssCommitment, error)
	GetCommitmentByHeight(keyId string, height uint64, qtype ...string) (TssCommitment, error)
	FindCommitments(keyId *string, byTypes []string, epoch *uint64, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]TssCommitment, error)
	GetBlames(epoch *uint64) ([]TssCommitment, error)
}

type TssKey struct {
	Id            string     `bson:"id"`
	Status        string     `bson:"status"` // created, active, deprecated, retired
	PublicKey     string     `bson:"public_key"`
	Owner         string     `bson:"owner"`
	Algo          TssKeyAlgo `bson:"algo"`
	CreatedHeight int64      `bson:"created_height"`
	Epoch         uint64     `bson:"epoch"`
	// Epochs is the requested lifespan in epochs (0 = no expiry).
	Epochs uint64 `bson:"epochs"`
	// ExpiryEpoch is the epoch at which the key expires (0 = no expiry).
	// Set to Epoch + Epochs when the key first becomes active.
	ExpiryEpoch uint64 `bson:"expiry_epoch"`
	// DeprecatedHeight is the block height at which this key was deprecated (0 = not deprecated).
	// Retirement fires at DeprecatedHeight + KeyDeprecationGracePeriod.
	DeprecatedHeight int64 `bson:"deprecated_height"`
}

type TssRequest struct {
	Id     string        `bson:"id"`
	Status TssSignStatus `bson:"status"`
	KeyId  string        `bson:"key_id"`
	Msg    string        `bson:"msg"`
	Sig    string        `bson:"sig"`
}

// CommitmentMetadata optionally stores error/reason for blame commitments (e.g. timeout vs error).
type CommitmentMetadata struct {
	Error  *string `json:"err"    bson:"err,omitempty"`
	Reason *string `json:"reason" bson:"reason,omitempty"`
}

type TssCommitment struct {
	//type = blame, reshare
	Type        string              `json:"type"         bson:"type"`
	BlockHeight uint64              `json:"block_height" bson:"block_height"`
	Epoch       uint64              `json:"epoch"        bson:"epoch"`
	Commitment  string              `json:"commitment"   bson:"commitment"`
	KeyId       string              `json:"key_id"       bson:"key_id"`
	TxId        string              `json:"tx_id"        bson:"tx_id"`
	PublicKey   *string             `json:"public_key"   bson:"public_key"`
	Metadata    *CommitmentMetadata `json:"metadata"     bson:"metadata,omitempty"`
	Timestamp   string              `json:"timestamp"    bson:"timestamp,omitempty"`
}

type TssKeyAlgo string

const (
	EcdsaType TssKeyAlgo = "ecdsa"
	EddsaType TssKeyAlgo = "eddsa"
)

type TssKeyStatus string

const (
	TssKeyCreated    string = "created"
	TssKeyActive     string = "active"
	TssKeyNew        string = "new"
	TssKeyDeprecated string = "deprecated"
	TssKeyRetired    string = "retired"
)

type TssSignStatus string

const (
	SignComplete TssSignStatus = "complete"
	SignPending  TssSignStatus = "unsigned"
)

type TssOp struct {
	Type   string `json:"type"`
	KeyId  string `json:"key_id"`
	Args   string `json:"args"`
	Epochs uint64 `json:"epochs,omitempty"`
}

