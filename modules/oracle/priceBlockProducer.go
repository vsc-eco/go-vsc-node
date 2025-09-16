package oracle

import (
	"context"
	"math"
	"vsc-node/modules/oracle/p2p"
)

type priceBlockProducer struct{ *Oracle }

type aggregatedPricePoints struct {
	prices  []float64
	volumes []float64
}

func (p *priceBlockProducer) handleSignal(
	sig *blockTickSignal,
	medianPricePoints map[string]pricePoint,
) error {
	p.logger.Debug("broadcasting new oracle block with median prices")

	block, err := p2p.MakeVscBlock(
		p.conf.Get().HiveUsername,
		p.conf.Get().HiveActiveKey,
		medianPricePoints,
	)
	if err != nil {
		return err
	}

	if err := p.BroadcastMessage(p2p.MsgPriceBlock, *block); err != nil {
		return err
	}

	return p.pollMedianPriceSignature(sig, block)
}

func (p *priceBlockProducer) pollMedianPriceSignature(
	sig *blockTickSignal,
	block *p2p.OracleBlock,
) error {
	p.broadcastPriceFlags.lock.Lock()
	p.broadcastPriceFlags.isCollectingSignatures = true
	p.broadcastPriceFlags.lock.Unlock()

	// room for network latency
	ctx, cancel := context.WithTimeout(context.Background(), listenDuration)
	defer func() {
		p.broadcastPriceFlags.lock.Lock()
		p.broadcastPriceFlags.isCollectingSignatures = false
		p.broadcastPriceFlags.lock.Unlock()
		cancel()
	}()

	<-ctx.Done()

	p.logger.Debug("collecting signature", "block-id", block.ID)

	sigThreshold := int(math.Ceil(float64(len(sig.electedMembers) * 2 / 3)))
	block.Signatures = make([]string, 0, sigThreshold)

	signedBlocks := p.broadcastPriceSig.Slice()
	for i := range signedBlocks {
		signedBlock := &signedBlocks[i]

		if validateSignedBlock(signedBlock) {
			block.Signatures = append(
				block.Signatures,
				signedBlock.Signatures[0],
			)
		}
	}

	p.submitToContract(block)

	return nil
}

func validateSignedBlock(block *p2p.OracleBlock) bool {
	if len(block.Signatures) != 1 {
		return false
	}

	/*
		if err := v.Struct(block); err != nil {
			return false
		}
	*/

	// TODO: validate signature
	return true
}
