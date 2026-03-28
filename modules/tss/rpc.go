package tss

import (
	"context"
	"math/big"
	"time"

	gorpc "github.com/libp2p/go-libp2p-gorpc"
	"github.com/libp2p/go-libp2p/core/peer"
)

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
			// Buffer the message for later replay
			tss.mgr.bufferLock.Lock()
			bufferedMsg := bufferedMessage{
				Data:    req.Data,
				From:    peerId.String(),
				IsBrcst: req.IsBroadcast,
				Cmt:     req.Cmt,
				CmtFrom: req.CmtFrom,
				Time:    receiveTime,
			}
			tss.mgr.messageBuffer[req.SessionId] = append(tss.mgr.messageBuffer[req.SessionId], bufferedMsg)
			bufferSize := len(tss.mgr.messageBuffer[req.SessionId])
			tss.mgr.bufferLock.Unlock()

			keys := make([]string, 0)
			tss.mgr.bufferLock.RLock()
			for k := range tss.mgr.actionMap {
				keys = append(keys, k)
			}
			bufferSize = len(tss.mgr.messageBuffer[req.SessionId])
			tss.mgr.bufferLock.RUnlock()

			log.Verbose("buffering message, dispatcher not found",
				"sessionId", req.SessionId, "myAccount", myAccount, "peerId", peerId.String(),
				"activeSessions", len(keys), "bufferSize", bufferSize)
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
	buffered := tss.mgr.messageBuffer[sessionId]
	if len(buffered) > 0 {
		delete(tss.mgr.messageBuffer, sessionId)
	}
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
