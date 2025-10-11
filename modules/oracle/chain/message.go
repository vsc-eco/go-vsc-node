package chain

import (
	"errors"
)

// You need message type (exists), sessionId --> it is the ID of the sign
// operation, payload --> signature
// sign-ltc-2394823984
//types = signature response, signature ask
// signature response:
// - msg type
// - sessionId
// - payload aka signature
//signature ask:
// - type
// - chainId -> btc, ltc

type messageType int

const (
	signatureRequest messageType = iota
	signatureResponse
)

var (
	errInvalidSessionID = errors.New("invalid session ID")
)

type chainOracleMessage struct {
	MessageType messageType `json:"message_type"`
	SessionID   string      `json:"session_id"`
	Payload     []string    `json:"payload"`
}

type signatureMessage struct {
	// base64 encoded string of 96 bytes is 128
	Signature string `json:"signature,omitempty" validate:"base64,required,len=128"`
	Signer    string `json:"signer,omitempty"    validate:"required"`
}
