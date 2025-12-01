package price

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
	"vsc-node/lib/dids"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/price/api"
	stateEngine "vsc-node/modules/state-processing"
	transactionpool "vsc-node/modules/transaction-pool"
)

// block producer
type Producer struct {
	p2p.OracleP2PSpec
	ctx              context.Context
	logger           *slog.Logger
	blockTickSignal  p2p.BlockTickSignal
	signatureChannel *SignatureResponseChannel
}

func (p *Producer) handle(medianPriceMap map[string]api.PricePoint) error {
	p.logger.Debug("making block", "median-prices", medianPriceMap)

	tx, err := makeTx(medianPriceMap)
	if err != nil {
		return fmt.Errorf("failed to create tx: %w", err)
	}

	// broadcast signature request
	sigRequestMsg := SignatureRequestMessage{
		SigHash:     tx.String(),
		MedianPrice: medianPriceMap,
	}

	msg, err := makePriceOracleMessage(signatureRequestCode, &sigRequestMsg)
	if err != nil {
		return fmt.Errorf("failed to make message: %w", err)
	}

	if err := p.Broadcast(p2p.MsgPriceOracle, msg); err != nil {
		return fmt.Errorf("failed to broadcast signature request: %w", err)
	}
	p.logger.Debug(
		"signature request broadcasted",
		"sig-hash", sigRequestMsg.SigHash,
		"median-price", sigRequestMsg.MedianPrice,
	)

	// make bls circuit
	txCid := tx.Cid()

	witnessDIDs := make([]dids.Member, len(p.blockTickSignal.ElectedMembers))
	for i := range p.blockTickSignal.ElectedMembers {
		witnessDIDs[i] = dids.BlsDID(p.blockTickSignal.ElectedMembers[i].Key)
	}

	circuit, err := dids.NewBlsCircuitGenerator(witnessDIDs).Generate(txCid)
	if err != nil {
		return fmt.Errorf("failed to generate bls circuit: %w", err)
	}

	// collect and verify signatures with bls circuit

	ctx, cancel := context.WithTimeout(p.ctx, 15*time.Second)
	defer cancel()

	if err := p.collectSignature(ctx, circuit); err != nil {
		return fmt.Errorf("failed to collect signatures: %w", err)
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
		Sigs: []common.Sig{{
			Algo: "bls-agg",
			Sig:  serializedCircuit.Signature,
			Bv:   serializedCircuit.BitVector,
			Kid:  "",
		}},
	}

	sigBytes, err := common.EncodeDagCbor(sigPackage)
	if err != nil {
		return fmt.Errorf("failed encode DagCbor: %w", err)
	}

	// submit contract
	vscTx := transactionpool.SerializedVSCTransaction{
		Tx:  txCid.Bytes(),
		Sig: sigBytes,
	}

	if err := p.submitToContract(vscTx); err != nil {
		return fmt.Errorf("failed to submit to contract: %w", err)
	}

	return nil
}

func (p *Producer) collectSignature(
	ctx context.Context,
	circuit dids.PartialBlsCircuit,
) error {
	signatureThreshold := (p.blockTickSignal.TotalElectionWeight * 2) / 3
	signedWeight := uint64(0)

	p.logger.Debug(
		"collecting signatures",
		"signature-threshold", signatureThreshold,
	)

	receiver := p.signatureChannel.Open()
	defer p.signatureChannel.Close()

	for signedWeight < signatureThreshold {
		select {
		case <-ctx.Done():
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("context error: %w", err)
			} else {
				return errors.New("signature collection timed out")
			}

		case msg := <-receiver:
			memberIndex := slices.IndexFunc(
				p.blockTickSignal.ElectedMembers,
				func(e elections.ElectionMember) bool {
					return e.Account == msg.Signer
				},
			)

			if memberIndex == -1 {
				p.logger.Debug(
					"invalid witness signature, dropping.",
					"signer", msg.Signer,
				)
				continue
			}

			member := &p.blockTickSignal.ElectedMembers[memberIndex]
			memberDID := dids.BlsDID(member.Key)

			added, err := circuit.AddAndVerify(memberDID, msg.Signature)
			if err != nil {
				p.logger.Error("failed to verify signature", "err", err)
				continue
			}

			if !added {
				p.logger.Debug("invalid signature, signature not added to circuit")
				continue
			}

			signedWeight += p.blockTickSignal.WeightMap[memberIndex]
			p.logger.Debug(
				"received witness signature, appending to witnessSigned",
				"signer", member.Account, "signature", msg.Signature,
			)
		}
	}

	return nil
}

// TODO: implement this function
func (p *Producer) submitToContract(transactionpool.SerializedVSCTransaction) error {
	p.logger.Error("submit to contract not implemented")
	return nil
}
