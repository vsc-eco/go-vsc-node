package election_proposer

import (
	"context"
	"encoding/hex"
	"fmt"
	"vsc-node/modules/common"
	libp2p "vsc-node/modules/p2p"
	"weak"

	"github.com/ipfs/go-cid"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
	blsu "github.com/protolambda/bls12-381-util"
)

type p2pSpec struct {
	electionProposer weak.Pointer[electionProposer]
}

type p2pMessageType byte

const (
	p2pMessageProposal p2pMessageType = iota
	p2pMessageSignature
)

type p2pMessage interface{}

type p2pMessageElectionProposal p2pMessageElectionSignature

const signatureSize = 96

type p2pMessageElectionSignature struct {
	Signature blsu.Signature
}

var _ libp2p.PubSubServiceParams[p2pMessage] = p2pSpec{}

func (d *electionProposer) startP2P() error {
	var err error
	d.service, err = libp2p.NewPubSubService(d.p2p, p2pSpec{weak.Make(d)})
	return err
}

func (d *electionProposer) stopP2P() error {
	if d.service == nil {
		return nil
	}
	return d.service.Close()
}

// ValidateMessage implements libp2p.PubSubServiceParams.
func (s p2pSpec) ValidateMessage(ctx context.Context, from peer.ID, msg *pubsub.Message, parsedMsg p2pMessage) bool {
	switch parsedMsg := parsedMsg.(type) {
	case p2pMessageElectionProposal:
		return s.ValidateMessage(ctx, from, msg, p2pMessageElectionSignature(parsedMsg))
	case p2pMessageElectionSignature:
		proposer := s.electionProposer.Value()
		if proposer == nil {
			return false
		}
		fromStr := from.String()
		res, err := proposer.witnesses.GetWitnessesByPeerId(fromStr)
		if err != nil {
			return false
		}

		if len(res) != 1 {
			return false
		}

		key, err := res[0].ConsensusKey()
		if err != nil {
			return false
		}

		// err = proposer.circuit.AddAndVerifyRaw(key, electionData.Signature)
		electionHeader, _, err := proposer.GenerateElection()
		if err != nil {
			return false
		}

		cid, err := electionHeader.Cid()
		if err != nil {
			return false
		}

		// Ensure that the signature is valid.
		return blsu.Verify(key.Identifier(), cid.Bytes(), &parsedMsg.Signature)
	default:
		return false
	}
}

// HandleMessage implements libp2p.PubSubServiceParams.
func (s p2pSpec) HandleMessage(ctx context.Context, from peer.ID, msg p2pMessage, send libp2p.SendFunc[p2pMessage]) error {
	switch msg := msg.(type) {
	case p2pMessageElectionProposal:
		proposer := s.electionProposer.Value()
		if proposer == nil {
			return fmt.Errorf("election proposer is stopped")
		}

		electionHeader, _, err := proposer.GenerateElection()
		if err != nil {
			return err
		}

		cid, err := electionHeader.Cid()
		if err != nil {
			return err
		}

		sig, err := signCid(proposer.conf, cid)
		if err != nil {
			return err
		}

		send(p2pMessageElectionSignature{sig})
	case p2pMessageElectionSignature:
		proposer := s.electionProposer.Value()
		if proposer == nil {
			return fmt.Errorf("election proposer is stopped")
		}

		circuit := proposer.circuit

		if circuit == nil {
			return fmt.Errorf("election proposer is not proposing an election right now")
		}

		res, err := proposer.witnesses.GetWitnessesByPeerId(from.String())
		if err != nil {
			return err
		}

		key, err := res[0].ConsensusKey()
		if err != nil {
			return err
		}

		sig := msg.Signature.Serialize()

		return circuit.AddAndVerifyRaw(key, sig[:])
	}

	return nil
}

// HandleMessage implements libp2p.PubSubServiceParams.
func (p2pSpec) HandleRawMessage(ctx context.Context, rawMsg *pubsub.Message, send libp2p.SendFunc[p2pMessage]) error {
	// Not typically necessary to implement this method.
	return nil
}

// ParseMessage implements libp2p.PubSubServiceParams.
func (s p2pSpec) ParseMessage(data []byte) (p2pMessage, error) {
	if len(data) < 1 {
		return nil, fmt.Errorf("message type not in message")
	}
	switch p2pMessageType(data[0]) {
	case p2pMessageProposal:
		data[0] = byte(p2pMessageSignature)
		sig, err := s.ParseMessage(data)
		return p2pMessageElectionProposal(sig.(p2pMessageElectionSignature)), err
	case p2pMessageSignature:
		if len(data) != 1+signatureSize {
			return nil, fmt.Errorf("invalid signature message size")
		}

		res := p2pMessageElectionSignature{}

		sig := [signatureSize]byte{}
		copy(sig[:], data[1:])
		err := res.Signature.Deserialize(&sig)
		if err != nil {
			return nil, err
		}

		return res, nil
	default:
		return nil, fmt.Errorf("unknown message type")
	}
}

// SerializeMessage implements libp2p.PubSubServiceParams.
func (s p2pSpec) SerializeMessage(msg p2pMessage) []byte {
	switch msg := msg.(type) {
	case p2pMessageElectionProposal:
		res := s.SerializeMessage(p2pMessageElectionSignature(msg))
		res[0] = byte(p2pMessageProposal)
		return res
	case p2pMessageElectionSignature:
		res := make([]byte, 0, 1+signatureSize)
		sig := msg.Signature.Serialize()
		return append(append(res, byte(p2pMessageSignature)), sig[:]...)
	default:
		panic("unknown message type --- THIS IS A BUG")
	}
}

// Topic implements libp2p.PubSubServiceParams.
func (p2pSpec) Topic() string {
	return "/vsc/mainnet/election-proposal/v1"
}

func signCid(conf common.IdentityConfig, cid cid.Cid) (blsu.Signature, error) {
	blsPrivKey := blsu.SecretKey{}
	var arr [32]byte
	blsPrivSeedHex := conf.Get().BlsPrivKeySeed
	blsPrivSeed, err := hex.DecodeString(blsPrivSeedHex)
	if err != nil {
		return blsu.Signature{}, fmt.Errorf("failed to decode bls priv seed: %w", err)
	}
	if len(blsPrivSeed) != 32 {
		return blsu.Signature{}, fmt.Errorf("bls priv seed must be 32 bytes")
	}

	copy(arr[:], blsPrivSeed)
	if err = blsPrivKey.Deserialize(&arr); err != nil {
		return blsu.Signature{}, fmt.Errorf("failed to deserialize bls priv key: %w", err)
	}

	sig := blsu.Sign(&blsPrivKey, cid.Bytes())
	if sig == nil {
		return blsu.Signature{}, fmt.Errorf("signing failed")
	}

	return *sig, nil
}
