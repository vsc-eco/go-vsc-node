package oracle

import (
	"context"
	"errors"
	"time"
	"vsc-node/modules/oracle/p2p"
)

var errBlockExpired = errors.New("block expired")

type priceBlockWitness struct{ *Oracle }

func (p *priceBlockWitness) handleSignal(sig *blockTickSignal) error {
	// room for network latency
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	<-ctx.Done()

	blocks := p.broadcastPriceBlocks.Slice()

	for _, block := range blocks {
		sig, err := p.signBlock(&block)
		if err != nil { // invalid block, skip
			continue
		}

		block.Signatures = append(block.Signatures, sig)

		msg := p2p.OracleMessage{Type: p2p.MsgPriceSignature, Data: block}
		if err := p.BroadcastMessage(&msg); err != nil {
			return err
		}
	}

	return nil
}

func (p *priceBlockWitness) signBlock(b *p2p.VSCBlock) (string, error) {
	timeThreshold := time.Now().UTC().Add(-20 * time.Second)

	blockExpired := timeThreshold.After(b.TimeStamp)
	if blockExpired {
		return "", errBlockExpired
	}

	// TODO: implement block verification + signing
	return "signature", nil
}
