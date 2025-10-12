package chain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"
	"vsc-node/lib/dids"
	"vsc-node/lib/utils"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/oracle/p2p"
)

type blockProducer struct {
	ctx                    context.Context
	username               string
	p2pSpec                p2p.OracleP2PSpec
	sigChan                *signatureChannels
	electedMembers         []elections.ElectionMember
	totalWeight            uint64
	electedMemberWeightMap []uint64
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

	// make chainDataPool and signature requests
	chainStatuses := o.fetchAllStatuses()

	o.logger.Debug("fetched all chain statuses", "chainStatuses", chainStatuses)

	// reset signatureChannels
	o.signatureChannels.clearMap()
	defer o.signatureChannels.clearMap()

	blockProducer := &blockProducer{
		ctx:                    o.ctx,
		username:               o.conf.Get().HiveUsername,
		p2pSpec:                p2pSpec,
		sigChan:                o.signatureChannels,
		electedMembers:         signal.ElectedMembers,
		totalWeight:            signal.TotalElectionWeight,
		electedMemberWeightMap: signal.WeightMap,
	}

	o.logger.Debug("created blockProducer", "blockProducer", blockProducer)

	for _, chain := range chainStatuses {
		if !chain.newBlocksToSubmit {
			continue
		}

		if err := blockProducer.handleChainSession(chain, o.logger); err != nil {
			o.logger.Error(
				"failed to process chain session",
				"network", chain.symbol, "err", err,
			)
		}
	}

	// verify witnesses signaure:
	// -- get schedule, then
	// -- get the current slot inforation
	// -- get the current election
	// -- verify the signature witnesses
	// --- need the username + pubkey of the witnesses
	// --- ^ spencer will write this function
}

func (bp *blockProducer) handleChainSession(
	chain chainSession,
	logger *slog.Logger,
) error {
	if len(bp.electedMemberWeightMap) != len(bp.electedMembers) {
		return errors.New(
			"invalid weightMap or members of the current election",
		)
	}

	logger.Debug("handling chain session", "chain", chain)
	// make + broadcast a signature request
	sessionID, err := makeChainSessionID(&chain)
	if err != nil {
		return fmt.Errorf("failed to make chain session: %w", err)
	}

	logger.Debug("created session ID", "sessionID", sessionID)

	payload := make([]string, len(chain.chainData))
	for i, block := range chain.chainData {
		payload[i], err = block.Serialize()
		if err != nil {
			return fmt.Errorf(
				"failed to serialize block %d: %w",
				block.BlockHeight(), err,
			)
		}
	}

	logger.Debug("created payload", "payload", payload)

	blockProducerMsg := chainOracleBlockProducerMessage{
		BlockProducer: bp.username,
		BlockHash:     payload,
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

	logger.Debug(
		"created signature request message",
		"message",
		signatureRequestMsg,
	)

	if err := bp.p2pSpec.Broadcast(p2p.MsgChainRelay, &signatureRequestMsg); err != nil {
		return fmt.Errorf("failed to broadcast message: %w", err)
	}

	// create transaction + bls circuit
	nonce, err := getAccountNonce(strings.ToLower(chain.symbol))
	if err != nil {
		return fmt.Errorf("failed to get nonce value: %w", err)
	}

	tx, err := makeSignableBlock(chain.contractId, chain.symbol, payload, nonce)
	if err != nil {
		return fmt.Errorf("failed to make transaction: %w", err)
	}

	witnessDIDs := utils.Map(
		bp.electedMembers,
		func(w elections.ElectionMember) dids.Member {
			return dids.BlsDID(w.Key)
		},
	)

	circuit, err := dids.NewBlsCircuitGenerator(witnessDIDs).Generate(tx.Cid())
	if err != nil {
		return fmt.Errorf("failed to generate bls circuit: %w", err)
	}

	// collect witness accounts, pubkeys and signatures
	sigChan, err := bp.sigChan.makeSession(sessionID)
	if err != nil {
		return fmt.Errorf("failed to make signature session: %w", err)
	}

	ctx, cancel := context.WithTimeout(bp.ctx, time.Minute)
	defer cancel()

	witnessAccounts := make([]string, len(bp.electedMembers))
	for i, member := range bp.electedMembers {
		witnessAccounts[i] = member.Account
	}

	type WitnessSignature struct {
		elections.ElectionMember
		Signatures string
	}

	weightThreshold := uint64(math.Floor((2.0 / 3.0) * float64(bp.totalWeight)))
	signedWeight := uint64(0)
	logger.Debug("set threshold", "threshold", weightThreshold)

	for signedWeight < weightThreshold {
		select {
		case <-ctx.Done():
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("context error: %w", err)
			} else {
				return errors.New("signature collection timed out.")
			}

		case msg := <-sigChan:
			memberIndex := slices.Index(witnessAccounts, msg.Signer)
			if memberIndex == -1 {
				logger.Debug(
					"invalid witness signature, dropping.",
					"signer", msg.Signer,
				)
				continue
			}

			member := bp.electedMembers[memberIndex]
			memberDID := dids.BlsDID(member.Key)

			signature := msg.Signature

			added, err := circuit.AddAndVerify(memberDID, signature)
			if err != nil {
				logger.Error("failed to add member to circuit.", "err", err)
				continue
			}

			if !added {
				logger.Debug("invalid member, signature not added to circuit")
				continue
			}

			signedWeight += bp.electedMemberWeightMap[memberIndex]

			logger.Debug(
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

	bpSig := common.Sig{
		Algo: "bls.agg",
		Sig:  serializedCircuit.Signature,
		Bv:   serializedCircuit.BitVector,
		Kid:  "",
	}

	// TODO: sign bpSig + submit to contract

	logger.Debug(
		"created and serialized tx",
		"tx", tx,
	)

	return nil
}
