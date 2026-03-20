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
			"failed to deserialize chain oracle message: from [%s], err [%w]",
			peerID, err,
		)
	}

	switch msg.MessageType {
	case signatureRequest:
		// Witness: independently verify chain data and sign
		c.logger.Debug("received signature request",
			"sessionID", msg.SessionID,
			"peer", peerID,
		)
		response, err := witnessChainData(c, &msg)
		if err != nil {
			c.logger.Debug("failed to witness chain data",
				"sessionID", msg.SessionID,
				"peer", peerID,
				"err", err,
			)
			return nil, nil
		}

		responseMsg, err := makeChainOracleMessage(
			signatureResponse,
			msg.SessionID,
			response,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create response message: %w", err)
		}

		return p2p.MakeOracleMessage(p2p.MsgChainRelay, responseMsg)

	case signatureResponse:
		// Producer: route signature to the waiting channel
		if err := receiveSignature(c, &msg); err != nil {
			// Not an error — may not have an active session for this
			c.logger.Debug("failed to receive signature",
				"sessionID", msg.SessionID,
				"err", err,
			)
		}

	default:
		return nil, ErrInvalidChainOracleMessageType
	}

	return nil, nil
}

func receiveSignature(c *ChainOracle, msg *chainOracleMessage) error {
	var response chainRelayResponse
	if err := json.Unmarshal(msg.Payload, &response); err != nil {
		return fmt.Errorf("invalid signature response: %w", err)
	}

	return c.signatureChannels.receiveSignature(
		msg.SessionID,
		signatureMessage{
			Signature: response.Signature,
			Account:   response.Account,
			BlsDid:    response.BlsDid,
		},
	)
}
