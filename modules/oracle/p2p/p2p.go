package p2p

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"log/slog"
	"time"
	libp2p "vsc-node/modules/p2p"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
)

const OracleTopic = "/vsc/mainet/oracle/v1"

type Msg *OracleMessage

type OracleMessage struct {
	Type MsgType `json:"type,omitempty" validate:"required"`
	Data any     `json:"data,omitempty" validate:"required"`
}

type OracleP2pParams interface {
	libp2p.PubSubServiceParams[Msg]
	Initialize(chan<- []AveragePricePoint, chan<- VSCBlock, chan<- VSCBlock)
}

type OracleP2pSpec struct {
	broadcastPriceChan      chan<- []AveragePricePoint
	priceBlockSignatureChan chan<- VSCBlock
	broadcastNewBlockChan   chan<- VSCBlock
}

var _ OracleP2pParams = &OracleP2pSpec{}

func (p *OracleP2pSpec) Initialize(
	broadcastPriceChan chan<- []AveragePricePoint,
	priceBlockSignatureChan chan<- VSCBlock,
	broadcastPriceBlockChan chan<- VSCBlock,
) {
	*p = OracleP2pSpec{
		broadcastPriceChan,
		priceBlockSignatureChan,
		broadcastPriceBlockChan,
	}
}

// Topic implements PubSubServiceParams[Msg]
func (*OracleP2pSpec) Topic() string {
	return OracleTopic
}

// ValidateMessage implements PubSubServiceParams[Msg]
func (p *OracleP2pSpec) ValidateMessage(
	ctx context.Context,
	from peer.ID,
	msg *pubsub.Message,
	parsedMsg Msg,
) bool {
	return false
}

// HandleMessage implements PubSubServiceParams[Msg]
func (p *OracleP2pSpec) HandleMessage(
	ctx context.Context,
	from peer.ID,
	msg Msg,
	send libp2p.SendFunc[Msg],
) error {
	switch msg.Type {
	case MsgPriceOracleSignature:
		b, err := parseMsg[VSCBlock](msg.Data)
		if err != nil {
			return err
		}
		b.TimeStamp = time.Now().UTC().UnixMilli()

		select {
		case p.priceBlockSignatureChan <- *b:
		default:
			log.Println("channel full")
		}

	case MsgPriceOracleNewBlock:
		b, err := parseMsg[VSCBlock](msg.Data)
		if err != nil {
			return err
		}
		b.TimeStamp = time.Now().UTC().UnixMilli()

		select {
		case p.broadcastNewBlockChan <- *b:
		default:
			log.Println("channel full")
		}

	case MsgPriceOracleBroadcast:
		b, err := parseMsg[[]AveragePricePoint](msg.Data)
		if err != nil {
			return err
		}
		p.broadcastPriceChan <- *b

		/*
			case MsgBtcChainRelay:
				headBlock, err := parseMsg[BlockRelay](msg.Data)
				if err != nil {
					return err
				}
				p.btcHeadBlockChan <- headBlock
		*/

	case MsgPriceOracleSignedBlock:
		return errors.New("not implemented")

	default:
		return errors.New("invalid message type")
	}

	return nil
}

func parseMsg[T any](data any) (*T, error) {
	v, ok := data.(T)
	if !ok {
		return nil, errors.New("invalid type")
	}
	return &v, nil
}

// HandleRawMessage implements PubSubServiceParams[OracleMessage]
func (p *OracleP2pSpec) HandleRawMessage(
	ctx context.Context,
	rawMsg *pubsub.Message,
	send libp2p.SendFunc[Msg],
) error {
	// Not needed(?)
	return nil
}

// ParseMessage implements PubSubServiceParams[Msg]
func (p *OracleP2pSpec) ParseMessage(data []byte) (Msg, error) {
	var msg Msg
	if err := json.Unmarshal(data, msg); err != nil {
		return nil, err
	}

	return msg, nil
}

// SerializeMessage implements PubSubServiceParams[OracleMessage]
func (p *OracleP2pSpec) SerializeMessage(msg Msg) []byte {
	jsonBytes, err := json.Marshal(msg)
	if err != nil {
		slog.Error(
			"failed to serialize oracle message.",
			"msg", msg,
			"err", err,
		)
		return nil
	}
	return jsonBytes
}
