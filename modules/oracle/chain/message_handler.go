package chain

import (
	"encoding/json"
	"errors"
	"fmt"
	"vsc-node/modules/oracle/httputils"
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

	msg, err := httputils.JsonUnmarshal[chainOracleMessage](p2pMsg.Data)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to deserialize chain oracle message: from [%s], err [%e]",
			peerID, err,
		)
	}

	switch msg.MessageType {
	case signatureRequest:
		return nil, nil

	case signatureResponse:
		if err := receiveSignature(c, msg); err != nil {
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

	return c.signatureChannels.receiveSignature(
		msg.SessionID,
		signatureResponse,
	)
}
