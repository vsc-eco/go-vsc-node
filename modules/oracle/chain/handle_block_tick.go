package chain

import (
	"context"
	"fmt"
	"sync"
	"time"
	"vsc-node/modules/oracle/p2p"
	transactionpool "vsc-node/modules/transaction-pool"
)

type session struct {
	vscTxShell transactionpool.VSCTransactionShell
	chainData  []string
	signatures []string
}

type vscTransaction struct {
	sessionID  string
	chainData  []byte
	signatures []string
}

func makeTransaction(
	contractId *string,
	payload []string,
) transactionpool.VSCTransactionShell {
	ops := make([]transactionpool.VSCTransactionOp, 0)
	return transactionpool.VSCTransactionShell{
		Headers: transactionpool.VSCTransactionHeader{
			Nonce:         0,
			RequiredAuths: []string{"did:vsc:consensus"},
			RcLimit:       500,
			NetId:         "vsc-mainnet",
		},
		Type:    "vsc-tx",
		Version: "0.2",
		Tx:      ops,
	}
}

// HandleBlockTick implements oracle.BlockTickHandler.
func (o *ChainOracle) HandleBlockTick(
	signal p2p.BlockTickSignal,
	p2pSpec p2p.OracleP2PSpec,
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
	// - Create the transaction structure with the block headers to submit
	// - Receiving node will do the same check as above, aka verify that new
	//   blocks must be submitted
	// - If true, then sign the exact same transaction and return signature
	// - Producer node will receive signatures through the p2p channel
	// - Producer node will aggregate those signatures into a single BLS circuit
	//   for submission on mainnet

	// make chainDataPool and signature requests

	chainStatuses := o.fetchAllStatuses()
	signatureRequests := make([]chainOracleMessage, 0, len(chainStatuses))
	sessionMap := make(map[string]session)

	for _, chainStatus := range chainStatuses {
		if !chainStatus.newBlocksToSubmit {
			continue
		}

		sessionID, err := makeChainSessionID(&chainStatus)
		if err != nil {
			o.logger.Error(
				"failed to create session ID",
				"block count", len(chainStatus.chainData),
				"err", err,
			)
			continue
		}

		signatureRequest, err := makeChainOracleMessage(
			signatureRequest,
			sessionID,
			chainStatus.chainData,
		)
		if err != nil {
			o.logger.Error(
				"failed to create signature requests",
				"sessionID", sessionID,
				"err", err,
			)
			continue
		}

		payload, err := makeTransactionPayload(chainStatus.chainData)
		if err != nil {
			o.logger.Error(
				"failed to make transaction payload",
				"sessionID", sessionID,
				"err", err,
			)
			continue
		}

		tx := makeTransaction(chainStatus.contractId, payload)
		signatureRequests = append(signatureRequests, *signatureRequest)

		sessionMap[sessionID] = session{
			vscTxShell: tx,
			chainData:  payload,
			signatures: make([]string, 0, 128),
		}
	}

	// broadcast signature requests + collect signatures
	// - Ask P2P channels for signatures for the transaction
	// - Receiving node will receive request to create transaction on VSC mainnet
	defer o.signatureChannels.clearMap()

	sessionMapMtx := &sync.Mutex{}
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
			sessionMapMtx,
			sigChan,
			sigRequest.SessionID,
			sessionMap,
		)
	}

	<-ctx.Done()

	// validate signatures + submit transaction
	for sessionID, session := range sessionMap {
		fmt.Println(sessionID, session)
	}

}

// validate signatures + make transaction
func makeTransactionPayload(blocks []chainBlock) ([]string, error) {
	payload := make([]string, len(blocks))
	var err error

	for i, block := range blocks {
		payload[i], err = block.Serialize()
		if err != nil {
			return nil, err
		}
	}

	return payload, nil
}

func collectSignature(
	ctx context.Context,
	mtx *sync.Mutex,
	sigChan <-chan signatureMessage,
	sessionID string,
	sessionMap map[string]session,
) {
	for {
		select {
		case <-ctx.Done():
			return

		case msg := <-sigChan:
			mtx.Lock()
			session := sessionMap[sessionID]
			session.signatures = append(session.signatures, msg.Signature)
			sessionMap[sessionID] = session
			mtx.Unlock()
		}
	}
}
