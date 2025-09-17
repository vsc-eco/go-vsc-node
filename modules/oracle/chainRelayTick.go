package oracle

import (
	"log"
	"vsc-node/modules/oracle/p2p"
)

type chainRelayHandler interface {
	handle(map[string]p2p.BlockRelay) error
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

	if err := handler.handle(blockMap); err != nil {
		log.Println("err", err)
	}
}

// chain relay producer

type chainRelayProducer struct {
	*Oracle
}

// handle implements chainRelayHandler.
func (c *chainRelayProducer) handle(map[string]p2p.BlockRelay) error {
	panic("unimplemented")
}

// chain relay witness

type chainRelayWitness struct {
	*Oracle
}

// handle implements chainRelayHandler.
func (c *chainRelayWitness) handle(map[string]p2p.BlockRelay) error {
	panic("unimplemented")
}
