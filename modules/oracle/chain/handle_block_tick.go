package chain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"time"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/oracle/p2p"
)

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
		ctx:            o.ctx,
		username:       o.conf.Get().HiveUsername,
		p2pSpec:        p2pSpec,
		sigChan:        o.signatureChannels,
		electedMembers: signal.ElectedMembers,
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

type blockProducer struct {
	ctx            context.Context
	username       string
	p2pSpec        p2p.OracleP2PSpec
	sigChan        *signatureChannels
	electedMembers []elections.ElectionMember
}

func (bp *blockProducer) handleChainSession(
	chain chainSession,
	logger *slog.Logger,
) error {
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

	threshold := int(math.Floor((2.0 / 3.0) * float64(len(bp.electedMembers))))
	logger.Debug("set threshold", "threshold", threshold)
	witnessSigned := make([]WitnessSignature, 0, threshold)
	for len(witnessSigned) < threshold {
		select {
		case <-ctx.Done():
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("context error: %w", err)
			} else {
				return errors.New("signature collection timed out.")
			}

		case msg := <-sigChan:
			i := slices.Index(witnessAccounts, msg.Signer)
			if i == -1 {
				logger.Debug(
					"invalid witness signature, dropping.",
					"signer", msg.Signer,
				)
				continue
			}

			witnessSignature := WitnessSignature{
				ElectionMember: bp.electedMembers[i],
				Signatures:     msg.Signature,
			}

			logger.Debug(
				"received witness signature, appending to witnessSigned",
				"signer", witnessSignature.Account,
				"signature", witnessSignature.Signatures,
			)

			witnessSigned = append(witnessSigned, witnessSignature)
		}
	}

	// make transaction + submit to contract
	tx, err := makeSignableBlock(chain.contractId, chain.symbol, payload, 0)
	if err != nil {
		return fmt.Errorf("failed to make transaction: %w", err)
	}

	logger.Debug(
		"created and serialized tx",
		"tx", tx,
	)

	tx.Cid().Hash() // TODO: sign this with bls private key

	fmt.Println("submit to contract, etc etc.:", tx)

	return nil
}
