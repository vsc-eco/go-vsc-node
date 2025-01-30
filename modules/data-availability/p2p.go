package data_availability

import (
	"context"
	"encoding/hex"
	"fmt"
	"vsc-node/lib/dids"
	"vsc-node/modules/announcements"
	libp2p "vsc-node/modules/p2p"

	"github.com/ipfs/go-cid"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
)

type p2pSpec struct {
	conf announcements.AnnouncementsConfig
}

type p2pMessageType byte

const (
	p2pMessageData p2pMessageType = iota
	p2pMessageSignature
)

const signatureLength = 96 // See bls.Signature.Serialize() for details. (github.com/protolambda/bls12-381-util)

type p2pMessage struct {
	data []byte
}

func (p p2pMessage) Data() []byte {
	return p.data[1:]
}

func (p p2pMessage) Type() p2pMessageType {
	return p2pMessageType(p.data[0])
}

func (p p2pMessage) Valid() bool {
	if len(p.data) == 0 {
		return false
	}
	switch p.Type() {
	case p2pMessageData:
		return len(p.Data()) > 0
	case p2pMessageSignature:
		return len(p.Data()) == signatureLength
	default:
		return false
	}
}

var _ libp2p.PubSubServiceParams[p2pMessage] = p2pSpec{}

func (d *DataAvailability) startP2P() error {
	var err error
	d.service, err = libp2p.NewPubSubService(d.p2p, p2pSpec{d.conf})
	return err
}

func (d *DataAvailability) stopP2P() error {
	if d.service == nil {
		return nil
	}
	return d.service.Close()
}

// ValidateMessage implements libp2p.PubSubServiceParams.
func (p2pSpec) ValidateMessage(ctx context.Context, from peer.ID, msg *pubsub.Message) bool {
	// Can add a blacklist for spammers or ignore previously seen messages.
	return true
}

// HandleMessage implements libp2p.PubSubServiceParams.
func (s p2pSpec) HandleMessage(ctx context.Context, from peer.ID, msg p2pMessage, send libp2p.SendFunc[p2pMessage]) error {
	switch msg.Type() {
	case p2pMessageData:
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

		c, err := cid.V0Builder{}.Sum(msg.Data())
		if err != nil {
			return fmt.Errorf("failed to create cid: %w", err)
		}

		sig, err := provider.SignRaw(c)
		if err != nil {
			return fmt.Errorf("failed to sign data: %w", err)
		}

		sigData := append([]byte{byte(p2pMessageSignature)}, sig[:]...)

		return send(p2pMessage{sigData})
	}
	return nil
}

// HandleMessage implements libp2p.PubSubServiceParams.
func (p2pSpec) HandleRawMessage(ctx context.Context, rawMsg *pubsub.Message, send libp2p.SendFunc[p2pMessage]) error {
	// Not typically necessary to implement this method.
	return nil
}

// ParseMessage implements libp2p.PubSubServiceParams.
func (p2pSpec) ParseMessage(data []byte) (p2pMessage, error) {
	res := p2pMessage{data}
	if !res.Valid() {
		return res, fmt.Errorf("invalid message")
	}
	return res, nil
}

// SerializeMessage implements libp2p.PubSubServiceParams.
func (p2pSpec) SerializeMessage(msg p2pMessage) []byte {
	return msg.data
}

// Topic implements libp2p.PubSubServiceParams.
func (p2pSpec) Topic() string {
	return "/vsc/mainnet/data-availability/v1"
}
