package p2p

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	libp2p "vsc-node/modules/p2p"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
)

const OracleTopic = "/vsc/mainnet/oracle/v1"

type Msg *OracleMessage

type OracleMessage struct {
	Type MsgType `json:"type,omitempty" validate:"required"`
	Data any     `json:"data,omitempty" validate:"required"`
}

type MessageHandler interface {
	Handle(peer.ID, Msg) (Msg, error)
}

type OracleP2pSpec struct {
	handler MessageHandler
	// broadcastPriceChan      chan<- []AveragePricePoint
	// priceBlockSignatureChan chan<- VSCBlock
	// broadcastPriceBlockChan chan<- VSCBlock
}

var _ libp2p.PubSubServiceParams[Msg] = &OracleP2pSpec{}

func NewP2pSpec(
	msgHandler MessageHandler,
	// broadcastPriceChan chan<- []AveragePricePoint,
	// priceBlockSignatureChan chan<- VSCBlock,
	// broadcastPriceBlockChan chan<- VSCBlock,
) *OracleP2pSpec {
	return &OracleP2pSpec{msgHandler}
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

var errTimeout = errors.New(
	"[oracle] PubSubServiceParams[Msg].HandleMessage timed out.",
)

// HandleMessage implements PubSubServiceParams[Msg]
func (p *OracleP2pSpec) HandleMessage(
	ctx context.Context,
	from peer.ID,
	msg Msg,
	send libp2p.SendFunc[Msg],
) error {
	select {
	case <-ctx.Done():
		return errTimeout

	default:
		response, err := p.handler.Handle(from, msg)
		if err != nil {
			return err
		}

		if response != nil {
			return send(response)
		}
	}

	/*
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
			case p.broadcastPriceBlockChan <- *b:
			default:
				log.Println("channel full")
			}

		case MsgPriceOracleBroadcast:
			b, err := parseMsg[[]AveragePricePoint](msg.Data)
			if err != nil {
				return err
			}
			p.broadcastPriceChan <- *b

				case MsgBtcChainRelay:
					headBlock, err := parseMsg[BlockRelay](msg.Data)
					if err != nil {
						return err
					}
					p.btcHeadBlockChan <- headBlock

		case MsgPriceOracleSignedBlock:
			return errors.New("not implemented")

		default:
			return errors.New("invalid message type")
		}
	*/

	return nil
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
