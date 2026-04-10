package chain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"vsc-node/lib/dids"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/oracle/p2p"
	transactionpool "vsc-node/modules/transaction-pool"
)

const (
	// Time to wait for witness signatures before proceeding. This is the
	// hard upper bound — the no-progress watchdog (below) usually fires
	// long before this when the threshold is unreachable.
	signatureCollectionTimeout = 90 * time.Second

	// signatureNoProgressTimeout is the maximum time the producer will
	// wait between successive accepted signatures before assuming no more
	// are coming. When down witnesses make the threshold unreachable, this
	// lets the producer give up in a few seconds instead of stalling for
	// the full signatureCollectionTimeout.
	//
	// Healthy nodes typically respond within a few hundred milliseconds,
	// so a 15s window is a comfortable safety margin for slow-but-alive
	// witnesses while still saving 75 seconds per relay attempt against
	// genuinely down nodes.
	signatureNoProgressTimeout = 15 * time.Second
)

func makeTransaction(
	contractId string,
	payload string,
	action string,
	symbol string,
	txNetId string,
	nonce uint64,
) transactionpool.VSCTransaction {
	op := transactionpool.VscContractCall{
		ContractId: contractId,
		Action:     action,
		Payload:    payload,
		Intents:    []contracts.Intent{},
		RcLimit:    100000,
		Caller:     "did:vsc:oracle:" + strings.ToLower(symbol),
		NetId:      txNetId,
	}
	vOp, _ := op.SerializeVSC()
	return transactionpool.VSCTransaction{
		Ops:     []transactionpool.VSCTransactionOp{vOp},
		Nonce:   nonce,
		NetId:   txNetId,
		RcLimit: uint64(op.RcLimit),
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
	var startHeight, endHeight uint64
	var rangeKey string

	if chainStatus.replaceBlock {
		rangeKey = "replace-" + chainStatus.replaceBlockHex[:16]
	} else {
		if blockCount == 0 {
			o.logger.Warn("processChainRelay called with empty chain data, skipping",
				"symbol", chainStatus.symbol,
			)
			return
		}
		startHeight = chainStatus.chainData[0].BlockHeight()
		endHeight = chainStatus.chainData[blockCount-1].BlockHeight()
		rangeKey = fmt.Sprintf("%d-%d", startHeight, endHeight)
	}

	// Skip if the start of this batch overlaps with a previously submitted
	// batch that hasn't been processed on-chain yet. This prevents overlapping
	// submissions when the mempool hasn't cleared.
	// Expires after 2 minutes to avoid deadlocks if the tx failed on-chain.
	if !chainStatus.replaceBlock {
		if lastEnd, ok := o.lastSubmittedEnd[chainStatus.symbol]; ok && startHeight <= lastEnd {
			submittedAt := o.lastSubmittedAt[chainStatus.symbol]
			expired := time.Since(submittedAt) > 2*time.Minute

			contractHeight, err := o.getContractBlockHeight(chainStatus.contractId)
			if err == nil && contractHeight >= lastEnd {
				// Contract caught up — clear the tracker
				delete(o.lastSubmittedEnd, chainStatus.symbol)
				delete(o.lastSubmittedAt, chainStatus.symbol)
			} else if expired {
				// Timeout — previous tx likely failed, allow retry
				o.logger.Info("dedup tracker expired, allowing retry",
					"symbol", chainStatus.symbol,
					"lastSubmittedEnd", lastEnd,
				)
				delete(o.lastSubmittedEnd, chainStatus.symbol)
				delete(o.lastSubmittedAt, chainStatus.symbol)
			} else {
				o.logger.Debug("skipping overlapping range (previous tx still pending)",
					"symbol", chainStatus.symbol,
					"range", rangeKey,
					"lastSubmittedEnd", lastEnd,
				)
				return
			}
		}
	}

	// Skip if we recently witnessed (signed) this range for another producer
	// AND the contract height has advanced (meaning their submission succeeded).
	witnessKey := fmt.Sprintf("%s:%s", strings.ToUpper(chainStatus.symbol), rangeKey)
	if witnessedAt, ok := o.recentlyWitnessed[witnessKey]; ok {
		if time.Since(witnessedAt) < 5*time.Minute {
			contractHeight, err := o.getContractBlockHeight(chainStatus.contractId)
			if err == nil && contractHeight >= endHeight {
				o.logger.Debug("skipping range recently witnessed (contract advanced)",
					"symbol", chainStatus.symbol,
					"range", rangeKey,
				)
				return
			}
			o.logger.Info("producing despite recent witness (contract did not advance)",
				"symbol", chainStatus.symbol,
				"range", rangeKey,
				"witnessedAgo", time.Since(witnessedAt),
			)
		}
		delete(o.recentlyWitnessed, witnessKey)
	}

	o.logger.Debug("initiating chain relay consensus",
		"symbol", chainStatus.symbol,
		"blocks", blockCount,
		"start", startHeight,
		"end", endHeight,
	)

	// Build the transaction payload
	var payloadJson []byte
	if !chainStatus.replaceBlock {
		txPayload, err := makeTransactionPayload(chainStatus.chainData)
		if err != nil {
			o.logger.Error("failed to make transaction payload",
				"symbol", chainStatus.symbol, "err", err,
			)
			return
		}

		payloadJson, err = json.Marshal(txPayload)
		if err != nil {
			o.logger.Error("failed to marshal payload",
				"symbol", chainStatus.symbol, "err", err,
			)
			return
		}
	}

	oracleDid := "did:vsc:oracle:" + strings.ToLower(chainStatus.symbol)
	var nonce uint64
	if o.nonceDb != nil {
		if nonceRecord, err := o.nonceDb.GetNonce(oracleDid); err == nil {
			nonce = nonceRecord.Nonce
		}
	}

	var action, txPayloadStr string
	if chainStatus.replaceBlock {
		action = "replaceBlocks"
		// replaceBlocks contract input is concatenated hex of 80-byte headers.
		// Payload is a Go string that gets json.Marshal'd by SerializeVSC,
		// which adds the JSON quotes automatically.
		txPayloadStr = chainStatus.replaceBlockHex
		o.logger.Info("submitting replaceBlocks to fix reorg",
			"symbol", chainStatus.symbol,
			"depth", chainStatus.replaceBlockDepth,
		)
	} else {
		action = "addBlocks"
		txPayloadStr = string(payloadJson)
	}

	tx := makeTransaction(chainStatus.contractId, txPayloadStr, action, chainStatus.symbol, o.sconf.NetId(), nonce)

	// Hash the transaction to get the CID that witnesses will sign
	signableBlock, err := tx.ToSignableBlock()
	if err != nil {
		o.logger.Error("failed to create signable block",
			"symbol", chainStatus.symbol, "err", err,
		)
		return
	}

	txCid := signableBlock.Cid()
	o.logger.Debug("producer computed cid",
		"symbol", chainStatus.symbol,
		"cid", txCid.String(),
	)

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
	sessionID, err := makeChainSessionID(&chainStatus, signal.BlockHeight)
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
		Nonce:      nonce,
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
	o.logger.Debug("broadcast signature request",
		"symbol", chainStatus.symbol,
		"sessionID", sessionID,
	)

	// Wait for signatures with two stacked timeouts:
	//   1. signatureCollectionTimeout — the hard upper bound (90s).
	//   2. signatureNoProgressTimeout — a watchdog that cancels the
	//      collection sub-context once no new signature has arrived for N
	//      seconds. When the threshold is unreachable (because some
	//      witnesses are down or partitioned), this lets the producer give
	//      up in seconds instead of waiting the full 90 seconds.
	//
	// The watchdog runs alongside collectChainSignatures and triggers via
	// context cancellation. The helper itself is unchanged — it still
	// embodies the "wait until threshold or timeout" primitive — and its
	// unit tests in collect_signatures_test.go continue to pass.
	threshold := electionResult.TotalWeight * 2 / 3

	collectCtx, collectCancel := context.WithCancel(o.ctx)
	defer collectCancel()

	var lastProgressMu sync.Mutex
	lastProgress := time.Now()
	noProgressFired := false

	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-collectCtx.Done():
				return
			case <-ticker.C:
				lastProgressMu.Lock()
				since := time.Since(lastProgress)
				lastProgressMu.Unlock()
				if since > signatureNoProgressTimeout {
					o.logger.Debug("signature collection no-progress watchdog firing",
						"symbol", chainStatus.symbol,
						"noProgressFor", since,
					)
					lastProgressMu.Lock()
					noProgressFired = true
					lastProgressMu.Unlock()
					collectCancel()
					return
				}
			}
		}
	}()

	// Wrap circuit.AddAndVerify so successful additions reset the
	// no-progress clock.
	verifyWithProgress := func(member dids.BlsDID, signature string) (bool, error) {
		added, err := circuit.AddAndVerify(member, signature)
		if added {
			lastProgressMu.Lock()
			lastProgress = time.Now()
			lastProgressMu.Unlock()
		}
		return added, err
	}

	collectResult := collectChainSignatures(
		collectCtx,
		o.logger,
		sigChan,
		signatureCollectionTimeout,
		selfWeight,
		threshold,
		selfDid,
		verifyWithProgress,
		func(did dids.BlsDID) uint64 { return findMemberWeight(&electionResult, did) },
		chainStatus.symbol,
	)

	// Drain the watchdog goroutine.
	collectCancel()

	if collectResult.Cancelled {
		// Distinguish a real shutdown (parent o.ctx cancelled) from the
		// watchdog firing on no-progress. Only the former should bail
		// without logging "not enough signatures".
		lastProgressMu.Lock()
		watchdogFired := noProgressFired
		lastProgressMu.Unlock()

		if !watchdogFired && o.ctx.Err() != nil {
			o.logger.Warn("context cancelled during signature collection",
				"symbol", chainStatus.symbol,
			)
			return
		}
		// Otherwise fall through to the threshold check below — the
		// watchdog gave up, but we should still log the under-threshold
		// state via the existing log line so monitoring tools see it.
	}

	signedWeight = collectResult.SignedWeight

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
	if o.txPool == nil {
		o.logger.Warn("transaction pool not configured, skipping submission",
			"symbol", chainStatus.symbol,
		)
		return
	}

	// Finalize the BLS circuit to produce the aggregated signature.
	finalCircuit, err := circuit.Finalize()
	if err != nil {
		o.logger.Error("failed to finalize BLS circuit",
			"symbol", chainStatus.symbol, "err", err,
		)
		return
	}

	serializedCircuit, err := finalCircuit.Serialize()
	if err != nil {
		o.logger.Error("failed to serialize BLS circuit",
			"symbol", chainStatus.symbol, "err", err,
		)
		return
	}

	sTx, err := tx.Serialize()
	if err != nil {
		o.logger.Error("failed to serialize transaction",
			"symbol", chainStatus.symbol, "err", err,
		)
		return
	}

	sigPackage := transactionpool.SignaturePackage{
		Type: "vsc-sig",
		Sigs: []common.Sig{
			{
				Algo: "BLS12-381",
				Kid:  "did:vsc:oracle:" + strings.ToLower(chainStatus.symbol),
				Sig:  serializedCircuit.Signature,
				Bv:   serializedCircuit.BitVector,
			},
		},
	}

	sigBytes, err := common.EncodeDagCbor(sigPackage)
	if err != nil {
		o.logger.Error("failed to encode signature package",
			"symbol", chainStatus.symbol, "err", err,
		)
		return
	}

	signedTx := transactionpool.SerializedVSCTransaction{
		Tx:  sTx.Tx,
		Sig: sigBytes,
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

	// Track the end height to prevent overlapping submissions
	if !chainStatus.replaceBlock {
		o.lastSubmittedEnd[chainStatus.symbol] = endHeight
		o.lastSubmittedAt[chainStatus.symbol] = time.Now()
	}
}

// utxoAddBlocksPayload matches the UTXO mapping contract's AddBlocksParams:
// a single concatenated hex string of fixed 80-byte block headers,
// plus the latest fee rate for the chain.
type utxoAddBlocksPayload struct {
	Blocks    string `json:"blocks"`
	LatestFee int64  `json:"latest_fee"`
}

// ethAddBlocksPayload matches the ETH mapping contract's AddBlocksParams:
// an array of individually hex-encoded RLP block headers (variable length).
type ethAddBlocksPayload struct {
	Blocks []string `json:"blocks"`
}

// makeTransactionPayload builds the chain-appropriate payload.
// UTXO chains (BTC/DASH/LTC) use concatenated hex + fee rate.
// ETH uses an array of individual hex strings.
func makeTransactionPayload(blocks []chainBlock) (any, error) {
	if len(blocks) == 0 {
		return &utxoAddBlocksPayload{}, nil
	}

	// Determine chain type from first block.
	if blocks[0].Type() == "ETH" {
		return makeEthPayload(blocks)
	}
	return makeUtxoPayload(blocks)
}

func makeUtxoPayload(blocks []chainBlock) (*utxoAddBlocksPayload, error) {
	var concatenated strings.Builder
	var latestFee int64

	for _, block := range blocks {
		hex, err := block.Serialize()
		if err != nil {
			return nil, err
		}
		concatenated.WriteString(hex)

		// Extract fee rate from BTC blocks (last block's rate wins).
		if btc, ok := block.(*btcChainData); ok {
			latestFee = btc.AverageFeeRate
		}
	}

	return &utxoAddBlocksPayload{
		Blocks:    concatenated.String(),
		LatestFee: latestFee,
	}, nil
}

func makeEthPayload(blocks []chainBlock) (*ethAddBlocksPayload, error) {
	headers := make([]string, len(blocks))
	for i, block := range blocks {
		hex, err := block.Serialize()
		if err != nil {
			return nil, err
		}
		headers[i] = hex
	}
	return &ethAddBlocksPayload{Blocks: headers}, nil
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
