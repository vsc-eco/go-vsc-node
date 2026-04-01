package tss

import (
	"context"
	"math/big"
	"strconv"
	"strings"
	"time"

	gorpc "github.com/libp2p/go-libp2p-gorpc"
	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	// Max number of buffered messages per session before new messages are dropped.
	maxMessagesPerSession = 100
	// Max number of distinct sessions that can have buffered messages.
	maxBufferedSessions = 50
	// Max size in bytes of a single message's Data field that will be buffered.
	// The largest TSS message is ECDSA reshare DGRound2Message1 (~175KB: two DLN proofs
	// at ~66KB each + ModProof at ~42KB), so 256KB gives adequate headroom.
	maxBufferedMessageSize = 256 * 1024 // 256 KB
	// Max block age for a session to be admitted into the buffer.
	// Sessions with block height older than (currentBlockHeight - maxBufferBlockAge) are rejected.
	// TSS sessions can run up to 60s; at ~3s/block that's ~20 blocks. Allow extra margin.
	maxBufferBlockAge uint64 = 30
)

// parseSessionBlockHeight extracts the block height from a session ID.
// Session IDs follow the format "{type}-{blockHeight}-{actionIndex}-{keyId}".
// Returns 0 if parsing fails.
func parseSessionBlockHeight(sessionId string) uint64 {
	parts := strings.SplitN(sessionId, "-", 4)
	if len(parts) < 2 {
		return 0
	}
	bh, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return 0
	}
	return bh
}

type TssRpc struct {
	mgr *TssManager
}

type TMsg struct {
	IsBroadcast bool
	SessionId   string
	Type        string
	Action      string
	KeyId       string
	Data        []byte
	Cmt         string
	CmtFrom     string
}

type TRes struct {
	Action string

	Data []byte
}

func (tss *TssRpc) ReceiveMsg(ctx context.Context, req *TMsg, res *TRes) error {
	receiveTime := time.Now()
	myAccount := tss.mgr.config.Get().HiveUsername
	peerId, _ := gorpc.GetRequestSender(ctx)

	log.Trace("received rpc message",
		"sessionId", req.SessionId, "type", req.Type, "fromPeer", peerId.String(),
		"isBroadcast", req.IsBroadcast, "cmt", req.Cmt, "cmtFrom", req.CmtFrom, "msgLen", len(req.Data))

	if req.Type == "ready" {
		res.Action = "ready_ack"
		return nil
	}

	if req.Type == "msg" {
		// Check if dispatcher exists
		tss.mgr.bufferLock.RLock()
		dispatcher := tss.mgr.actionMap[req.SessionId]
		tss.mgr.bufferLock.RUnlock()

		if dispatcher != nil {
			// Validate session is still active
			tss.mgr.bufferLock.RLock()
			_, sessionActive := tss.mgr.sessionMap[req.SessionId]
			tss.mgr.bufferLock.RUnlock()

			if !sessionActive {
				log.Warn("session not active, dropping message", "sessionId", req.SessionId)
				return nil
			}

			peerIds := []string{peerId.String()}
			witness, err := tss.mgr.witnessDb.GetWitnessesByPeerId(peerIds)
			if err != nil || len(witness) == 0 {
				log.Warn("failed to get witness", "peerId", peerId.String(), "sessionId", req.SessionId, "err", err)
				return nil
			}
			act := witness[0].Account
			i := big.NewInt(0)
			i.SetBytes([]byte(act))

			log.Trace("processing message",
				"sessionId", req.SessionId, "from", act, "to", myAccount, "isBroadcast", req.IsBroadcast, "cmt", req.Cmt)

			processStart := time.Now()
			dispatcher.HandleP2P(req.Data, act, req.IsBroadcast, req.Cmt, req.CmtFrom)
			processDuration := time.Since(processStart)
			log.Trace("message processed",
				"sessionId", req.SessionId, "from", act, "duration", processDuration)

			// Replay any buffered messages for this session
			tss.replayBufferedMessages(req.SessionId, dispatcher)
		} else {
			// Reject oversized messages
			if len(req.Data) > maxBufferedMessageSize {
				log.Warn("dropping oversized buffered message",
					"sessionId", req.SessionId, "peerId", peerId.String(), "size", len(req.Data))
				return nil
			}

			// Reject sessions with stale block heights
			sessionBh := parseSessionBlockHeight(req.SessionId)
			currentBh := tss.mgr.lastBlockHeight.Load()
			if currentBh > 0 && sessionBh > 0 && sessionBh+maxBufferBlockAge < currentBh {
				log.Warn("dropping message for stale session",
					"sessionId", req.SessionId, "peerId", peerId.String(),
					"sessionBlockHeight", sessionBh, "currentBlockHeight", currentBh)
				return nil
			}

			// Buffer the message for later replay
			tss.mgr.bufferLock.Lock()

			// Enforce per-session message limit
			if tss.mgr.messageBuffer.MsgCount(req.SessionId) >= maxMessagesPerSession {
				tss.mgr.bufferLock.Unlock()
				log.Warn("message buffer full for session, dropping message",
					"sessionId", req.SessionId, "peerId", peerId.String(), "limit", maxMessagesPerSession)
				return nil
			}

			// Enforce total session limit with eviction of oldest session
			if !tss.mgr.messageBuffer.Has(req.SessionId) && tss.mgr.messageBuffer.Len() >= maxBufferedSessions {
				_, lowestBh := tss.mgr.messageBuffer.PeekMinBlockHeight()
				if sessionBh > lowestBh {
					evictedId := tss.mgr.messageBuffer.EvictMin()
					log.Warn("buffer full, evicting oldest session",
						"evictedSessionId", evictedId, "evictedBlockHeight", lowestBh,
						"newSessionId", req.SessionId, "newBlockHeight", sessionBh)
				} else {
					tss.mgr.bufferLock.Unlock()
					log.Warn("buffer full and incoming session is not newer, dropping message",
						"sessionId", req.SessionId, "peerId", peerId.String(),
						"sessionBlockHeight", sessionBh, "lowestBufferedBlockHeight", lowestBh)
					return nil
				}
			}

			bufferedMsg := bufferedMessage{
				Data:    req.Data,
				From:    peerId.String(),
				IsBrcst: req.IsBroadcast,
				Cmt:     req.Cmt,
				CmtFrom: req.CmtFrom,
				Time:    receiveTime,
			}
			tss.mgr.messageBuffer.Push(req.SessionId, sessionBh, bufferedMsg)
			bufferSize := tss.mgr.messageBuffer.MsgCount(req.SessionId)
			bufferedSessions := tss.mgr.messageBuffer.Len()
			tss.mgr.bufferLock.Unlock()

			log.Verbose("buffering message, dispatcher not found",
				"sessionId", req.SessionId, "myAccount", myAccount, "peerId", peerId.String(),
				"bufferedSessions", bufferedSessions, "bufferSize", bufferSize)
			tss.mgr.metrics.IncrementMessageDrop()
		}
	}

	totalDuration := time.Since(receiveTime)
	if totalDuration > 100*time.Millisecond {
		log.Warn("slow message processing", "sessionId", req.SessionId, "duration", totalDuration)
	}

	return nil
}

// replayBufferedMessages replays buffered messages for a session when dispatcher becomes ready
func (tss *TssRpc) replayBufferedMessages(sessionId string, dispatcher Dispatcher) {
	tss.mgr.bufferLock.Lock()
	buffered := tss.mgr.messageBuffer.Drain(sessionId)
	tss.mgr.bufferLock.Unlock()

	if len(buffered) > 0 {
		log.Verbose("replaying buffered messages", "sessionId", sessionId, "count", len(buffered))
		for _, msg := range buffered {
			// Get account from peerId
			peerId, err := peer.Decode(msg.From)
			if err != nil {
				log.Error("failed to decode peerId for buffered message", "sessionId", sessionId, "peerId", msg.From, "err", err)
				continue
			}

			peerIds := []string{peerId.String()}
			witness, err := tss.mgr.witnessDb.GetWitnessesByPeerId(peerIds)
			if err != nil || len(witness) == 0 {
				log.Warn("failed to get witness for buffered message", "sessionId", sessionId, "peerId", msg.From, "err", err)
				continue
			}
			act := witness[0].Account

			age := time.Since(msg.Time)
			log.Trace("replaying buffered message", "sessionId", sessionId, "from", act, "age", age)

			// Only replay messages that are not too old (e.g., less than 1 minute)
			if age < 1*time.Minute {
				dispatcher.HandleP2P(msg.Data, act, msg.IsBrcst, msg.Cmt, msg.CmtFrom)
			} else {
				log.Warn("skipping old buffered message", "sessionId", sessionId, "age", age)
			}
		}
	}
}
