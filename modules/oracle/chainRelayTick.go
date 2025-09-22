package oracle

import (
	"errors"
	"fmt"
	"math"
	"time"
	"vsc-node/modules/oracle/p2p"
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

	// room for network latency
	c.chainOracle.SignBlockBuf.UnlockTimeout(10 * time.Second)

	sigThreshold := int(math.Ceil(float64(len(sig.electedMembers)) * 2.0 / 3.0))
	oracleBlock.Signatures = make([]string, 0, sigThreshold)

	signedBlocks := c.chainOracle.SignBlockBuf.Slice()
	for _, signedBlock := range signedBlocks {
		// if sig not valid, continue
		signature := signedBlock.Signatures[0]

		// if signature not valid, continue
		oracleBlock.Signatures = append(oracleBlock.Signatures, signature)
	}

	if len(oracleBlock.Signatures) < sigThreshold {
		return errors.New("failed to meet signature threshold")
	}

	return c.submitToContract(oracleBlock)
}

// chain relay witness

type chainRelayWitness struct {
	*Oracle
}

// handle implements chainRelayHandler.
func (c *chainRelayWitness) handle(
	sig blockTickSignal,
	localBlockMap map[string]p2p.BlockRelay,
) error {
	// room for network latency
	c.chainOracle.NewBlockBuf.UnlockTimeout(10 * time.Second)

	panic("unimplemented")
}
