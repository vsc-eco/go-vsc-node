package tss

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"
	"vsc-node/modules/common"
	tss_helpers "vsc-node/modules/tss/helpers"

	libp2p "vsc-node/modules/p2p"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/protocol"
	multiaddr "github.com/multiformats/go-multiaddr"
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
			// Non-blocking send: the collector may have already exited
			// (threshold reached or WaitForSigsTimeout fired) and a blocking
			// send here would park this goroutine forever, leaking a pubsub
			// concurrency-limit slot. Additional sigs past threshold or after
			// timeout don't change the outcome, so drop them.
			select {
			case sigChan <- sigMsg{
				Account:   msg.Account,
				SessionId: sessId,
				Sig:       sig,
			}:
			default:
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

	// V-A: fund-holding retiring/draining-gen committee members are ALSO valid
	// readiness attesters for a migration sweep, even when churned out of the
	// current election. Deterministic on-chain set (gated on v2; empty → inert), so
	// every node accepts the same widened attester set and verifies each against the
	// election that carries its BLS key. CACHED per target height (council A3):
	// this runs once per untrusted gossip message, so the underlying contract-state
	// read must not be per-message.
	retiringSet := s.tssMgr.retiringGenSignerSetCached(targetBlock)

	// Build a set of valid election members for cheap pre-filtering
	// before the expensive BLS signature verification.
	electionMembers := make(map[string]bool, len(election.Members)+len(retiringSet.signerElection))
	for _, m := range election.Members {
		electionMembers[m.Account] = true
	}
	for acct := range retiringSet.signerElection {
		electionMembers[acct] = true
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
	maxAttestations := len(election.Members) + len(retiringSet.signerElection)
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
			Account:             account,
			TargetBlock:         targetBlock,
			VersionMajor:        gossipUint(attMap["version_major"]),
			VersionConsensus:    gossipUint(attMap["version_consensus"]),
			VersionNonConsensus: gossipUint(attMap["version_non_consensus"]),
			Sig:                 sig,
		}

		// Verify against the current election; if the attester is a churned-out
		// retiring-gen committee member (V-A), verify against that gen's commitment
		// epoch election, which carries its BLS key.
		verified := s.tssMgr.verifyAttestation(att, election)
		if !verified {
			if relec, isRetiring := retiringSet.signerElection[account]; isRetiring {
				verified = s.tssMgr.verifyAttestation(att, relec)
			}
		}
		if !verified {
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

// gossipUint coerces a gossip-decoded numeric field (typically float64 over pubsub) to uint64.
// Missing/invalid fields default to 0 (treated as version 0.0.0).
func gossipUint(v interface{}) uint64 {
	switch n := v.(type) {
	case float64:
		if n < 0 {
			return 0
		}
		return uint64(n)
	case uint64:
		return n
	case int64:
		if n < 0 {
			return 0
		}
		return uint64(n)
	case int:
		if n < 0 {
			return 0
		}
		return uint64(n)
	default:
		return 0
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

	// Ensure a live connection before sending. TSS reshare delivers its round
	// messages over on-demand unicast RPC streams; between reshares these
	// connections go idle and can be torn down by NAT idle-eviction or transport
	// keepalive timeouts. If libp2p reports the peer as disconnected, re-dial
	// from its announced addresses so THIS send can proceed over a fresh
	// connection instead of failing outright. (This handles the
	// reported-disconnected case; a half-open "Connected" connection is healed
	// by the ClosePeer below on an idle-death send error.) Transport-only: the
	// message bytes are unchanged.
	if !tss.isPeerConnected(peerId) {
		tss.reconnectPeer(context.Background(), peerId, witness.PeerAddrs)
		if !tss.isPeerConnected(peerId) {
			log.Warn("peer not connected",
				"sessionId", sessionId, "to", participant.Account, "peerId", peerId.String())
			return fmt.Errorf("peer %s not connected", peerId.String())
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
		// An unambiguous idle-death error means the connection is dead even
		// though libp2p may briefly still report the peer as "connected"
		// (half-open, killed by NAT idle-eviction / keepalive timeout). Evict it
		// so it is re-established by the next keepalive pass or the pre-send
		// reconnect above, instead of subsequent sends repeatedly failing over
		// the same dead connection. Gated on isTransportErr (idle-death only, see
		// there) so a live-but-busy peer's connection is never dropped — note
		// ClosePeer is connection-wide and the reshare path does not retry
		// individual messages. Transport-only: message bytes are unchanged, so
		// this cannot affect the party list or the commitment CID.
		if isTransportErr(err) {
			if cerr := tss.p2p.Host().Network().ClosePeer(peerId); cerr != nil {
				log.Trace("failed to evict stale peer connection",
					"peerId", peerId.String(), "err", cerr)
			}
		}
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

// parseWitnessAddrs converts a witness's announced multiaddr strings into
// multiaddrs, skipping any that fail to parse.
func parseWitnessAddrs(addrStrs []string) []multiaddr.Multiaddr {
	addrs := make([]multiaddr.Multiaddr, 0, len(addrStrs))
	for _, s := range addrStrs {
		a, err := multiaddr.NewMultiaddr(s)
		if err != nil {
			continue
		}
		addrs = append(addrs, a)
	}
	return addrs
}

// isTransportErr reports whether an RPC error indicates a genuinely dead,
// idle-killed connection. It matches ONLY libp2p's own idle-detection strings
// ("no recent network activity", "keepalive timeout") — the exact symptoms of
// the production half-open stalls (e.g. "stream reset: connection closed:
// keepalive timeout", "timeout: no recent network activity"), so real coverage
// is retained.
//
// It deliberately does NOT match "stream reset" / "connection closed" /
// "canceled by remote" / "context deadline exceeded" on their own: those are
// also produced by a LIVE-but-busy peer under resource-manager or stream-limit
// pressure (and by our own ClosePeer). Because the caller reacts with a
// connection-wide ClosePeer, and the reshare send path does NOT retry
// individual messages, treating those ambiguous errors as death would evict a
// healthy connection mid-reshare — dropping any concurrent in-flight round
// message on it and stalling that peer for the full timeout. Narrow on purpose.
func isTransportErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, tok := range []string{
		"no recent network activity",
		"keepalive timeout",
	} {
		if strings.Contains(s, tok) {
			return true
		}
	}
	return false
}

// reconnectPeer re-establishes a direct connection to a peer from its announced
// addresses. The dial is bounded by both a 10s timeout and the caller's ctx so
// it unwinds promptly on shutdown. Transport-only: it does not read or modify
// any TSS message, party list, or commitment.
func (tss *TssManager) reconnectPeer(ctx context.Context, peerId peer.ID, addrStrs []string) {
	addrs := parseWitnessAddrs(addrStrs)
	if len(addrs) == 0 {
		return
	}
	tss.p2p.Peerstore().AddAddrs(peerId, addrs, peerstore.TempAddrTTL)
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := tss.p2p.Connect(dialCtx, peer.AddrInfo{ID: peerId, Addrs: addrs}); err != nil {
		log.Trace("reconnect failed", "peerId", peerId.String(), "err", err)
	}
}

// runConnectionKeepalive keeps direct libp2p connections to the other witnesses
// warm for the lifetime of the node. TSS reshare delivers its round messages
// over on-demand unicast RPC streams to specific peers; between reshares those
// direct connections sit idle and are torn down by NAT idle-eviction,
// connection-manager trimming, or transport keepalive timeouts. Ordinary block
// production is unaffected because it rides the continuously active gossip mesh,
// but the next reshare then fails to deliver its messages over a
// dead-but-"Connected" stream, stalling the reshare until the connection
// happens to be alive again. This loop is the production counterpart of the TSS
// test harness (startPeerKeepalive + ConnManager().Protect) — the mechanism
// that makes reshares succeed under test but which was absent in production.
// Transport-only: it never reads or modifies party lists, btss inputs, or
// serialized commitments.
func (tss *TssManager) runConnectionKeepalive(ctx context.Context) {
	const keepaliveInterval = 15 * time.Second
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Guard against a startup race where the p2p host is not yet
			// initialized — P2PServer.Host() panics rather than returning nil,
			// and an unrecovered panic in this background goroutine would crash
			// the whole node.
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Warn("keepalive pass panicked, skipping", "panic", r)
					}
				}()
				tss.warmCommitteePeers(ctx)
			}()
		}
	}
}

// warmCommitteePeers protects, (re)connects, and probes every other enabled
// witness so each direct TSS connection stays alive before the next reshare
// needs it. Per-peer work runs concurrently so a few slow or unreachable peers
// cannot stall warming of the healthy ones.
func (tss *TssManager) warmCommitteePeers(ctx context.Context) {
	self := tss.config.Get().HiveUsername
	witnessList, err := tss.witnessDb.GetLastestWitnesses()
	if err != nil {
		log.Trace("keepalive: failed to list witnesses", "err", err)
		return
	}
	host := tss.p2p.Host()
	// GetLastestWitnesses returns every announcement row (height-descending),
	// not one per account, so dedup to the latest row per account. This both
	// cuts per-pass work (accounts re-announce many times) and evaluates each
	// account on its most recent announcement — current peer_id / enabled state
	// — instead of warming stale rows. We intentionally warm the full enabled
	// witness set (no network filter): the committee is always a subset of it,
	// and skipping a member risks reintroducing the very idle-death this fixes.
	seen := make(map[string]bool)
	var wg sync.WaitGroup
	for _, w := range witnessList {
		if w.Account == self || seen[w.Account] {
			continue
		}
		seen[w.Account] = true
		if !w.Enabled || w.PeerId == "" {
			continue
		}
		peerId, err := peer.Decode(w.PeerId)
		if err != nil {
			continue
		}
		// Keep committee peers out of the connection-manager trim set.
		tss.p2p.ConnManager().Protect(peerId, "tss-committee")
		if addrs := parseWitnessAddrs(w.PeerAddrs); len(addrs) > 0 {
			tss.p2p.Peerstore().AddAddrs(peerId, addrs, peerstore.TempAddrTTL)
		}
		wg.Add(1)
		go func(peerId peer.ID, account string, addrStrs []string) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					log.Warn("keepalive peer task panicked", "account", account, "panic", r)
				}
			}()
			if host.Network().Connectedness(peerId) != network.Connected {
				// Re-establish a dropped connection ahead of the next reshare.
				tss.reconnectPeer(ctx, peerId, addrStrs)
				return
			}
			// Connected: send a cheap probe to generate traffic and reset
			// NAT/transport idle timers. A failed probe is deliberately NOT
			// treated as a dead connection — under heavy TSS load a busy peer can
			// miss a probe while remaining reachable, and closing a live
			// connection mid-reshare is exactly the harm we are avoiding. Genuine
			// dead-connection eviction happens in SendMsg on a real send failure
			// (unambiguous transport error), and libp2p's own keepalive will
			// eventually drop a truly dead connection, after which the branch
			// above reconnects it on the next pass.
			if err := tss.pingPeerAlive(ctx, peerId); err != nil {
				log.Trace("keepalive: probe failed (connection left intact)", "account", account, "err", err)
			}
		}(peerId, w.Account, w.PeerAddrs)
	}
	wg.Wait()
}

// pingPeerAlive sends a lightweight readiness RPC over the direct connection to
// generate traffic that resets NAT / transport idle timers, keeping the
// connection warm. It reuses the existing "ready" handler (rpc.go), which
// replies with a cheap ack and has no side effects. The returned error is used
// only for logging — a failed probe is deliberately not treated as a dead
// connection (see warmCommitteePeers).
func (tss *TssManager) pingPeerAlive(ctx context.Context, peerId peer.ID) error {
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tMsg := TMsg{Type: "ready"}
	tRes := TRes{}
	return tss.client.CallContext(pingCtx, peerId, "vsc.tss", "ReceiveMsg", &tMsg, &tRes)
}
