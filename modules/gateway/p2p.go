package gateway

import (
	"context"
	"encoding/json"
	"vsc-node/modules/common"
	libp2p "vsc-node/modules/p2p"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
)

type p2pMessage struct {
	Type    string `json:"type"`
	Version string `json:"v"`
	Op      string `json:"op"`
	Data    string `json:"data"`
}

type signRequest struct {
	TxId        string `json:"tx_id"`
	BlockHeight uint64 `json:"block_height"`
}

type signResponse struct {
	TxId string `json:"tx_id"`
	Sig  string `json:"sig"`
}

type p2pSpec struct {
	conf common.IdentityConfig

	ms *MultiSig
}

// ValidateMessage is the gossipsub topic validator (registered in
// modules/p2p/pubsub.go RegisterTopicValidator). Returning false means the
// message is NEITHER relayed onward through the mesh NOR dispatched to
// HandleMessage on this node, so this is the ingress chokepoint for the
// /gateway/v1 topic. Only the two protocol message types are admitted:
//
//   - sign_request  — gated by current-election peer.ID. The leader publishes
//     it; a peer not in electionPeerIDs cannot trigger expensive signing work
//     on every witness (finding S7). The allow-set is now rebuilt every
//     ACTION_INTERVAL (multisig.go BlockTick) so a leader whose libp2p identity
//     rotated mid-epoch is admitted within ~1 min, not the next epoch (MED #55).
//
//   - sign_response — admitted unconditionally so gossip can relay a cosigner's
//     response toward the leader through intermediate (non-leader) nodes. It is
//     deliberately NOT peer.ID-gated: gating it by peer.ID would drop the
//     response of any witness whose libp2p identity rotated ahead of the
//     witness-DB resync (MED #55 / M26-K5-M1), self-excluding that witness from
//     the multisig. The security boundary for a sign_response is downstream and
//     identity-rotation-independent: waitForSigs (multisig.go) recovers the
//     secp256k1 signer pubkey from the signature over the real tx hash, requires
//     it to be in the committee publicList, and dedups per pubkey — so a forged
//     or outsider response is rejected there, and the collector channel is a
//     16-deep non-blocking buffer that drops on overflow (bounds the flood,
//     MED #3 / #6). A sign_response flood cannot be dropped at THIS validator
//     without either re-introducing MED #55 (peer.ID gate) or breaking mesh
//     relay (a leader-only TxId gate), so it is bounded here, not closed here.
//
// Any other (unknown or zero-value Type:"") message is rejected outright. The
// previous code returned true for everything that was not a sign_request, which
// let a malformed/zero-value message (Type=="", see MED #110) and arbitrary
// junk types propagate through the mesh.
func (s p2pSpec) ValidateMessage(ctx context.Context, from peer.ID, msg *pubsub.Message, parsedMsg p2pMessage) bool {
	switch parsedMsg.Type {
	case "sign_request":
		allowed := s.ms.electionPeerIDs.Load()
		if allowed == nil {
			return false
		}
		return (*allowed)[from.String()]
	case "sign_response":
		// Relay-safe, identity-rotation-safe. Authenticated downstream by
		// recovered secp256k1 pubkey in waitForSigs, not by peer.ID.
		return true
	default:
		return false
	}
}

func (s p2pSpec) HandleMessage(ctx context.Context, from peer.ID, msg p2pMessage, send libp2p.SendFunc[p2pMessage]) error {
	if msg.Type == "sign_request" {
		// MED M38-MED-4 (#112): ms.bh is written by BlockTick concurrently, so
		// read it through loadBh — a bare field read here raced.
		if s.ms.loadBh() == 0 {
			return nil
		}
		if msg.Op == "key_rotation" {
			var signReq signRequest
			err := json.Unmarshal([]byte(msg.Data), &signReq)
			if err != nil {
				return nil
			}

			err = s.ms.waitCheckBh(ROTATION_INTERVAL, signReq.BlockHeight)

			if err != nil {
				return nil
			}

			signPkg, err := s.ms.keyRotation(signReq.BlockHeight)

			if err != nil {
				return nil
			}

			if signPkg.TxId == signReq.TxId {
				kp := s.ms.getSigningKp()
				if kp == nil {
					return nil
				}
				sig, err := signPkg.Tx.Sign(*kp, s.ms.hiveClient.ChainID)
				if err != nil {
					return nil
				}

				resp := signResponse{
					TxId: signPkg.TxId,
					Sig:  sig,
				}

				data, _ := json.Marshal(resp)

				send(p2pMessage{
					Type:    "sign_response",
					Version: "1",
					Op:      "key_rotation",
					Data:    string(data),
				})
			}
		} else if msg.Op == "execute_actions" {

			var signReq signRequest
			err := json.Unmarshal([]byte(msg.Data), &signReq)
			if err != nil {
				return nil
			}

			err = s.ms.waitCheckBh(ACTION_INTERVAL, signReq.BlockHeight)

			if err != nil {
				return nil
			}

			signPkg, err := s.ms.executeActions(signReq.BlockHeight)

			log.Verbose("executeActions signPkg", "txId", signPkg.TxId)

			if err != nil {
				return nil
			}

			if signPkg.TxId == signReq.TxId {
				kp := s.ms.getSigningKp()
				if kp == nil {
					return nil
				}
				sig, err := signPkg.Tx.Sign(*kp, s.ms.hiveClient.ChainID)
				if err != nil {
					return nil
				}

				resp := signResponse{
					TxId: signPkg.TxId,
					Sig:  sig,
				}

				data, _ := json.Marshal(resp)

				send(p2pMessage{
					Type:    "sign_response",
					Version: "1",
					Op:      "execute_actions",
					Data:    string(data),
				})
			}
		} else if msg.Op == "fr_sync" {
			var signReq signRequest
			err := json.Unmarshal([]byte(msg.Data), &signReq)
			if err != nil {
				return nil
			}

			err = s.ms.waitCheckBh(SYNC_INTERVAL, signReq.BlockHeight)

			if err != nil {
				return nil
			}

			signPkg, err := s.ms.syncBalance(signReq.BlockHeight)

			if err != nil {
				return nil
			}

			if signPkg.TxId == signReq.TxId {
				kp := s.ms.getSigningKp()
				if kp == nil {
					return nil
				}
				sig, err := signPkg.Tx.Sign(*kp, s.ms.hiveClient.ChainID)
				if err != nil {
					return nil
				}

				resp := signResponse{
					TxId: signPkg.TxId,
					Sig:  sig,
				}

				data, _ := json.Marshal(resp)

				send(p2pMessage{
					Type:    "sign_response",
					Version: "1",
					Op:      "fr_sync",
					Data:    string(data),
				})
			}
		}
		// s.ms.executeActions()
		// sig, err := s.ms.signRequest(ctx, signReq)
		// if err != nil {
		// 	return err
		// }
		// resp := signResponse{
		// 	TxId: signReq.TxId,
		// 	Sig:  sig,
		// }
		// data, _ := json.Marshal(resp)
		// msg := p2pMessage{
		// 	Type:    "sign_response",
		// 	Version: "1",
		// 	Data:    string(data),
		// }
		// send(msg)
	} else if msg.Type == "sign_response" {
		var signResp signResponse
		err := json.Unmarshal([]byte(msg.Data), &signResp)
		if err != nil {
			return nil
		}
		// Guarded read + non-blocking send. The collector goroutine in
		// waitForSigs exits on its ctx timeout, so a blocking send here could
		// hang the pubsub dispatch goroutine forever on a full buffer with no
		// reader. Dropping a late signature is harmless — collection is
		// best-effort and the action is retried on the next tick.
		if ch := s.ms.getMsgChan(signResp.TxId); ch != nil {
			select {
			case ch <- &msg:
			default:
			}
		}
	}
	return nil
}

func (p2pSpec) HandleRawMessage(ctx context.Context, rawMsg *pubsub.Message, send libp2p.SendFunc[p2pMessage]) error {
	// Not typically necessary to implement this method.
	return nil
}

// ParseMessage decodes the wire bytes. The json.Unmarshal error is now
// propagated (MED #110 / M38-MED-1): the topic validator in
// modules/p2p/pubsub.go treats a ParseMessage error as an invalid message and
// drops it (no relay, no dispatch), and the dispatch goroutine likewise returns
// on the error. Swallowing it turned malformed input into a zero-value
// p2pMessage{Type:""} that previously sailed through ValidateMessage.
func (p2pSpec) ParseMessage(data []byte) (p2pMessage, error) {
	res := p2pMessage{}
	err := json.Unmarshal(data, &res)
	return res, err
}

func (p2pSpec) SerializeMessage(msg p2pMessage) []byte {
	ff, _ := json.Marshal(msg)
	return ff
}

func (p2pSpec) Topic() string {
	return "/gateway/v1"
}

var _ libp2p.PubSubServiceParams[p2pMessage] = p2pSpec{}

type p2pService struct {
}

func (ms *MultiSig) startP2P() error {
	service, err := libp2p.NewPubSubService(ms.p2p, p2pSpec{
		ms: ms,
	})
	if err != nil {
		return err
	}
	ms.service = service
	return nil
}

func (txp *MultiSig) stopP2P() error {

	return nil
}
