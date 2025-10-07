package chain

import (
	"context"
	"sync"
	"time"
	"vsc-node/modules/oracle/p2p"
)

// HandleBlockTick implements oracle.BlockTickHandler.
func (o *ChainOracle) HandleBlockTick(
	signal p2p.BlockTickSignal,
	p2pSpec p2p.OracleVscSpec,
) {
	// NOTE: when the node is the not the producer, it is not a witness. The
	// action of witness is triggered for incoming p2p message.
	if !signal.IsProducer {
		return
	}
	// ## Process end to end ##
	// - Node is witness & producer
	// - Using the contract state of latest block in combination with latest
	//   block on Bitcoin mainnet
	// - Assuming this is true.
	// - That means the latest block on BTC mainnet is atleast 3 confirmations
	//   higher
	// - Create the transaction structure with the block headers to submit
	// - Receiving node will do the same check as above, aka verify that new
	//   blocks must be submitted
	// - If true, then sign the exact same transaction and return signature
	// - Producer node will receive signatures through the p2p channel
	// - Producer node will aggregate those signatures into a single BLS circuit
	//   for submission on mainnet

	// make chainDataPool and signature requests
	var (
		chainDataPool     = o.fetchAllBlocks()
		signatureRequests = make([]chainOracleMessage, 0, len(chainDataPool))
		signatureMap      = make(map[string][]signatureMessage)
	)
	for _, chainSession := range chainDataPool {
		signatureRequest, err := makeChainOracleMessage(
			signatureRequest,
			chainSession.sessionID,
			chainSession.chainData,
		)

		if err != nil {
			o.logger.Error(
				"failed to create signature requests",
				"sessionID", chainSession.sessionID,
				"err", err,
			)
			continue
		}

		signatureRequests = append(signatureRequests, *signatureRequest)
		signatureMap[chainSession.sessionID] = make([]signatureMessage, 0)
	}

	// broadcast signature requests + collect signatures
	// - Ask P2P channels for signatures for the transaction
	// - Receiving node will receive request to create transaction on VSC mainnet
	defer o.signatureChannels.clearMap()

	signatureMapMtx := &sync.Mutex{}
	ctx, cancel := context.WithTimeout(o.ctx, 30*time.Second)
	defer cancel()

	for i := range signatureRequests {
		sigRequest := &signatureRequests[i]
		sigChan, err := o.signatureChannels.makeSession(sigRequest.SessionID)
		if err != nil {
			o.logger.Error("fialed to make signature session",
				"sessionID", sigRequest.SessionID,
				"err", err,
			)
			continue
		}

		if err := p2pSpec.Broadcast(p2p.MsgChainRelay, sigRequest); err != nil {
			o.logger.Error(
				"failed to broadcast signature request",
				"sessionID", sigRequest.SessionID,
				"err", err,
			)
			continue
		}

		go collectSignature(
			ctx,
			signatureMapMtx,
			sigChan,
			sigRequest.SessionID,
			signatureMap,
		)
	}

	<-ctx.Done()
	if err := ctx.Err(); err != nil {
		o.logger.Error("chainOracle context error", "err", err)
		return
	}

}

func collectSignature(
	ctx context.Context,
	mtx *sync.Mutex,
	sigChan <-chan signatureMessage,
	sessionID string,
	signatureMap map[string][]signatureMessage,
) {
	for {
		select {
		case <-ctx.Done():
			return

		case msg := <-sigChan:
			mtx.Lock()
			signatureMap[sessionID] = append(signatureMap[sessionID], msg)
			mtx.Unlock()
		}
	}
}
