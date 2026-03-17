package tss_db

import (
	a "vsc-node/modules/aggregate"

	"go.mongodb.org/mongo-driver/bson"
)

// MaxKeyEpochs is the maximum number of epochs a key may be created or renewed for at once.
const MaxKeyEpochs = uint64(52)

type TssKeys interface {
	a.Plugin
	InsertKey(id string, t TssKeyAlgo, epochs uint64) error
	FindKey(id string) (TssKey, error)
	SetKey(key TssKey) error
	FindNewKeys(blockHeight uint64) ([]TssKey, error)
	FindEpochKeys(epoch uint64) ([]TssKey, error)
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
	GetLatestCommitment(keyId string, qtype string) (TssCommitment, error)
	GetCommitmentByHeight(keyId string, height uint64, qtype ...string) (TssCommitment, error)
	GetBlames(...SearchOption) ([]TssCommitment, error)
}

type TssKey struct {
	Id            string     `bson:"id"`
	Status        string     `bson:"status"` //new, active, deleted (not implemented)
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
	Type        string               `json:"type"         bson:"type"`
	BlockHeight uint64               `json:"block_height" bson:"block_height"`
	Epoch       uint64               `json:"epoch"        bson:"epoch"`
	Commitment  string               `json:"commitment"   bson:"commitment"`
	KeyId       string               `json:"key_id"       bson:"key_id"`
	TxId        string               `json:"tx_id"        bson:"tx_id"`
	PublicKey   *string              `json:"public_key"   bson:"public_key"`
	Metadata    *CommitmentMetadata  `json:"metadata"     bson:"metadata,omitempty"`
}

type TssKeyAlgo string

const (
	EcdsaType TssKeyAlgo = "ecdsa"
	EddsaType TssKeyAlgo = "eddsa"
)

type TssKeyStatus string

const (
	TssKeyActive string = "active"
	TssKeyNew    string = "new"
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

type SearchOption func(m *bson.M) error

func HeightGt(height uint64) SearchOption {
	return func(m *bson.M) error {
		(*m)["block_height"] = bson.M{"$gt": height}
		return nil
	}
}

func ByEpoch(epoch uint64) SearchOption {
	return func(m *bson.M) error {
		(*m)["epoch"] = epoch
		return nil
	}
}

func ByType(typ ...string) SearchOption {
	return func(m *bson.M) error {
		(*m)["type"] = bson.M{"$in": typ}
		return nil
	}
}
