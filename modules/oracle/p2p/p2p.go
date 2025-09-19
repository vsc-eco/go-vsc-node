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

type Msg *oracleMessage

type oracleMessage struct {
	Code MsgCode         `json:"type,omitempty" validate:"required"`
	Data json.RawMessage `json:"data,omitempty" validate:"required"`
}

func MakeOracleMessage(code MsgCode, data any) (Msg, error) {
	jbytes, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return &oracleMessage{code, jbytes}, nil
}

type MessageHandler interface {
	Handle(peer.ID, Msg) (Msg, error)
}

type OracleP2pSpec struct {
	handler MessageHandler
}

var _ libp2p.PubSubServiceParams[Msg] = &OracleP2pSpec{}

func NewP2pSpec(msgHandler MessageHandler) *OracleP2pSpec {
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
	if parsedMsg == nil {
		return false
	}

	switch parsedMsg.Code {
	case MsgPriceBroadcast,
		MsgPriceBlock,
		MsgPriceSignature,
		MsgChainRelayBlock:

	default:
		return false
	}

	return len(parsedMsg.Data) > 0
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
