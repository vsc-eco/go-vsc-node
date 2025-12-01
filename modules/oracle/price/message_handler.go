package price

import (
	"encoding/json"
	"fmt"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/price/api"

	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	averagePriceCode messageCode = iota
	signatureRequestCode
	signatureResponseCode
)

type messageCode int

type PriceOracleMessage struct {
	MessageCode messageCode     `json:"message_code"`
	Payload     json.RawMessage `json:"payload"`
}

type SignatureRequestMessage struct {
	SigHash     string                    `json:"tx_cid"`
	MedianPrice map[string]api.PricePoint `json:"median_price"`
}

type SignatureResponseMessage struct {
	Signer    string `json:"signer"`
	Signature string `json:"signature"`
}

func makePriceOracleMessage(code messageCode, data any) (*PriceOracleMessage, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize data: %w", err)
	}

	msg := &PriceOracleMessage{
		MessageCode: code,
		Payload:     payload,
	}

	return msg, nil
}

// Handle implements p2p.MessageHandler.
func (o *PriceOracle) Handle(
	peerID peer.ID,
	incomingMsg p2p.Msg,
	blockProducerID string,
	electionsMembers []elections.ElectionMember,
) (p2p.Msg, error) {
	o.logger.Debug("message received", "data", string(incomingMsg.Data))

	msg := PriceOracleMessage{}
	if err := json.Unmarshal(incomingMsg.Data, &msg); err != nil {
		return nil, fmt.Errorf("failed to deserialize message: %w", err)
	}

	switch msg.MessageCode {
	case averagePriceCode:
		buf := make(map[string]api.PricePoint)
		if err := json.Unmarshal(msg.Payload, &buf); err != nil {
			return nil, fmt.Errorf("failed to deserialize average price message: %w", err)
		}

		if err := o.priceChannel.Send(buf); err != nil {
			return nil, fmt.Errorf("failed to consume average price: %w", err)
		}

	case signatureRequestCode:
		buf := SignatureRequestMessage{}
		if err := json.Unmarshal(msg.Payload, &buf); err != nil {
			return nil, fmt.Errorf("failed to deserialize signature request message: %w", err)
		}

		if err := o.sigRequestChannel.Send(buf); err != nil {
			return nil, fmt.Errorf("failed to consume signature request: %w", err)
		}

	case signatureResponseCode:
		buf := SignatureResponseMessage{}
		if err := json.Unmarshal(msg.Payload, &buf); err != nil {
			return nil, fmt.Errorf("failed to deserialize signature response message: %w", err)
		}

		if err := o.sigResponseChannel.Send(buf); err != nil {
			return nil, fmt.Errorf("failed to consume signature response: %w", err)
		}

	default:
		return nil, p2p.ErrInvalidMessageType
	}

	return nil, nil
}
