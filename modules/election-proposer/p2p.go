package election_proposer

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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

type p2pMessage struct {
	Type    string `json:"type"`
	Version string `json:"v"`
	Op      string `json:"op"`
	Data    string `json:"data"`
}

type signRequest struct {
	Epoch       uint64 `json:"epoch"`
	BlockHeight uint64 `json:"block_height"`
}

type signResponse struct {
	Epoch   uint64 `json:"epoch"`
	Account string `json:"account"`
	Sig     string `json:"sig"`
}

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

	return true
	// switch parsedMsg := parsedMsg.(type) {
	// case p2pMessageElectionProposal:
	// 	return s.ValidateMessage(ctx, from, msg, p2pMessageElectionProposal(parsedMsg))
	// case p2pMessageElectionSignature:
	// 	proposer := s.electionProposer.Value()
	// 	if proposer == nil {
	// 		return false
	// 	}
	// 	fromStr := from.String()
	// 	res, err := proposer.witnesses.GetWitnessesByPeerId(fromStr)

	// 	fmt.Println("res, err", res, err)
	// 	if err != nil {
	// 		return false
	// 	}

	// 	if len(res) != 1 {
	// 		return false
	// 	}

	// 	key, err := res[0].ConsensusKey()
	// 	fmt.Println("key, err", res, err)
	// 	if err != nil {
	// 		return false
	// 	}

	// 	// err = proposer.circuit.AddAndVerifyRaw(key, electionData.Signature)
	// 	electionHeader, _, err := proposer.GenerateElection()

	// 	fmt.Println("electionHeader, _, err", electionHeader, err)
	// 	if err != nil {
	// 		return false
	// 	}

	// 	cid, err := electionHeader.Cid()
	// 	if err != nil {
	// 		return false
	// 	}

	// 	// Ensure that the signature is valid.
	// 	return blsu.Verify(key.Identifier(), cid.Bytes(), &parsedMsg.Signature)
	// default:
	// 	return false
	// }
}

// HandleMessage implements libp2p.PubSubServiceParams.
func (s p2pSpec) HandleMessage(ctx context.Context, from peer.ID, msg p2pMessage, send libp2p.SendFunc[p2pMessage]) error {
	ep := s.electionProposer.Value()
	if msg.Type == "sign_request" {
		if s.electionProposer.Value().bh == 0 {
			return nil
		}
		if msg.Op == "hold_election" {
			var signReq signRequest
			err := json.Unmarshal([]byte(msg.Data), &signReq)
			if err != nil {
				return nil
			}

			// err = s.electionProposer.waitCheckBh(ROTATION_INTERVAL, signReq.BlockHeight)

			// if err != nil {
			// 	return nil
			// }

			electionHeader, err := ep.makeElection(signReq.BlockHeight)

			if err != nil {
				return nil
			}

			if electionHeader.Epoch != signReq.Epoch {
				return nil
			}

			cid, err := electionHeader.Cid()

			if err != nil {
				return nil
			}

			sig, err := signCid(ep.conf, cid)

			if err != nil {
				return nil
			}

			sigBytes := sig.Serialize()

			sigStr := base64.URLEncoding.EncodeToString(sigBytes[:])

			resp := signResponse{
				Epoch:   electionHeader.Epoch,
				Sig:     sigStr,
				Account: ep.conf.Get().HiveUsername,
			}

			data, _ := json.Marshal(resp)

			send(p2pMessage{
				Type:    "sign_response",
				Version: "1",
				Op:      "hold_election",
				Data:    string(data),
			})
		}
	} else if msg.Type == "sign_response" {
		if msg.Op == "hold_election" {
			var signResp signResponse
			err := json.Unmarshal([]byte(msg.Data), &signResp)
			if err != nil {
				return nil
			}

			// err = s.electionProposer.waitCheckBh(ROTATION_INTERVAL, signResp.BlockHeight)

			if ep.sigChannels[signResp.Epoch] != nil {
				ep.sigChannels[signResp.Epoch] <- &signResp
			}
		}
	}

	// switch msg := msg.(type) {
	// case p2pMessageElectionProposal:
	// 	proposer := s.electionProposer.Value()
	// 	if proposer == nil {
	// 		return fmt.Errorf("election proposer is stopped")
	// 	}

	// 	electionHeader, _, err := proposer.GenerateElection()
	// 	if err != nil {
	// 		return err
	// 	}

	// 	cid, err := electionHeader.Cid()
	// 	if err != nil {
	// 		return err
	// 	}

	// 	sig, err := signCid(proposer.conf, cid)
	// 	if err != nil {
	// 		return err
	// 	}

	// 	send(p2pMessageElectionSignature{sig})
	// case p2pMessageElectionSignature:
	// 	proposer := s.electionProposer.Value()
	// 	if proposer == nil {
	// 		return fmt.Errorf("election proposer is stopped")
	// 	}

	// 	circuit := proposer.circuit

	// 	if circuit == nil {
	// 		return fmt.Errorf("election proposer is not proposing an election right now")
	// 	}

	// 	res, err := proposer.witnesses.GetWitnessesByPeerId(from.String())
	// 	if err != nil {
	// 		return err
	// 	}

	// 	key, err := res[0].ConsensusKey()
	// 	if err != nil {
	// 		return err
	// 	}

	// 	sig := msg.Signature.Serialize()

	// 	return circuit.AddAndVerifyRaw(key, sig[:])
	// }

	return nil
}

// HandleMessage implements libp2p.PubSubServiceParams.
func (p2pSpec) HandleRawMessage(ctx context.Context, rawMsg *pubsub.Message, send libp2p.SendFunc[p2pMessage]) error {
	// Not typically necessary to implement this method.
	return nil
}

// ParseMessage implements libp2p.PubSubServiceParams.
func (s p2pSpec) ParseMessage(data []byte) (p2pMessage, error) {
	msg := p2pMessage{}
	err := json.Unmarshal(data, &msg)
	return msg, err

	// if len(data) < 1 {
	// 	return nil, fmt.Errorf("message type not in message")
	// }
	// switch p2pMessageType(data[0]) {
	// case p2pMessageProposal:
	// 	data[0] = byte(p2pMessageSignature)
	// 	sig, err := s.ParseMessage(data)
	// 	return p2pMessageElectionProposal(sig.(p2pMessageElectionProposal)), err
	// case p2pMessageSignature:
	// 	if len(data) != 1+signatureSize {
	// 		return nil, fmt.Errorf("invalid signature message size")
	// 	}

	// 	res := p2pMessageElectionSignature{}

	// 	sig := [signatureSize]byte{}
	// 	copy(sig[:], data[1:])
	// 	err := res.Signature.Deserialize(&sig)
	// 	if err != nil {
	// 		return nil, err
	// 	}

	// 	return res, nil
	// default:
	// 	return nil, fmt.Errorf("unknown message type")
	// }
}

// SerializeMessage implements libp2p.PubSubServiceParams.
func (s p2pSpec) SerializeMessage(msg p2pMessage) []byte {
	jsonBytes, _ := json.Marshal(msg)
	return jsonBytes
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
