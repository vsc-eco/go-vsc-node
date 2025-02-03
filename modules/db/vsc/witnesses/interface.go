package witnesses

import a "vsc-node/modules/aggregate"

type Witnesses interface {
	a.Plugin
	StoreNodeAnnouncement(nodeId string) error
	SetWitnessUpdate(accountInfo SetWitnessUpdateType) error
	GetLastestWitnesses() ([]Witness, error)
	GetWitnessesAtBlockHeight(bh uint64) ([]Witness, error)
	GetWitnesseByPeerId(peerIds []string) ([]Witness, error)
}
