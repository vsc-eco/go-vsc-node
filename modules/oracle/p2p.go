package oracle

import (
	"context"
	"encoding/json"
	"log/slog"
	"vsc-node/modules/common"
	libp2p "vsc-node/modules/p2p"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
)

type Msg = *OracleMessage

type OracleMessage struct{}

type p2pSpec struct {
	conf   common.IdentityConfig
	oracle *Oracle
}

// p2pSpec implements PubSubServiceParams
func (*p2pSpec) Topic() string {
	return "oracle"
}

// ValidateMessage implements PubSubServiceParams[Msg any]
func (p *p2pSpec) ValidateMessage(
	ctx context.Context,
	from peer.ID,
	msg *pubsub.Message,
	parsedMsg Msg,
) bool {
	return false
}

// HandleMessage implements PubSubServiceParams[Msg any]
func (p *p2pSpec) HandleMessage(
	ctx context.Context,
	from peer.ID,
	msg Msg,
	send libp2p.SendFunc[Msg],
) error {
	// TODO: implement this
	return nil
}

// HandleRawMessage implements PubSubServiceParams[Msg any]
func (p *p2pSpec) HandleRawMessage(
	ctx context.Context,
	rawMsg *pubsub.Message,
	send libp2p.SendFunc[Msg],
) error {
	// Not needed(?)
	return nil
}

// ParseMessage implements PubSubServiceParams[Msg any]
func (p *p2pSpec) ParseMessage(data []byte) (Msg, error) {
	var msg Msg
	if err := json.Unmarshal(data, msg); err != nil {
		return nil, err
	}

	return msg, nil
}

// SerializeMessage implements PubSubServiceParams[Msg any]
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
