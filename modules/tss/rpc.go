package tss

import (
	"context"
	"fmt"
	"math/big"
	"time"

	gorpc "github.com/libp2p/go-libp2p-gorpc"
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
		if tss.mgr.actionMap[req.SessionId] != nil {
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
			tss.mgr.actionMap[req.SessionId].HandleP2P(req.Data, act, req.IsBroadcast, req.Cmt, req.CmtFrom)
			processDuration := time.Since(processStart)
			fmt.Printf("[TSS] [RPC] Message processed sessionId=%s from=%s duration=%v\n",
				req.SessionId, act, processDuration)
		} else {
			keys := make([]string, 0)
			for k := range tss.mgr.actionMap {
				keys = append(keys, k)
			}
			fmt.Printf("[TSS] [RPC] WARN: Dropping message - dispatcher not found sessionId=%s myAccount=%s peerId=%s activeSessions=%d\n",
				req.SessionId, myAccount, peerId.String(), len(keys))
			fmt.Println("actionMap.keys()", keys, tss.mgr.config.Get().HiveUsername, req.SessionId)
			fmt.Println("Dropping message", req.SessionId)
		}
	}

	totalDuration := time.Since(receiveTime)
	if totalDuration > 100*time.Millisecond {
		fmt.Printf("[TSS] [RPC] WARN: Slow message processing sessionId=%s duration=%v\n",
			req.SessionId, totalDuration)
	}

	return nil
}
