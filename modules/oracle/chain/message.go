package chain

import (
	"encoding/json"
	"errors"
)

type messageType int

const (
	signatureRequest messageType = iota
	signatureResponse
)

var (
	errInvalidSessionID = errors.New("invalid session ID")
)

type chainOracleMessage struct {
	MessageType messageType     `json:"message_type"`
	SessionID   string          `json:"session_id"`
	Payload     json.RawMessage `json:"data"`
}

// chainRelayRequest is the payload for a signatureRequest message.
// The producer broadcasts this to ask witnesses to verify and sign chain data.
type chainRelayRequest struct {
	ContractId string `json:"contract_id"`
	NetId      string `json:"net_id"`
	Nonce      uint64 `json:"nonce"`
}

// chainRelayResponse is the payload for a signatureResponse message.
// Witnesses send this back with their BLS signature over the transaction CID.
type chainRelayResponse struct {
	Signature string `json:"signature"` // base64 BLS signature
	Account   string `json:"account"`   // signer's hive account
	BlsDid    string `json:"bls_did"`   // signer's BLS DID
}

func makeChainOracleMessage(
	msgType messageType,
	sessionID string,
	chainData any,
) (*chainOracleMessage, error) {
	if len(sessionID) == 0 {
		return nil, errInvalidSessionID
	}

	dataBytes, err := json.Marshal(chainData)
	if err != nil {
		return nil, err
	}

	msg := &chainOracleMessage{
		MessageType: msgType,
		SessionID:   sessionID,
		Payload:     dataBytes,
	}

	return msg, nil
}
