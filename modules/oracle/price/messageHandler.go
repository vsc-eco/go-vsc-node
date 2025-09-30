package price

import (
	"errors"
	"strings"
	"time"
	"vsc-node/modules/oracle/httputils"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/threadsafe"

	"github.com/libp2p/go-libp2p/core/peer"
)

// Handle implements p2p.MessageHandler.
func (o *PriceOracle) Handle(peerID peer.ID, msg p2p.Msg) (p2p.Msg, error) {
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
