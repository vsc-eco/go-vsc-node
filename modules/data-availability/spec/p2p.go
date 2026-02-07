package data_availability_spec

import (
	"context"
	"fmt"
	"vsc-node/lib/datalayer"
	"vsc-node/modules/common"
	libp2p "vsc-node/modules/p2p"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
)

type p2pSpec struct {
	conf common.IdentityConfig
	dl   *datalayer.DataLayer
}

type P2pSpec = *p2pSpec

func New(conf common.IdentityConfig, dl *datalayer.DataLayer) P2pSpec {
	return &p2pSpec{
		conf,
		dl,
	}
}

func (s p2pSpec) Conf() common.IdentityConfig {
	return s.conf
}

func (s p2pSpec) Datalayer() *datalayer.DataLayer {
	return s.dl
}

type p2pMessageType byte

const (
	P2pMessageData p2pMessageType = iota
	P2pMessageSignature
)

func (mt p2pMessageType) String() string {
	switch mt {
	case P2pMessageData:
		return "Data"
	case P2pMessageSignature:
		return "Signature"
	default:
		panic("unknown message type")
	}
}

const signatureLength = 96 // See bls.Signature.Serialize() for details. (github.com/protolambda/bls12-381-util)

type P2pMessage struct {
	data []byte
}

func NewP2pMessage(Type p2pMessageType, data []byte) P2pMessage {
	return P2pMessage{
		data: append([]byte{byte(Type)}, data...),
	}
}

func (p P2pMessage) Data() []byte {
	return p.data[1:]
}

func (p P2pMessage) Type() p2pMessageType {
	return p2pMessageType(p.data[0])
}

func (p P2pMessage) Valid() bool {
	if len(p.data) == 0 {
		return false
	}
	switch p.Type() {
	case P2pMessageData:
		return len(p.Data()) > 0
	case P2pMessageSignature:
		return len(p.Data()) == signatureLength
	default:
		return false
	}
}

var _ libp2p.PubSubServiceParams[P2pMessage] = p2pSpec{}

// ValidateMessage implements libp2p.PubSubServiceParams.
func (p2pSpec) ValidateMessage(ctx context.Context, from peer.ID, msg *pubsub.Message, parsedMsg P2pMessage) bool {
	// Can add a blacklist for spammers or ignore previously seen messages.
	return true
}

// HandleMessage implements libp2p.PubSubServiceParams.
func (s p2pSpec) HandleMessage(ctx context.Context, from peer.ID, msg P2pMessage, send libp2p.SendFunc[P2pMessage]) error {
	return nil
}

// HandleMessage implements libp2p.PubSubServiceParams.
func (p2pSpec) HandleRawMessage(ctx context.Context, rawMsg *pubsub.Message, send libp2p.SendFunc[P2pMessage]) error {
	// Not typically necessary to implement this method.
	return nil
}

// var count = atomic.Int32{}

// ParseMessage implements libp2p.PubSubServiceParams.
func (p2pSpec) ParseMessage(data []byte) (P2pMessage, error) {
	// fmt.Println("common message count:", count.Add(1))
	res := P2pMessage{data}
	if !res.Valid() {
		return res, fmt.Errorf("invalid message")
	}
	return res, nil
}

// SerializeMessage implements libp2p.PubSubServiceParams.
func (p2pSpec) SerializeMessage(msg P2pMessage) []byte {
	return msg.data
}

// Topic implements libp2p.PubSubServiceParams.
func (p2pSpec) Topic() string {
	return "/data-availability/v1"
}
