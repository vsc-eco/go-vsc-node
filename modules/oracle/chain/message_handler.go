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
		c.logger.Debug("invalid message type")
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
		signatureRequest := chainOracleBlockProducerMessage{}
		if err := json.Unmarshal(msg.Payload, &signatureRequest); err != nil {
			return nil, fmt.Errorf(
				"failed to deserialize block producer message: %w",
				err,
			)
		}

		c.logger.Debug(
			"signature request received",
			"block-producer", signatureRequest.BlockProducer,
			"signature-hash", signatureRequest.SigHash,
		)

		w := chainOracleWitness{
			logger:            c.logger,
			chainMap:          c.chainRelayers,
			username:          c.conf.Get().HiveUsername,
			privateBlsKeySeed: c.conf.Get().BlsPrivKeySeed,
			sessionID:         msg.SessionID,
			chainRelayMap:     c.chainRelayers,
			blockProducer:     blockProducer,
		}

		signatureMsg, err := w.witnessChainData(&signatureRequest)
		if err != nil {
			if errors.Is(err, errInvalidBlockProducer) {
				c.logger.Debug("not block producer, dropping message")
				return nil, nil
			}

			c.logger.Debug("error witnessing chain data", "err", err)
			return nil, err
		}

		c.logger.Debug(
			"chain data verified, chain hash signed",
			"signature", signatureMsg.Signature,
		)

		payload, err := json.Marshal(signatureMsg)
		if err != nil {
			return nil, fmt.Errorf(
				"failed to serialize signature response: %w",
				err,
			)
		}

		msg := chainOracleMessage{
			MessageType: signatureResponse,
			SessionID:   msg.SessionID,
			Payload:     json.RawMessage(payload),
		}

		return p2p.MakeOracleMessage(p2p.MsgChainRelay, &msg)

	case signatureResponse:
		payload := chainOracleWitnessMessage{}
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			return nil, fmt.Errorf("failed to deserialize payload: %w", err)
		}

		c.logger.Debug(
			"signature response received",
			"signature", payload.Signature,
			"signer", payload.Signer,
		)

		if err := signatureMessageValidator.Struct(&payload); err != nil {
			return nil, fmt.Errorf("failed to validate signature response: %w", err)
		}

		if err := c.signatureChannels.receiveSignature(msg.SessionID, payload); err != nil {
			return nil, fmt.Errorf("failed to receive signature: %w", err)
		}

		c.logger.Debug(
			"signature received",
			"signature", payload.Signature,
			"signer", payload.Signer,
		)

	default:
		return nil, ErrInvalidChainOracleMessageType
	}

	return nil, nil
}
