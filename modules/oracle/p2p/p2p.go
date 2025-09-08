package p2p

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"vsc-node/modules/common"
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

type p2pSpec struct {
	conf               common.IdentityConfig
	btcHeadBlockChan   chan<- *BlockRelay
	broadcastPriceChan chan<- []AveragePricePoint
}

var _ libp2p.PubSubServiceParams[Msg] = &p2pSpec{}

func New(
	conf common.IdentityConfig,
	btcHeadBlockChan chan<- *BlockRelay,
	broadcastPriceChan chan<- []AveragePricePoint,
) *p2pSpec {
	return &p2pSpec{conf, btcHeadBlockChan, broadcastPriceChan}
}

// Topic implements PubSubServiceParams[Msg]
func (*p2pSpec) Topic() string {
	return OracleTopic
}

// ValidateMessage implements PubSubServiceParams[Msg]
func (p *p2pSpec) ValidateMessage(
	ctx context.Context,
	from peer.ID,
	msg *pubsub.Message,
	parsedMsg Msg,
) bool {
	return false
}

// HandleMessage implements PubSubServiceParams[Msg]
func (p *p2pSpec) HandleMessage(
	ctx context.Context,
	from peer.ID,
	msg Msg,
	send libp2p.SendFunc[Msg],
) error {
	switch msg.Type {
	case MsgPriceOracleBroadcast:
		pricePoints, err := parseMsg[[]AveragePricePoint](msg.Data)
		if err != nil {
			return err
		}
		p.broadcastPriceChan <- *pricePoints

	case MsgBtcChainRelay:
		headBlock, err := parseMsg[BlockRelay](msg.Data)
		if err != nil {
			return err
		}
		p.btcHeadBlockChan <- headBlock

	case MsgPriceOracleSignedBlock:
		return errors.New("not implemented")

	case MsgPriceOracleNewBlock:
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
func (p *p2pSpec) HandleRawMessage(
	ctx context.Context,
	rawMsg *pubsub.Message,
	send libp2p.SendFunc[Msg],
) error {
	// Not needed(?)
	return nil
}

// ParseMessage implements PubSubServiceParams[Msg]
func (p *p2pSpec) ParseMessage(data []byte) (Msg, error) {
	var msg Msg
	if err := json.Unmarshal(data, msg); err != nil {
		return nil, err
	}

	return msg, nil
}

// SerializeMessage implements PubSubServiceParams[OracleMessage]
func (p *p2pSpec) SerializeMessage(msg Msg) []byte {
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
