package oracle

import (
	"context"
	"fmt"
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

	o.chainFlags.Lock()
	o.chainFlags.isBroadcastTickInterval = true
	o.chainFlags.Unlock()

	defer func() {
		o.chainFlags.Lock()
		o.chainFlags.isBroadcastTickInterval = false
		o.chainFlags.Unlock()
	}()

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

	// room for network latency
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	<-ctx.Done()
	return nil
}

// chain relay witness

type chainRelayWitness struct {
	*Oracle
}

// handle implements chainRelayHandler.
func (c *chainRelayWitness) handle(
	blockTickSignal,
	map[string]p2p.BlockRelay,
) error {
	panic("unimplemented")
}
