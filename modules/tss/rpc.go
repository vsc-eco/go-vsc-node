package tss

import (
	"context"
	"fmt"
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

	fmt.Printf("[TSS] [RPC] Received message sessionId=%s type=%s fromPeer=%s isBroadcast=%v cmt=%s cmtFrom=%s msgLen=%d\n",
		req.SessionId, req.Type, peerId.String(), req.IsBroadcast, req.Cmt, req.CmtFrom, len(req.Data))

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
				fmt.Printf("[TSS] [RPC] WARN: Session not active, dropping message sessionId=%s\n", req.SessionId)
				return nil
			}

			peerIds := []string{peerId.String()}
			witness, err := tss.mgr.witnessDb.GetWitnessesByPeerId(peerIds)
			if err != nil || len(witness) == 0 {
				fmt.Printf("[TSS] [RPC] WARN: Failed to get witness for peerId=%s sessionId=%s err=%v\n",
					peerId.String(), req.SessionId, err)
				return nil
			}
			act := witness[0].Account
			i := big.NewInt(0)
			i.SetBytes([]byte(act))

			fmt.Printf("[TSS] [RPC] Processing message sessionId=%s from=%s to=%s isBroadcast=%v cmt=%s\n",
				req.SessionId, act, myAccount, req.IsBroadcast, req.Cmt)

			processStart := time.Now()
			dispatcher.HandleP2P(req.Data, act, req.IsBroadcast, req.Cmt, req.CmtFrom)
			processDuration := time.Since(processStart)
			fmt.Printf("[TSS] [RPC] Message processed sessionId=%s from=%s duration=%v\n",
				req.SessionId, act, processDuration)

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
			bufferSize := len(tss.mgr.messageBuffer[req.SessionId])
			tss.mgr.bufferLock.RUnlock()

			fmt.Printf("[TSS] [RPC] WARN: Buffering message - dispatcher not found sessionId=%s myAccount=%s peerId=%s activeSessions=%d bufferSize=%d (maxAge=1m)\n",
				req.SessionId, myAccount, peerId.String(), len(keys), bufferSize)
			fmt.Println("actionMap.keys()", keys, tss.mgr.config.Get().HiveUsername, req.SessionId)
			fmt.Println("Buffering message", req.SessionId)
		}
	}

	totalDuration := time.Since(receiveTime)
	if totalDuration > 100*time.Millisecond {
		fmt.Printf("[TSS] [RPC] WARN: Slow message processing sessionId=%s duration=%v\n",
			req.SessionId, totalDuration)
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
		fmt.Printf("[TSS] [RPC] Replaying %d buffered messages for sessionId=%s\n", len(buffered), sessionId)
		for _, msg := range buffered {
			// Get account from peerId
			peerId, err := peer.Decode(msg.From)
			if err != nil {
				fmt.Printf("[TSS] [RPC] ERROR: Failed to decode peerId for buffered message sessionId=%s peerId=%s err=%v\n",
					sessionId, msg.From, err)
				continue
			}

			peerIds := []string{peerId.String()}
			witness, err := tss.mgr.witnessDb.GetWitnessesByPeerId(peerIds)
			if err != nil || len(witness) == 0 {
				fmt.Printf("[TSS] [RPC] ERROR: Failed to get witness for buffered message sessionId=%s peerId=%s err=%v\n",
					sessionId, msg.From, err)
				continue
			}
			act := witness[0].Account

			age := time.Since(msg.Time)
			fmt.Printf("[TSS] [RPC] Replaying buffered message sessionId=%s from=%s age=%v\n",
				sessionId, act, age)

			// Only replay messages that are not too old (e.g., less than 1 minute)
			if age < 1*time.Minute {
				dispatcher.HandleP2P(msg.Data, act, msg.IsBrcst, msg.Cmt, msg.CmtFrom)
			} else {
				fmt.Printf("[TSS] [RPC] WARN: Skipping old buffered message sessionId=%s age=%v\n",
					sessionId, age)
			}
		}
	}
}
