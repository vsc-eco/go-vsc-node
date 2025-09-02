package p2p

import (
	"context"
	"encoding/json"
	"log/slog"
	"vsc-node/modules/common"
	libp2p "vsc-node/modules/p2p"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
)

const oracleTopic = "/vsc/mainet/oracle/v1"

const (
	MsgBtcChainRelay MsgType = iota
	MsgOraclePriceObserve
	MsgOraclePriceBroadcast
)

type MsgType int

type Msg *OracleMessage

type OracleMessage struct {
	Type MsgType `json:"type,omitempty" validate:"required"`
	Data any     `json:"data,omitempty" validate:"required"`
}

type p2pSpec struct {
	conf common.IdentityConfig
}

var _ libp2p.PubSubServiceParams[Msg] = &p2pSpec{}

func New(conf common.IdentityConfig) *p2pSpec {
	return &p2pSpec{
		conf,
	}
}

// Topic implements PubSubServiceParams[Msg]
func (*p2pSpec) Topic() string {
	return oracleTopic
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
	var err error

	switch msg.Type {
	case MsgOraclePriceBroadcast:
		panic("not implemented")

	case MsgOraclePriceObserve:
		panic("not implemented")

	case MsgBtcChainRelay:
		panic("not implemented")

	default:
		panic("invalid message type")
	}

	return err
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
