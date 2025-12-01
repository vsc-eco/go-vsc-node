package witnesses

import a "vsc-node/modules/aggregate"

type Witnesses interface {
	a.Plugin
	StoreNodeAnnouncement(nodeId string) error
	SetWitnessUpdate(accountInfo SetWitnessUpdateType) error
	GetLastestWitnesses(options ...SearchOption) ([]Witness, error)
	GetWitnessesAtBlockHeight(bh uint64, options ...SearchOption) ([]Witness, error)
	GetWitnessesByPeerId(peerIds []string, options ...SearchOption) ([]Witness, error)
	GetWitnessAtHeight(account string, bh *uint64) (*Witness, error)
}

type SearchConfig struct {
	ExpirationBlocks uint64
	BlockHeight      uint64
}

type SearchOption func(cfg *SearchConfig) error

func SearchExpiration(blocks uint64) SearchOption {
	return func(cfg *SearchConfig) error {
		cfg.ExpirationBlocks = blocks
		return nil
	}
}

func SearchHeight(bh uint64) SearchOption {
	return func(cfg *SearchConfig) error {
		cfg.BlockHeight = bh
		return nil
	}
}
