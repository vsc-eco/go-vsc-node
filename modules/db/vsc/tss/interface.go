package tss_db

import a "vsc-node/modules/aggregate"

type TssKeys interface {
	a.Plugin
	InsertKey(id string, t TssKeyType, owner string) error
	FindKey(id string) (TssKey, error)
	SetKey(id string, publicKey string) error
}

type TssRequests interface {
	a.Plugin
	SetSignedRequest(req TssRequest) error
	FindUnsignedRequests(blockHeight uint64) ([]TssRequest, error)
	FindRequest(keyID string, msgHex string) (*TssRequest, error)
}

type TssCommitments interface {
	a.Plugin
	SetCommitmentData(commitment TssCommitment) error
	GetCommitment(keyId string, epoch uint64) (TssCommitment, error)
	GetLatestCommitment(keyId string) (TssCommitment, error)
}

type TssKey struct {
	Id            string     `bson:"id"`
	Status        string     `bson:"status"` //new, active, deleted (not implemented)
	PublicKey     string     `bson:"public_key"`
	Owner         string     `bson:"owner"`
	Type          TssKeyType `bson:"type"`
	CreatedHeight int64      `bson:"created_height"`
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
	Type        string `json:"type" bson:"type"`
	BlockHeight uint64 `json:"block_height" bson:"block_height"`
	Epoch       uint64 `json:"epoch" bson:"epoch"`
	Commitment  string `json:"commitment" bson:"commitment"`
	KeyId       string `json:"key_id" bson:"key_id"`
	TxId        string `json:"tx_id" bson:"tx_id"`
}

type TssKeyType string

const (
	EcdsaType TssKeyType = "ecdsa"
	EddsaType TssKeyType = "eddsa"
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
