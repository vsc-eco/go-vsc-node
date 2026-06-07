package election_proposer

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
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

// ValidateMessage rejects (1) structurally invalid messages and (2) messages
// from non-witness peers. Combines two audit fixes:
//
//   - W-M4 (structural + GC guard): only the two election message types and the
//     hold_election op are ever valid; a nil proposer (GC'd) fails open here and
//     is re-guarded in HandleMessage, so there is no weak.Pointer nil deref.
//   - H-2 / F5 (witness allowlist — DoS/spam only, NOT consensus-affecting): the
//     election CID is deterministic from on-chain data and the deterministic
//     anchor (CP-0b) closed the anchor-poison vector, so this gate only stops an
//     UNAUTHENTICATED peer from forcing every node to do real work (a
//     sign_request for a not-yet-stored epoch drives makeElection → the
//     state-engine barrier → O(witnesses) ledger reads). GossipSub runs the
//     default StrictSign policy, so msg.GetFrom() is the cryptographically
//     authenticated author — a witness peer id cannot be spoofed.
//
// FAIL OPEN on the witness check: only a message whose authenticated author
// resolves DEFINITIVELY to no witness record is dropped. A nil proposer, an
// invalid/empty author, or ANY witness-DB lookup error → ALLOW, so a transient
// DB hiccup or an un-announced peer id can never stall election liveness.
// Determinism is not required here — this is a per-node propagation filter, not
// a CID input.
func (s p2pSpec) ValidateMessage(ctx context.Context, from peer.ID, msg *pubsub.Message, parsedMsg p2pMessage) bool {
	if parsedMsg.Type != "sign_request" && parsedMsg.Type != "sign_response" {
		return false
	}
	if parsedMsg.Op != "hold_election" {
		return false
	}
	proposer := s.electionProposer.Value()
	if proposer == nil {
		return true
	}
	author := msg.GetFrom()
	if author.Validate() != nil {
		// Unauthenticated / missing author (e.g. if signing were ever relaxed) —
		// fail open rather than risk dropping legitimate traffic.
		return true
	}
	res, err := proposer.witnesses.GetWitnessesByPeerId([]string{author.String()})
	if err != nil {
		// DB hiccup — never drop a legitimate message over a transient read.
		return true
	}
	if len(res) == 0 {
		log.Verbose("election pubsub: dropping message from non-witness peer",
			"author", author.String(), "type", parsedMsg.Type)
		return false
	}
	return true
}

// HandleMessage implements libp2p.PubSubServiceParams.
func (s p2pSpec) HandleMessage(
	ctx context.Context,
	from peer.ID,
	msg p2pMessage,
	send libp2p.SendFunc[p2pMessage],
) error {
	ep := s.electionProposer.Value()
	if ep == nil {
		return nil
	}
	if msg.Type == "sign_request" {
		if ep.bh.Load() == 0 {
			log.Verbose("sign_request: skipping (local bh=0, not yet synced)", "from", from.String())
			return nil
		}
		if msg.Op == "hold_election" {
			var signReq signRequest
			err := json.Unmarshal([]byte(msg.Data), &signReq)
			if err != nil {
				log.Warn("sign_request: malformed payload", "from", from.String(), "err", err)
				return nil
			}

			// Re-derive the election for the requested EPOCH. The anchor is
			// computed deterministically inside makeElection from the on-chain
			// previous election, so the peer-supplied BlockHeight is advisory
			// only (kept in logs for diagnostics) and cannot poison the anchor
			// or shift the sample window (audit F5 / CP-0b).
			electionHeader, electionData, err := ep.makeElection(signReq.Epoch)

			if err != nil {
				log.Warn("sign_request: makeElection failed; not signing",
					"from", from.String(),
					"req_epoch", signReq.Epoch,
					"req_block_height", signReq.BlockHeight,
					"err", err)
				return nil
			}

			if electionHeader.Epoch != signReq.Epoch {
				log.Warn("sign_request: epoch mismatch in re-derived header; not signing",
					"from", from.String(),
					"req_epoch", signReq.Epoch,
					"req_block_height", signReq.BlockHeight,
					"local_epoch", electionHeader.Epoch)
				return nil
			}

			if existing := ep.elections.GetElection(signReq.Epoch); existing != nil {
				log.Verbose("sign_request: election already stored locally; not signing again",
					"from", from.String(),
					"req_epoch", signReq.Epoch)
				return nil
			}

			cid, err := electionHeader.Cid()

			if err != nil {
				log.Warn("sign_request: CID compute failed; not signing",
					"from", from.String(),
					"req_epoch", signReq.Epoch,
					"req_block_height", signReq.BlockHeight,
					"err", err)
				return nil
			}

			memberAccounts := make([]string, 0, len(electionData.Members))
			for _, m := range electionData.Members {
				memberAccounts = append(memberAccounts, m.Account)
			}
			log.Verbose("sign_request: signing election",
				"from", from.String(),
				"req_epoch", signReq.Epoch,
				"req_block_height", signReq.BlockHeight,
				"cid", cid.String(),
				"data_cid", electionHeader.Data,
				"member_count", len(electionData.Members),
				"members", strings.Join(memberAccounts, ","))

			sig, err := signCid(ep.conf, cid)

			if err != nil {
				log.Warn("sign_request: sign failed",
					"from", from.String(),
					"req_epoch", signReq.Epoch,
					"err", err)
				return nil
			}

			sigBytes := sig.Serialize()

			sigStr := base64.URLEncoding.EncodeToString(sigBytes[:])

			log.Verbose("sign_request: produced signature",
				"from", from.String(),
				"req_epoch", signReq.Epoch,
				"req_block_height", signReq.BlockHeight,
				"account", ep.conf.Get().HiveUsername,
				"msg_hex", hex.EncodeToString(cid.Bytes()),
				"sig", sigStr)

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

			// H-15: look the channel up under the map lock, then send OUTSIDE
			// the lock. The send must be outside because a blocking send held
			// under sigChannelsMu would block HoldElection's delete (which also
			// takes sigChannelsMu, under electionMu) once the collector stops
			// receiving — wedging blockTick and the node. This closes the
			// concurrent map read/write crash without changing delivery.
			ep.sigChannelsMu.Lock()
			sigCh := ep.sigChannels[signResp.Epoch]
			ep.sigChannelsMu.Unlock()
			if sigCh != nil {
				sigCh <- &signResp
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
	return "/election-proposal/v1"
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
