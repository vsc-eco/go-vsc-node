package oracle

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/threadsafe"
)

type chainRelayHandler interface {
	handle(blockTickSignal, map[string]p2p.BlockRelay) error
}

func (o *Oracle) handleChainRelayTickInterval(sig blockTickSignal) {
	if !sig.isBlockProducer && !sig.isWitness {
		return
	}

	blockMap := o.chainOracle.FetchBlocks()

	var handler chainRelayHandler
	if sig.isBlockProducer {
		handler = &chainRelayProducer{o}
	} else {
		handler = &chainRelayWitness{o}
	}

	if err := handler.handle(sig, blockMap); err != nil {
		o.logger.Error(
			"failed to process chain relay tick interval",
			"is-producer", sig.isBlockProducer,
			"is-witness", sig.isWitness,
		)
	}
}

// chain relay producer

type chainRelayProducer struct{ *Oracle }

// handle implements chainRelayHandler.
func (c *chainRelayProducer) handle(
	sig blockTickSignal,
	blockMap map[string]p2p.BlockRelay,
) error {
	// broadcast block
	oracleBlock, err := p2p.MakeOracleBlock(
		c.conf.Get().HiveUsername,
		c.conf.Get().HiveActiveKey,
		blockMap,
	)
	if err != nil {
		return fmt.Errorf("failed to create oracle block")
	}

	if err := c.BroadcastMessage(p2p.MsgChainRelayBlock, oracleBlock); err != nil {
		return fmt.Errorf("failed to broadcast oracle block: %w", err)
	}

	// collect and verify signatures
	c.logger.Debug("collecting signature", "block-id", oracleBlock.ID)

	sigThreshold := int(math.Ceil(float64(len(sig.electedMembers)) * 2.0 / 3.0))
	oracleBlock.Signatures = make([]string, 0, sigThreshold)
	collector := c.signatureCollector(oracleBlock, sigThreshold)

	// room for network latency
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err = c.chainOracle.SignBlockBuf.Collect(ctx, collector); err != nil {
		return errors.New("failed to meet signature threshold")
	}

	return c.submitToContract(oracleBlock)
}

func (c *chainRelayProducer) signatureCollector(
	oracleBlock *p2p.OracleBlock,
	sigThreshold int,
) threadsafe.CollectFunc[*p2p.OracleBlock] {
	const earlyReturn = false

	return func(ob *p2p.OracleBlock) bool {
		if len(ob.Signatures) != 1 {
			return earlyReturn
		}

		sig := ob.Signatures[0]

		if err := c.validateSignature(oracleBlock, ob.Signatures[0]); err != nil {
			c.logger.Error(
				"failed to validate signature",
				"block-id", oracleBlock.ID,
				"err", err, "signature", sig,
			)
			return earlyReturn
		}
		oracleBlock.Signatures = append(oracleBlock.Signatures, sig)

		return len(oracleBlock.Signatures) >= sigThreshold
	}
}

// chain relay witness

type chainRelayWitness struct{ *Oracle }

// handle implements chainRelayHandler.
func (c *chainRelayWitness) handle(
	sig blockTickSignal,
	localBlockMap map[string]p2p.BlockRelay,
) error {
	// room for network latency
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c.chainOracle.NewBlockBuf.Collect(ctx, func(ob *p2p.OracleBlock) bool {
		return false
	})

	return nil
}
