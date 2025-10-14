package tss

import (
	"context"
	"fmt"
	"math/big"

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
	// Process the received message and return a response
	// For now, we just echo back the received message
	// fmt.Println("Received message:", req)

	peerId, _ := gorpc.GetRequestSender(ctx)

	if req.Type == "msg" {
		// fmt.Println("type is msg", tss.mgr.actionMap[req.SessionId])
		if tss.mgr.actionMap[req.SessionId] != nil {
			witness, _ := tss.mgr.witnessDb.GetWitnessesByPeerId(peerId.String())
			act := witness[0].Account
			i := big.NewInt(0)
			i.SetBytes([]byte(act))

			// fmt.Println("Received act", act, "on", tss.mgr.config.Get().HiveUsername, req.IsBroadcast, req.Cmt)

			// fmt.Println("req.SessionId", req.SessionId, tss.mgr.actionMap[req.SessionId])
			tss.mgr.actionMap[req.SessionId].HandleP2P(req.Data, act, req.IsBroadcast, req.Cmt, req.CmtFrom)
		} else {
			fmt.Println("Dropping message", req.SessionId)
		}
	}

	return nil
}
