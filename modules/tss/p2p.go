package tss

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"time"
	"vsc-node/modules/common"
	tss_helpers "vsc-node/modules/tss/helpers"

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
	// Always accept signature-related and gossip readiness messages — they
	// are not tied to a specific TSS session.
	if parsedMsg.Type == "ask_sigs" || parsedMsg.Type == "res_sig" || parsedMsg.Type == "ready_gossip" {
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
					log.Trace(
						"validate message: action type mismatch, dropping",
						"session",
						parsedMsg.Session,
						"msgAction",
						parsedMsg.Action,
						"expectedAction",
						sessionInfo.action,
					)
					return false // Action type doesn't match
				}
			}
		}
	}

	return true
}

func (s p2pSpec) HandleMessage(
	ctx context.Context,
	from peer.ID,
	msg p2pMessage,
	send libp2p.SendFunc[p2pMessage],
) error {
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

			// Refuse to sign systemic blames: if fewer than threshold+1
			// nodes remain unblamed, the protocol could not have succeeded
			// (not enough shares for Lagrange interpolation). For 19 nodes
			// with threshold 12: maxBlamed = 19-13 = 6, so 7+ is systemic.
			if baseCommitment.Type == "blame" && baseCommitment.Commitment != "" {
				blameElection := s.tssMgr.electionDb.GetElection(baseCommitment.Epoch)
				if blameElection != nil {
					bv := new(big.Int)
					blameBytes, _ := base64.RawURLEncoding.DecodeString(baseCommitment.Commitment)
					bv.SetBytes(blameBytes)
					blamedCount := 0
					for idx := range blameElection.Members {
						if bv.Bit(idx) == 1 {
							blamedCount++
						}
					}
					blameThreshold, _ := tss_helpers.GetThreshold(len(blameElection.Members))
					maxBlamed := len(blameElection.Members) - (blameThreshold + 1)
					if blamedCount > maxBlamed {
						log.Info("refusing to sign systemic blame",
							"sessionId", sessId,
							"blamedCount", blamedCount,
							"maxBlamed", maxBlamed)
						return nil
					}
				}
			}

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

	if msg.Type == "ready_gossip" {
		s.handleReadyGossip(msg)
	}

	return nil
}

// handleReadyGossip processes a bundle of signed readiness attestations from
// another node. Each attestation's BLS signature is verified against the
// signer's consensus key from the election before being stored.
func (s p2pSpec) handleReadyGossip(msg p2pMessage) {
	targetBlockF, ok := msg.Data["target_block"].(float64)
	if !ok {
		return
	}
	targetBlock := uint64(targetBlockF)

	// Staleness check: reject if target block is in the past.
	currentBh := s.tssMgr.lastBlockHeight.Load()
	if targetBlock < currentBh {
		return
	}

	// Reject if unreasonably far in the future.
	rotateInterval := getRotateInterval(s.tssMgr.sconf)
	if targetBlock > currentBh+2*rotateInterval {
		return
	}

	// Look up the election to verify attestation signatures.
	election, err := s.tssMgr.electionDb.GetElectionByHeight(targetBlock)
	if err != nil || election.Members == nil {
		return
	}

	// Build a set of valid election members for cheap pre-filtering
	// before the expensive BLS signature verification.
	electionMembers := make(map[string]bool, len(election.Members))
	for _, m := range election.Members {
		electionMembers[m.Account] = true
	}

	// Check settle period: if we're within DEFAULT_SETTLE_BLOCKS of the
	// target, only accept attestations for accounts we've already seen.
	inSettlePeriod := false
	if currentBh > 0 && targetBlock > currentBh {
		blocksUntil := targetBlock - currentBh
		inSettlePeriod = blocksUntil <= DEFAULT_SETTLE_BLOCKS
	}

	attList, ok := msg.Data["attestations"].([]interface{})
	if !ok {
		return
	}

	// Cap bundle size at the election member count to prevent a malicious peer
	// from sending an oversized bundle that wastes CPU on BLS verification.
	maxAttestations := len(election.Members)
	if len(attList) > maxAttestations {
		log.Warn("oversized gossip bundle, truncating",
			"received", len(attList), "max", maxAttestations,
			"targetBlock", targetBlock)
		attList = attList[:maxAttestations]
	}

	dedupKey := strconv.FormatUint(targetBlock, 10)
	newCount := 0

	s.tssMgr.gossipLock.Lock()
	defer s.tssMgr.gossipLock.Unlock()

	if s.tssMgr.gossipAttestations[dedupKey] == nil {
		s.tssMgr.gossipAttestations[dedupKey] = make(map[string]ReadyAttestation)
	}
	existing := s.tssMgr.gossipAttestations[dedupKey]

	for _, raw := range attList {
		attMap, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		account, _ := attMap["account"].(string)
		sig, _ := attMap["sig"].(string)
		if account == "" || sig == "" {
			continue
		}

		// Only accept attestations from actual election members.
		if !electionMembers[account] {
			continue
		}

		// Skip if we already have this attestation.
		if _, has := existing[account]; has {
			continue
		}

		// During settle period, reject attestations from accounts not already seen.
		if inSettlePeriod {
			log.Trace("rejecting new attestation during settle period",
				"account", account, "targetBlock", targetBlock)
			continue
		}

		att := ReadyAttestation{
			Account:     account,
			TargetBlock: targetBlock,
			Sig:         sig,
		}

		if !s.tssMgr.verifyAttestation(att, election) {
			log.Warn("rejecting attestation with invalid BLS signature",
				"account", account, "targetBlock", targetBlock)
			continue
		}

		existing[account] = att
		newCount++
	}

	if newCount > 0 {
		log.Trace("merged gossip attestations",
			"targetBlock", targetBlock,
			"new", newCount, "total", len(existing))
	}
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

// SendMsg sends a TSS message to a participant. This is a single-attempt send;
// retries are handled by retryFailedMsgs() in the dispatcher layer.
func (tss *TssManager) SendMsg(
	sessionId string,
	participant Participant,
	moniker string,
	msg []byte,
	isBroadcast bool,
	commiteeType string,
	cmtFrom string,
) error {
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
			"sessionId", sessionId, "from", fromAccount, "to", participant.Account,
			"peerId", witness.PeerId, "err", err)
		return err
	}

	if !tss.isPeerConnected(peerId) {
		log.Warn("peer not connected",
			"sessionId", sessionId, "to", participant.Account, "peerId", peerId.String())
		return fmt.Errorf("peer %s not connected", peerId.String())
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

	log.Trace("sending message",
		"sessionId", sessionId, "from", fromAccount, "to", participant.Account,
		"isBroadcast", isBroadcast, "cmt", commiteeType, "cmtFrom", cmtFrom, "msgLen", len(msg))

	// Use CallContext with a deadline so the RPC (and its underlying libp2p
	// stream) is cancelled when the timeout fires.
	rpcTimeout := tss.sconf.TssParams().RpcTimeout
	rpcCtx, rpcCancel := context.WithTimeout(context.Background(), rpcTimeout)
	err = tss.client.CallContext(rpcCtx, peerId, "vsc.tss", "ReceiveMsg", &tMsg, &tRes)
	rpcCancel()
	if rpcCtx.Err() == context.DeadlineExceeded {
		log.Warn("RPC call timeout",
			"sessionId", sessionId, "to", participant.Account, "peerId", peerId.String(), "timeout", rpcTimeout)
	}
	duration := time.Since(startTime)

	if err != nil {
		log.Error("RPC call failed",
			"sessionId", sessionId, "from", fromAccount, "to", participant.Account,
			"peerId", peerId.String(), "duration", duration, "err", err)
		tss.metrics.IncrementMessageSendFailure()
		return err
	}

	log.Trace("RPC call success",
		"sessionId", sessionId, "from", fromAccount, "to", participant.Account, "duration", duration)
	tss.metrics.RecordMessageSendLatency(duration)
	return nil
}

// countReadyParticipants pings each participant's TSS RPC layer and returns
// the number of reachable peers. It never modifies or filters the participant
// list — callers use the count as a go/no-go gate while keeping the
// deterministic party list intact. Used for signing pre-flight checks.
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
