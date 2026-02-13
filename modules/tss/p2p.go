package tss

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
	"vsc-node/modules/common"

	libp2p "vsc-node/modules/p2p"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/network"
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
	return "/vsc/mainnet/tss/v1"
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

// SendMsg sends a TSS message to a participant with retry logic and connection health checks
func (tss *TssManager) SendMsg(sessionId string, participant Participant, moniker string, msg []byte, isBroadcast bool, commiteeType string, cmtFrom string) error {
	return tss.sendMsgWithRetry(sessionId, participant, moniker, msg, isBroadcast, commiteeType, cmtFrom, 0)
}

// sendMsgWithRetry implements retry logic with exponential backoff
func (tss *TssManager) sendMsgWithRetry(sessionId string, participant Participant, moniker string, msg []byte, isBroadcast bool, commiteeType string, cmtFrom string, attempt int) error {
	const maxRetries = 3
	const baseRetryDelay = 1 * time.Second

	startTime := time.Now()
	fromAccount := tss.config.Get().HiveUsername

	witness, err := tss.witnessDb.GetWitnessAtHeight(participant.Account, nil)

	if err != nil {
		fmt.Printf("[TSS] [P2P] ERROR: GetWitnessAtHeight failed sessionId=%s from=%s to=%s err=%v\n",
			sessionId, fromAccount, participant.Account, err)
		fmt.Println("GetWitnessAtHeight", err)
		return err
	}

	peerId, err := peer.Decode(witness.PeerId)

	if err != nil {
		fmt.Printf("[TSS] [P2P] ERROR: PeerId decode failed sessionId=%s from=%s to=%s peerId=%s err=%v\n",
			sessionId, fromAccount, participant.Account, witness.PeerId, err)
		return err
	}

	// Check connection health before sending
	if !tss.isPeerConnected(peerId) {
		if attempt < maxRetries {
			retryDelay := baseRetryDelay * time.Duration(1<<uint(attempt)) // Exponential backoff: 1s, 2s, 4s
			fmt.Printf("[TSS] [P2P] WARN: Peer not connected, will retry sessionId=%s to=%s peerId=%s attempt=%d/%d delay=%v\n",
				sessionId, participant.Account, peerId.String(), attempt+1, maxRetries, retryDelay)
			time.Sleep(retryDelay)
			return tss.sendMsgWithRetry(sessionId, participant, moniker, msg, isBroadcast, commiteeType, cmtFrom, attempt+1)
		} else {
			fmt.Printf("[TSS] [P2P] ERROR: Peer not connected after %d attempts sessionId=%s to=%s peerId=%s\n",
				maxRetries, sessionId, participant.Account, peerId.String())
			return fmt.Errorf("peer %s not connected after %d retries", peerId.String(), maxRetries)
		}
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

	if attempt == 0 {
		fmt.Printf("[TSS] [P2P] Sending message sessionId=%s from=%s to=%s isBroadcast=%v cmt=%s cmtFrom=%s msgLen=%d\n",
			sessionId, fromAccount, participant.Account, isBroadcast, commiteeType, cmtFrom, len(msg))
	} else {
		fmt.Printf("[TSS] [P2P] Retrying message sessionId=%s from=%s to=%s attempt=%d/%d msgLen=%d\n",
			sessionId, fromAccount, participant.Account, attempt+1, maxRetries, len(msg))
	}

	// Add timeout to RPC call using a channel
	errChan := make(chan error, 1)
	go func() {
		errChan <- tss.client.Call(peerId, "vsc.tss", "ReceiveMsg", &tMsg, &tRes)
	}()

	select {
	case err = <-errChan:
		// RPC call completed
	case <-time.After(30 * time.Second):
		err = fmt.Errorf("RPC call timeout after 30s")
		fmt.Printf("[TSS] [P2P] ERROR: RPC call timeout sessionId=%s to=%s peerId=%s\n",
			sessionId, participant.Account, peerId.String())
	}
	duration := time.Since(startTime)

		if err != nil {
			if attempt < maxRetries {
				retryDelay := baseRetryDelay * time.Duration(1<<uint(attempt))
				fmt.Printf("[TSS] [P2P] ERROR: RPC Call failed, will retry sessionId=%s from=%s to=%s peerId=%s duration=%v attempt=%d/%d delay=%v err=%v\n",
					sessionId, fromAccount, participant.Account, peerId.String(), duration, attempt+1, maxRetries, retryDelay, err)
				tss.metrics.IncrementMessageRetry()
				time.Sleep(retryDelay)
				return tss.sendMsgWithRetry(sessionId, participant, moniker, msg, isBroadcast, commiteeType, cmtFrom, attempt+1)
			} else {
				fmt.Printf("[TSS] [P2P] ERROR: RPC Call failed after %d attempts sessionId=%s from=%s to=%s peerId=%s duration=%v err=%v\n",
					maxRetries, sessionId, fromAccount, participant.Account, peerId.String(), duration, err)
				tss.metrics.IncrementMessageSendFailure()
				return err
			}
		} else {
			if attempt > 0 {
				fmt.Printf("[TSS] [P2P] RPC Call succeeded on retry sessionId=%s from=%s to=%s attempt=%d duration=%v\n",
					sessionId, fromAccount, participant.Account, attempt+1, duration)
			} else {
				fmt.Printf("[TSS] [P2P] RPC Call success sessionId=%s from=%s to=%s duration=%v\n",
					sessionId, fromAccount, participant.Account, duration)
			}
			tss.metrics.RecordMessageSendLatency(duration)
		}

	return nil
}

// isPeerConnected checks if a peer is currently connected
func (tss *TssManager) isPeerConnected(peerId peer.ID) bool {
	host := tss.p2p.Host()
	connState := host.Network().Connectedness(peerId)
	connected := connState == network.Connected
	if !connected {
		fmt.Printf("[TSS] [P2P] Peer connection check: peerId=%s state=%v\n", peerId.String(), connState)
	}
	return connected
}
