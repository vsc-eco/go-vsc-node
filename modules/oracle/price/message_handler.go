package price

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/oracle/httputils"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/threadsafe"

	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	averagePriceCode messageCode = iota
)

type messageCode int

type PriceOracleMessage struct {
	MessageCode messageCode     `json:"message_code"`
	Payload     json.RawMessage `json:"payload"`
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
	switch msg.Code {
	case p2p.MsgPriceBroadcast:
		data, err := httputils.JsonUnmarshal[map[string]p2p.AveragePricePoint](
			msg.Data,
		)
		if err != nil {
			return nil, err
		}

		pricePoints := collectPricePoint(peerID, data)
		if err := o.pricePoints.Consume(pricePoints); err != nil {
			if errors.Is(err, threadsafe.ErrLockedChannel) {
				o.logger.Debug(
					"unable to collect broadcasted average price points in the current block interval.",
				)
			} else {
				o.logger.Error("failed to collect pricePoint", "err", err)
			}
		}

	case p2p.MsgPriceSignature:
		block, err := httputils.JsonUnmarshal[p2p.OracleBlock](msg.Data)
		if err != nil {
			return nil, err
		}

		block.TimeStamp = time.Now().UTC()
		if err := o.signatures.Consume(*block); err != nil {
			if errors.Is(err, threadsafe.ErrLockedChannel) {
				o.logger.Debug(
					"unable to collect broadcasted signatures in the current block interval.",
				)
			} else {
				o.logger.Error("failed to collect signatures", "err", err)
			}
		}

	case p2p.MsgPriceBlock:
		block, err := httputils.JsonUnmarshal[p2p.OracleBlock](msg.Data)
		if err != nil {
			return nil, err
		}

		block.TimeStamp = time.Now().UTC()
		if err := o.producerBlocks.Consume(*block); err != nil {
			if errors.Is(err, threadsafe.ErrLockedChannel) {
				o.logger.Debug(
					"unable to collect and verify price block in the current block interval.",
				)
			} else {
				o.logger.Error("failed to collect price block", "err", err)
			}
		}

	default:
		return nil, p2p.ErrInvalidMessageType
	}

	return response, nil
}

func collectPricePoint(
	peerID peer.ID,
	data *map[string]p2p.AveragePricePoint,
) PricePointMap {
	recvTimeStamp := time.Now().UTC()

	m := make(PricePointMap)
	for symbol, avgPricePoint := range *data {
		sym := strings.ToUpper(symbol)

		pp := PricePoint{
			Price:       avgPricePoint.Price,
			Volume:      avgPricePoint.Volume,
			PeerID:      peerID,
			CollectedAt: recvTimeStamp,
		}

		v, ok := m[sym]
		if !ok {
			v = []PricePoint{pp}
		} else {
			v = append(v, pp)
		}

		m[sym] = v
	}

	return m
}
