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

var (
	ErrInvalidMessageType = errors.New("invalid message type")
	errTimeout            = errors.New(
		"[oracle] PubSubServiceParams[Msg].HandleMessage timed out.",
	)
)

type OracleVscSpec interface {
	Broadcast(MsgCode, any) error
	SubmitToContract(*OracleBlock) error
	ValidateSignature(*OracleBlock, string) error
}

type MessageHandler interface {
	Handle(peer.ID, Msg) (Msg, error)
}

type OracleP2PMessageHandler struct {
	handler MessageHandler
}

var _ libp2p.PubSubServiceParams[Msg] = &OracleP2PMessageHandler{}

func NewP2pSpec(msgHandler MessageHandler) *OracleP2PMessageHandler {
	return &OracleP2PMessageHandler{msgHandler}
}

// Topic implements PubSubServiceParams[Msg]
func (*OracleP2PMessageHandler) Topic() string {
	return OracleTopic
}

// ValidateMessage implements PubSubServiceParams[Msg]
func (p *OracleP2PMessageHandler) ValidateMessage(
	ctx context.Context,
	from peer.ID,
	msg *pubsub.Message,
	parsedMsg Msg,
) bool {
	if parsedMsg == nil {
		return false
	}

	switch parsedMsg.Code {
	case MsgPriceBroadcast,
		MsgPriceBlock,
		MsgPriceSignature,
		MsgChainRelay:

	default:
		return false
	}

	return len(parsedMsg.Data) > 0
}

// HandleMessage implements PubSubServiceParams[Msg]
func (p *OracleP2PMessageHandler) HandleMessage(
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

	return nil
}

// HandleRawMessage implements PubSubServiceParams[OracleMessage]
func (p *OracleP2PMessageHandler) HandleRawMessage(
	ctx context.Context,
	rawMsg *pubsub.Message,
	send libp2p.SendFunc[Msg],
) error {
	// Not needed(?)
	return nil
}

// ParseMessage implements PubSubServiceParams[Msg]
func (p *OracleP2PMessageHandler) ParseMessage(data []byte) (Msg, error) {
	var msg Msg
	if err := json.Unmarshal(data, msg); err != nil {
		return nil, err
	}

	return msg, nil
}

// SerializeMessage implements PubSubServiceParams[OracleMessage]
func (p *OracleP2PMessageHandler) SerializeMessage(msg Msg) []byte {
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
