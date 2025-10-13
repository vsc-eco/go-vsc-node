package chain

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"vsc-node/lib/dids"
	"vsc-node/lib/utils"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/oracle/p2p"
	stateEngine "vsc-node/modules/state-processing"
	transactionpool "vsc-node/modules/transaction-pool"

	"github.com/hasura/go-graphql-client"
)

type blockProducer struct {
	ctx                    context.Context
	logger                 *slog.Logger
	username               string
	p2pSpec                p2p.OracleP2PSpec
	sigChan                *signatureChannels
	electedMembers         []elections.ElectionMember
	totalWeight            uint64
	electedMemberWeightMap []uint64
}

// input expected by the contract
type AddBlocksInput struct {
	Blocks    string `json:"blocks"`
	LatestFee int64  `json:"latest_fee"`
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

	blockProducer := &blockProducer{
		ctx:                    o.ctx,
		logger:                 o.logger,
		username:               o.conf.Get().HiveUsername,
		p2pSpec:                p2pSpec,
		sigChan:                o.signatureChannels,
		electedMembers:         signal.ElectedMembers,
		totalWeight:            signal.TotalElectionWeight,
		electedMemberWeightMap: signal.WeightMap,
	}

	for symbol := range o.chainRelayers {
		o.logger.Debug("block tick event", "symbol", symbol)
		chainSession, err := o.makeChainSession(symbol)
		if err != nil {
			o.logger.Error("failed to get chain session",
				"symbol", symbol,
				"err", err,
			)
			continue
		}

		if err := blockProducer.handleChainSession(chainSession); err != nil {
			o.logger.Error(
				"failed to process chain session",
				"network", symbol, "err", err,
			)
		}
	}
}

func (bp *blockProducer) handleChainSession(
	chain chainSession,
) error {
	bp.logger.Debug("handling chain session", "sessionID", chain.sessionID)

	if len(bp.electedMemberWeightMap) != len(bp.electedMembers) {
		return errors.New(
			"invalid weightMap or members of the current election",
		)
	}

	sessionID := chain.sessionID
	bp.logger.Debug("created session ID", "sessionID", sessionID)

	// make signature receiver
	sigChan, err := bp.sigChan.makeSession(sessionID)
	if err != nil {
		return fmt.Errorf("failed to make signature session: %w", err)
	}
	defer bp.sigChan.removeSession(sessionID)

	// create tx + chain hash
	payloadBlocks := make([]string, len(chain.chainData))
	for i, block := range chain.chainData {
		payloadBlocks[i], err = block.Serialize()
		if err != nil {
			return fmt.Errorf(
				"failed to serialize block %d: %w",
				block.BlockHeight(), err,
			)
		}
	}
	latestFeeRate := chain.chainData[len(chain.chainData)-1].AverageFee()
	payloadStruct := AddBlocksInput{
		Blocks:    strings.Join(payloadBlocks, ""),
		LatestFee: latestFeeRate,
	}
	payload, err := json.Marshal(payloadStruct)
	if err != nil {
		return fmt.Errorf("failed to marshal tx payload: %w", err)
	}

	nonce, err := getAccountNonce(strings.ToLower(chain.symbol))
	if err != nil {
		return fmt.Errorf("failed to get nonce value: %w", err)
	}

	tx, err := makeSignableBlock(chain.contractId, chain.symbol, string(payload), nonce)
	if err != nil {
		return fmt.Errorf("failed to make transaction: %w", err)
	}
	txCid := tx.Cid()

	// create transaction + bls circuit
	witnessDIDs := utils.Map(
		bp.electedMembers,
		func(w elections.ElectionMember) dids.Member {
			return dids.BlsDID(w.Key)
		},
	)

	circuit, err := dids.NewBlsCircuitGenerator(witnessDIDs).Generate(txCid)
	if err != nil {
		return fmt.Errorf("failed to generate bls circuit: %w", err)
	}

	// collect witness data
	witnessAccounts := make([]string, len(bp.electedMembers))
	for i := range bp.electedMembers {
		member := &bp.electedMembers[i]
		witnessAccounts[i] = member.Account
	}

	weightThreshold := (bp.totalWeight * 2) / 3
	signedWeight := uint64(0)
	bp.logger.Debug("set threshold", "threshold", weightThreshold)

	// broadcast signature request
	blockProducerMsg := chainOracleBlockProducerMessage{
		BlockProducer: bp.username,
		SigHash:       txCid.String(),
	}

	msgJsonBytes, err := json.Marshal(&blockProducerMsg)
	if err != nil {
		return fmt.Errorf("failed to serialize block producer message: %w", err)
	}

	signatureRequestMsg := chainOracleMessage{
		MessageType: signatureRequest,
		SessionID:   sessionID,
		Payload:     json.RawMessage(msgJsonBytes),
	}

	if err := bp.p2pSpec.Broadcast(p2p.MsgChainRelay, &signatureRequestMsg); err != nil {
		return fmt.Errorf("failed to broadcast message: %w", err)
	}

	bp.logger.Debug(
		"broadcasted signature request message",
		"message", signatureRequestMsg,
	)

	// collect witness signatures
	ctx, cancel := context.WithTimeout(bp.ctx, time.Minute)
	defer cancel()

	for signedWeight < weightThreshold {
		select {
		case <-ctx.Done():
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("context error: %w", err)
			} else {
				return errors.New("signature collection timed out")
			}

		case msg := <-sigChan:
			memberIndex := slices.Index(witnessAccounts, msg.Signer)
			if memberIndex == -1 {
				bp.logger.Debug(
					"invalid witness signature, dropping.",
					"signer", msg.Signer,
				)
				continue
			}

			member := &bp.electedMembers[memberIndex]
			memberDID := dids.BlsDID(member.Key)

			signature := msg.Signature

			added, err := circuit.AddAndVerify(memberDID, signature)
			if err != nil {
				bp.logger.Error("failed to add member to circuit.", "err", err)
				continue
			}

			if !added {
				bp.logger.Debug(
					"invalid member, signature not added to circuit",
				)
				continue
			}

			signedWeight += bp.electedMemberWeightMap[memberIndex]

			bp.logger.Debug(
				"received witness signature, appending to witnessSigned",
				"signer", member.Account,
				"signature", signature,
			)
		}
	}

	// make transaction + submit to contract
	blsCircuit, err := circuit.Finalize()
	if err != nil {
		return fmt.Errorf("failed to finalize circuit: %w", err)
	}

	serializedCircuit, err := blsCircuit.Serialize()
	if err != nil {
		return fmt.Errorf("failed to finalize circuit: %w", err)
	}

	sigPackage := stateEngine.TransactionSig{
		Type: "vsc-sig",
		Sigs: []common.Sig{
			{
				Algo: "bls-agg",
				Sig:  serializedCircuit.Signature,
				Bv:   serializedCircuit.BitVector,
				Kid:  "",
			},
		},
	}

	sigBytes, err := common.EncodeDagCbor(sigPackage)
	if err != nil {
		return fmt.Errorf("failed encode DagCbor: %w", err)
	}

	//Submit to graphql SubmitTransaction()
	vscTx := transactionpool.SerializedVSCTransaction{
		Tx:  tx.Cid().Bytes(),
		Sig: sigBytes,
	}

	if err := bp.submitToContract(vscTx); err != nil {
		return fmt.Errorf("failed to submit to contract: %w", err)
	}

	return nil
}

func (bp *blockProducer) submitToContract(
	vscTx transactionpool.SerializedVSCTransaction,
) error {
	client := graphql.NewClient("https://api.vsc.eco/api/v1/graphql", nil)

	var query struct {
		Result struct{} `graphql:"submitTransactionV1(tx: $tx, sig: $sig)"`
	}

	enc := base64.StdEncoding.EncodeToString
	variables := map[string]any{
		"tx":  graphql.String(enc(vscTx.Tx)),
		"sig": graphql.String(enc(vscTx.Sig)),
	}

	opName := graphql.OperationName("SubmitContract")

	ctx, cancel := context.WithTimeout(bp.ctx, 15*time.Second)
	defer cancel()

	if err := client.Query(ctx, &query, variables, opName); err != nil {
		return fmt.Errorf("failed graphql query: %w", err)
	}

	return nil
}
