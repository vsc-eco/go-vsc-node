package oracle

type blockTickSignal struct {
	isBlockProducer bool
	isWitness       bool
}

func makeBlockTickSignal(isBlockProducer, isWitness bool) blockTickSignal {
	return blockTickSignal{isBlockProducer, isWitness}
}
