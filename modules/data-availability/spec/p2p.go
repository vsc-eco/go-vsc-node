package data_availability_spec

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
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
	switch msg.Type() {
	case P2pMessageData:
		blsPrivKey := &dids.BlsPrivKey{}
		var arr [32]byte
		blsPrivSeedHex := s.conf.Get().BlsPrivKeySeed
		blsPrivSeed, err := hex.DecodeString(blsPrivSeedHex)
		if err != nil {
			return fmt.Errorf("failed to decode bls priv seed: %w", err)
		}
		if len(blsPrivSeed) != 32 {
			return fmt.Errorf("bls priv seed must be 32 bytes")
		}

		copy(arr[:], blsPrivSeed)
		if err = blsPrivKey.Deserialize(&arr); err != nil {
			return fmt.Errorf("failed to deserialize bls priv key: %w", err)
		}

		provider, err := dids.NewBlsProvider(blsPrivKey)
		if err != nil {
			return fmt.Errorf("failed to create bls provider: %w", err)
		}

		c, err := s.dl.PutRaw(msg.Data(), datalayer.PutRawOptions{Pin: true})
		if err != nil {
			return fmt.Errorf("failed to create cid: %w", err)
		}

		sig, err := provider.SignRaw(*c)
		if err != nil {
			return fmt.Errorf("failed to sign data: %w", err)
		}

		sigData := append([]byte{byte(P2pMessageSignature)}, sig[:]...)

		return send(P2pMessage{sigData})
	}
	return nil
}

// HandleMessage implements libp2p.PubSubServiceParams.
func (p2pSpec) HandleRawMessage(ctx context.Context, rawMsg *pubsub.Message, send libp2p.SendFunc[P2pMessage]) error {
	// Not typically necessary to implement this method.
	return nil
}

var count = atomic.Int32{}

// ParseMessage implements libp2p.PubSubServiceParams.
func (p2pSpec) ParseMessage(data []byte) (P2pMessage, error) {
	fmt.Println("common message count:", count.Add(1))
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
	return "/vsc/mainnet/data-availability/v1"
}
