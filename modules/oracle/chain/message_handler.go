package chain

import (
	"encoding/json"
	"errors"
	"fmt"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/oracle/p2p"

	"github.com/go-playground/validator/v10"
	"github.com/libp2p/go-libp2p/core/peer"
)

var (
	ErrInvalidChainOracleMessage = errors.New(
		"invalid chain oracle message",
	)
	ErrInvalidChainOracleMessageType = errors.New(
		"invalid chain oracle message type",
	)

	signatureMessageValidator = validator.New(
		validator.WithRequiredStructEnabled(),
	)
)

// Handle implements p2p.MessageHandler.
func (c *ChainOracle) Handle(
	peerID peer.ID,
	p2pMsg p2p.Msg,
	blockProducer string,
	_ []elections.ElectionMember,
) (p2p.Msg, error) {
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
		blockProducerSigRequest := chainOracleBlockProducerMessage{}
		if err := json.Unmarshal(msg.Payload, &blockProducerSigRequest); err != nil {
			return nil, fmt.Errorf(
				"failed to deserialize block producer message: %w",
				err,
			)
		}

		c.logger.Debug(
			"received message of type signatureRequest",
			"blockProducerSigRequest",
			blockProducerSigRequest,
		)

		w := chainOracleWitness{
			logger:            c.logger,
			username:          c.conf.Get().HiveUsername,
			privateBlsKeySeed: c.conf.Get().BlsPrivKeySeed,
			sessionID:         msg.SessionID,
			chainRelayMap:     c.chainRelayers,
			blockProducer:     blockProducer,
		}

		c.logger.Debug("created chain oracle witness", "witness (w)", w)

		signatureMsg, err := w.witnessChainData(&blockProducerSigRequest)
		if err != nil {
			if errors.Is(err, errInvalidBlockProducer) {
				c.logger.Debug(
					"dropping signature request not from block producer",
				)
				return nil, nil
			} else if errors.Is(err, errInvalidChainHash) {
				c.logger.Debug("invalid chain hash")
				return nil, nil
			}

			c.logger.Debug("error witnessing chain data", "err", err)
			return nil, err
		}

		payload, err := json.Marshal(signatureMsg)
		if err != nil {
			return nil, fmt.Errorf(
				"failed to serialize signature response: %w",
				err,
			)
		}

		c.logger.Debug(
			"got signature back from witnessChainData",
			"signatureMsg",
			signatureMsg,
			"marshalled payload",
			payload,
		)

		msg := chainOracleMessage{
			MessageType: signatureResponse,
			SessionID:   msg.SessionID,
			Payload:     json.RawMessage(payload),
		}

		return p2p.MakeOracleMessage(p2p.MsgChainRelay, &msg)

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

func receiveSignature(
	c *ChainOracle,
	msg *chainOracleMessage,
) error {
	var signatureResponse chainOracleWitnessMessage
	if err := json.Unmarshal(msg.Payload, &signatureResponse); err != nil {
		return err
	}

	c.logger.Debug(
		"received message of type signatureResponse",
		"signatureResponse",
		signatureResponse,
	)

	if err := signatureMessageValidator.Struct(&signatureResponse); err != nil {
		return fmt.Errorf("invalid signature message: %w", err)
	}

	return c.signatureChannels.receiveSignature(
		msg.SessionID,
		signatureResponse,
	)
}
