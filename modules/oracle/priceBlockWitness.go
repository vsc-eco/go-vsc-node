package oracle

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"
	"vsc-node/modules/oracle/p2p"
)

var errBlockExpired = errors.New("block expired")

const float64Epsilon = 1e-9

type priceBlockWitness struct{ *Oracle }

func (p *priceBlockWitness) handleSignal(
	sig *blockTickSignal,
	medianPricePoints map[string]pricePoint,
) error {
	// room for network latency
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	<-ctx.Done()

	blocks := p.broadcastPriceBlocks.Slice()

	for _, block := range blocks {
		ok := p.validateBlock(&block, medianPricePoints)
		if !ok {
			p.logger.Debug(
				"block rejected",
				"block-id", block.ID,
				"block-data", block.Data,
			)
			continue
		}

		sig, err := p.signBlock(&block)
		if err != nil {
			return err
		}

		block.Signatures = append(block.Signatures, sig)

		if err := p.BroadcastMessage(p2p.MsgPriceSignature, block); err != nil {
			return err
		}
	}

	return nil
}

// TODO: what do i need to validate?
func (p *priceBlockWitness) validateBlock(
	block *p2p.OracleBlock,
	localMedianPrice map[string]pricePoint,
) bool {
	broadcastedMedianPrices := make(map[string]pricePoint)
	if err := json.Unmarshal(block.Data, &broadcastedMedianPrices); err != nil {
		return false
	}

	for sym, pricePoint := range localMedianPrice {
		bpp, ok := broadcastedMedianPrices[sym]
		if !ok {
			return false
		}

		var (
			priceOk  = float64Eq(pricePoint.price, bpp.price)
			volumeOk = float64Eq(pricePoint.volume, bpp.volume)
		)

		if !priceOk || !volumeOk {
			return false
		}
	}

	return false
}

func (p *priceBlockWitness) signBlock(b *p2p.OracleBlock) (string, error) {
	timeThreshold := time.Now().UTC().Add(-20 * time.Second)

	blockExpired := timeThreshold.After(b.TimeStamp)
	if blockExpired {
		return "", errBlockExpired
	}

	// TODO: implement block verification + signing
	return "signature", nil
}

func float64Eq(a, b float64) bool {
	return math.Abs(a-b) < float64Epsilon
}
