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

	// Round and type info for filtering stale messages
	Round   uint64 `json:"round,omitempty"`   // Block height of the session
	Action  string `json:"action,omitempty"`  // keygen, sign, reshare
	Session string `json:"session,omitempty"` // Full session ID
}

// ValidateMessage implements libp2p.PubSubServiceParams.
func (p p2pSpec) ValidateMessage(ctx context.Context, from peer.ID, msg *pubsub.Message, parsedMsg p2pMessage) bool {
	// Always accept signature-related messages (ask_sigs, res_sig) as they use different channels
	if parsedMsg.Type == "ask_sigs" || parsedMsg.Type == "res_sig" {
		return true
	}

	// For TSS round messages, validate they're relevant to current session
	if parsedMsg.Session != "" && parsedMsg.Round > 0 {
		// Check if this session exists and is still active
		p.tssMgr.bufferLock.RLock()
		sessionInfo, sessionActive := p.tssMgr.sessionMap[parsedMsg.Session]
		_, dispatcherExists := p.tssMgr.actionMap[parsedMsg.Session]
		p.tssMgr.bufferLock.RUnlock()

		if !sessionActive && !dispatcherExists {
			// Session not found - could be from old round, drop silently
			log.Trace("validate message: session not found, dropping",
				"session", parsedMsg.Session, "round", parsedMsg.Round)
			return false
		}

		// Verify the round matches current expected round for this session
		if sessionActive {
			// Allow messages within a reasonable window (current round ± 1)
			if parsedMsg.Round < sessionInfo.bh && sessionInfo.bh-parsedMsg.Round > 1 {
				log.Trace("validate message: old round, dropping",
					"session", parsedMsg.Session, "msgRound", parsedMsg.Round, "sessionRound", sessionInfo.bh)
				return false // Message from old round
			}

			// Verify action type matches (keygen, sign, reshare)
			if parsedMsg.Action != "" && sessionInfo.action != "" {
				if parsedMsg.Action != string(sessionInfo.action) {
					log.Trace("validate message: action type mismatch, dropping",
						"session", parsedMsg.Session, "msgAction", parsedMsg.Action, "expectedAction", sessionInfo.action)
					return false // Action type doesn't match
				}
			}
		}
	}

	return true
}

func (s p2pSpec) HandleMessage(ctx context.Context, from peer.ID, msg p2pMessage, send libp2p.SendFunc[p2pMessage]) error {
	if msg.Type == "ask_sigs" {
		sessId, ok := msg.Data["session_id"].(string)

		if !ok {
			return nil
		}

		s.tssMgr.bufferLock.RLock()
		entry, hasResult := s.tssMgr.sessionResults[sessId]
		s.tssMgr.bufferLock.RUnlock()
		if hasResult {
			baseCommitment := entry.result.Serialize()
			commitBytes, _ := common.EncodeDagCbor(baseCommitment)

			commitCid, _ := common.HashBytes(commitBytes, multicodec.DagCbor)

			log.Trace("committed cid", "commitCid", commitCid)
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

			log.Trace("sending signature", "sig", sigStr)
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

		s.tssMgr.bufferLock.RLock()
		sigChan := s.tssMgr.sigChannels[sessId]
		s.tssMgr.bufferLock.RUnlock()
		if sigChan != nil {
			sigChan <- sigMsg{
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
		log.Error("GetWitnessAtHeight failed",
			"sessionId", sessionId, "from", fromAccount, "to", participant.Account, "err", err)
		return err
	}

	peerId, err := peer.Decode(witness.PeerId)

	if err != nil {
		log.Error("PeerId decode failed",
			"sessionId", sessionId, "from", fromAccount, "to", participant.Account, "peerId", witness.PeerId, "err", err)
		return err
	}

	// Check connection health before sending
	if !tss.isPeerConnected(peerId) {
		if attempt < maxRetries {
			retryDelay := baseRetryDelay * time.Duration(1<<uint(attempt)) // Exponential backoff: 1s, 2s, 4s
			log.Verbose("peer not connected, retrying with backoff",
				"sessionId", sessionId, "to", participant.Account, "peerId", peerId.String(), "attempt", attempt+1, "maxRetries", maxRetries, "delay", retryDelay)
			time.Sleep(retryDelay)
			return tss.sendMsgWithRetry(sessionId, participant, moniker, msg, isBroadcast, commiteeType, cmtFrom, attempt+1)
		} else {
			log.Warn("peer not connected after retries",
				"sessionId", sessionId, "to", participant.Account, "peerId", peerId.String(), "maxRetries", maxRetries)
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
		log.Trace("sending message",
			"sessionId", sessionId, "from", fromAccount, "to", participant.Account, "isBroadcast", isBroadcast, "cmt", commiteeType, "cmtFrom", cmtFrom, "msgLen", len(msg))
	} else {
		log.Verbose("retrying message send",
			"sessionId", sessionId, "from", fromAccount, "to", participant.Account, "attempt", attempt+1, "maxRetries", maxRetries, "msgLen", len(msg))
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
		log.Warn("RPC call timeout",
			"sessionId", sessionId, "to", participant.Account, "peerId", peerId.String())
	}
	duration := time.Since(startTime)

	if err != nil {
		if attempt < maxRetries {
			retryDelay := baseRetryDelay * time.Duration(1<<uint(attempt))
			log.Error("RPC call failed, will retry",
				"sessionId", sessionId, "from", fromAccount, "to", participant.Account, "peerId", peerId.String(), "duration", duration, "attempt", attempt+1, "maxRetries", maxRetries, "delay", retryDelay, "err", err)
			tss.metrics.IncrementMessageRetry()
			time.Sleep(retryDelay)
			return tss.sendMsgWithRetry(sessionId, participant, moniker, msg, isBroadcast, commiteeType, cmtFrom, attempt+1)
		} else {
			log.Error("RPC call failed after all retries",
				"sessionId", sessionId, "from", fromAccount, "to", participant.Account, "peerId", peerId.String(), "duration", duration, "maxRetries", maxRetries, "err", err)
			tss.metrics.IncrementMessageSendFailure()
			return err
		}
	} else {
		if attempt > 0 {
			log.Trace("RPC call succeeded on retry",
				"sessionId", sessionId, "from", fromAccount, "to", participant.Account, "attempt", attempt+1, "duration", duration)
		} else {
			log.Trace("RPC call success",
				"sessionId", sessionId, "from", fromAccount, "to", participant.Account, "duration", duration)
		}
		tss.metrics.RecordMessageSendLatency(duration)
	}

	return nil
}

// checkParticipantReadiness sends a TSS-level "ready" ping to each participant
// and returns only those that respond within the deadline. This filters out
// zombie nodes that are connected at the libp2p level but not functioning
// at the application level. Always includes self.
func (tss *TssManager) checkParticipantReadiness(participants []Participant, sessionId string, label string) []Participant {
	selfAccount := tss.config.Get().HiveUsername
	readyTimeout := 5 * time.Second

	type readyResult struct {
		participant Participant
		ok          bool
		reason      string
	}

	results := make(chan readyResult, len(participants))

	for _, p := range participants {
		if p.Account == selfAccount {
			results <- readyResult{participant: p, ok: true}
			continue
		}

		go func(p Participant) {
			witness, err := tss.witnessDb.GetWitnessAtHeight(p.Account, nil)
			if err != nil || witness.PeerId == "" {
				results <- readyResult{participant: p, ok: false, reason: "no_witness"}
				return
			}

			peerId, err := peer.Decode(witness.PeerId)
			if err != nil {
				results <- readyResult{participant: p, ok: false, reason: "bad_peer_id"}
				return
			}

			tMsg := TMsg{
				SessionId: sessionId,
				Type:      "ready",
			}
			tRes := TRes{}

			errChan := make(chan error, 1)
			go func() {
				errChan <- tss.client.Call(peerId, "vsc.tss", "ReceiveMsg", &tMsg, &tRes)
			}()

			select {
			case err := <-errChan:
				if err != nil {
					results <- readyResult{participant: p, ok: false, reason: fmt.Sprintf("rpc_error: %v", err)}
				} else {
					results <- readyResult{participant: p, ok: true}
				}
			case <-time.After(readyTimeout):
				results <- readyResult{participant: p, ok: false, reason: "timeout"}
			}
		}(p)
	}

	ready := make([]Participant, 0, len(participants))
	for range participants {
		r := <-results
		if r.ok {
			ready = append(ready, r.participant)
		} else {
			log.Warn("excluding unresponsive participant",
				"label", label, "sessionId", sessionId, "account", r.participant.Account, "reason", r.reason)
		}
	}

	log.Verbose("readiness check complete",
		"label", label, "sessionId", sessionId, "total", len(participants), "ready", len(ready))

	return ready
}

// countReadyParticipants checks how many participants respond to a readiness
// ping, WITHOUT modifying the participant list. Use this for pre-flight gates
// where the party list must remain deterministic across all nodes.
func (tss *TssManager) countReadyParticipants(participants []Participant, sessionId string, label string) int {
	selfAccount := tss.config.Get().HiveUsername
	readyTimeout := 5 * time.Second

	type readyResult struct {
		ok      bool
		account string
		reason  string
	}

	results := make(chan readyResult, len(participants))

	for _, p := range participants {
		if p.Account == selfAccount {
			results <- readyResult{ok: true, account: p.Account}
			continue
		}

		go func(p Participant) {
			witness, err := tss.witnessDb.GetWitnessAtHeight(p.Account, nil)
			if err != nil || witness.PeerId == "" {
				results <- readyResult{ok: false, account: p.Account, reason: "no_witness"}
				return
			}

			peerId, err := peer.Decode(witness.PeerId)
			if err != nil {
				results <- readyResult{ok: false, account: p.Account, reason: "bad_peer_id"}
				return
			}

			tMsg := TMsg{
				SessionId: sessionId,
				Type:      "ready",
			}
			tRes := TRes{}

			errChan := make(chan error, 1)
			go func() {
				errChan <- tss.client.Call(peerId, "vsc.tss", "ReceiveMsg", &tMsg, &tRes)
			}()

			select {
			case err := <-errChan:
				if err != nil {
					results <- readyResult{ok: false, account: p.Account, reason: fmt.Sprintf("rpc_error: %v", err)}
				} else {
					results <- readyResult{ok: true, account: p.Account}
				}
			case <-time.After(readyTimeout):
				results <- readyResult{ok: false, account: p.Account, reason: "timeout"}
			}
		}(p)
	}

	count := 0
	for range participants {
		r := <-results
		if r.ok {
			count++
		} else {
			log.Verbose("unresponsive participant (pre-flight, not excluded)",
				"label", label, "sessionId", sessionId, "account", r.account, "reason", r.reason)
		}
	}

	log.Verbose("readiness count complete",
		"label", label, "sessionId", sessionId, "total", len(participants), "ready", count)

	return count
}

// isPeerConnected checks if a peer is currently connected
func (tss *TssManager) isPeerConnected(peerId peer.ID) bool {
	host := tss.p2p.Host()
	connState := host.Network().Connectedness(peerId)
	connected := connState == network.Connected
	if !connected {
		log.Trace("peer connection check", "peerId", peerId.String(), "state", connState)
	}
	return connected
}
