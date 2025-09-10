package oracle

import "vsc-node/modules/db/vsc/elections"

type blockTickSignal struct {
	isBlockProducer bool
	isWitness       bool
	electedMembers  []elections.ElectionMember
}

func makeBlockTickSignal(
	isBlockProducer, isWitness bool,
	electedMembers []elections.ElectionMember,
) blockTickSignal {
	return blockTickSignal{
		isBlockProducer, isWitness, electedMembers,
	}
}
