package chain

import (
	"context"
	"errors"
	"sync"
	"time"
	"vsc-node/modules/oracle/p2p"
)

type session struct {
	chainData  []byte
	signatures []string
}

type vscTransaction struct {
	sessionID  string
	chainData  []byte
	signatures []string
}

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
		sessionMap        = make(map[string]session)
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
		sessionMap[chainSession.sessionID] = session{
			chainData:  chainSession.chainData,
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
		tx, err := makeTransaction(sessionID, &session)
		if err != nil {
			o.logger.Error(
				"failed to create VSC transaction",
				"session", sessionID,
				"err", err,
			)
			continue
		}

		if err := submitToContract(tx); err != nil {
			o.logger.Error(
				"failed to submit contract",
				"session", sessionID,
				"err", err,
			)
			continue
		}
	}

}

func submitToContract(tx *vscTransaction) error {
	if tx == nil {
		return errors.New("nil transaction")
	}

	// TODO: submit transaction

	return nil
}

// validate signatures + make transaction
func makeTransaction(
	sessionID string,
	session *session,
) (*vscTransaction, error) {
	// TODO: verify signatures with bls circuit

	// create transaction
	tx := &vscTransaction{
		sessionID:  sessionID,
		chainData:  session.chainData,
		signatures: session.signatures,
	}

	return tx, nil
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
