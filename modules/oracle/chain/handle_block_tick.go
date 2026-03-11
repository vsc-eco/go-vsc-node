package chain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/oracle/p2p"
	transactionpool "vsc-node/modules/transaction-pool"
)

const (
	// Time to wait for witness signatures before proceeding.
	signatureCollectionTimeout = 12 * time.Second
)

func makeTransaction(
	contractId string,
	payload string,
	symbol string,
	txNetId string,
) transactionpool.VSCTransaction {
	op := transactionpool.VscContractCall{
		ContractId: contractId,
		Action:     "add_blocks",
		Payload:    payload,
		Intents:    []contracts.Intent{},
		RcLimit:    1000,
		Caller:     "did:vsc:oracle:" + strings.ToLower(symbol),
		NetId:      txNetId,
	}
	vOp, _ := op.SerializeVSC()
	return transactionpool.VSCTransaction{
		Ops:   []transactionpool.VSCTransactionOp{vOp},
		Nonce: 0,
		NetId: txNetId,
	}
}

// HandleBlockTick is called when it's time to relay chain data.
// If this node is the block producer, it initiates the P2P consensus flow:
// 1. Fetch new chain blocks
// 2. Build the relay transaction
// 3. Broadcast signature request to elected witnesses
// 4. Collect BLS signatures until >=2/3 threshold
// 5. Submit the signed transaction
func (o *ChainOracle) HandleBlockTick(
	signal p2p.BlockTickSignal,
	p2pSpec p2p.OracleP2PSpec,
) {
	if !signal.IsProducer {
		return
	}

	chainStatuses := o.fetchAllStatuses()

	for _, chainStatus := range chainStatuses {
		if !chainStatus.newBlocksToSubmit {
			continue
		}

		o.processChainRelay(chainStatus, signal, p2pSpec)
	}
}

func (o *ChainOracle) processChainRelay(
	chainStatus chainSession,
	signal p2p.BlockTickSignal,
	p2pSpec p2p.OracleP2PSpec,
) {
	blockCount := len(chainStatus.chainData)
	startHeight := chainStatus.chainData[0].BlockHeight()
	endHeight := chainStatus.chainData[blockCount-1].BlockHeight()

	o.logger.Debug("initiating chain relay consensus",
		"symbol", chainStatus.symbol,
		"blocks", blockCount,
		"start", startHeight,
		"end", endHeight,
	)

	// Build the transaction payload
	payload, err := makeTransactionPayload(chainStatus.chainData)
	if err != nil {
		o.logger.Error("failed to make transaction payload",
			"symbol", chainStatus.symbol, "err", err,
		)
		return
	}

	payloadJson, err := json.Marshal(payload)
	if err != nil {
		o.logger.Error("failed to marshal payload",
			"symbol", chainStatus.symbol, "err", err,
		)
		return
	}

	tx := makeTransaction(chainStatus.contractId, string(payloadJson), chainStatus.symbol, o.sconf.NetId())

	// Hash the transaction to get the CID that witnesses will sign
	signableBlock, err := tx.ToSignableBlock()
	if err != nil {
		o.logger.Error("failed to create signable block",
			"symbol", chainStatus.symbol, "err", err,
		)
		return
	}

	txCid := signableBlock.Cid()

	// Get the current election to set up the BLS circuit
	electionResult, err := o.electionDb.GetElectionByHeight(signal.BlockHeight)
	if err != nil {
		o.logger.Error("failed to get election result",
			"symbol", chainStatus.symbol, "err", err,
		)
		return
	}

	memberKeys := electionResult.MemberKeys()
	circuit, err := dids.NewBlsCircuitGenerator(memberKeys).Generate(txCid)
	if err != nil {
		o.logger.Error("failed to create BLS circuit",
			"symbol", chainStatus.symbol, "err", err,
		)
		return
	}

	// Self-sign: producer signs its own data
	blsProvider, err := o.conf.BlsProvider()
	if err != nil {
		o.logger.Error("failed to get BLS provider",
			"symbol", chainStatus.symbol, "err", err,
		)
		return
	}

	selfSig, err := blsProvider.Sign(txCid)
	if err != nil {
		o.logger.Error("failed to self-sign",
			"symbol", chainStatus.symbol, "err", err,
		)
		return
	}

	selfDid, err := o.conf.BlsDID()
	if err != nil {
		o.logger.Error("failed to get own BLS DID",
			"symbol", chainStatus.symbol, "err", err,
		)
		return
	}

	added, err := circuit.AddAndVerify(selfDid, selfSig)
	if err != nil || !added {
		o.logger.Error("failed to add self-signature to circuit",
			"symbol", chainStatus.symbol, "err", err, "added", added,
		)
		return
	}

	selfWeight := findMemberWeight(&electionResult, selfDid)
	signedWeight := selfWeight

	// Create a session to receive signatures from peers
	sessionID, err := makeChainSessionID(&chainStatus)
	if err != nil {
		o.logger.Error("failed to create session ID",
			"symbol", chainStatus.symbol, "err", err,
		)
		return
	}

	sigChan, err := o.signatureChannels.makeSession(sessionID)
	if err != nil {
		if errors.Is(err, errChannelExists) {
			// A previous tick is already collecting signatures for this
			// session — nothing to do.
			o.logger.Debug("signature session already active, skipping",
				"symbol", chainStatus.symbol,
				"sessionID", sessionID,
			)
			return
		}
		o.logger.Error("failed to create signature session",
			"symbol", chainStatus.symbol, "err", err,
		)
		return
	}
	defer o.signatureChannels.clearSession(sessionID)

	// Broadcast signature request to all witnesses
	request := chainRelayRequest{
		ContractId: chainStatus.contractId,
		NetId:      o.sconf.NetId(),
	}

	requestMsg, err := makeChainOracleMessage(signatureRequest, sessionID, request)
	if err != nil {
		o.logger.Error("failed to create request message",
			"symbol", chainStatus.symbol, "err", err,
		)
		return
	}

	if err := p2pSpec.Broadcast(p2p.MsgChainRelay, requestMsg); err != nil {
		o.logger.Error("failed to broadcast signature request",
			"symbol", chainStatus.symbol, "err", err,
		)
		return
	}


	// Wait for signatures with timeout
	threshold := electionResult.TotalWeight * 2 / 3
	timer := time.NewTimer(signatureCollectionTimeout)
	defer timer.Stop()

	for signedWeight <= threshold {
		select {
		case <-o.ctx.Done():
			o.logger.Warn("context cancelled during signature collection",
				"symbol", chainStatus.symbol,
			)
			return

		case <-timer.C:
			o.logger.Debug("signature collection timeout",
				"symbol", chainStatus.symbol,
				"signedWeight", signedWeight,
				"threshold", threshold,
			)
			goto checkThreshold

		case sigMsg := <-sigChan:
			if sigMsg.BlsDid == "" || sigMsg.Signature == "" {
				continue
			}

			// Skip our own signature (already added)
			if sigMsg.BlsDid == selfDid.String() {
				continue
			}

			member := dids.BlsDID(sigMsg.BlsDid)
			added, err := circuit.AddAndVerify(member, sigMsg.Signature)
			if err != nil {
				o.logger.Debug("failed to verify signature",
					"symbol", chainStatus.symbol,
					"account", sigMsg.Account,
					"err", err,
				)
				continue
			}

			if added {
				weight := findMemberWeight(&electionResult, member)
				signedWeight += weight
				o.logger.Debug("received signature",
					"symbol", chainStatus.symbol,
					"account", sigMsg.Account,
					"weight", weight,
					"signedWeight", signedWeight,
					"threshold", threshold,
				)
			}
		}
	}

checkThreshold:
	if signedWeight <= threshold {
		o.logger.Info("not enough signatures for chain relay",
			"symbol", chainStatus.symbol,
			"signedWeight", signedWeight,
			"threshold", threshold,
			"totalWeight", electionResult.TotalWeight,
		)
		return
	}

	o.logger.Info("chain relay consensus reached",
		"symbol", chainStatus.symbol,
		"signedWeight", signedWeight,
		"threshold", threshold,
		"signers", circuit.SignerCount(),
	)

	// Submit the transaction
	if o.txCrafter == nil || o.txPool == nil {
		o.logger.Warn("transaction crafter or pool not configured, skipping submission",
			"symbol", chainStatus.symbol,
		)
		return
	}

	signedTx, err := o.txCrafter.SignFinal(tx)
	if err != nil {
		o.logger.Error("failed to sign transaction",
			"symbol", chainStatus.symbol, "err", err,
		)
		return
	}

	txCidResult, err := o.txPool.IngestTx(signedTx, transactionpool.IngestOptions{Broadcast: true})
	if err != nil {
		o.logger.Error("failed to submit transaction",
			"symbol", chainStatus.symbol, "err", err,
		)
		return
	}

	o.logger.Info("chain relay transaction submitted",
		"symbol", chainStatus.symbol,
		"txCid", txCidResult.String(),
		"blocks", fmt.Sprintf("%d-%d", startHeight, endHeight),
		"signers", circuit.SignerCount(),
	)
}

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

// findMemberWeight returns the election weight for a given BLS DID.
func findMemberWeight(election *elections.ElectionResult, did dids.BlsDID) uint64 {
	for i, member := range election.Members {
		if dids.BlsDID(member.Key) == did {
			if i < len(election.Weights) {
				return election.Weights[i]
			}
			return 1
		}
	}
	return 0
}
