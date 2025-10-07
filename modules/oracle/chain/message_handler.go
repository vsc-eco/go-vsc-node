package chain

import (
	"encoding/json"
	"errors"
	"fmt"
	"vsc-node/modules/oracle/p2p"

	"github.com/libp2p/go-libp2p/core/peer"
)

var (
	ErrInvalidChainOracleMessage = errors.New(
		"invalid chain oracle message",
	)
	ErrInvalidChainOracleMessageType = errors.New(
		"invalid chain oracle message type",
	)
)

// Handle implements p2p.MessageHandler.
func (c *ChainOracle) Handle(peerID peer.ID, p2pMsg p2p.Msg) (p2p.Msg, error) {
	if p2pMsg.Code != p2p.MsgChainRelay {
		return nil, ErrInvalidChainOracleMessage
	}

	var msg chainOracleMessage
	if err := json.Unmarshal(p2pMsg.Data, &msg); err != nil {
		return nil, fmt.Errorf(
			"failed to deserialize chain oracle message: from [%s], err [%e]",
			peerID, err,
		)
	}

	switch msg.MessageType {
	case signatureRequest:
		signature, err := witnessChainData(&msg)
		if err != nil {
			return nil, err
		}

		response := chainOracleMessage{
			MessageType: signatureResponse,
			SessionID:   msg.SessionID,
			Payload:     json.RawMessage(signature),
		}

		return p2p.MakeOracleMessage(p2p.MsgChainRelay, &response)

	case signatureResponse:
		if err := receiveSignature(c, &msg); err != nil {
			return nil, fmt.Errorf(
				"failed to receive signature: session ID [%s], err [%w]",
				msg.SessionID, err,
			)
		}

	default:
		return nil, ErrInvalidChainOracleMessageType
	}

	return nil, nil
}

func receiveSignature(c *ChainOracle, msg *chainOracleMessage) error {
	var signatureResponse signatureMessage
	if err := json.Unmarshal(msg.Payload, &signatureResponse); err != nil {
		return err
	}

	// TODO: validate the signature message (adding a validator)

	return c.signatureChannels.receiveSignature(
		msg.SessionID,
		signatureResponse,
	)
}
