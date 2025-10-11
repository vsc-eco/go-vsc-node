package chain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/oracle/p2p"
	transactionpool "vsc-node/modules/transaction-pool"
)

type session struct {
	vscTransaction transactionpool.SerializedVSCTransaction
	chainData      []string
	signatures     []string
}

type vscTransaction struct {
	sessionID  string
	chainData  []byte
	signatures []string
}

func makeTransaction(
	contractId *string,
	payload []string,
	symbol string,
) transactionpool.VSCTransaction {
	ops := make([]transactionpool.VSCTransactionOp, 0)
	op := transactionpool.VscContractCall{
		ContractId: *contractId,
		Action:     "add_blocks",
		Payload:    "",
		Intents:    []contracts.Intent{},
		RcLimit:    1000,
		Caller:     "did:vsc:oracle:" + symbol,
		NetId:      "vsc-mainnet",
	}
	vOp, _ := op.SerializeVSC()
	ops = append(ops, vOp)
	return transactionpool.VSCTransaction{
		Ops:   ops,
		Nonce: 0,
		NetId: "vsc-mainnet",
		// Headers: transactionpool.VSCTransactionHeader{
		// 	Nonce:         0,
		// 	RequiredAuths: []string{"did:vsc:oracle:" + symbol},

		// 	NetId: "vsc-mainnet",
		// },
		// Type:    "vsc-tx",
		// Version: "0.2",

		// Tx: ops,
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

	// make chainDataPool and signature requests
	chainStatuses := o.fetchAllStatuses()

	// reset signatureChannels
	o.signatureChannels.clearMap()
	defer o.signatureChannels.clearMap()

	blockProducer := &blockProducer{
		username:       o.conf.Get().HiveUsername,
		p2pSpec:        p2pSpec,
		sigChan:        o.signatureChannels,
		electedMembers: signal.ElectedMembers,
	}

	for _, chain := range chainStatuses {
		if !chain.newBlocksToSubmit {
			continue
		}

		if err := blockProducer.handleChainSession(chain); err != nil {
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
	username       string
	p2pSpec        p2p.OracleP2PSpec
	sigChan        *signatureChannels
	electedMembers []elections.ElectionMember
}

func (bp *blockProducer) handleChainSession(chain chainSession) error {
	// make + broadcast a signature request
	sessionID, err := makeChainSessionID(&chain)
	if err != nil {
		return fmt.Errorf("failed to make chain session: %w", err)
	}

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

	if err := bp.p2pSpec.Broadcast(p2p.MsgChainRelay, &signatureRequestMsg); err != nil {
		return fmt.Errorf("failed to broadcast message: %w", err)
	}

	// collect witness accounts, pubkeys and signatures
	sigChan, err := bp.sigChan.makeSession(sessionID)
	if err != nil {
		return fmt.Errorf("failed to make signature session: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.TODO(), time.Minute)
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
				continue
			}

			witnessSigned = append(witnessSigned, WitnessSignature{
				ElectionMember: bp.electedMembers[i],
				Signatures:     msg.Signature,
			})
		}
	}

	// make transaction + submit to contract
	tx := makeTransaction(chain.contractId, payload, chain.symbol)
	serializedTx, err := tx.Serialize()
	if err != nil {
		return fmt.Errorf("failed to make transaction: %w", err)
	}

	fmt.Println("submit to contract, etc etc.:", serializedTx)

	return nil
}
