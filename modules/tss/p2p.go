package tss

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"vsc-node/modules/common"

	libp2p "vsc-node/modules/p2p"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/multiformats/go-multicodec"
	blsu "github.com/protolambda/bls12-381-util"
)

var protocolId = protocol.ID("/vsc.network/tss/1.0.0")

type p2pSpec struct {
	tssMgr *TssManager
}

type p2pMessage struct {
	Type    string                 `json:"type"`
	Account string                 `json:"account"`
	Data    map[string]interface{} `json:"data"`
}

// ValidateMessage implements libp2p.PubSubServiceParams.
func (p2pSpec) ValidateMessage(ctx context.Context, from peer.ID, msg *pubsub.Message, parsedMsg p2pMessage) bool {
	// Can add a blacklist for spammers or ignore previously seen messages.
	return true
}

func (s p2pSpec) HandleMessage(ctx context.Context, from peer.ID, msg p2pMessage, send libp2p.SendFunc[p2pMessage]) error {
	if msg.Type == "ask_sigs" {
		sessId, ok := msg.Data["session_id"].(string)

		if !ok {
			return nil
		}

		// fmt.Println("sessId", sessId, s.tssMgr.sessionResults[sessId] != nil)
		if s.tssMgr.sessionResults[sessId] != nil {
			baseCommitment := s.tssMgr.sessionResults[sessId].Serialize()
			commitBytes, _ := common.EncodeDagCbor(baseCommitment)

			commitCid, _ := common.HashBytes(commitBytes, multicodec.DagCbor)

			fmt.Println("commitedCid", commitCid)
			blsPrivKey := blsu.SecretKey{}
			var arr [32]byte
			blsPrivSeedHex := s.tssMgr.config.Get().BlsPrivKeySeed
			blsPrivSeed, err := hex.DecodeString(blsPrivSeedHex)
			if err != nil {
				return nil
			}
			if len(blsPrivSeed) != 32 {
				return nil
			}

			copy(arr[:], blsPrivSeed)
			if err = blsPrivKey.Deserialize(&arr); err != nil {
				return nil
			}
			sig := blsu.Sign(&blsPrivKey, commitCid.Bytes())

			sigBytes := sig.Serialize()

			sigStr := base64.URLEncoding.EncodeToString(sigBytes[:])

			fmt.Println("sigStr", sigStr)
			send(p2pMessage{
				Type:    "res_sig",
				Account: s.tssMgr.config.Get().HiveUsername,
				Data: map[string]interface{}{
					"sig":        sigStr,
					"session_id": sessId,
				},
			})
		}
	}

	if msg.Type == "res_sig" {
		sessId, ok := msg.Data["session_id"].(string)

		if !ok {
			return nil
		}

		sig, ok := msg.Data["sig"].(string)
		if !ok {
			return nil
		}

		// fmt.Println("sig ret", msg, s.tssMgr.sigChannels[sessId] != nil)

		if s.tssMgr.sigChannels[sessId] != nil {
			s.tssMgr.sigChannels[sessId] <- sigMsg{
				Account:   msg.Account,
				SessionId: sessId,
				Sig:       sig,
			}
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
	return "/tss/v1"
}

var _ libp2p.PubSubServiceParams[p2pMessage] = p2pSpec{}

type p2pService struct {
}

func (txp *TssManager) startP2P() error {
	service, err := libp2p.NewPubSubService(txp.p2p, p2pSpec{
		tssMgr: txp,
	})

	if err != nil {
		return err
	}
	txp.pubsub = service

	return nil
}

func (txp *TssManager) stopP2P() error {

	return nil
}

func (tss *TssManager) SendMsg(sessionId string, participant Participant, moniker string, msg []byte, isBroadcast bool, commiteeType string, cmtFrom string) error {
	witness, err := tss.witnessDb.GetWitnessAtHeight(participant.Account, nil)

	// fmt.Println("participant.Account", participant.Account)
	if err != nil {
		fmt.Println("GetWitnessAtHeight", err)
		return err
	}

	peerId, err := peer.Decode(witness.PeerId)

	if err != nil {
		return err
	}

	tMsg := TMsg{
		IsBroadcast: isBroadcast,
		SessionId:   sessionId,
		Type:        "msg",
		Data:        msg,
		Cmt:         commiteeType,
		CmtFrom:     cmtFrom,
	}
	tRes := TRes{}

	// fmt.Println("Sending message from", tss.config.Get().HiveUsername, "to", participant.Account, "isBroadcast", isBroadcast)
	return tss.client.Call(peerId, "vsc.tss", "ReceiveMsg", &tMsg, &tRes)
}
