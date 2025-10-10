package tss_db

import (
	a "vsc-node/modules/aggregate"

	"go.mongodb.org/mongo-driver/bson"
)

type TssKeys interface {
	a.Plugin
	InsertKey(id string, t TssKeyAlgo) error
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
}

type TssRequest struct {
	Id     string     `bson:"id"`
	Status SignStatus `bson:"status"`
	KeyId  string     `bson:"key_id"`
	Msg    string     `bson:"msg"`
	Sig    string     `bson:"sig"`
}

type TssCommitment struct {
	//type = blame, reshare
	Type        string  `json:"type"         bson:"type"`
	BlockHeight uint64  `json:"block_height" bson:"block_height"`
	Epoch       uint64  `json:"epoch"        bson:"epoch"`
	Commitment  string  `json:"commitment"   bson:"commitment"`
	KeyId       string  `json:"key_id"       bson:"key_id"`
	TxId        string  `json:"tx_id"        bson:"tx_id"`
	PublicKey   *string `json:"public_key"   bson:"public_key"`
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

type SignStatus string

const (
	SignComplete SignStatus = "complete"
	SignPending  SignStatus = "pending"
)

type TssOp struct {
	Type  string `json:"type"`
	KeyId string `json:"key_id"`
	Args  string `json:"args"`
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
