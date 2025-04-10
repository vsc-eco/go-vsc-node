package gateway

import (
	"context"
	"encoding/json"
	"fmt"
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

// ValidateMessage implements libp2p.PubSubServiceParams.
func (p2pSpec) ValidateMessage(ctx context.Context, from peer.ID, msg *pubsub.Message, parsedMsg p2pMessage) bool {
	// Can add a blacklist for spammers or ignore previously seen messages.
	return true
}

func (s p2pSpec) HandleMessage(ctx context.Context, from peer.ID, msg p2pMessage, send libp2p.SendFunc[p2pMessage]) error {
	if msg.Type == "sign_request" {
		if s.ms.bh == 0 {
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
				sig, err := s.ms.hiveCreator.Sign(signPkg.Tx)
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

			fmt.Println("executeActions signPkg", signPkg)

			if err != nil {
				return nil
			}

			if signPkg.TxId == signReq.TxId {
				sig, err := s.ms.hiveCreator.Sign(signPkg.Tx)
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
				sig, err := s.ms.hiveCreator.Sign(signPkg.Tx)
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
		if s.ms.msgChan[signResp.TxId] != nil {
			s.ms.msgChan[signResp.TxId] <- &msg
		}
	}
	return nil
}

func (p2pSpec) HandleRawMessage(ctx context.Context, rawMsg *pubsub.Message, send libp2p.SendFunc[p2pMessage]) error {
	// Not typically necessary to implement this method.
	return nil
}

func (p2pSpec) ParseMessage(data []byte) (p2pMessage, error) {
	res := p2pMessage{}
	json.Unmarshal(data, &res)
	return res, nil
}

func (p2pSpec) SerializeMessage(msg p2pMessage) []byte {
	ff, _ := json.Marshal(msg)
	return ff
}

func (p2pSpec) Topic() string {
	return "/vsc/mainnet/gateway/v1"
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
