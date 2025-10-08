package chain

import (
	"encoding/json"
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
	MessageType messageType     `json:"message_type"`
	SessionID   string          `json:"session_id"`
	Payload     json.RawMessage `json:"data"`
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
