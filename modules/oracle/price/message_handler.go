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
)

type messageCode int

type PriceOracleMessage struct {
	MessageCode messageCode     `json:"message_code"`
	Payload     json.RawMessage `json:"payload"`
}

type SignatureRequestMessage struct {
	TxCid       string                    `json:"tx_cid"`
	MedianPrice map[string]api.PricePoint `json:"median_price"`
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

	msg := PriceOracleMessage{}
	if err := json.Unmarshal(incomingMsg.Data, &msg); err != nil {
		return nil, fmt.Errorf("failed to deserialize message: %w", err)
	}

	var response p2p.Msg
	switch msg.MessageCode {
	case averagePriceCode:

	default:
		return nil, p2p.ErrInvalidMessageType
	}

	return response, nil
}
